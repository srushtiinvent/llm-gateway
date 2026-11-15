// Package server is the HTTP layer: it receives requests, checks keys and limits,
// asks the router for an answer and writes the response.
package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/cache"
	"github.com/srushtiinvent/llm-gateway/internal/config"
	"github.com/srushtiinvent/llm-gateway/internal/limiter"
	"github.com/srushtiinvent/llm-gateway/internal/router"
	"github.com/srushtiinvent/llm-gateway/internal/store"
)

// Deps are the building blocks the server needs.
type Deps struct {
	Config  *config.Config
	Store   store.Store
	Router  *router.Router
	Cache   *cache.Cache
	Queue   *limiter.Queue
	Limiter *limiter.Limiter
	Auth    *auth.Authenticator
	Budget  *auth.Budget
	Logger  *slog.Logger
}

// Server handles all HTTP requests.
type Server struct {
	Deps
	free    *router.Router // the default route for keys without provider rules: free providers only
	logs    *logWriter
	signups signupGuard
	checks  signupGuard
	started time.Time
}

// New creates the server and starts its background log writer.
func New(d Deps) *Server {
	s := &Server{Deps: d, logs: newLogWriter(d.Store, d.Logger), started: time.Now(), free: d.Router}
	var free []*router.Entry
	for _, e := range d.Router.Entries() {
		if !e.Paid {
			free = append(free, e)
		}
	}
	if len(free) != len(d.Router.Entries()) {
		s.free = d.Router.Derive(free) // same entries (shared quotas and breakers), paid ones left out
	}
	return s
}

// routerFor returns the providers a key may use, in the order it should try them.
// Keys without rules get every free provider in the gateway's order. A paid provider is
// only reachable by a key whose rules name it.
func (s *Server) routerFor(k *store.Key) *router.Router {
	if len(k.Providers) == 0 {
		return s.free
	}
	var entries []*router.Entry
	for _, n := range k.Providers {
		if e, ok := s.Router.Get(n); ok {
			entries = append(entries, e)
		}
	}
	return s.Router.Derive(entries)
}

// Close flushes pending request logs. Call it when shutting down.
func (s *Server) Close() { s.logs.close() }

// Handler returns the full HTTP handler with all routes and middleware.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /healthz", s.handleHealth)
	mux.HandleFunc("GET /readyz", s.handleReady)
	mux.HandleFunc("GET /v1/models", s.handleModels)
	mux.HandleFunc("GET /v1/usage", s.handleUserUsage)
	mux.HandleFunc("GET /v1/quota", s.handleQuota)
	mux.HandleFunc("POST /v1/chat/completions", s.handleChat)
	mux.HandleFunc("POST /v1/signup", s.handleSignup)
	mux.HandleFunc("POST /v1/byok/check", s.handleByokCheck)
	mux.HandleFunc("GET /v1/public/stats", s.handlePublicStats)

	mux.HandleFunc("GET /admin/overview", s.admin(s.handleOverview))
	mux.HandleFunc("GET /admin/providers", s.admin(s.handleProviders))
	mux.HandleFunc("POST /admin/providers/{name}/fault", s.admin(s.handleFault))
	mux.HandleFunc("GET /admin/logs", s.admin(s.handleLogs))
	mux.HandleFunc("GET /admin/keys", s.admin(s.handleListKeys))
	mux.HandleFunc("POST /admin/keys", s.admin(s.handleCreateKey))
	mux.HandleFunc("POST /admin/keys/{id}/revoke", s.admin(s.handleRevokeKey))
	mux.HandleFunc("POST /admin/keys/{id}/providers", s.admin(s.handleKeyProviders))
	mux.HandleFunc("POST /admin/cache/clear", s.admin(s.handleClearCache))

	if dir := s.Config.WebDist; dir != "" {
		if _, err := os.Stat(filepath.Join(dir, "index.html")); err == nil {
			mux.Handle("GET /", spaHandler(dir))
			s.Logger.Info("serving website", "dir", dir)
		} else {
			s.Logger.Warn("WEB_DIST is set but index.html was not found; website not served", "dir", dir)
		}
	}

	return s.recoverer(s.requestLog(s.cors(mux)))
}

// ---------- small helpers ----------

