// Command loadtest sends many requests to the gateway at once and prints the numbers you
// can put on a resume: throughput, latency percentiles, cache hit rate, fallbacks, errors.
//
//	go run ./cmd/loadtest -key gw_yourkey -n 300 -c 20 -prompts 40
//
// Tip: run it against MOCK_PROVIDERS=true to measure the gateway itself (how much time it adds),
// and against real providers to see end-to-end behaviour. Say which one you measured!
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type result struct {
	status   int
	latency  time.Duration
	cache    string
	provider string
	attempts int
	err      error
}

var topics = []string{"a mutex", "a semaphore", "a hash map", "a B-tree", "a bloom filter", "a circuit breaker", "a token bucket",
	"consistent hashing", "a write-ahead log", "MVCC", "a trie", "a skip list", "an LRU cache", "backpressure", "idempotency",
	"eventual consistency", "a load balancer", "a reverse proxy", "a thread pool", "an event loop", "a b+ tree", "sharding",
	"a heap", "garbage collection", "a deadlock", "a race condition", "a monotonic clock", "a quorum", "a gossip protocol",
	"a merkle tree", "a ring buffer", "a rate limiter", "exponential backoff", "a DNS lookup", "TCP slow start", "a CDN",
	"a database index", "a transaction", "a two-phase commit", "a vector clock"}

func main() {
	url := flag.String("url", "http://localhost:8080", "gateway address")
	key := flag.String("key", os.Getenv("GATEWAY_KEY"), "gateway API key (or set GATEWAY_KEY)")
	n := flag.Int("n", 200, "total number of requests")
	c := flag.Int("c", 10, "how many requests at the same time")
	prompts := flag.Int("prompts", 30, "how many different questions to cycle through (fewer = more cache hits)")
	model := flag.String("model", "auto", "model field: auto, fastest, or a provider name")
	stream := flag.Bool("stream", false, "use streaming responses")
	timeout := flag.Duration("timeout", 60*time.Second, "per-request timeout")
	jsonOut := flag.String("json", "", "also write the summary as JSON to this file")
	flag.Parse()

	if *key == "" {
		fmt.Fprintln(os.Stderr, "Please pass -key gw_... (create one with POST /admin/keys, see README)")
		os.Exit(2)
	}
	pool := make([]string, *prompts)
	for i := range pool {
		pool[i] = fmt.Sprintf("In two sentences, explain %s. (variant %d)", topics[i%len(topics)], i/len(topics))
	}

	client := &http.Client{Timeout: *timeout, Transport: &http.Transport{MaxIdleConnsPerHost: *c + 2}}
	jobs := make(chan int)
	results := make([]result, *n)
	var wg sync.WaitGroup
	start := time.Now()

	for w := 0; w < *c; w++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			rnd := rand.New(rand.NewSource(seed))
			for i := range jobs {
				results[i] = one(client, *url, *key, *model, pool[rnd.Intn(len(pool))], *stream)
			}
		}(int64(w) + time.Now().UnixNano())
	}
	for i := 0; i < *n; i++ {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)

	sum := summarize(results, elapsed)
	sum.print(*n, *c, *prompts, *model, *stream)
	if *jsonOut != "" {
		b, _ := json.MarshalIndent(sum, "", "  ")
		_ = os.WriteFile(*jsonOut, b, 0o644)
		fmt.Println("\nSummary also saved to", *jsonOut)
	}
}

