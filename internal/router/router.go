// Package router is the brain of the gateway. For each request it decides which
// provider to ask, skips providers that are broken or out of free quota, retries
// when a failure looks temporary, and falls back to the next provider when it must.
package router

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
	"github.com/srushtiinvent/llm-gateway/internal/resilience"
)

// Options controls the router's behaviour.
type Options struct {
	MaxRetries      int           // extra tries on the SAME provider after a temporary failure
	AttemptTimeout  time.Duration // time limit for one provider call (non-streaming)
	BackoffBase     time.Duration // first retry delay
	BackoffMax      time.Duration // retry delay ceiling
	CooldownDefault time.Duration // pause after a 429 that gave no Retry-After
	CooldownMax     time.Duration // never pause longer than this
}

// DefaultOptions are sensible starting values.
func DefaultOptions() Options {
	return Options{
		MaxRetries: 1, AttemptTimeout: 25 * time.Second,
		BackoffBase: 300 * time.Millisecond, BackoffMax: 3 * time.Second,
		CooldownDefault: 60 * time.Second, CooldownMax: 6 * time.Hour,
	}
}

// Entry is one provider plus everything the gateway tracks about it.
type Entry struct {
	Name           string
	P              provider.Provider
	Faults         *provider.Faults
	Breaker        *resilience.Breaker
	Quota          *quota.Tracker
	MaxInputTokens int
	Mock           bool

	ok, fail atomic.Int64
	ewmaBits atomic.Uint64 // moving average of latency in ms, stored as float64 bits

	mu        sync.Mutex
	lastErr   string
	lastErrAt time.Time
}

// EntryConfig describes how to build an Entry.
type EntryConfig struct {
	Provider         provider.Provider
	Limits           quota.Limits
	MaxInputTokens   int
	Mock             bool
	BreakerThreshold int
	BreakerOpenFor   time.Duration
}

// NewEntry wraps a provider with fault injection, a circuit breaker and a quota tracker.
func NewEntry(c EntryConfig) *Entry {
	f := provider.NewFaults()
	return &Entry{
		Name:           c.Provider.Name(),
		P:              &provider.Faulty{Provider: c.Provider, F: f},
		Faults:         f,
		Breaker:        resilience.NewBreaker(c.BreakerThreshold, c.BreakerOpenFor),
		Quota:          quota.New(c.Limits),
		MaxInputTokens: c.MaxInputTokens,
		Mock:           c.Mock,
	}
}

func (e *Entry) observe(d time.Duration) {
	ms := float64(d) / float64(time.Millisecond)
	for {
		old := e.ewmaBits.Load()
		next := ms
		if cur := math.Float64frombits(old); cur > 0 {
			next = 0.3*ms + 0.7*cur // recent calls count more
		}
		if e.ewmaBits.CompareAndSwap(old, math.Float64bits(next)) {
			return
		}
	}
}

// AvgLatencyMs is the moving average latency (0 = no data yet).
func (e *Entry) AvgLatencyMs() float64 { return math.Float64frombits(e.ewmaBits.Load()) }

func (e *Entry) recordFailure(err *provider.Error) {
	e.fail.Add(1)
	e.mu.Lock()
	e.lastErr = err.Error()
	e.lastErrAt = time.Now()
	e.mu.Unlock()
}

// Router chooses providers.
type Router struct {
	entries []*Entry
	byName  map[string]*Entry
	opts    Options
}

// New creates a Router. Entries are tried in the order given ("auto" strategy).
func New(entries []*Entry, opts Options) *Router {
	r := &Router{entries: entries, byName: map[string]*Entry{}, opts: opts}
	for _, e := range entries {
		r.byName[e.Name] = e
	}
	return r
}

// Entries returns the providers in priority order.
func (r *Router) Entries() []*Entry { return r.entries }

// Get finds a provider by name.
func (r *Router) Get(name string) (*Entry, bool) { e, ok := r.byName[name]; return e, ok }

// Strategy says how to pick providers for one request.
type Strategy struct {
	Mode string // "auto", "fastest" or "pinned"
	Pin  string // provider name when Mode is "pinned"
}

