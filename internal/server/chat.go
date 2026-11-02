package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/cache"
	"github.com/srushtiinvent/llm-gateway/internal/limiter"
	"github.com/srushtiinvent/llm-gateway/internal/openai"
	"github.com/srushtiinvent/llm-gateway/internal/router"
	"github.com/srushtiinvent/llm-gateway/internal/store"
)

func validate(req *openai.ChatRequest) string {
	if len(req.Messages) == 0 {
		return "'messages' must contain at least one message."
	}
	if len(req.Messages) > 200 {
		return "'messages' may contain at most 200 messages."
	}
	total := 0
	for i, m := range req.Messages {
		switch m.Role {
		case "system", "user", "assistant":
		default:
			return fmt.Sprintf("messages[%d].role must be 'system', 'user' or 'assistant'.", i)
		}
		total += len(m.Content)
	}
	if total > 400_000 {
		return "The conversation is too long (over 400,000 characters)."
	}
	if req.Temperature != nil && (*req.Temperature < 0 || *req.Temperature > 2) {
		return "'temperature' must be between 0 and 2."
	}
	if req.MaxTokens != nil && *req.MaxTokens < 1 {
		return "'max_tokens' must be at least 1."
	}
	return ""
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}

func untilUTCMidnight() time.Duration {
	now := time.Now().UTC()
	return now.Truncate(24 * time.Hour).Add(24 * time.Hour).Sub(now)
}

func gatewayHeaders(w http.ResponseWriter, info *openai.GatewayInfo) {
	h := w.Header()
	h.Set("X-Gateway-Provider", info.Provider)
	h.Set("X-Gateway-Cache", info.Cache)
	h.Set("X-Gateway-Attempts", fmt.Sprint(info.Attempts))
	if info.LatencyMs > 0 {
		h.Set("X-Gateway-Latency-Ms", fmt.Sprint(info.LatencyMs))
	}
	if len(info.Tried) > 0 {
		h.Set("X-Gateway-Tried", strings.Join(info.Tried, ","))
	}
}

func writeSSE(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "data: %s\n\n", b)
	return err
}

// handleChat is POST /v1/chat/completions: the main endpoint.
//
//  1. who are you?            (API key)
//  2. are you asking too often? (per-key rate limit and daily budget)
//  3. do I already know this?   (cache)
//  4. is there room right now?  (queue / backpressure)
//  5. ask a provider            (router: fallback, retries, quotas, circuit breakers)
//  6. remember and log the result
func (s *Server) handleChat(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	ctx := r.Context()

	// 1. identity
	key, err := s.Auth.Authenticate(ctx, r)
	if err != nil {
		switch {
		case errors.Is(err, auth.ErrMissingKey):
			writeError(w, http.StatusUnauthorized, "authentication_error", "missing_api_key", err.Error(), 0, nil)
		case errors.Is(err, auth.ErrInvalidKey):
			writeError(w, http.StatusUnauthorized, "authentication_error", "invalid_api_key", err.Error(), 0, nil)
		default:
			s.Logger.Error("key lookup failed", "err", err)
			writeError(w, http.StatusServiceUnavailable, "server_error", "auth_unavailable", "Could not check your API key right now. Try again shortly.", 3*time.Second, nil)
		}
		return
	}

	// 2. limits for this key (per-minute rate; the daily budget comes after the cache)
	if ok, wait := s.Limiter.Allow(key.ID, key.RPMLimit); !ok {
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "rate_limit_exceeded",
			fmt.Sprintf("Too many requests for this key (limit: %d per minute). Try again in %d seconds.", key.RPMLimit, int(wait.Seconds())+1), wait, nil)
		return
	}

	// read and check the request
	r.Body = http.MaxBytesReader(w, r.Body, 1<<20)
	var req openai.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_json", "Request body must be valid JSON (max 1 MB): "+err.Error(), 0, nil)
		return
	}
	if msg := validate(&req); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "invalid_request", msg, 0, nil)
		return
	}

	st := s.Router.Resolve(req.Model)
	lg := store.RequestLog{KeyID: key.ID, RequestedModel: req.Model, Stream: req.Stream, Status: "ok"}
	defer func() {
		lg.LatencyMs = time.Since(start).Milliseconds()
		s.logs.add(lg)
	}()

	// 3. cache
	bypass := r.Header.Get("X-Gateway-Cache") == "bypass" || strings.Contains(strings.ToLower(r.Header.Get("Cache-Control")), "no-cache")
	ckey := cache.Key(&req, st.Route())
	if !bypass {
		if hit, ok := s.Cache.Get(ckey); ok {
			s.serveCached(w, &req, hit, start, &lg)
			return
		}
	}

	// Daily budget: counts only requests that reach a provider, so it is checked AFTER the cache
	// (answers from the cache are free and keep working even when the budget is used up).
	if !s.Budget.Allow(ctx, key) {
		lg.Status, lg.Error = "error", "daily_budget_exceeded"
		writeError(w, http.StatusTooManyRequests, "rate_limit_error", "daily_budget_exceeded",
			fmt.Sprintf("This key has used its daily budget (%d requests that reached a provider). It resets at 00:00 UTC. Repeated questions answered from the cache are free.", key.DailyLimit),
			untilUTCMidnight(), nil)
		return
	}

	// 4. wait for a free slot (or be told to come back later)
	release, waited, err := s.Queue.Acquire(ctx)
	if err != nil {
		lg.Status = "error"
		if errors.Is(err, limiter.ErrOverloaded) {
			lg.Error = "overloaded"
			writeError(w, http.StatusServiceUnavailable, "overloaded_error", "overloaded", "The gateway is busy right now. Please try again in a few seconds.", 5*time.Second, nil)
		} else {
			lg.Error = "client_disconnected"
		}
		return
	}
	defer release()

	// 5 + 6. ask a provider
	if req.Stream {
		s.chatStream(w, r, &req, st, ckey, bypass, key, start, waited, &lg)
		return
	}
	s.chatOnce(w, r, &req, st, ckey, bypass, key, start, waited, &lg)
}