func one(client *http.Client, url, key, model, prompt string, stream bool) result {
	body, _ := json.Marshal(map[string]any{"model": model, "stream": stream,
		"messages": []map[string]string{{"role": "user", "content": prompt}}})
	req, _ := http.NewRequest("POST", url+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	t0 := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return result{err: err, latency: time.Since(t0)}
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	r := result{status: resp.StatusCode, latency: time.Since(t0),
		cache: resp.Header.Get("X-Gateway-Cache"), provider: resp.Header.Get("X-Gateway-Provider")}
	fmt.Sscan(resp.Header.Get("X-Gateway-Attempts"), &r.attempts)
	return r
}

type pcts struct {
	Count int     `json:"count"`
	AvgMs float64 `json:"avg_ms"`
	P50Ms float64 `json:"p50_ms"`
	P95Ms float64 `json:"p95_ms"`
	P99Ms float64 `json:"p99_ms"`
	MaxMs float64 `json:"max_ms"`
}

type summary struct {
	Requests     int            `json:"requests"`
	DurationSec  float64        `json:"duration_sec"`
	Throughput   float64        `json:"requests_per_sec"`
	Status       map[string]int `json:"status_counts"`
	SuccessRate  float64        `json:"success_rate"`
	CacheHitRate float64        `json:"cache_hit_rate"`
	Fallbacks    int            `json:"requests_needing_extra_attempts"`
	Providers    map[string]int `json:"served_by_provider"`
	All          pcts           `json:"latency_all_successful"`
	Miss         pcts           `json:"latency_provider_calls"`
	Hit          pcts           `json:"latency_cache_hits"`
}

func calc(d []time.Duration) pcts {
	if len(d) == 0 {
		return pcts{}
	}
	sort.Slice(d, func(i, j int) bool { return d[i] < d[j] })
	ms := func(x time.Duration) float64 { return float64(x.Microseconds()) / 1000 }
	at := func(p float64) float64 { return ms(d[int(p*float64(len(d)-1)+0.5)]) }
	var sum time.Duration
	for _, x := range d {
		sum += x
	}
	return pcts{Count: len(d), AvgMs: ms(sum) / float64(len(d)), P50Ms: at(0.5), P95Ms: at(0.95), P99Ms: at(0.99), MaxMs: ms(d[len(d)-1])}
}

func summarize(rs []result, elapsed time.Duration) summary {
	s := summary{Requests: len(rs), DurationSec: elapsed.Seconds(), Status: map[string]int{}, Providers: map[string]int{}}
	var all, miss, hit []time.Duration
	ok := 0
	for _, r := range rs {
		key := fmt.Sprint(r.status)
		if r.err != nil {
			key = "network_error"
		}
		s.Status[key]++
		if r.err != nil || r.status != 200 {
			continue
		}
		ok++
		all = append(all, r.latency)
		if r.cache == "hit" {
			hit = append(hit, r.latency)
		} else {
			miss = append(miss, r.latency)
			if r.attempts > 1 {
				s.Fallbacks++
			}
		}
		s.Providers[r.provider]++
	}
	if len(rs) > 0 {
		s.SuccessRate = float64(ok) / float64(len(rs))
		s.Throughput = float64(len(rs)) / elapsed.Seconds()
	}
	if ok > 0 {
		s.CacheHitRate = float64(len(hit)) / float64(ok)
	}
	s.All, s.Miss, s.Hit = calc(all), calc(miss), calc(hit)
	return s
}

func (s summary) print(n, c, prompts int, model string, stream bool) {
	fmt.Println()
	fmt.Println("  Load test results")
	fmt.Println("  -----------------")
	fmt.Printf("  %d requests, %d at a time, %d different questions, model=%s, stream=%v\n", n, c, prompts, model, stream)
	fmt.Printf("  Took %.1fs  ->  %.1f requests/second\n", s.DurationSec, s.Throughput)
	var codes []string
	for k := range s.Status {
		codes = append(codes, k)
	}
	sort.Strings(codes)
	var parts []string
	for _, k := range codes {
		parts = append(parts, fmt.Sprintf("%s: %d", k, s.Status[k]))
	}
	fmt.Printf("  Results by HTTP status: %s\n", strings.Join(parts, ", "))
	fmt.Printf("  Success rate: %.1f%%   Cache hit rate: %.1f%%   Needed extra attempts (retry/fallback): %d\n", s.SuccessRate*100, s.CacheHitRate*100, s.Fallbacks)
	fmt.Println("  Served by:", fmtMap(s.Providers))
	row := func(name string, p pcts) {
		if p.Count == 0 {
			return
		}
		fmt.Printf("  %-26s n=%-5d avg %7.1f ms   p50 %7.1f   p95 %7.1f   p99 %7.1f   max %7.1f\n", name, p.Count, p.AvgMs, p.P50Ms, p.P95Ms, p.P99Ms, p.MaxMs)
	}
	fmt.Println()
	row("All successful requests", s.All)
	row("Provider calls (cache miss)", s.Miss)
	row("Cache hits", s.Hit)
	fmt.Println()
	if s.Status["429"] > 0 {
		fmt.Println("  Note: 429 means a rate limit was reached (your key's per-minute limit, daily budget, or free quotas).")
		fmt.Println("  That is the gateway protecting you. For a bigger test, create a key with higher limits.")
	}
}

func fmtMap(m map[string]int) string {
	var keys []string
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", k, m[k]))
	}
	return strings.Join(parts, ", ")
}
