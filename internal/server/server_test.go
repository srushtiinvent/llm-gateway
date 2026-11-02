package server

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/cache"
	"github.com/srushtiinvent/llm-gateway/internal/config"
	"github.com/srushtiinvent/llm-gateway/internal/limiter"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
	"github.com/srushtiinvent/llm-gateway/internal/router"
	"github.com/srushtiinvent/llm-gateway/internal/store"
)

const adminTok = "test-admin"

type harness struct {
	t   *testing.T
	ts  *httptest.Server
	srv *Server
	rt  *router.Router
	st  store.Store
}

type hopts struct {
	maxConcurrent, maxWaiting int
	latency                   time.Duration
	limits                    []quota.Limits // per provider (a, b, c)
	st                        store.Store
}

func newHarness(t *testing.T, o hopts) *harness {
	t.Helper()
	if o.maxConcurrent == 0 {
		o.maxConcurrent = 8
	}
	if o.maxWaiting == 0 {
		o.maxWaiting = 20
	}
	if o.st == nil {
		o.st = store.NewMemory()
	}
	names := []string{"alpha", "beta", "gamma"}
	var entries []*router.Entry
	for i, n := range names {
		var l quota.Limits
		if i < len(o.limits) {
			l = o.limits[i]
		}
		entries = append(entries, router.NewEntry(router.EntryConfig{
			Provider: provider.NewMock(n, n+"-model", o.latency), Limits: l, BreakerThreshold: 3, BreakerOpenFor: time.Minute, Mock: true,
		}))
	}
	opts := router.DefaultOptions()
	opts.MaxRetries = 0
	rt := router.New(entries, opts)
	cfg := &config.Config{DefaultKeyRPM: 20, DefaultKeyDaily: 100, AllowedOrigins: "*", PriceInPerM: 1, PriceOutPerM: 2}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := New(Deps{
		Config: cfg, Store: o.st, Router: rt, Cache: cache.New(100, time.Hour),
		Queue:   limiter.NewQueue(o.maxConcurrent, o.maxWaiting, 200*time.Millisecond),
		Limiter: limiter.New(), Auth: auth.New(o.st, adminTok), Budget: auth.NewBudget(o.st), Logger: logger,
	})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(func() { ts.Close(); srv.Close() })
	return &harness{t: t, ts: ts, srv: srv, rt: rt, st: o.st}
}

