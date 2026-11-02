package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/cache"
	"github.com/srushtiinvent/llm-gateway/internal/limiter"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/router"
	"github.com/srushtiinvent/llm-gateway/internal/store"
)

// admin wraps a handler so that only requests with the admin token get through.
func (s *Server) admin(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.Auth.IsAdmin(r) {
			writeError(w, http.StatusUnauthorized, "authentication_error", "admin_required",
				"Admin token required: send the header 'Authorization: Bearer <ADMIN_TOKEN>'.", 0, nil)
			return
		}
		h(w, r)
	}
}

// windows the dashboard can ask for, and the size of one chart bar for each.
var windows = map[string]struct{ d, bucket time.Duration }{
	"1h":  {time.Hour, time.Minute},
	"6h":  {6 * time.Hour, 10 * time.Minute},
	"24h": {24 * time.Hour, 30 * time.Minute},
	"7d":  {7 * 24 * time.Hour, 3 * time.Hour},
}

type overview struct {
	Window    string `json:"window"`
	BucketSec int    `json:"bucket_sec"`
	Store     string `json:"store"`
	UptimeSec int64  `json:"uptime_sec"`

	Stats   store.Stats `json:"stats"`
	HitRate float64     `json:"cache_hit_rate"` // 0..1 of all requests in the window

	Cost struct {
		SpentUSD float64 `json:"notional_spent_usd"`
		SavedUSD float64 `json:"notional_saved_usd"`
		Note     string  `json:"note"`
	} `json:"cost"`

	Providers []router.ProviderStatus `json:"providers"`
	Cache     cache.Stats             `json:"cache"`
	Queue     limiter.QueueStats      `json:"queue"`
}

// GET /admin/overview?window=1h|6h|24h|7d  -- everything the dashboard needs in one call.
func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	win, ok := windows[name]
	if !ok {
		name, win = "24h", windows["24h"]
	}
	now := time.Now()
	since := now.Add(-win.d)
	st, err := s.Store.Stats(r.Context(), since, win.bucket)
	if err != nil {
		s.Logger.Error("stats query failed", "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "stats_unavailable", "Could not read statistics.", 0, nil)
		return
	}
	st.Series = store.FillSeries(st.Series, since, now, win.bucket)
	if st.ByProvider == nil {
		st.ByProvider = []store.ProviderStat{}
	}

	o := overview{
		Window: name, BucketSec: int(win.bucket.Seconds()), Store: s.Store.Kind(),
		UptimeSec: int64(time.Since(s.started).Seconds()),
		Stats:     st, Providers: s.Router.Snapshot(), Cache: s.Cache.Stats(), Queue: s.Queue.Stats(),
	}
	if st.Requests > 0 {
		o.HitRate = float64(st.CacheHits) / float64(st.Requests)
	}
	c := s.Config
	o.Cost.SpentUSD = float64(st.PromptTokens)/1e6*c.PriceInPerM + float64(st.CompletionTokens)/1e6*c.PriceOutPerM
	o.Cost.SavedUSD = float64(st.SavedPromptTokens)/1e6*c.PriceInPerM + float64(st.SavedCompletionTokens)/1e6*c.PriceOutPerM
	o.Cost.Note = "Estimates of what this traffic would cost on a paid plan. Free tiers cost nothing."
	writeJSON(w, http.StatusOK, o)
}

// GET /admin/providers
func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.Router.Snapshot()})
}

// POST /admin/providers/{name}/fault   body: {"mode":"down"|"slow"|"ratelimited"|"none"}
// The failure simulator: pretend a provider is broken to watch the gateway recover.
func (s *Server) handleFault(w http.ResponseWriter, r *http.Request) {
	e, ok := s.Router.Get(r.PathValue("name"))
	if !ok {
		writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_provider", "No such provider.", 0, nil)
		return
	}
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Body must be like {\"mode\":\"down\"}.", 0, nil)
		return
	}
	mode, err := provider.ParseFault(body.Mode)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_mode", err.Error(), 0, nil)
		return
	}
	e.Faults.Set(mode)
	if mode == provider.FaultNone { // "repair" the provider: forget the failures and cooldown it caused
		e.Breaker.Reset()
		e.Quota.ClearCooldown()
	}
	s.Logger.Info("fault mode changed", "provider", e.Name, "mode", mode)
	writeJSON(w, http.StatusOK, map[string]any{"provider": e.Name, "fault": mode})
}

// GET /admin/logs?limit=50
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	logs, err := s.Store.RecentLogs(r.Context(), limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "logs_unavailable", "Could not read logs.", 0, nil)
		return
	}
	if logs == nil {
		logs = []store.RequestLog{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"logs": logs})
}

// GET /admin/keys
func (s *Server) handleListKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := s.Store.ListKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "server_error", "keys_unavailable", "Could not read keys.", 0, nil)
		return
	}
	if keys == nil {
		keys = []store.Key{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": keys})
}

// POST /admin/keys   body: {"name":"my-app","rpm":20,"daily":500}
func (s *Server) handleCreateKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name  string `json:"name"`
		RPM   *int   `json:"rpm"`
		Daily *int   `json:"daily"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Body must be like {\"name\":\"my-app\"}.", 0, nil)
		return
	}
	name := strings.TrimSpace(body.Name)
	if name == "" || len(name) > 60 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_name", "'name' is required (max 60 characters).", 0, nil)
		return
	}
	rpm, daily := s.Config.DefaultKeyRPM, s.Config.DefaultKeyDaily
	if body.RPM != nil {
		rpm = *body.RPM
	}
	if body.Daily != nil {
		daily = *body.Daily
	}
	if rpm < 0 || daily < 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_limit", "'rpm' and 'daily' cannot be negative (0 = unlimited).", 0, nil)
		return
	}
	plain, prefix, hash := auth.GenerateKey()
	k, err := s.Store.CreateKey(r.Context(), name, prefix, hash, rpm, daily)
	if err != nil {
		s.Logger.Error("create key failed", "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "key_not_created", "Could not create the key.", 0, nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key":  plain,
		"note": "Copy this key now. It is shown only once and cannot be recovered.",
		"id":   k.ID, "name": k.Name, "prefix": k.Prefix, "rpm_limit": k.RPMLimit, "daily_limit": k.DailyLimit,
	})
}

// POST /admin/keys/{id}/revoke
func (s *Server) handleRevokeKey(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_id", "Key id must be a number.", 0, nil)
		return
	}
	if err := s.Store.RevokeKey(r.Context(), id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "invalid_request_error", "unknown_key", "No such key.", 0, nil)
			return
		}
		writeError(w, http.StatusInternalServerError, "server_error", "revoke_failed", "Could not revoke the key.", 0, nil)
		return
	}
	s.Auth.Forget()
	writeJSON(w, http.StatusOK, map[string]any{"revoked": id})
}

// POST /admin/cache/clear
func (s *Server) handleClearCache(w http.ResponseWriter, r *http.Request) {
	s.Cache.Clear()
	writeJSON(w, http.StatusOK, map[string]any{"cleared": true})
}
