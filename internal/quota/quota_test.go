package quota

import (
	"testing"
	"time"
)

func newClock(l Limits) (*Tracker, *time.Time) {
	now := time.Unix(1_000_000, 0)
	tr := New(l)
	tr.now = func() time.Time { return now }
	return tr, &now
}

func TestRPMLimitAndRecovery(t *testing.T) {
	tr, now := newClock(Limits{RPM: 3})
	for i := 0; i < 3; i++ {
		if _, d := tr.Acquire(10); d != nil {
			t.Fatalf("request %d should be allowed", i)
		}
		*now = now.Add(5 * time.Second)
	}
	_, d := tr.Acquire(10)
	if d == nil || d.Reason != "rpm" {
		t.Fatalf("4th request in a minute must be denied for rpm, got %+v", d)
	}
	if d.RetryAfter <= 0 || d.RetryAfter > time.Minute {
		t.Fatalf("RetryAfter should be within a minute, got %v", d.RetryAfter)
	}
	*now = now.Add(50 * time.Second) // first request is now older than a minute
	if _, d := tr.Acquire(10); d != nil {
		t.Fatalf("should be allowed again, got %+v", d)
	}
}

func TestRPDLimit(t *testing.T) {
	tr, now := newClock(Limits{RPD: 2})
	tr.Acquire(1)
	*now = now.Add(time.Hour)
	tr.Acquire(1)
	_, d := tr.Acquire(1)
	if d == nil || d.Reason != "rpd" {
		t.Fatalf("expected rpd denial, got %+v", d)
	}
	*now = now.Add(23*time.Hour + time.Minute) // first one is now over 24h old
	if _, d := tr.Acquire(1); d != nil {
		t.Fatal("daily window should have slid")
	}
}

func TestTPMLimit(t *testing.T) {
	tr, _ := newClock(Limits{TPM: 1000})
	if _, d := tr.Acquire(600); d != nil {
		t.Fatal("first allowed")
	}
	_, d := tr.Acquire(600)
	if d == nil || d.Reason != "tpm" {
		t.Fatalf("expected tpm denial, got %+v", d)
	}
	// a single oversized request on an idle provider is still let through (the provider decides)
	tr2, _ := newClock(Limits{TPM: 1000})
	if _, d := tr2.Acquire(5000); d != nil {
		t.Fatal("oversized request on idle provider should not be blocked by us")
	}
}

func TestCancelRefundsAndSettleCorrectsTokens(t *testing.T) {
	tr, _ := newClock(Limits{RPM: 1, TPM: 1000})
	r, _ := tr.Acquire(900)
	r.Cancel()
	if _, d := tr.Acquire(900); d != nil {
		t.Fatal("cancelled reservation should give the quota back")
	}
	tr2, _ := newClock(Limits{TPM: 1000})
	r2, _ := tr2.Acquire(900)
	r2.Settle(100) // real usage was far less than the estimate
	if _, d := tr2.Acquire(800); d != nil {
		t.Fatalf("after settle(100) there should be room, got %+v", d)
	}
}

func TestCooldownBlocksThenExpires(t *testing.T) {
	tr, now := newClock(Limits{})
	tr.Cooldown(30 * time.Second)
	_, d := tr.Acquire(1)
	if d == nil || d.Reason != "cooldown" {
		t.Fatalf("expected cooldown, got %+v", d)
	}
	*now = now.Add(31 * time.Second)
	if _, d := tr.Acquire(1); d != nil {
		t.Fatal("cooldown should have ended")
	}
}

func TestSnapshot(t *testing.T) {
	tr, _ := newClock(Limits{RPM: 10, RPD: 100, TPM: 5000})
	tr.Acquire(100)
	tr.Acquire(200)
	s := tr.Snapshot()
	if s.RPMUsed != 2 || s.RPDUsed != 2 || s.TPMUsed != 300 || s.RPMLimit != 10 || s.RPDLimit != 100 {
		t.Fatalf("unexpected snapshot %+v", s)
	}
}

func TestUnlimitedNeverDenies(t *testing.T) {
	tr, _ := newClock(Limits{})
	for i := 0; i < 500; i++ {
		if _, d := tr.Acquire(1000); d != nil {
			t.Fatal("0 means unlimited")
		}
	}
}

func TestDailyResetAtFixedTimeInProviderZone(t *testing.T) {
	r, err := ParseReset("00:00 America/Los_Angeles")
	if err != nil {
		t.Fatal(err)
	}
	pt := r.Loc
	// 23:00 Pacific on Nov 14 = 12:30 IST Nov 15: still the Nov 14 quota day.
	now := time.Date(2026, 11, 14, 23, 0, 0, 0, pt)
	tr := New(Limits{RPD: 2, DailyReset: r})
	tr.now = func() time.Time { return now }
	tr.Acquire(1)
	tr.Acquire(1)
	_, d := tr.Acquire(1)
	if d == nil || d.Reason != "rpd" || d.RetryAfter != time.Hour {
		t.Fatalf("want rpd denial until Pacific midnight (1h), got %+v", d)
	}
	if got := tr.NextDailyReset(); !got.Equal(time.Date(2026, 11, 15, 0, 0, 0, 0, pt)) {
		t.Fatalf("next reset = %v", got)
	}
	// One minute after Pacific midnight the quota is fresh, although the calls were < 24h ago.
	now = now.Add(61 * time.Minute)
	if _, d := tr.Acquire(1); d != nil {
		t.Fatalf("new quota day should allow requests, got %+v", d)
	}
	if s := tr.Snapshot(); s.RPDUsed != 1 || s.DailyReset == nil || s.DailyResetRule != "00:00 America/Los_Angeles" {
		t.Fatalf("snapshot: %+v", s)
	}
}

func TestParseReset(t *testing.T) {
	for _, ok := range []string{"", "rolling", "UTC", "05:30 Asia/Kolkata", "0:00 America/Los_Angeles"} {
		if _, err := ParseReset(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"25:00 UTC", "00:00 Mars/Olympus", "noon UTC"} {
		if _, err := ParseReset(bad); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
}

func TestPeekDoesNotUseQuota(t *testing.T) {
	tr, _ := newClock(Limits{RPM: 1})
	for i := 0; i < 3; i++ {
		if d := tr.Peek(10); d != nil {
			t.Fatalf("peek %d: %+v", i, d)
		}
	}
	tr.Acquire(10)
	if d := tr.Peek(10); d == nil || d.Reason != "rpm" {
		t.Fatalf("after the one request: %+v", d)
	}
}