func (h *harness) do(method, path, token string, body any, hdr map[string]string) (*http.Response, []byte) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest(method, h.ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (h *harness) newKey(name string, rpm, daily int) string {
	h.t.Helper()
	resp, b := h.do("POST", "/admin/keys", adminTok, map[string]any{"name": name, "rpm": rpm, "daily": daily}, nil)
	if resp.StatusCode != 201 {
		h.t.Fatalf("create key: %d %s", resp.StatusCode, b)
	}
	var out struct{ Key string }
	json.Unmarshal(b, &out)
	return out.Key
}

type chatOut struct {
	Choices []struct {
		Message struct{ Content string }
	}
	Usage   struct{ PromptTokens, CompletionTokens int }
	Gateway struct {
		Provider string
		Cache    string
		Attempts int
		Tried    []string
	}
	Error struct{ Code, Message string }
}

func (h *harness) chat(key, model, text string, hdr map[string]string) (*http.Response, chatOut) {
	h.t.Helper()
	resp, b := h.do("POST", "/v1/chat/completions", key,
		map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": text}}}, hdr)
	var out chatOut
	if err := json.Unmarshal(b, &out); err != nil {
		h.t.Fatalf("bad JSON %q: %v", b, err)
	}
	return resp, out
}

func (h *harness) fault(name, mode string) {
	h.t.Helper()
	resp, b := h.do("POST", "/admin/providers/"+name+"/fault", adminTok, map[string]string{"mode": mode}, nil)
	if resp.StatusCode != 200 {
		h.t.Fatalf("fault: %d %s", resp.StatusCode, b)
	}
}

func TestAuthRequired(t *testing.T) {
	h := newHarness(t, hopts{})
	if resp, out := h.chat("", "auto", "hi", nil); resp.StatusCode != 401 || out.Error.Code != "missing_api_key" {
		t.Fatalf("no key: %d %+v", resp.StatusCode, out.Error)
	}
	if resp, out := h.chat("gw_wrong", "auto", "hi", nil); resp.StatusCode != 401 || out.Error.Code != "invalid_api_key" {
		t.Fatalf("bad key: %d %+v", resp.StatusCode, out.Error)
	}
	if resp, _ := h.do("GET", "/admin/overview", "", nil, nil); resp.StatusCode != 401 {
		t.Fatal("admin endpoints need the admin token")
	}
	if resp, _ := h.do("GET", "/admin/overview", "not-the-token", nil, nil); resp.StatusCode != 401 {
		t.Fatal("wrong admin token")
	}
	key := h.newKey("app", 0, 0)
	if resp, _ := h.do("GET", "/admin/overview", key, nil, nil); resp.StatusCode != 401 {
		t.Fatal("a normal gateway key must not open admin endpoints")
	}
}

func TestChatCacheAndHeaders(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)

	resp, out := h.chat(key, "auto", "What is Go?", nil)
	if resp.StatusCode != 200 || out.Gateway.Provider != "alpha" || out.Gateway.Cache != "miss" || out.Gateway.Attempts != 1 {
		t.Fatalf("first call: %d %+v", resp.StatusCode, out.Gateway)
	}
	if resp.Header.Get("X-Gateway-Provider") != "alpha" || resp.Header.Get("X-Gateway-Cache") != "miss" {
		t.Fatalf("headers: %v", resp.Header)
	}
	if !strings.Contains(out.Choices[0].Message.Content, "What is Go?") {
		t.Fatalf("answer: %q", out.Choices[0].Message.Content)
	}

	resp, out = h.chat(key, "auto", "What is Go?", nil)
	if out.Gateway.Cache != "hit" || out.Gateway.Attempts != 0 || resp.Header.Get("X-Gateway-Cache") != "hit" {
		t.Fatalf("second identical call must come from the cache: %+v", out.Gateway)
	}
	_, out = h.chat(key, "auto", "What is Go?", map[string]string{"X-Gateway-Cache": "bypass"})
	if out.Gateway.Cache != "miss" {
		t.Fatal("bypass header must skip the cache")
	}
	_, out = h.chat(key, "beta", "What is Go?", nil)
	if out.Gateway.Provider != "beta" || out.Gateway.Cache != "miss" {
		t.Fatalf("pinning a provider uses its own cache entry: %+v", out.Gateway)
	}
	_, out = h.chat(key, "gpt-4o", "What is Go?", nil)
	if out.Gateway.Cache != "hit" {
		t.Fatal("unknown model names behave like auto (and share its cache)")
	}
}

func TestFallbackWhenProviderIsDown(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	h.fault("alpha", "down")
	resp, out := h.chat(key, "auto", "ping", nil)
	if resp.StatusCode != 200 || out.Gateway.Provider != "beta" {
		t.Fatalf("beta should answer when alpha is down: %d %+v", resp.StatusCode, out.Gateway)
	}
	if strings.Join(out.Gateway.Tried, ",") != "alpha:server_error,beta:ok" {
		t.Fatalf("tried: %v", out.Gateway.Tried)
	}
	h.fault("alpha", "none")
	_, out = h.chat(key, "auto", "ping again", nil)
	if out.Gateway.Provider != "alpha" {
		t.Fatalf("alpha should be back: %+v", out.Gateway)
	}
}

func TestConversationHistoryAcrossProviders(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	h.fault("alpha", "ratelimited") // alpha "ran out of quota"
	resp, b := h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "auto", "messages": []map[string]string{
		{"role": "user", "content": "my name is Srushti"},
		{"role": "assistant", "content": "Nice to meet you!"},
		{"role": "user", "content": "what is my name?"},
	}}, nil)
	var out chatOut
	json.Unmarshal(b, &out)
	if resp.StatusCode != 200 || out.Gateway.Provider != "beta" || !strings.Contains(out.Choices[0].Message.Content, "Srushti") {
		t.Fatalf("beta must know the name from the history: %d %s", resp.StatusCode, b)
	}
}

