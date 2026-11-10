package resilience

import (
	"context"
	"math/rand"
	"time"
)

// Backoff returns how long to wait before retry number `attempt` (0 = first retry).
//
// It doubles each time (base, 2*base, 4*base ...) up to max, then picks a random value
// between 0 and that ("full jitter"). The randomness matters: if many requests fail at
// the same moment and all retry after exactly the same delay, they would hit the
// provider together again. Jitter spreads them out.
//
// rnd must return a number in [0,1); pass nil to use math/rand.
func Backoff(attempt int, base, max time.Duration, rnd func() float64) time.Duration {
	if rnd == nil {
		rnd = rand.Float64
	}
	d := base
	for i := 0; i < attempt && d < max; i++ {
		d *= 2
	}
	if d > max {
		d = max
	}
	return time.Duration(rnd() * float64(d))
}

// Sleep waits for d, or returns early with the context's error if it is cancelled.
func Sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