func (s *Server) chatOnce(w http.ResponseWriter, r *http.Request, req *openai.ChatRequest, st router.Strategy,
	ckey string, bypass bool, key *store.Key, start time.Time, waited time.Duration, lg *store.RequestLog) {

	resp, tr, err := s.Router.Chat(r.Context(), req, st)
	lg.Attempts = tr.Calls
	lg.Tried = strings.Join(tr.Summary(), ",")
	if err != nil {
		s.writeRouteError(w, r.Context(), err, tr, lg)
		return
	}
	s.Budget.Used(key)

	if resp.Usage.PromptTokens == 0 && resp.Usage.CompletionTokens == 0 { // provider did not report usage
		resp.Usage.PromptTokens = req.EstimatePromptTokens()
		if len(resp.Choices) > 0 {
			resp.Usage.CompletionTokens = openai.EstimateTokens(resp.Choices[0].Message.Content)
		}
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}
	resp.ID, resp.Object = openai.NewID(), "chat.completion"
	if resp.Created == 0 {
		resp.Created = openai.Now()
	}
	if !bypass {
		s.Cache.Put(ckey, resp, tr.Provider)
	}
	info := &openai.GatewayInfo{
		Provider: tr.Provider, Cache: "miss", LatencyMs: time.Since(start).Milliseconds(),
		Attempts: tr.Calls, Tried: tr.Summary(), Trimmed: tr.Trimmed, QueuedMs: waited.Milliseconds(),
	}
	resp.Gateway = info

	lg.Provider, lg.Model = tr.Provider, resp.Model
	lg.PromptTokens, lg.CompletionTokens = resp.Usage.PromptTokens, resp.Usage.CompletionTokens

	gatewayHeaders(w, info)
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) chatStream(w http.ResponseWriter, r *http.Request, req *openai.ChatRequest, st router.Strategy,
	ckey string, bypass bool, key *store.Key, start time.Time, waited time.Duration, lg *store.RequestLog) {

	ctx := r.Context()
	sr, tr, err := s.Router.ChatStream(ctx, req, st)
	lg.Attempts = tr.Calls
	lg.Tried = strings.Join(tr.Summary(), ",")
	if err != nil {
		s.writeRouteError(w, ctx, err, tr, lg)
		return
	}
	s.Budget.Used(key)
	lg.Provider, lg.Model = tr.Provider, tr.Model

	// From here on we are committed to this provider: the first bytes are about to reach the user.
	gatewayHeaders(w, &openai.GatewayInfo{Provider: tr.Provider, Cache: "miss", Attempts: tr.Calls, Tried: tr.Summary()})
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no") // ask proxies not to hold the stream back
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	id := openai.NewID()
	var text strings.Builder
	var usage *openai.Usage
	finished := false
	var streamErr error
	for {
		chunk, err := sr.Stream.Next()
		if errors.Is(err, io.EOF) {
			finished = true
			break
		}
		if err != nil {
			streamErr = err
			break
		}
		chunk.ID, chunk.Object = id, "chat.completion.chunk"
		if chunk.Created == 0 {
			chunk.Created = openai.Now()
		}
		for _, c := range chunk.Choices {
			text.WriteString(c.Delta.Content)
		}
		if chunk.Usage != nil {
			usage = chunk.Usage
		}
		if werr := writeSSE(w, chunk); werr != nil { // user disconnected
			streamErr = werr
			break
		}
		_ = rc.Flush()
	}

	clientGone := ctx.Err() != nil || (streamErr != nil && !finished && isWriteError(streamErr))
	sr.Finish(streamErr, clientGone)

	switch {
	case streamErr != nil && !clientGone:
		lg.Status, lg.Error = "error", truncate("stream interrupted: "+streamErr.Error(), 300)
		_ = writeSSE(w, map[string]any{"error": map[string]any{"message": "The provider stopped answering in the middle of the reply.", "type": "server_error", "code": "stream_interrupted"}})
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	case clientGone:
		lg.Status, lg.Error = "error", "client_disconnected"
	default:
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		_ = rc.Flush()
	}

	pt, ct := req.EstimatePromptTokens(), openai.EstimateTokens(text.String())
	if usage != nil && (usage.PromptTokens > 0 || usage.CompletionTokens > 0) {
		pt, ct = usage.PromptTokens, usage.CompletionTokens
	}
	lg.PromptTokens, lg.CompletionTokens = pt, ct

	if finished && streamErr == nil && !bypass && text.Len() > 0 { // remember the full answer for next time
		s.Cache.Put(ckey, &openai.ChatResponse{
			ID: id, Object: "chat.completion", Created: openai.Now(), Model: tr.Model,
			Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: text.String()}, FinishReason: "stop"}},
			Usage:   openai.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct},
		}, tr.Provider)
	}
}

