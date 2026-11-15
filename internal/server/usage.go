package server

import (
	"net/http"
	"time"
)

// handleUserUsage returns the authenticated gateway key's own daily allowance.
// The gateway key is validated exactly as it is for chat requests; no admin token
// is accepted or required.
func (s *Server) handleUserUsage(w http.ResponseWriter, r *http.Request) {
	key, err := s.Auth.Authenticate(r.Context(), r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "A valid gateway key is required to view your usage.", 0, nil)
		return
	}
	used, err := s.Budget.Usage(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "usage_unavailable", "Could not load your usage right now.", 0, nil)
		return
	}
	remaining := 0
	if key.DailyLimit > 0 {
		remaining = key.DailyLimit - used
		if remaining < 0 {
			remaining = 0
		}
	}
	reset := nextUTCMidnight()
	writeJSON(w, http.StatusOK, map[string]any{
		"rpm_limit":         key.RPMLimit,
		"rpm_available_now": s.Limiter.Remaining(key.ID, key.RPMLimit),
		"daily_limit":       key.DailyLimit,
		"daily_used":        used,
		"daily_remaining":   remaining,
		"unlimited":         key.DailyLimit <= 0,
		"resets_at":         reset,
	})
}

// GET /v1/quota: everything a client needs to decide whether to call now, wait, or stop.
//
//   - key:       this key's per-minute and daily allowance (daily resets at 00:00 UTC)
//   - providers: for each provider this key may use, whether it can take a request right now,
//     what blocks it if not, its remaining quota and when that resets
//   - can_call_now / next_available_at: the short answer
//
// Reading it uses no quota. Answers served from the cache are free and do not need any of it.
func (s *Server) handleQuota(w http.ResponseWriter, r *http.Request) {
	key, err := s.Auth.Authenticate(r.Context(), r)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", "A valid gateway key is required to view quota.", 0, nil)
		return
	}
	now := time.Now()
	used, err := s.Budget.Usage(r.Context(), key)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "usage_unavailable", "Could not load your usage right now.", 0, nil)
		return
	}
	dayReset := nextUTCMidnight()

	type window struct {
		Limit     int        `json:"limit"` // 0 = no limit
		Used      int        `json:"used"`
		Remaining *int       `json:"remaining"` // null when there is no limit
		ResetsAt  *time.Time `json:"resets_at,omitempty"` // UTC
		ResetRule string     `json:"reset_rule,omitempty"`
	}
	remaining := func(limit, used int) *int {
		if limit <= 0 {
			return nil
		}
		v := max(0, limit-used)
		return &v
	}

	// --- the key ---
	keyOK, keyNext := true, now
	minuteLeft := s.Limiter.Remaining(key.ID, key.RPMLimit)
	if key.RPMLimit > 0 && minuteLeft < 1 {
		keyOK = false
		keyNext = now.Add(time.Duration(float64(time.Minute) / float64(key.RPMLimit))).UTC()
	}
	if key.DailyLimit > 0 && used >= key.DailyLimit {
		keyOK, keyNext = false, dayReset
	}
	keyProviders := key.Providers
	if keyProviders == nil {
		keyProviders = []string{}
	}
	keyOut := map[string]any{
		"name": key.Name,
		"per_minute": window{Limit: key.RPMLimit, Used: max(0, key.RPMLimit-minuteLeft),
			Remaining: remaining(key.RPMLimit, max(0, key.RPMLimit-minuteLeft))},
		"per_day": window{Limit: key.DailyLimit, Used: used, Remaining: remaining(key.DailyLimit, used),
			ResetsAt: &dayReset, ResetRule: "00:00 UTC"},
		"providers": keyProviders,
		"ok":        keyOK,
	}

	// --- the providers this key may use ---
	type provOut struct {
		Name        string     `json:"name"`
		Model       string     `json:"model"`
		Paid        bool       `json:"paid"`
		Available   bool       `json:"available"`
		BlockedBy   string     `json:"blocked_by,omitempty"` // circuit_open, cooldown, rpm, tpm or rpd
		AvailableAt *time.Time `json:"available_at,omitempty"`
		PerMinute   window     `json:"per_minute"`
		PerDay      window     `json:"per_day"`
	}
	provs := []provOut{}
	anyProv, provNext := false, time.Time{}
	for _, e := range s.routerFor(key).Entries() {
		q := e.Quota.Snapshot()
		p := provOut{
			Name: e.Name, Model: e.P.Model(), Paid: e.Paid, Available: true,
			PerMinute: window{Limit: q.RPMLimit, Used: q.RPMUsed, Remaining: remaining(q.RPMLimit, q.RPMUsed)},
			PerDay: window{Limit: q.RPDLimit, Used: q.RPDUsed, Remaining: remaining(q.RPDLimit, q.RPDUsed),
				ResetsAt: q.DailyReset, ResetRule: q.DailyResetRule},
		}
		if b := e.Breaker.Snapshot(); b.State.String() == "open" {
			at := now.Add(b.RetryIn).UTC()
			p.Available, p.BlockedBy, p.AvailableAt = false, "circuit_open", &at
		} else if d := e.Quota.Peek(1000); d != nil {
			at := now.Add(d.RetryAfter).UTC()
			p.Available, p.BlockedBy, p.AvailableAt = false, d.Reason, &at
		}
		if p.Available {
			anyProv = true
		} else if provNext.IsZero() || p.AvailableAt.Before(provNext) {
			provNext = *p.AvailableAt
		}
		provs = append(provs, p)
	}

	out := map[string]any{"key": keyOut, "providers": provs, "can_call_now": keyOK && anyProv}
	if !(keyOK && anyProv) {
		next := keyNext
		if !anyProv && provNext.After(next) {
			next = provNext
		}
		if !next.IsZero() {
			out["next_available_at"] = next.UTC()
		}
		switch {
		case !keyOK && key.DailyLimit > 0 && used >= key.DailyLimit:
			out["reason"] = errKeyDailyBudget
		case !keyOK:
			out["reason"] = errKeyRateLimit
		default:
			out["reason"] = errNoProvider
		}
	}
	writeJSON(w, http.StatusOK, out)
}