// Route is a short name used in cache keys and logs.
func (s Strategy) Route() string {
	if s.Mode == "pinned" {
		return s.Pin
	}
	return s.Mode
}

// Resolve turns the "model" field of a request into a Strategy:
//
//	"fastest"          -> try the provider with the lowest recent latency first
//	a provider's name  -> use ONLY that provider (no fallback)
//	anything else      -> "auto": try providers in priority order, with fallback
//
// Treating unknown model names as "auto" means existing apps that send "gpt-4o" still work.
func (r *Router) Resolve(model string) Strategy {
	m := strings.ToLower(strings.TrimSpace(model))
	if m == "fastest" {
		return Strategy{Mode: "fastest"}
	}
	if _, ok := r.byName[m]; ok {
		return Strategy{Mode: "pinned", Pin: m}
	}
	return Strategy{Mode: "auto"}
}

func (r *Router) candidates(st Strategy) []*Entry {
	switch st.Mode {
	case "pinned":
		return []*Entry{r.byName[st.Pin]}
	case "fastest":
		c := append([]*Entry(nil), r.entries...)
		sort.SliceStable(c, func(i, j int) bool { return c[i].AvgLatencyMs() < c[j].AvgLatencyMs() }) // unknown (0) first, so every provider gets measured
		return c
	}
	return r.entries
}

// Attempt is one line of the "what happened" story.
type Attempt struct {
	Provider string `json:"provider"`
	Outcome  string `json:"outcome"`
}

// Trace records what the router did for one request.
type Trace struct {
	Attempts []Attempt
	Calls    int // calls actually made to providers
	Provider string
	Model    string
	Trimmed  int // old messages dropped to fit the provider's context limit

	entry *Entry
	start time.Time
}

func (t *Trace) add(provider, outcome string) {
	t.Attempts = append(t.Attempts, Attempt{provider, outcome})
}

// Summary returns lines like "gemini:rate_limited", "groq:ok".
func (t *Trace) Summary() []string {
	out := make([]string, 0, len(t.Attempts))
	for _, a := range t.Attempts {
		out = append(out, a.Provider+":"+a.Outcome)
	}
	return out
}

// FailedError means no provider could answer.
type FailedError struct {
	Trace      *Trace
	Last       *provider.Error
	Kind       string        // "bad_request", "quota_exhausted" or "unavailable"
	RetryAfter time.Duration // suggested wait (0 = unknown)
	Pinned     bool
}

func (e *FailedError) Error() string {
	msg := "all providers failed"
	if e.Kind == "quota_exhausted" {
		msg = "free-tier quota used up on every provider"
	}
	if e.Last != nil {
		msg += ": " + e.Last.Error()
	}
	return msg
}

func maxOut(req *openai.ChatRequest) int {
	if req.MaxTokens != nil && *req.MaxTokens > 0 {
		return *req.MaxTokens
	}
	return 512 // assumed answer length when the app did not say
}

// trim drops the oldest messages (never system messages, never the last one) until the
// conversation fits `max` tokens. Different models have different context sizes, so a
// long chat that fits Gemini may be too long for a smaller backup model.
func trim(req *openai.ChatRequest, max int) (*openai.ChatRequest, int) {
	if max <= 0 || req.EstimatePromptTokens() <= max {
		return req, 0
	}
	msgs := append([]openai.Message(nil), req.Messages...)
	dropped := 0
	for {
		cp := *req
		cp.Messages = msgs
		if cp.EstimatePromptTokens() <= max {
			break
		}
		idx := -1
		for i := 0; i < len(msgs)-1; i++ { // never the last message
			if msgs[i].Role != "system" {
				idx = i
				break
			}
		}
		if idx < 0 {
			break
		}
		msgs = append(msgs[:idx], msgs[idx+1:]...)
		dropped++
	}
	cp := *req
	cp.Messages = msgs
	return &cp, dropped
}

func asProviderError(name string, err error) *provider.Error {
	var pe *provider.Error
	if errors.As(err, &pe) {
		return pe
	}
	return &provider.Error{Provider: name, Kind: provider.KindOther, Message: err.Error(), Err: err}
}

