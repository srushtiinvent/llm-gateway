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
	reset := time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
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
