// Package resilience has the safety tools that keep one failing provider from hurting everyone:
// a circuit breaker and a retry delay with jitter.
package resilience

import (
	"sync"
	"time"
)

// State of a circuit breaker.
type State int

const (
	Closed   State = iota // healthy: requests flow normally
	Open                  // broken: requests are blocked for a while
	HalfOpen              // testing: one trial request is allowed through
)

func (s State) String() string {
	switch s {
	case Open:
		return "open"
	case HalfOpen:
		return "half_open"
	default:
		return "closed"
	}
}

// Breaker is a circuit breaker, like the one in your house's fuse box.
//
//	closed    -> normal. After `threshold` failures in a row it trips to open.
//	open      -> all calls are skipped for `openFor`, so we stop hammering a broken provider.
//	half-open -> after the wait, ONE trial call is allowed. Success closes it, failure re-opens it.
type Breaker struct {
	mu        sync.Mutex
	state     State
	failures  int
	openedAt  time.Time
	probing   bool
	trips     int
	threshold int
	openFor   time.Duration
	now       func() time.Time
}

// NewBreaker creates a closed breaker.
func NewBreaker(threshold int, openFor time.Duration) *Breaker {
	if threshold < 1 {
		threshold = 1
	}
	return &Breaker{threshold: threshold, openFor: openFor, now: time.Now}
}

// Allow says whether a call may go ahead. In the half-open state only the first caller gets true.
// Every Allow() that returns true must be followed by Success, Failure or Release.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case Closed:
		return true
	case Open:
		if b.now().Sub(b.openedAt) >= b.openFor {
			b.state = HalfOpen
			b.probing = true
			return true
		}
		return false
	default: // HalfOpen
		if !b.probing {
			b.probing = true
			return true
		}
		return false
	}
}

// Success records a good call and closes the breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.failures = 0
	b.state = Closed
	b.probing = false
}

// Failure records a bad call. Enough of them in a row open the breaker.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case HalfOpen:
		b.open()
	case Closed:
		b.failures++
		if b.failures >= b.threshold {
			b.open()
		}
	}
}

// Release is for a call that ended in a way that says nothing about the provider's health
// (for example the user cancelled). It frees the half-open trial slot.
func (b *Breaker) Release() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == HalfOpen {
		b.probing = false
	}
}

// Reset puts the breaker back to the healthy state (used by the failure simulator).
func (b *Breaker) Reset() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.state, b.failures, b.probing = Closed, 0, false
}

func (b *Breaker) open() {
	b.state = Open
	b.openedAt = b.now()
	b.probing = false
	b.failures = 0
	b.trips++
}

// Snapshot describes the breaker for the dashboard.
type Snapshot struct {
	State    State
	Failures int
	Trips    int
	RetryIn  time.Duration // when open: how long until the next trial call
}

// Snapshot returns the current state without changing it.
func (b *Breaker) Snapshot() Snapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := Snapshot{State: b.state, Failures: b.failures, Trips: b.trips}
	if b.state == Open {
		if left := b.openFor - b.now().Sub(b.openedAt); left > 0 {
			s.RetryIn = left
		}
	}
	return s
}
