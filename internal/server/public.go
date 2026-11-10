package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/openai"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
	"github.com/srushtiinvent/llm-gateway/internal/router"
)

// ---------- public layer: lets anyone try the site without the admin token ----------

// clientIP finds the visitor's address. Behind a reverse proxy (Render) the proxy appends the
// real address to X-Forwarded-For, so we count `hops` entries from the END (entries earlier in
// the list can be forged by the visitor).
func clientIP(r *http.Request, hops int) string {
	if hops > 0 {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			i := len(parts) - hops
			if i < 0 {
				i = 0
			}
			if ip := strings.TrimSpace(parts[i]); ip != "" {
				return ip
			}
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// signupGuard limits how many free keys one address (and the whole site) can create per day.
type signupGuard struct {
	mu     sync.Mutex
	day    string
	byIP   map[string]int
	global int
}

func (g *signupGuard) allow(ip string, perIP, global int) (ok bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	today := time.Now().UTC().Format("2006-01-02")
	if g.day != today || g.byIP == nil {
		g.day, g.byIP, g.global = today, map[string]int{}, 0
	}
	if global > 0 && g.global >= global {
		return false, "site_full"
	}
	if perIP > 0 && g.byIP[ip] >= perIP {
		return false, "ip_limit"
	}
	g.byIP[ip]++
	g.global++
	return true, ""
}

const visitorKeyPrefix = "visitor-"

// POST /v1/signup  -> a personal free key with small limits. No admin token needed.
func (s *Server) handleSignup(w http.ResponseWriter, r *http.Request) {
	c := s.Config
	if !c.PublicSignup {
		writeError(w, http.StatusForbidden, "invalid_request_error", "signup_disabled", "Public sign-up is turned off on this gateway.", 0, nil)
		return
	}
	if ok, why := s.signups.allow(clientIP(r, c.TrustedProxyHops), c.SignupPerIPPerDay, c.SignupGlobalPerDay); !ok {
		msg := "You already made the maximum number of free keys today. Use the key you have, or try again tomorrow."
		if why == "site_full" {
			msg = "Today's free keys are all taken (this demo runs on free-tier limits). Please try again tomorrow."
		}
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "signup_limit", msg, untilUTCMidnight(), nil)
		return
	}
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	name := visitorKeyPrefix + hex.EncodeToString(b)
	plain, prefix, hash := auth.GenerateKey()
	k, err := s.Store.CreateKey(r.Context(), name, prefix, hash, c.PublicKeyRPM, c.PublicKeyDaily)
	if err != nil {
		s.Logger.Error("signup key failed", "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "key_not_created", "Could not create your key. Try again.", 0, nil)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"key": plain, "id": k.ID, "name": k.Name, "prefix": k.Prefix,
		"rpm_limit": k.RPMLimit, "daily_limit": k.DailyLimit,
		"note": "Copy this key now. It is shown only once.",
	})
}

type publicProvider struct {
	Name           string         `json:"name"`
	Model          string         `json:"model"`
	Breaker        string         `json:"breaker"`
	Quota          quota.Snapshot `json:"quota"`
	RequestsOK     int64          `json:"requests_ok"`
	RequestsFailed int64          `json:"requests_failed"`
	AvgLatencyMs   float64        `json:"avg_latency_ms"`
	Fault          string         `json:"fault"`
}

type publicFlight struct {
	TS        time.Time `json:"ts"`
	Provider  string    `json:"provider"`
	Status    string    `json:"status"`
	CacheHit  bool      `json:"cache_hit"`
	LatencyMs int64     `json:"latency_ms"`
	Attempts  int       `json:"attempts"`
	Tried     string    `json:"tried"`
}

var publicCache = struct {
	sync.Mutex
	at   map[string]time.Time
	body map[string][]byte
}{at: map[string]time.Time{}, body: map[string][]byte{}}

// GET /v1/public/stats?window=1h|6h|24h|7d
// Read-only numbers for the Radar page. No keys, no prompts, no error text. Cached for 3 seconds
// so many visitors polling do not hammer the database.
func (s *Server) handlePublicStats(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("window")
	if _, ok := windows[name]; !ok {
		name = "24h"
	}
	publicCache.Lock()
	if b, ok := publicCache.body[name]; ok && time.Since(publicCache.at[name]) < 3*time.Second {
		publicCache.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(b)
		return
	}
	publicCache.Unlock()

	o, err := s.buildOverview(r.Context(), name)
	if err != nil {
		s.Logger.Error("public stats failed", "err", err)
		writeError(w, http.StatusInternalServerError, "server_error", "stats_unavailable", "Could not read statistics.", 0, nil)
		return
	}
	provs := make([]publicProvider, 0, len(o.Providers))
	for _, p := range o.Providers {
		provs = append(provs, publicProvider{p.Name, p.Model, p.Breaker, p.Quota, p.RequestsOK, p.RequestsFailed, p.AvgLatencyMs, p.Fault})
	}
	recent := []publicFlight{}
	if logs, err := s.Store.RecentLogs(r.Context(), 20); err == nil {
		for _, l := range logs {
			recent = append(recent, publicFlight{l.TS, l.Provider, l.Status, l.CacheHit, l.LatencyMs, l.Attempts, l.Tried})
		}
	}
	c := s.Config
	out := map[string]any{
		"window": o.Window, "bucket_sec": o.BucketSec, "uptime_sec": o.UptimeSec,
		"stats": o.Stats, "cache_hit_rate": o.HitRate, "cost": o.Cost,
		"providers": provs, "recent": recent,
		"signup": map[string]any{"enabled": c.PublicSignup, "rpm_limit": c.PublicKeyRPM, "daily_limit": c.PublicKeyDaily, "require_own_keys": c.RequireOwnKeys},
	}
	b, _ := json.Marshal(out)
	publicCache.Lock()
	publicCache.body[name], publicCache.at[name] = b, time.Now()
	publicCache.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

// ---------- bring your own provider keys ----------
//
// A visitor can send their own Gemini / Groq / OpenRouter keys with a request, as headers
// (X-Provider-Key-Gemini: ...). The gateway builds temporary providers from them, uses them for
// that one request and forgets them. Keys are never stored, logged or cached.

type reqInfoKey struct{}

type reqInfo struct {
	rt   *router.Router
	byok bool
}

func infoFrom(r *http.Request) *reqInfo {
	if v, ok := r.Context().Value(reqInfoKey{}).(*reqInfo); ok {
		return v
	}
	return &reqInfo{}
}

var byokKeyRE = regexp.MustCompile(`^[A-Za-z0-9_.\-]{12,256}$`)

func baseName(n string) string { return strings.TrimPrefix(strings.ToLower(n), "mock-") }

// parseByok returns {"gemini": "...", "groq": "..."} for providers this gateway knows about.
// Malformed values are ignored.
func (s *Server) parseByok(r *http.Request) map[string]string {
	out := map[string]string{}
	for _, pc := range s.Config.Providers {
		b := baseName(pc.Name)
		v := strings.TrimSpace(r.Header.Get("X-Provider-Key-" + strings.ToUpper(b[:1]) + b[1:]))
		if byokKeyRE.MatchString(v) {
			out[b] = v
		}
	}
	return out
}

func byokFingerprint(keys map[string]string) string {
	names := make([]string, 0, len(keys))
	for n := range keys {
		names = append(names, n)
	}
	sort.Strings(names)
	h := sha256.New()
	for _, n := range names {
		h.Write([]byte(n + "=" + keys[n] + ";"))
	}
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// byokEntries builds temporary provider entries (same order, models and URLs as the site's own).
// Quotas are not tracked here: the visitor's own provider account enforces them.
func (s *Server) byokEntries(keys map[string]string) []*router.Entry {
	c := s.Config
	var entries []*router.Entry
	for _, pc := range c.Providers {
		k, ok := keys[baseName(pc.Name)]
		if !ok {
			continue
		}
		var p provider.Provider
		if pc.Mock {
			p = provider.NewMock(pc.Name, pc.Model, pc.MockLatency)
		} else {
			var headers map[string]string
			if pc.Name == "openrouter" {
				headers = map[string]string{"X-Title": "LLM Gateway"}
			}
			p = provider.NewOpenAICompat(pc.Name, pc.BaseURL, k, pc.Model, headers)
		}
		entries = append(entries, router.NewEntry(router.EntryConfig{
			Provider: p, MaxInputTokens: pc.MaxInputTokens, Mock: pc.Mock,
			BreakerThreshold: c.BreakerThreshold, BreakerOpenFor: c.BreakerOpenFor,
		}))
	}
	return entries
}

// POST /v1/byok/check: tries each key the visitor sent with a one-token request, so the page can say
// "connected" or "key rejected" right away. Limited per IP. Nothing is stored.
func (s *Server) handleByokCheck(w http.ResponseWriter, r *http.Request) {
	if ok, _ := s.checks.allow(clientIP(r, s.Config.TrustedProxyHops), 60, 0); !ok {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "check_limit", "Too many key checks today. Try again tomorrow.", untilUTCMidnight(), nil)
		return
	}
	keys := s.parseByok(r)
	one := 1
	type result struct {
		OK      bool   `json:"ok"`
		Message string `json:"message,omitempty"`
		Ms      int64  `json:"ms,omitempty"`
	}
	out := map[string]result{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, e := range s.byokEntries(keys) {
		wg.Add(1)
		go func(e *router.Entry) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
			defer cancel()
			t0 := time.Now()
			_, err := e.P.Chat(ctx, &openai.ChatRequest{Model: "auto", MaxTokens: &one, Messages: []openai.Message{{Role: "user", Content: "hi"}}})
			res := result{OK: err == nil, Ms: time.Since(t0).Milliseconds()}
			if err != nil {
				res.Message = "could not be used"
				var pe *provider.Error
				if errors.As(err, &pe) {
					switch pe.Kind {
					case provider.KindAuth:
						res.Message = "key rejected by the provider"
					case provider.KindRateLimited:
						res.OK, res.Message = true, "key works (provider is rate limiting right now)"
					case provider.KindTimeout, provider.KindNetwork:
						res.Message = "provider did not answer in time"
					}
				}
			}
			mu.Lock()
			out[baseName(e.Name)] = res
			mu.Unlock()
		}(e)
	}
	wg.Wait()
	writeJSON(w, http.StatusOK, map[string]any{"results": out})
}