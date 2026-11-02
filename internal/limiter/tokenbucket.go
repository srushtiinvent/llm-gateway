// Package limiter protects the gateway itself: a per-key rate limit and a queue that
// caps how many requests run at once.
package limiter

import (
	"sync"
	"time"
)

// Limiter is a token-bucket rate limiter, one bucket per API key.
//
// Picture a bucket that holds up to `perMinute` tokens and is refilled steadily.
// Each request takes one token. If the bucket is empty the request is refused and
// we tell the caller how long until a token is available.
type Limiter struct {
	mu      sync.Mutex
	buckets map[int64]*bucket
	now     func() time.Time
	calls   int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// New creates a Limiter.
func New() *Limiter { return &Limiter{buckets: map[int64]*bucket{}, now: time.Now} }

// Allow takes one token for `id` if there is one. perMinute <= 0 means unlimited.
func (l *Limiter) Allow(id int64, perMinute int) (bool, time.Duration) {
	if perMinute <= 0 {
		return true, 0
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()

	l.calls++
	if l.calls%1000 == 0 { // now and then, forget buckets nobody has used for a while
		for k, b := range l.buckets {
			if now.Sub(b.last) > 10*time.Minute {
				delete(l.buckets, k)
			}
		}
	}

	capacity := float64(perMinute)
	ratePerSec := capacity / 60
	b, ok := l.buckets[id]
	if !ok {
		b = &bucket{tokens: capacity, last: now}
		l.buckets[id] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * ratePerSec
	if b.tokens > capacity {
		b.tokens = capacity
	}
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}
	wait := time.Duration((1 - b.tokens) / ratePerSec * float64(time.Second))
	return false, wait
}
