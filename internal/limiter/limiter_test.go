package limiter

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestTokenBucket(t *testing.T) {
	l := New()
	now := time.Unix(1000, 0)
	l.now = func() time.Time { return now }

	for i := 0; i < 2; i++ {
		if ok, _ := l.Allow(1, 2); !ok {
			t.Fatalf("request %d within the limit must pass", i)
		}
	}
	ok, wait := l.Allow(1, 2)
	if ok {
		t.Fatal("third request in the same instant must be refused")
	}
	if wait < 25*time.Second || wait > 31*time.Second {
		t.Fatalf("2 per minute means about 30s per token, got %v", wait)
	}
	if ok, _ := l.Allow(2, 2); !ok { // a different key has its own bucket
		t.Fatal("other keys are not affected")
	}
	now = now.Add(31 * time.Second)
	if ok, _ := l.Allow(1, 2); !ok {
		t.Fatal("a token should have refilled")
	}
	if ok, _ := l.Allow(1, 0); !ok { // 0 = unlimited
		t.Fatal("unlimited keys always pass")
	}
}

func TestQueueBackpressure(t *testing.T) {
	q := NewQueue(1, 1, 300*time.Millisecond)
	ctx := context.Background()

	rel1, waited, err := q.Acquire(ctx)
	if err != nil || waited != 0 {
		t.Fatalf("first acquire should be instant, err=%v", err)
	}

	got2 := make(chan error, 1)
	go func() {
		rel, _, err := q.Acquire(ctx) // waits in line
		if err == nil {
			rel()
		}
		got2 <- err
	}()
	time.Sleep(50 * time.Millisecond)

	if _, _, err := q.Acquire(ctx); !errors.Is(err, ErrOverloaded) { // line is full
		t.Fatalf("third request should be rejected immediately, got %v", err)
	}
	if q.Stats().Waiting != 1 || q.Stats().InFlight != 1 {
		t.Fatalf("stats %+v", q.Stats())
	}

	rel1() // frees the slot for the waiting request
	if err := <-got2; err != nil {
		t.Fatalf("waiting request should get the slot, got %v", err)
	}
	if q.Stats().Rejected != 1 {
		t.Fatalf("rejected = %d", q.Stats().Rejected)
	}
}

func TestQueueWaitTimeout(t *testing.T) {
	q := NewQueue(1, 5, 50*time.Millisecond)
	rel, _, _ := q.Acquire(context.Background())
	defer rel()
	_, _, err := q.Acquire(context.Background())
	if !errors.Is(err, ErrOverloaded) {
		t.Fatalf("waiting too long must give ErrOverloaded, got %v", err)
	}
}

func TestQueueClientCancel(t *testing.T) {
	q := NewQueue(1, 5, time.Second)
	rel, _, _ := q.Acquire(context.Background())
	defer rel()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := q.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want context error, got %v", err)
	}
}
