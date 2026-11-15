// Package quota tracks how much of each provider's FREE limit has been used,
// so the gateway can steer requests away from a provider before it gets blocked.
//
// Free plans typically limit: requests per minute (RPM), requests per day (RPD) and
// tokens per minute (TPM). The tracker keeps a rolling record of recent calls and
// refuses a new one when it would go over a limit.
package quota

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// Limits are a provider's free-tier limits. 0 means "no limit".
type Limits struct {
	RPM int
	RPD int
	TPM int
	// DailyReset, when set, makes the daily limit a calendar day that starts at a fixed
	// time in the provider's own time zone (Google resets Gemini's free quota at midnight
	// Pacific time). When nil, the daily limit is a rolling 24-hour window.
	DailyReset *Reset
}

// Reset is a time of day in a time zone, such as 00:00 America/Los_Angeles.
type Reset struct {
	Hour, Minute int
	Loc          *time.Location
}

// String formats the reset like "00:00 America/Los_Angeles".
func (r *Reset) String() string {
	if r == nil {
		return "rolling 24h"
	}
	return fmt.Sprintf("%02d:%02d %s", r.Hour, r.Minute, r.Loc)
}

// ParseReset reads "HH:MM Zone/Name" (or just "Zone/Name" for midnight). Empty input returns nil.
func ParseReset(s string) (*Reset, error) {
	s = strings.TrimSpace(s)
	if s == "" || strings.EqualFold(s, "rolling") {
		return nil, nil
	}
	hm, zone, ok := strings.Cut(s, " ")
	if !ok {
		hm, zone = "00:00", s
	}
	var h, m int
	if _, err := fmt.Sscanf(hm, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return nil, fmt.Errorf("reset time %q: want HH:MM followed by a time zone, e.g. \"00:00 America/Los_Angeles\"", s)
	}
	loc, err := time.LoadLocation(strings.TrimSpace(zone))
	if err != nil {
		return nil, fmt.Errorf("reset time %q: unknown time zone: %w", s, err)
	}
	return &Reset{Hour: h, Minute: m, Loc: loc}, nil
}

// Window returns the start of the current quota day and the start of the next one.
func (r *Reset) Window(now time.Time) (start, next time.Time) {
	t := now.In(r.Loc)
	start = time.Date(t.Year(), t.Month(), t.Day(), r.Hour, r.Minute, 0, 0, r.Loc)
	if start.After(t) {
		start = time.Date(t.Year(), t.Month(), t.Day()-1, r.Hour, r.Minute, 0, 0, r.Loc)
	}
	next = time.Date(start.Year(), start.Month(), start.Day()+1, r.Hour, r.Minute, 0, 0, r.Loc)
	return start, next
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

// Peek reports whether a request of about estTokens tokens would be allowed right now,
// without using any quota. nil means it would be allowed.
func (t *Tracker) Peek(estTokens int) *Denial {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.prune(now)
	return t.check(now, estTokens)
}

// check applies every limit. Caller holds the lock.
func (t *Tracker) check(now time.Time, estTokens int) *Denial {
	if now.Before(t.cooldown) {
		return &Denial{Reason: "cooldown", RetryAfter: t.cooldown.Sub(now)}
	}
	if t.limits.RPM > 0 {
		reqs, _, oldest := t.windowStats(now.Add(-time.Minute))
		if reqs >= t.limits.RPM {
			return &Denial{Reason: "rpm", RetryAfter: oldest.Add(time.Minute).Sub(now)}
		}
	}
	if t.limits.TPM > 0 {
		_, tokens, oldest := t.windowStats(now.Add(-time.Minute))
		if tokens+estTokens > t.limits.TPM && tokens > 0 {
			return &Denial{Reason: "tpm", RetryAfter: oldest.Add(time.Minute).Sub(now)}
		}
	}
	if t.limits.RPD > 0 {
		reqs, freesAt := t.daily(now)
		if reqs >= t.limits.RPD {
			return &Denial{Reason: "rpd", RetryAfter: freesAt.Sub(now)}
		}
	}
	return nil
}

// daily counts requests in the current quota day and says when a slot frees up again:
// the next reset for a calendar-day limit, or when the oldest call leaves a rolling window.
// Caller holds the lock.
func (t *Tracker) daily(now time.Time) (reqs int, freesAt time.Time) {
	if r := t.limits.DailyReset; r != nil {
		start, next := r.Window(now)
		reqs, _, _ = t.windowStats(start.Add(-time.Nanosecond))
		return reqs, next
	}
	reqs, _, oldest := t.windowStats(now.Add(-24 * time.Hour))
	return reqs, oldest.Add(24 * time.Hour)
}

// NextDailyReset returns when the provider's daily quota resets, or the zero time for a rolling window.
func (t *Tracker) NextDailyReset() time.Time {
	if t.limits.DailyReset == nil {
		return time.Time{}
	}
	_, next := t.limits.DailyReset.Window(t.now())
	return next
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
	if d := t.check(now, estTokens); d != nil {
		return nil, d
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
	// DailyReset is when the daily count goes back to zero ("" for a rolling 24h window),
	// and DailyResetRule is the configured rule, e.g. "00:00 America/Los_Angeles".
	DailyReset     *time.Time `json:"daily_reset,omitempty"`
	DailyResetRule string     `json:"daily_reset_rule"`
}

// Snapshot returns current usage.
func (t *Tracker) Snapshot() Snapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	now := t.now()
	t.prune(now)
	rpm, tpm, oldestMin := t.windowStats(now.Add(-time.Minute))
	rpd, dayFrees := t.daily(now)
	s := Snapshot{
		RPMUsed: rpm, RPMLimit: t.limits.RPM,
		RPDUsed: rpd, RPDLimit: t.limits.RPD,
		TPMUsed: tpm, TPMLimit: t.limits.TPM,
		DailyResetRule: t.limits.DailyReset.String(),
	}
	if t.limits.DailyReset != nil {
		u := dayFrees.UTC()
		s.DailyReset = &u
	}
	if now.Before(t.cooldown) {
		s.CooldownSec = int(t.cooldown.Sub(now).Round(time.Second) / time.Second)
	}
	// "resets in": for the daily limit when it is the tighter one, else the minute window.
	switch {
	case t.limits.RPD > 0 && rpd >= t.limits.RPD:
		s.ResetsInSec = int(dayFrees.Sub(now) / time.Second)
	case rpm > 0:
		s.ResetsInSec = int(oldestMin.Add(time.Minute).Sub(now) / time.Second)
	}
	if s.ResetsInSec < 0 {
		s.ResetsInSec = 0
	}
	return s
}