func TestAllProvidersDown(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	for _, n := range []string{"alpha", "beta", "gamma"} {
		h.fault(n, "down")
	}
	resp, out := h.chat(key, "auto", "anyone?", nil)
	if resp.StatusCode != 503 || out.Error.Code != "all_providers_failed" {
		t.Fatalf("%d %+v", resp.StatusCode, out.Error)
	}
}

func TestFreeQuotaExhaustedGives429WithRetryAfter(t *testing.T) {
	one := quota.Limits{RPM: 1}
	h := newHarness(t, hopts{limits: []quota.Limits{one, one, one}})
	key := h.newKey("app", 0, 0)
	for i, want := range []string{"alpha", "beta", "gamma"} { // each provider's single free request, in order
		_, out := h.chat(key, "auto", "q"+string(rune('a'+i)), nil)
		if out.Gateway.Provider != want {
			t.Fatalf("request %d: %+v, want %s", i, out.Gateway, want)
		}
	}
	resp, out := h.chat(key, "auto", "qz", nil)
	if resp.StatusCode != 429 || out.Error.Code != "free_quota_exhausted" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("all quotas used: %d %+v retry-after=%q", resp.StatusCode, out.Error, resp.Header.Get("Retry-After"))
	}
	_, out = h.chat(key, "auto", "qa", nil) // but a repeated question still works: it comes from the cache
	if out.Gateway.Cache != "hit" {
		t.Fatal("the cache keeps answering even when every free quota is used up")
	}
}

func TestPerKeyRateLimitAndDailyBudget(t *testing.T) {
	h := newHarness(t, hopts{})
	limited := h.newKey("limited", 2, 0)
	for i := 0; i < 2; i++ {
		if resp, _ := h.chat(limited, "auto", "same", nil); resp.StatusCode != 200 {
			t.Fatalf("request %d should pass", i)
		}
	}
	resp, out := h.chat(limited, "auto", "same", nil)
	if resp.StatusCode != 429 || out.Error.Code != "rate_limit_exceeded" || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("third request in a minute: %d %+v", resp.StatusCode, out.Error)
	}
	other := h.newKey("other", 0, 0)
	if resp, _ := h.chat(other, "auto", "same", nil); resp.StatusCode != 200 {
		t.Fatal("another key must not be affected")
	}

	budget := h.newKey("budget", 0, 2)
	for i := 0; i < 2; i++ {
		h.chat(budget, "auto", "unique "+string(rune('a'+i)), nil)
	}
	resp, out = h.chat(budget, "auto", "unique c", nil)
	if resp.StatusCode != 429 || out.Error.Code != "daily_budget_exceeded" {
		t.Fatalf("daily budget: %d %+v", resp.StatusCode, out.Error)
	}
	if resp, out := h.chat(budget, "auto", "unique a", nil); resp.StatusCode != 200 || out.Gateway.Cache != "hit" {
		t.Fatalf("a cache hit must still work after the daily budget is used up: %d", resp.StatusCode)
	}
}

func TestBackpressureWhenOverloaded(t *testing.T) {
	h := newHarness(t, hopts{maxConcurrent: 1, maxWaiting: 1, latency: 400 * time.Millisecond})
	key := h.newKey("app", 0, 0)
	var mu sync.Mutex
	codes := map[int]int{}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, _ := h.chat(key, "auto", "slow question "+string(rune('a'+i)), nil)
			mu.Lock()
			codes[resp.StatusCode]++
			mu.Unlock()
		}(i)
		time.Sleep(30 * time.Millisecond)
	}
	wg.Wait()
	if codes[503] < 1 || codes[200] < 1 {
		t.Fatalf("with 1 slot and 1 waiting spot, 4 quick requests must see some 503s and some 200s: %v", codes)
	}
}

