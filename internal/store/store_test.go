package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

// The same checks run against both implementations, so they behave identically.
// The Postgres run needs TEST_DATABASE_URL (it is skipped otherwise).
func stores(t *testing.T) map[string]Store {
	out := map[string]Store{"memory": NewMemory()}
	if url := os.Getenv("TEST_DATABASE_URL"); url != "" {
		pg, err := NewPostgres(context.Background(), url)
		if err != nil {
			t.Fatalf("postgres: %v", err)
		}
		ctx := context.Background()
		pg.db.ExecContext(ctx, "TRUNCATE api_keys, request_logs RESTART IDENTITY")
		t.Cleanup(func() { pg.Close() })
		out["postgres"] = pg
	}
	return out
}

func TestKeys(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			k, err := s.CreateKey(ctx, "app", "gw_abcdef", "hash1", 20, 500)
			if err != nil || k.ID == 0 {
				t.Fatalf("create: %v %+v", err, k)
			}
			got, err := s.FindKeyByHash(ctx, "hash1")
			if err != nil || got.Name != "app" || got.RPMLimit != 20 || got.DailyLimit != 500 || got.Revoked {
				t.Fatalf("find: %v %+v", err, got)
			}
			if _, err := s.FindKeyByHash(ctx, "nope"); !errors.Is(err, ErrNotFound) {
				t.Fatalf("unknown hash: %v", err)
			}
			if err := s.RevokeKey(ctx, k.ID); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.FindKeyByHash(ctx, "hash1"); !got.Revoked {
				t.Fatal("key should be revoked")
			}
			if err := s.RevokeKey(ctx, 9999); !errors.Is(err, ErrNotFound) {
				t.Fatalf("revoke unknown: %v", err)
			}
			list, _ := s.ListKeys(ctx)
			if len(list) != 1 {
				t.Fatalf("list: %+v", list)
			}
		})
	}
}

func TestLogsAndStats(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			now := time.Now()
			logs := []RequestLog{
				{TS: now.Add(-10 * time.Minute), KeyID: 1, Provider: "groq", Status: "ok", LatencyMs: 100, PromptTokens: 10, CompletionTokens: 20},
				{TS: now.Add(-9 * time.Minute), KeyID: 1, Provider: "groq", Status: "ok", LatencyMs: 300, PromptTokens: 10, CompletionTokens: 20},
				{TS: now.Add(-8 * time.Minute), KeyID: 1, Provider: "gemini", Status: "ok", LatencyMs: 200, PromptTokens: 5, CompletionTokens: 5},
				{TS: now.Add(-7 * time.Minute), KeyID: 1, Provider: "groq", Status: "ok", CacheHit: true, LatencyMs: 2, PromptTokens: 10, CompletionTokens: 20},
				{TS: now.Add(-6 * time.Minute), KeyID: 2, Provider: "", Status: "error", LatencyMs: 50, Error: "all failed"},
				{TS: now.Add(-3 * time.Hour), KeyID: 1, Provider: "groq", Status: "ok", LatencyMs: 999, PromptTokens: 1, CompletionTokens: 1},
			}
			for _, l := range logs {
				if err := s.InsertLog(ctx, l); err != nil {
					t.Fatal(err)
				}
			}

			recent, err := s.RecentLogs(ctx, 2)
			if err != nil || len(recent) != 2 || recent[0].ID < recent[1].ID {
				t.Fatalf("recent (newest first): %v %+v", err, recent)
			}

			n, _ := s.CountProviderCalls(ctx, 1, now.Add(-time.Hour))
			if n != 3 { // 3 real provider calls by key 1 in the last hour (cache hit and old row excluded)
				t.Fatalf("CountProviderCalls = %d, want 3", n)
			}

			st, err := s.Stats(ctx, now.Add(-time.Hour), 10*time.Minute)
			if err != nil {
				t.Fatal(err)
			}
			if st.Requests != 5 || st.CacheHits != 1 || st.Errors != 1 {
				t.Fatalf("counts: %+v", st)
			}
			if st.PromptTokens != 25 || st.CompletionTokens != 45 || st.SavedPromptTokens != 10 || st.SavedCompletionTokens != 20 {
				t.Fatalf("tokens: %+v", st)
			}
			if st.AvgLatencyMs != 200 || st.P50LatencyMs != 200 || st.P95LatencyMs < 280 || st.P95LatencyMs > 300 {
				t.Fatalf("latency avg=%v p50=%v p95=%v", st.AvgLatencyMs, st.P50LatencyMs, st.P95LatencyMs)
			}
			if st.AvgCacheLatencyMs != 2 {
				t.Fatalf("cache latency %v", st.AvgCacheLatencyMs)
			}
			if len(st.ByProvider) != 3 || st.ByProvider[0].Provider != "groq" || st.ByProvider[0].Requests != 2 {
				t.Fatalf("by provider: %+v", st.ByProvider)
			}
			var total int64
			for _, p := range st.Series {
				total += p.Requests
			}
			if total != 5 {
				t.Fatalf("series should add up to 5 requests, got %d (%+v)", total, st.Series)
			}

			filled := FillSeries(st.Series, now.Add(-time.Hour), now, 10*time.Minute)
			if len(filled) < 6 || len(filled) > 8 {
				t.Fatalf("filled series length %d", len(filled))
			}

			purged, err := s.PurgeLogs(ctx, now.Add(-2*time.Hour))
			if err != nil || purged != 1 {
				t.Fatalf("purge: %d %v", purged, err)
			}
			if err := s.Ping(ctx); err != nil {
				t.Fatal(err)
			}
		})
	}
}