type errorBody struct {
	Error struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    string `json:"code"`
	} `json:"error"`
	Gateway map[string]any `json:"gateway,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError sends an OpenAI-style error. retryAfter > 0 adds a Retry-After header.
func writeError(w http.ResponseWriter, status int, typ, code, msg string, retryAfter time.Duration, extra map[string]any) {
	if retryAfter > 0 {
		w.Header().Set("Retry-After", fmt.Sprint(int(math.Ceil(retryAfter.Seconds()))))
	}
	var b errorBody
	b.Error.Message, b.Error.Type, b.Error.Code = msg, typ, code
	b.Gateway = extra
	writeJSON(w, status, b)
}

func spaHandler(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/v1/") || strings.HasPrefix(r.URL.Path, "/admin/") {
			writeError(w, http.StatusNotFound, "invalid_request_error", "not_found", "No such endpoint.", 0, nil)
			return
		}
		p := filepath.Join(dir, filepath.Clean("/"+r.URL.Path))
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			files.ServeHTTP(w, r)
			return
		}
		http.ServeFile(w, r, filepath.Join(dir, "index.html")) // single-page app: unknown paths show the app
	})
}

// ---------- middleware ----------

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (w *statusWriter) WriteHeader(code int) {
	if !w.wrote {
		w.status, w.wrote = code, true
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if !w.wrote {
		w.status, w.wrote = http.StatusOK, true
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Unwrap lets http.ResponseController reach the real writer (needed for streaming).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *Server) recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.Logger.Error("panic while handling request", "path", r.URL.Path, "panic", fmt.Sprint(rec), "stack", string(debug.Stack()))
				writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "Something went wrong inside the gateway.", 0, nil)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func (s *Server) requestLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		if r.URL.Path == "/healthz" {
			level = slog.LevelDebug // uptime pings every few minutes would fill the log
		}
		s.Logger.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) cors(next http.Handler) http.Handler {
	allowed := strings.TrimSpace(s.Config.AllowedOrigins)
	list := map[string]bool{}
	for _, o := range strings.Split(allowed, ",") {
		if o = strings.TrimSpace(o); o != "" {
			list[o] = true
		}
	}
	allowHeaders := []string{"Authorization", "Content-Type", "Cache-Control", "X-Admin-Token", "X-Gateway-Cache",
		"X-Gateway-No-Cache", "X-Gateway-Simulate", "X-Gateway-Strict-Model"}
	for _, pc := range s.Config.Providers {
		if !pc.Paid {
			allowHeaders = append(allowHeaders, byokHeader(pc.Name))
		}
	}
	allowHeadersStr := strings.Join(allowHeaders, ", ")
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := r.Header.Get("Origin")
		if origin != "" {
			switch {
			case list["*"]:
				w.Header().Set("Access-Control-Allow-Origin", "*")
			case list[origin]:
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Add("Vary", "Origin")
			}
			w.Header().Set("Access-Control-Allow-Headers", allowHeadersStr)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Expose-Headers", "Retry-After, X-Gateway-Provider, X-Gateway-Cache, X-Gateway-Latency-Ms, X-Gateway-Attempts, X-Gateway-Tried, X-Gateway-Model")
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ---------- simple endpoints ----------

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "ok", "providers": len(s.Router.Entries()), "store": s.Store.Kind(),
		"uptime_sec": int(time.Since(s.started).Seconds()),
	})
}

// handleReady also checks the database (use it for a deeper check; /healthz stays light so
// that uptime pings do not keep a sleeping database awake).
func (s *Server) handleReady(w http.ResponseWriter, r *http.Request) {
	if err := s.Store.Ping(r.Context()); err != nil {
		writeError(w, http.StatusServiceUnavailable, "server_error", "store_unavailable", "Database is not reachable.", 0, nil)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ready", "store": s.Store.Kind()})
}

func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	type model struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		OwnedBy string `json:"owned_by"`
		Note    string `json:"note,omitempty"`
	}
	out := []model{
		{ID: "auto", Object: "model", OwnedBy: "gateway", Note: "try providers in priority order, with fallback"},
		{ID: "fastest", Object: "model", OwnedBy: "gateway", Note: "try the provider with the lowest recent latency first"},
	}
	for _, e := range s.Router.Entries() {
		note := "only this provider (" + e.P.Model() + "), no fallback"
		if e.Paid {
			note += "; paid, only for keys allowed to use it"
		}
		out = append(out, model{ID: e.Name, Object: "model", OwnedBy: e.Name, Note: note})
	}
	for _, m := range s.Router.Models() {
		out = append(out, model{ID: m, Object: "model", OwnedBy: "provider",
			Note: "falls back to other models unless you send X-Gateway-Strict-Model: true"})
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": out})
}