type callFn func(ctx context.Context, e *Entry, req *openai.ChatRequest) (out any, tokens int, err error)

// run is the heart of the gateway: walk through the candidate providers and return the first success.
func (r *Router) run(ctx context.Context, req *openai.ChatRequest, st Strategy, timeout time.Duration, call callFn) (any, *Trace, error) {
	tr := &Trace{}
	var lastErr *provider.Error
	var minRetry time.Duration
	setRetry := func(d time.Duration) {
		if d > 0 && (minRetry == 0 || d < minRetry) {
			minRetry = d
		}
	}
	var finals []string // one verdict per provider: "quota", "bad_request" or "other"

	for _, e := range r.candidates(st) {
		rq, dropped := trim(req, e.MaxInputTokens)
		final := "other"

		for attempt := 0; attempt <= r.opts.MaxRetries; attempt++ {
			if attempt == 0 && !e.Breaker.Allow() { // circuit open: do not even try
				tr.add(e.Name, "skipped_circuit_open")
				setRetry(e.Breaker.Snapshot().RetryIn)
				break
			}

			est := rq.EstimatePromptTokens() + maxOut(rq)
			res, den := e.Quota.Acquire(est)
			if den != nil { // out of free quota: skip before wasting a call
				e.Breaker.Release()
				tr.add(e.Name, "skipped_quota_"+den.Reason)
				setRetry(den.RetryAfter)
				final = "quota"
				break
			}

			actx, cancel := ctx, context.CancelFunc(func() {})
			if timeout > 0 {
				actx, cancel = context.WithTimeout(ctx, timeout)
			}
			start := time.Now()
			out, tokens, err := call(actx, e, rq)
			cancel()
			tr.Calls++

			if err == nil {
				e.Breaker.Success()
				res.Settle(tokens)
				e.observe(time.Since(start))
				e.ok.Add(1)
				tr.add(e.Name, "ok")
				tr.Provider, tr.Model, tr.Trimmed = e.Name, e.P.Model(), dropped
				tr.entry, tr.start = e, start
				return out, tr, nil
			}

			if ctx.Err() != nil { // the caller gave up: not the provider's fault
				res.Cancel()
				e.Breaker.Release()
				return nil, tr, ctx.Err()
			}

			perr := asProviderError(e.Name, err)
			lastErr = perr
			e.recordFailure(perr)
			tr.add(e.Name, perr.Kind.String())

			if perr.Refundable() {
				res.Cancel() // probably did not count against the free quota
			} else {
				res.Settle(est)
			}
			if perr.CountsAgainstBreaker() {
				e.Breaker.Failure()
			} else {
				e.Breaker.Release()
			}

			switch perr.Kind {
			case provider.KindRateLimited:
				d := perr.RetryAfter
				if d <= 0 {
					d = r.opts.CooldownDefault
				}
				if d > r.opts.CooldownMax {
					d = r.opts.CooldownMax
				}
				e.Quota.Cooldown(d) // the provider says we are over the limit: believe it
				setRetry(d)
				final = "quota"
			case provider.KindBadRequest:
				final = "bad_request"
			default:
				final = "other"
			}

			// Temporary failure and retries left? Wait a little (with jitter) and try the same provider again.
			if perr.Retryable() && attempt < r.opts.MaxRetries && e.Breaker.Allow() {
				if err := resilience.Sleep(ctx, resilience.Backoff(attempt, r.opts.BackoffBase, r.opts.BackoffMax, nil)); err != nil {
					e.Breaker.Release()
					return nil, tr, err
				}
				continue
			}
			break
		}
		finals = append(finals, final)
	}

	fe := &FailedError{Trace: tr, Last: lastErr, RetryAfter: minRetry, Pinned: st.Mode == "pinned", Kind: "unavailable"}
	allBad, allQuota := len(finals) > 0, len(finals) > 0
	for _, f := range finals {
		allBad = allBad && f == "bad_request"
		allQuota = allQuota && f == "quota"
	}
	switch {
	case allBad:
		fe.Kind = "bad_request"
	case allQuota:
		fe.Kind = "quota_exhausted"
	}
	return nil, tr, fe
}

