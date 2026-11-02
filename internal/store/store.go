// Package store saves API keys and request logs.
//
// There are two versions behind one interface: Memory (no setup, forgotten on restart,
// great for first tries) and Postgres (permanent, for the deployed site).
//
// Privacy note: only metadata is stored (who, which provider, how long, how many tokens).
// The text of questions and answers is never written to the database.
package store

import (
	"context"
	"errors"
	"sort"
	"time"
)

// ErrNotFound is returned when a key does not exist.
var ErrNotFound = errors.New("not found")

// Key is a gateway API key (what an app uses to call the gateway).
type Key struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	Prefix     string    `json:"prefix"` // first characters, safe to show
	RPMLimit   int       `json:"rpm_limit"`
	DailyLimit int       `json:"daily_limit"`
	Revoked    bool      `json:"revoked"`
	CreatedAt  time.Time `json:"created_at"`
	Hash       string    `json:"-"`
}

// RequestLog is one line of history for one chat request.
type RequestLog struct {
	ID               int64     `json:"id"`
	TS               time.Time `json:"ts"`
	KeyID            int64     `json:"key_id"`
	Provider         string    `json:"provider"`
	Model            string    `json:"model"`
	RequestedModel   string    `json:"requested_model"`
	Status           string    `json:"status"` // "ok" or "error"
	CacheHit         bool      `json:"cache_hit"`
	Stream           bool      `json:"stream"`
	LatencyMs        int64     `json:"latency_ms"`
	PromptTokens     int       `json:"prompt_tokens"`
	CompletionTokens int       `json:"completion_tokens"`
	Attempts         int       `json:"attempts"`
	Tried            string    `json:"tried"`
	Error            string    `json:"error"`
}

// ProviderStat summarises one provider over a time window.
type ProviderStat struct {
	Provider     string  `json:"provider"`
	Requests     int64   `json:"requests"`
	Errors       int64   `json:"errors"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	Tokens       int64   `json:"tokens"`
}

// Point is one bar of the requests-over-time chart.
type Point struct {
	T            time.Time `json:"t"`
	Requests     int64     `json:"requests"`
	CacheHits    int64     `json:"cache_hits"`
	Errors       int64     `json:"errors"`
	AvgLatencyMs float64   `json:"avg_latency_ms"`
}

// Stats summarises a time window.
type Stats struct {
	Requests              int64          `json:"requests"`
	CacheHits             int64          `json:"cache_hits"`
	Errors                int64          `json:"errors"`
	PromptTokens          int64          `json:"prompt_tokens"`           // tokens actually sent to providers
	CompletionTokens      int64          `json:"completion_tokens"`       // tokens actually produced by providers
	SavedPromptTokens     int64          `json:"saved_prompt_tokens"`     // tokens NOT sent thanks to the cache
	SavedCompletionTokens int64          `json:"saved_completion_tokens"` // tokens NOT generated thanks to the cache
	AvgLatencyMs          float64        `json:"avg_latency_ms"`          // provider calls only
	P50LatencyMs          float64        `json:"p50_latency_ms"`
	P95LatencyMs          float64        `json:"p95_latency_ms"`
	AvgCacheLatencyMs     float64        `json:"avg_cache_latency_ms"`
	ByProvider            []ProviderStat `json:"by_provider"`
	Series                []Point        `json:"series"`
}

// Store is the storage interface.
type Store interface {
	CreateKey(ctx context.Context, name, prefix, hash string, rpm, daily int) (Key, error)
	FindKeyByHash(ctx context.Context, hash string) (*Key, error)
	ListKeys(ctx context.Context) ([]Key, error)
	RevokeKey(ctx context.Context, id int64) error

	InsertLog(ctx context.Context, l RequestLog) error
	RecentLogs(ctx context.Context, limit int) ([]RequestLog, error)
	// CountProviderCalls counts requests by a key that actually reached a provider (not cache hits) since `since`.
	CountProviderCalls(ctx context.Context, keyID int64, since time.Time) (int, error)
	Stats(ctx context.Context, since time.Time, bucket time.Duration) (Stats, error)
	PurgeLogs(ctx context.Context, before time.Time) (int64, error)

	Ping(ctx context.Context) error
	Kind() string // "memory" or "postgres"
	Close() error
}

// FillSeries adds empty buckets so a chart has no gaps, and sorts by time.
func FillSeries(points []Point, since, until time.Time, bucket time.Duration) []Point {
	if bucket <= 0 {
		return points
	}
	by := map[int64]Point{}
	for _, p := range points {
		by[p.T.Truncate(bucket).Unix()] = p
	}
	var out []Point
	for t := since.Truncate(bucket); !t.After(until); t = t.Add(bucket) {
		if p, ok := by[t.Unix()]; ok {
			p.T = t
			out = append(out, p)
		} else {
			out = append(out, Point{T: t})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].T.Before(out[j].T) })
	return out
}

func percentile(sorted []float64, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	idx := p * float64(len(sorted)-1)
	lo := int(idx)
	hi := lo + 1
	if hi >= len(sorted) {
		return sorted[lo]
	}
	frac := idx - float64(lo)
	return sorted[lo]*(1-frac) + sorted[hi]*frac
}
