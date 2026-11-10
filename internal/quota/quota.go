// Package quota tracks how much of each provider's FREE limit has been used,
// so the gateway can steer requests away from a provider before it gets blocked.
//
// Free plans typically limit: requests per minute (RPM), requests per day (RPD) and
// tokens per minute (TPM). The tracker keeps a rolling record of recent calls and
// refuses a new one when it would go over a limit.
package quota

import (
	"sync"
	"time"
)

// Limits are a provider's free-tier limits. 0 means "no limit".
type Limits struct {
	RPM int
	RPD int
	TPM int
}

type entry struct {
	t      time.Time
	tokens int
}

// Denial explains why a request was not allowed.
type Denial struct {
	Reason     string        // "cooldown", "rpm", "rpd" or "tpm"
	RetryAfter time.Duration // when it is worth trying again
}

// Tracker counts usage for one provider.
type Tracker struct {
	mu       sync.Mutex
	limits   Limits
	entries  []*entry // oldest first, only the last 24 hours
	cooldown time.Time
	now      func() time.Time
}

// New creates a tracker.
func New(l Limits) *Tracker { return &Tracker{limits: l, now: time.Now} }

const maxEntries = 20000 // safety cap when there is no daily limit

// prune drops entries older than 24 hours. Caller holds the lock.
func (t *Tracker) prune(now time.Time) {
	cut := now.Add(-24 * time.Hour)
	i := 0
	for i < len(t.entries) && t.entries[i].t.Before(cut) {
		i++
	}
	if i > 0 {
		t.entries = append(t.entries[:0], t.entries[i:]...)
	}
	if len(t.entries) > maxEntries {
		t.entries = append(t.entries[:0], t.entries[len(t.entries)-maxEntries:]...)
	}
}

// windowStats counts requests and tokens newer than `since`, and returns the oldest entry time in that window.
func (t *Tracker) windowStats(since time.Time) (reqs, tokens int, oldest time.Time) {
	for i := len(t.entries) - 1; i >= 0; i-- {
		e := t.entries[i]
		if !e.t.After(since) {
			break
		}
		reqs++
		tokens += e.tokens
		oldest = e.t
	}
	return
}

// Reservation is a claim on quota taken before a call. Settle it with the real token
// count when the call succeeds, or Cancel it if the call failed without using quota.
type Reservation struct {
	t    *Tracker
	e    *entry
	done bool
}

// Acquire tries to use one request (and about estTokens tokens) of quota.
// It returns a Reservation if allowed, or a Denial saying why not.
func (t *Tracker) Acquire(estTokens int) (*Reservation, *Denial) {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.prune(now)

	if now.Before(t.cooldown) {
		return nil, &Denial{Reason: "cooldown", RetryAfter: t.cooldown.Sub(now)}
	}
	if t.limits.RPM > 0 {
		reqs, _, oldest := t.windowStats(now.Add(-time.Minute))
		if reqs >= t.limits.RPM {
			return nil, &Denial{Reason: "rpm", RetryAfter: oldest.Add(time.Minute).Sub(now)}
		}
	}
	if t.limits.TPM > 0 {
		_, tokens, oldest := t.windowStats(now.Add(-time.Minute))
		if tokens+estTokens > t.limits.TPM && tokens > 0 {
			return nil, &Denial{Reason: "tpm", RetryAfter: oldest.Add(time.Minute).Sub(now)}
		}
	}
	if t.limits.RPD > 0 {
		reqs, _, oldest := t.windowStats(now.Add(-24 * time.Hour))
		if reqs >= t.limits.RPD {
			return nil, &Denial{Reason: "rpd", RetryAfter: oldest.Add(24 * time.Hour).Sub(now)}
		}
	}
	e := &entry{t: now, tokens: estTokens}
	t.entries = append(t.entries, e)
	return &Reservation{t: t, e: e}, nil
}

// Settle replaces the estimated token count with the real one.
func (r *Reservation) Settle(actualTokens int) {
	if r == nil {
		return
	}
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	if actualTokens > 0 {
		r.e.tokens = actualTokens
	}
}

// Cancel gives the quota back (the call failed in a way that probably did not count).
func (r *Reservation) Cancel() {
	if r == nil {
		return
	}
	r.t.mu.Lock()
	defer r.t.mu.Unlock()
	if r.done {
		return
	}
	r.done = true
	for i := len(r.t.entries) - 1; i >= 0; i-- {
		if r.t.entries[i] == r.e {
			r.t.entries = append(r.t.entries[:i], r.t.entries[i+1:]...)
			return
		}
	}
}

// Cooldown blocks the provider for d. Used when the provider itself said "429, slow down".
// The gateway learns the real limit this way even if our configured numbers were wrong.
func (t *Tracker) Cooldown(d time.Duration) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if until := t.now().Add(d); until.After(t.cooldown) {
		t.cooldown = until
	}
}

// ClearCooldown removes a cooldown (used by the failure simulator when a fault is switched off).
func (t *Tracker) ClearCooldown() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cooldown = time.Time{}
}

// Snapshot is the usage picture shown on the dashboard.
type Snapshot struct {
	RPMUsed     int `json:"rpm_used"`
	RPMLimit    int `json:"rpm_limit"`
	RPDUsed     int `json:"rpd_used"`
	RPDLimit    int `json:"rpd_limit"`
	TPMUsed     int `json:"tpm_used"`
	TPMLimit    int `json:"tpm_limit"`
	CooldownSec int `json:"cooldown_sec"`  // > 0 when the provider told us to back off
	ResetsInSec int `json:"resets_in_sec"` // seconds until the oldest request in the busiest window frees a slot
}

// Snapshot returns current usage.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.prune(now)
	rpm, tpm, oldestMin := t.windowStats(now.Add(-time.Minute))
	rpd, _, oldestDay := t.windowStats(now.Add(-24 * time.Hour))
	s := Snapshot{
		RPMUsed: rpm, RPMLimit: t.limits.RPM,
		RPDUsed: rpd, RPDLimit: t.limits.RPD,
		TPMUsed: tpm, TPMLimit: t.limits.TPM,
	}
	if now.Before(t.cooldown) {
		s.CooldownSec = int(t.cooldown.Sub(now).Round(time.Second) / time.Second)
	}
	// "resets in": for the daily limit when it is the tighter one, else the minute window.
	switch {
	case t.limits.RPD > 0 && rpd >= t.limits.RPD:
		s.ResetsInSec = int(oldestDay.Add(24*time.Hour).Sub(now) / time.Second)
	case rpm > 0:
		s.ResetsInSec = int(oldestMin.Add(time.Minute).Sub(now) / time.Second)
	}
	if s.ResetsInSec < 0 {
		s.ResetsInSec = 0
	}
	return s
}