// Chat gets a full (non-streaming) answer.
func (r *Router) Chat(ctx context.Context, req *openai.ChatRequest, st Strategy) (*openai.ChatResponse, *Trace, error) {
	out, tr, err := r.run(ctx, req, st, r.opts.AttemptTimeout, func(ctx context.Context, e *Entry, rq *openai.ChatRequest) (any, int, error) {
		resp, err := e.P.Chat(ctx, rq)
		if err != nil {
			return nil, 0, err
		}
		return resp, resp.Usage.TotalTokens, nil
	})
	if err != nil {
		return nil, tr, err
	}
	return out.(*openai.ChatResponse), tr, nil
}

// StreamResult is an open streaming answer. Fallback is only possible BEFORE the first
// piece is sent to the user; once text has started flowing, a failure ends the stream.
type StreamResult struct {
	Stream provider.Stream
	Trace  *Trace
	entry  *Entry
	once   sync.Once
}

// ChatStream opens a streaming answer on the first provider that accepts the request.
func (r *Router) ChatStream(ctx context.Context, req *openai.ChatRequest, st Strategy) (*StreamResult, *Trace, error) {
	out, tr, err := r.run(ctx, req, st, 0, func(ctx context.Context, e *Entry, rq *openai.ChatRequest) (any, int, error) {
		s, err := e.P.ChatStream(ctx, rq)
		if err != nil {
			return nil, 0, err
		}
		return s, 0, nil
	})
	if err != nil {
		return nil, tr, err
	}
	return &StreamResult{Stream: out.(provider.Stream), Trace: tr, entry: tr.entry}, tr, nil
}

// Finish closes the stream. Pass the error that ended it (nil if it finished normally, or the
// caller's context error if the user disconnected).
func (s *StreamResult) Finish(err error, clientGone bool) {
	s.once.Do(func() {
		_ = s.Stream.Close()
		if err != nil && !clientGone {
			s.entry.recordFailure(asProviderError(s.entry.Name, err))
			s.entry.Breaker.Failure()
		}
	})
}

// ProviderStatus is what the dashboard shows for one provider.
type ProviderStatus struct {
	Name                string         `json:"name"`
	Model               string         `json:"model"`
	Mock                bool           `json:"mock"`
	Breaker             string         `json:"breaker"` // closed, open, half_open
	ConsecutiveFailures int            `json:"consecutive_failures"`
	Trips               int            `json:"breaker_trips"`
	RetryInSec          int            `json:"retry_in_sec"`
	Quota               quota.Snapshot `json:"quota"`
	RequestsOK          int64          `json:"requests_ok"`
	RequestsFailed      int64          `json:"requests_failed"`
	AvgLatencyMs        float64        `json:"avg_latency_ms"`
	Fault               string         `json:"fault"`
	LastError           string         `json:"last_error,omitempty"`
	LastErrorAt         *time.Time     `json:"last_error_at,omitempty"`
}

// Snapshot returns the status of every provider.
func (r *Router) Snapshot() []ProviderStatus {
	out := make([]ProviderStatus, 0, len(r.entries))
	for _, e := range r.entries {
		b := e.Breaker.Snapshot()
		ps := ProviderStatus{
			Name: e.Name, Model: e.P.Model(), Mock: e.Mock,
			Breaker: b.State.String(), ConsecutiveFailures: b.Failures, Trips: b.Trips,
			RetryInSec: int(b.RetryIn.Round(time.Second) / time.Second),
			Quota:      e.Quota.Snapshot(),
			RequestsOK: e.ok.Load(), RequestsFailed: e.fail.Load(),
			AvgLatencyMs: math.Round(e.AvgLatencyMs()),
			Fault:        string(e.Faults.Get()),
		}
		e.mu.Lock()
		ps.LastError = e.lastErr
		if !e.lastErrAt.IsZero() {
			t := e.lastErrAt
			ps.LastErrorAt = &t
		}
		e.mu.Unlock()
		out = append(out, ps)
	}
	return out
}
