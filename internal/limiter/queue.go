package limiter

import (
	"context"
	"errors"
	"sync/atomic"
	"time"
)

// ErrOverloaded means the queue is full (or the wait took too long). The caller should
// answer "try again shortly" instead of piling more work onto a busy server.
var ErrOverloaded = errors.New("gateway is overloaded, try again shortly")

// Queue limits how many requests run at the same time. Extra requests wait in line,
// up to a maximum line length and a maximum waiting time. This is called "backpressure":
// when the system is busy it says so, instead of slowing down for everyone.
type Queue struct {
	slots       chan struct{}
	maxWaiting  int64
	waitTimeout time.Duration

	waiting  atomic.Int64
	inFlight atomic.Int64
	rejected atomic.Int64
	served   atomic.Int64
}

// NewQueue allows `maxConcurrent` requests at once and `maxWaiting` more in line.
func NewQueue(maxConcurrent, maxWaiting int, waitTimeout time.Duration) *Queue {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &Queue{slots: make(chan struct{}, maxConcurrent), maxWaiting: int64(maxWaiting), waitTimeout: waitTimeout}
}

// Acquire waits for a free slot. It returns a release function (call it when the request
// is finished) and how long the request waited.
func (q *Queue) Acquire(ctx context.Context) (release func(), waited time.Duration, err error) {
	start := time.Now()
	release = func() {
		q.inFlight.Add(-1)
		<-q.slots
	}

	select { // fast path: a slot is free right now
	case q.slots <- struct{}{}:
		q.inFlight.Add(1)
		q.served.Add(1)
		return release, 0, nil
	default:
	}

	if q.waiting.Add(1) > q.maxWaiting { // the line is full
		q.waiting.Add(-1)
		q.rejected.Add(1)
		return nil, 0, ErrOverloaded
	}
	defer q.waiting.Add(-1)

	timer := time.NewTimer(q.waitTimeout)
	defer timer.Stop()
	select {
	case q.slots <- struct{}{}:
		q.inFlight.Add(1)
		q.served.Add(1)
		return release, time.Since(start), nil
	case <-timer.C:
		q.rejected.Add(1)
		return nil, time.Since(start), ErrOverloaded
	case <-ctx.Done():
		return nil, time.Since(start), ctx.Err()
	}
}

// QueueStats is a snapshot for the dashboard.
type QueueStats struct {
	InFlight      int64 `json:"in_flight"`
	Waiting       int64 `json:"waiting"`
	MaxConcurrent int   `json:"max_concurrent"`
	MaxWaiting    int64 `json:"max_waiting"`
	Rejected      int64 `json:"rejected"`
	Served        int64 `json:"served"`
}

// Stats returns the current numbers.
func (q *Queue) Stats() QueueStats {
	return QueueStats{
		InFlight: q.inFlight.Load(), Waiting: q.waiting.Load(),
		MaxConcurrent: cap(q.slots), MaxWaiting: q.maxWaiting,
		Rejected: q.rejected.Load(), Served: q.served.Load(),
	}
}