func isWriteError(err error) bool {
	// Errors from the provider are *provider.Error values; anything else while streaming to the
	// user (broken pipe, reset connection) means the user went away.
	var pe interface{ Retryable() bool }
	return !errors.As(err, &pe)
}

// serveCached answers from memory without calling any provider.
func (s *Server) serveCached(w http.ResponseWriter, req *openai.ChatRequest, hit *cache.Entry, start time.Time, lg *store.RequestLog) {
	resp := hit.Response
	text := ""
	if len(resp.Choices) > 0 {
		text = resp.Choices[0].Message.Content
	}
	lg.Provider, lg.Model, lg.CacheHit = hit.Provider, resp.Model, true
	lg.PromptTokens, lg.CompletionTokens = resp.Usage.PromptTokens, resp.Usage.CompletionTokens

	info := &openai.GatewayInfo{Provider: hit.Provider, Cache: "hit", LatencyMs: time.Since(start).Milliseconds(), Attempts: 0}

	if !req.Stream {
		resp.ID, resp.Object, resp.Created = openai.NewID(), "chat.completion", openai.Now()
		resp.Gateway = info
		gatewayHeaders(w, info)
		writeJSON(w, http.StatusOK, resp)
		return
	}

	// Streaming client, cached answer: replay it as a short stream so the client code works unchanged.
	gatewayHeaders(w, info)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	id, created := openai.NewID(), openai.Now()
	stop := "stop"
	mk := func(d openai.Delta, fin *string) *openai.StreamChunk {
		return &openai.StreamChunk{ID: id, Object: "chat.completion.chunk", Created: created, Model: resp.Model,
			Choices: []openai.StreamChoice{{Index: 0, Delta: d, FinishReason: fin}}}
	}
	_ = writeSSE(w, mk(openai.Delta{Role: "assistant"}, nil))
	_ = writeSSE(w, mk(openai.Delta{Content: text}, nil))
	_ = writeSSE(w, mk(openai.Delta{}, &stop))
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	_ = rc.Flush()
}

// writeRouteError turns a router failure into an HTTP error the caller can act on.
func (s *Server) writeRouteError(w http.ResponseWriter, ctx context.Context, err error, tr *router.Trace, lg *store.RequestLog) {
	lg.Status = "error"
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		lg.Error = "client_disconnected"
		return
	}
	extra := map[string]any{"tried": tr.Summary()}
	var fe *router.FailedError
	if errors.As(err, &fe) {
		lg.Error = truncate(fe.Error(), 300)
		switch fe.Kind {
		case "bad_request":
			writeError(w, http.StatusBadRequest, "invalid_request_error", "provider_rejected_request",
				"The AI provider rejected this request: "+fe.Last.Message, 0, extra)
		case "quota_exhausted":
			msg := "Every provider has used up its free-tier limit right now."
			if fe.Pinned {
				msg = "That provider has used up its free-tier limit right now."
			}
			if fe.RetryAfter > 0 {
				msg += fmt.Sprintf(" The next slot frees up in about %s.", fe.RetryAfter.Round(time.Second))
			}
			writeError(w, http.StatusTooManyRequests, "rate_limit_error", "free_quota_exhausted", msg, fe.RetryAfter, extra)
		default:
			msg := "No AI provider could answer right now."
			if fe.Last != nil {
				msg += " Last error: " + fe.Last.Error()
			}
			writeError(w, http.StatusServiceUnavailable, "server_error", "all_providers_failed", msg, minDuration(fe.RetryAfter, 30*time.Second), extra)
		}
		return
	}
	lg.Error = truncate(err.Error(), 300)
	s.Logger.Error("unexpected routing error", "err", err)
	writeError(w, http.StatusInternalServerError, "server_error", "internal_error", "Something went wrong inside the gateway.", 0, extra)
}

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return 0
	}
	if a < b {
		return a
	}
	return b
}
