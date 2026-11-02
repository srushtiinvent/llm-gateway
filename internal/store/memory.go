package store

import (
	"context"
	"sort"
	"sync"
	"time"
)

// Memory keeps everything in RAM. Nothing survives a restart.
type Memory struct {
	mu     sync.Mutex
	keys   []Key
	logs   []RequestLog
	nextK  int64
	nextL  int64
	maxLog int
}

// NewMemory creates an empty in-memory store.
func NewMemory() *Memory { return &Memory{nextK: 1, nextL: 1, maxLog: 20000} }

func (m *Memory) CreateKey(_ context.Context, name, prefix, hash string, rpm, daily int) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k := Key{ID: m.nextK, Name: name, Prefix: prefix, Hash: hash, RPMLimit: rpm, DailyLimit: daily, CreatedAt: time.Now()}
	m.nextK++
	m.keys = append(m.keys, k)
	return k, nil
}

func (m *Memory) FindKeyByHash(_ context.Context, hash string) (*Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, k := range m.keys {
		if k.Hash == hash {
			c := k
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) ListKeys(_ context.Context) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]Key(nil), m.keys...), nil
}

func (m *Memory) RevokeKey(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.keys {
		if m.keys[i].ID == id {
			m.keys[i].Revoked = true
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) InsertLog(_ context.Context, l RequestLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.ID = m.nextL
	m.nextL++
	if l.TS.IsZero() {
		l.TS = time.Now()
	}
	m.logs = append(m.logs, l)
	if len(m.logs) > m.maxLog {
		m.logs = append([]RequestLog(nil), m.logs[len(m.logs)-m.maxLog:]...)
	}
	return nil
}

func (m *Memory) RecentLogs(_ context.Context, limit int) ([]RequestLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if limit <= 0 || limit > len(m.logs) {
		limit = len(m.logs)
	}
	out := make([]RequestLog, 0, limit)
	for i := len(m.logs) - 1; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.logs[i])
	}
	return out, nil
}

func (m *Memory) CountProviderCalls(_ context.Context, keyID int64, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, l := range m.logs {
		if l.KeyID == keyID && !l.CacheHit && !l.TS.Before(since) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) Stats(_ context.Context, since time.Time, bucket time.Duration) (Stats, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var st Stats
	var lat []float64
	var latSum, cacheLatSum float64
	prov := map[string]*ProviderStat{}
	provLatSum := map[string]float64{}
	provOK := map[string]int64{}
	series := map[int64]*Point{}
	seriesLatSum := map[int64]float64{}
	seriesLatN := map[int64]int64{}

	for _, l := range m.logs {
		if l.TS.Before(since) {
			continue
		}
		st.Requests++
		b := l.TS.Truncate(bucket).Unix()
		pt := series[b]
		if pt == nil {
			pt = &Point{T: l.TS.Truncate(bucket)}
			series[b] = pt
		}
		pt.Requests++
		if l.CacheHit {
			st.CacheHits++
			pt.CacheHits++
			st.SavedPromptTokens += int64(l.PromptTokens)
			st.SavedCompletionTokens += int64(l.CompletionTokens)
			cacheLatSum += float64(l.LatencyMs)
			continue
		}
		name := l.Provider
		ps := prov[name]
		if ps == nil {
			ps = &ProviderStat{Provider: name}
			prov[name] = ps
		}
		ps.Requests++
		ps.Tokens += int64(l.PromptTokens + l.CompletionTokens)
		if l.Status == "error" {
			st.Errors++
			pt.Errors++
			ps.Errors++
			continue
		}
		st.PromptTokens += int64(l.PromptTokens)
		st.CompletionTokens += int64(l.CompletionTokens)
		lat = append(lat, float64(l.LatencyMs))
		latSum += float64(l.LatencyMs)
		provLatSum[name] += float64(l.LatencyMs)
		provOK[name]++
		seriesLatSum[b] += float64(l.LatencyMs)
		seriesLatN[b]++
	}
	if len(lat) > 0 {
		sort.Float64s(lat)
		st.AvgLatencyMs = latSum / float64(len(lat))
		st.P50LatencyMs = percentile(lat, 0.5)
		st.P95LatencyMs = percentile(lat, 0.95)
	}
	if st.CacheHits > 0 {
		st.AvgCacheLatencyMs = cacheLatSum / float64(st.CacheHits)
	}
	for name, ps := range prov {
		if provOK[name] > 0 {
			ps.AvgLatencyMs = provLatSum[name] / float64(provOK[name])
		}
		st.ByProvider = append(st.ByProvider, *ps)
	}
	sort.Slice(st.ByProvider, func(i, j int) bool { return st.ByProvider[i].Requests > st.ByProvider[j].Requests })
	for b, pt := range series {
		if n := seriesLatN[b]; n > 0 {
			pt.AvgLatencyMs = seriesLatSum[b] / float64(n)
		}
		st.Series = append(st.Series, *pt)
	}
	return st, nil
}

func (m *Memory) PurgeLogs(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	kept := m.logs[:0]
	var n int64
	for _, l := range m.logs {
		if l.TS.Before(before) {
			n++
		} else {
			kept = append(kept, l)
		}
	}
	m.logs = kept
	return n, nil
}

func (m *Memory) Ping(context.Context) error { return nil }
func (m *Memory) Kind() string               { return "memory" }
func (m *Memory) Close() error               { return nil }