func TestStreaming(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	read := func(text string) (events []string, hdr http.Header) {
		b, _ := json.Marshal(map[string]any{"model": "auto", "stream": true, "messages": []map[string]string{{"role": "user", "content": text}}})
		req, _ := http.NewRequest("POST", h.ts.URL+"/v1/chat/completions", bytes.NewReader(b))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			t.Fatalf("status %d type %s", resp.StatusCode, resp.Header.Get("Content-Type"))
		}
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if l := sc.Text(); strings.HasPrefix(l, "data: ") {
				events = append(events, strings.TrimPrefix(l, "data: "))
			}
		}
		return events, resp.Header
	}
	assemble := func(events []string) string {
		var sb strings.Builder
		for _, e := range events {
			if e == "[DONE]" {
				continue
			}
			var c struct {
				Choices []struct{ Delta struct{ Content string } }
			}
			json.Unmarshal([]byte(e), &c)
			for _, ch := range c.Choices {
				sb.WriteString(ch.Delta.Content)
			}
		}
		return sb.String()
	}

	ev, hdr := read("tell me a story")
	if len(ev) < 4 || ev[len(ev)-1] != "[DONE]" || hdr.Get("X-Gateway-Cache") != "miss" || hdr.Get("X-Gateway-Provider") != "alpha" {
		t.Fatalf("stream shape: %d events, last=%q hdr=%v", len(ev), ev[len(ev)-1], hdr)
	}
	first := assemble(ev)
	if !strings.Contains(first, "tell me a story") {
		t.Fatalf("assembled: %q", first)
	}

	ev, hdr = read("tell me a story") // same question again: replayed from the cache
	if hdr.Get("X-Gateway-Cache") != "hit" || ev[len(ev)-1] != "[DONE]" || assemble(ev) != first {
		t.Fatalf("cached replay mismatch: hdr=%v %q", hdr, assemble(ev))
	}

	h.fault("alpha", "down") // streaming falls back too, as long as nothing was sent yet
	_, hdr = read("another story")
	if hdr.Get("X-Gateway-Provider") != "beta" {
		t.Fatalf("stream fallback: %v", hdr)
	}
}

func TestValidation(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	cases := []struct {
		name string
		body string
		code string
	}{
		{"not json", `{nope`, "invalid_json"},
		{"no messages", `{"model":"auto","messages":[]}`, "invalid_request"},
		{"bad role", `{"messages":[{"role":"robot","content":"x"}]}`, "invalid_request"},
		{"bad temperature", `{"messages":[{"role":"user","content":"x"}],"temperature":5}`, "invalid_request"},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", h.ts.URL+"/v1/chat/completions", strings.NewReader(c.body))
		req.Header.Set("Authorization", "Bearer "+key)
		resp, _ := http.DefaultClient.Do(req)
		var out chatOut
		json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if resp.StatusCode != 400 || out.Error.Code != c.code {
			t.Errorf("%s: %d %+v", c.name, resp.StatusCode, out.Error)
		}
	}
}

func TestRevokedKeyStopsWorking(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("temp", 0, 0)
	if resp, _ := h.chat(key, "auto", "works", nil); resp.StatusCode != 200 {
		t.Fatal("key should work")
	}
	resp, b := h.do("GET", "/admin/keys", adminTok, nil, nil)
	var list struct{ Keys []store.Key }
	json.Unmarshal(b, &list)
	if resp.StatusCode != 200 || len(list.Keys) != 1 || strings.Contains(string(b), key) {
		t.Fatalf("key list must exist and never contain the secret: %s", b)
	}
	if resp, _ := h.do("POST", "/admin/keys/1/revoke", adminTok, nil, nil); resp.StatusCode != 200 {
		t.Fatal("revoke failed")
	}
	if resp, _ := h.chat(key, "auto", "works", nil); resp.StatusCode != 401 {
		t.Fatalf("revoked key must be refused, got %d", resp.StatusCode)
	}
}

