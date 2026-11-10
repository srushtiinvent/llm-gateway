package resilience

import (
	"context"
	"testing"
	"time"
)

func TestBreakerOpensAfterThresholdAndRecovers(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBreaker(3, 30*time.Second)
	b.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if !b.Allow() {
			t.Fatal("closed breaker must allow")
		}
		b.Failure()
	}
	if b.Snapshot().State != Closed {
		t.Fatal("2 failures < threshold 3: still closed")
	}
	b.Allow()
	b.Failure() // third failure in a row
	if b.Snapshot().State != Open {
		t.Fatal("should be open after 3 failures")
	}
	if b.Allow() {
		t.Fatal("open breaker must block")
	}

	now = now.Add(31 * time.Second) // wait is over
	if !b.Allow() {
		t.Fatal("after the wait one trial call must be allowed")
	}
	if b.Snapshot().State != HalfOpen {
		t.Fatal("should be half-open")
	}
	if b.Allow() {
		t.Fatal("only ONE trial call at a time in half-open")
	}
	b.Success()
	if b.Snapshot().State != Closed || !b.Allow() {
		t.Fatal("successful trial should close the breaker")
	}
}

func TestBreakerFailedTrialReopens(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBreaker(1, 10*time.Second)
	b.now = func() time.Time { return now }
	b.Allow()
	b.Failure()
	now = now.Add(11 * time.Second)
	if !b.Allow() {
		t.Fatal("trial expected")
	}
	b.Failure()
	if b.Snapshot().State != Open {
		t.Fatal("failed trial should re-open")
	}
	if b.Allow() {
		t.Fatal("must block again")
	}
	if b.Snapshot().Trips != 2 {
		t.Fatalf("trips = %d, want 2", b.Snapshot().Trips)
	}
}

func TestBreakerSuccessResetsFailureCount(t *testing.T) {
	b := NewBreaker(3, time.Second)
	for i := 0; i < 2; i++ {
		b.Allow()
		b.Failure()
	}
	b.Allow()
	b.Success()
	for i := 0; i < 2; i++ {
		b.Allow()
		b.Failure()
	}
	if b.Snapshot().State != Closed {
		t.Fatal("failures must be consecutive to trip the breaker")
	}
}

func TestBreakerReleaseFreesTrialSlot(t *testing.T) {
	now := time.Unix(1000, 0)
	b := NewBreaker(1, time.Second)
	b.now = func() time.Time { return now }
	b.Allow()
	b.Failure()
	now = now.Add(2 * time.Second)
	if !b.Allow() {
		t.Fatal("trial expected")
	}
	b.Release() // e.g. the user cancelled; says nothing about health
	if !b.Allow() {
		t.Fatal("after Release another trial must be possible")
	}
}

func TestBackoffGrowsAndIsCapped(t *testing.T) {
	one := func() float64 { return 0.999999 }
	base, max := 100*time.Millisecond, 1*time.Second
	prev := time.Duration(0)
	for attempt := 0; attempt < 3; attempt++ {
		d := Backoff(attempt, base, max, one)
		if d <= prev {
			t.Fatalf("attempt %d: %v should grow past %v", attempt, d, prev)
		}
		prev = d
	}
	if d := Backoff(10, base, max, one); d > max {
		t.Fatalf("backoff %v above max %v", d, max)
	}
	if d := Backoff(3, base, max, func() float64 { return 0 }); d != 0 {
		t.Fatal("jitter 0 should give 0")
	}
}

func TestSleepStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	start := time.Now()
	if err := Sleep(ctx, 5*time.Second); err == nil {
		t.Fatal("expected context error")
	}
	if time.Since(start) > time.Second {
		t.Fatal("Sleep did not stop early")
	}
}