func TestOverviewLogsAndFaultValidation(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	h.chat(key, "auto", "one", nil)
	h.chat(key, "auto", "one", nil) // cache hit
	h.fault("beta", "down")
	h.chat(key, "beta", "two", nil)    // pinned to a broken provider: error
	time.Sleep(150 * time.Millisecond) // logs are written in the background

	resp, b := h.do("GET", "/admin/overview?window=1h", adminTok, nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("overview %d %s", resp.StatusCode, b)
	}
	var o struct {
		Window string
		Stats  struct {
			Requests  int64
			CacheHits int64 `json:"cache_hits"`
			Errors    int64
			Series    []struct{ Requests int64 }
		}
		CacheHitRate float64 `json:"cache_hit_rate"`
		Providers    []struct {
			Name, Breaker, Fault string
		}
		Cache struct{ Hits int64 }
		Queue struct {
			MaxConcurrent int `json:"max_concurrent"`
		}
	}
	json.Unmarshal(b, &o)
	if o.Stats.Requests != 3 || o.Stats.CacheHits != 1 || o.Stats.Errors != 1 {
		t.Fatalf("stats: %+v", o.Stats)
	}
	if o.CacheHitRate < 0.33 || o.CacheHitRate > 0.34 || len(o.Providers) != 3 || o.Providers[1].Fault != "down" || len(o.Stats.Series) < 50 {
		t.Fatalf("overview: %s", b)
	}

	_, b = h.do("GET", "/admin/logs?limit=10", adminTok, nil, nil)
	var logs struct{ Logs []store.RequestLog }
	json.Unmarshal(b, &logs)
	if len(logs.Logs) != 3 || logs.Logs[0].Error == "" {
		t.Fatalf("logs (newest first, newest failed): %s", b)
	}
	if strings.Contains(string(b), "two") && strings.Contains(string(b), "content") {
		t.Fatal("prompts must not be stored in logs")
	}

	if resp, _ := h.do("POST", "/admin/providers/nope/fault", adminTok, map[string]string{"mode": "down"}, nil); resp.StatusCode != 404 {
		t.Fatal("unknown provider")
	}
	if resp, _ := h.do("POST", "/admin/providers/beta/fault", adminTok, map[string]string{"mode": "explode"}, nil); resp.StatusCode != 400 {
		t.Fatal("unknown fault mode")
	}
}

func TestPublicEndpointsAndCORS(t *testing.T) {
	h := newHarness(t, hopts{})
	if resp, _ := h.do("GET", "/healthz", "", nil, nil); resp.StatusCode != 200 {
		t.Fatal("healthz")
	}
	if resp, _ := h.do("GET", "/readyz", "", nil, nil); resp.StatusCode != 200 {
		t.Fatal("readyz")
	}
	resp, b := h.do("GET", "/v1/models", "", nil, nil)
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"auto"`) || !strings.Contains(string(b), `"alpha"`) {
		t.Fatalf("models: %s", b)
	}
	resp, _ = h.do("OPTIONS", "/v1/chat/completions", "", nil, map[string]string{"Origin": "http://localhost:5173", "Access-Control-Request-Method": "POST"})
	if resp.StatusCode != 204 || resp.Header.Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("CORS preflight: %d %v", resp.StatusCode, resp.Header)
	}
}

func TestSwitchingFaultOffRepairsTheProvider(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	h.fault("alpha", "ratelimited") // the gateway learns "alpha is rate limited" and pauses it
	if _, out := h.chat(key, "auto", "one", nil); out.Gateway.Provider != "beta" {
		t.Fatalf("beta expected, got %+v", out.Gateway)
	}
	h.fault("alpha", "none") // repair: alpha must be usable again immediately
	if _, out := h.chat(key, "auto", "two", nil); out.Gateway.Provider != "alpha" {
		t.Fatalf("alpha should be back right away, got %+v", out.Gateway)
	}
}
