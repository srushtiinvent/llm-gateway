package provider

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

// FaultMode is a pretend problem you can switch on for a provider.
// This powers the "failure simulator": press a button, watch the gateway recover.
type FaultMode string

const (
	FaultNone        FaultMode = "none"
	FaultDown        FaultMode = "down"        // provider answers 503
	FaultSlow        FaultMode = "slow"        // provider takes several extra seconds
	FaultRateLimited FaultMode = "ratelimited" // provider answers 429
)

// ParseFault checks a mode name.
func ParseFault(s string) (FaultMode, error) {
	switch FaultMode(s) {
	case FaultNone, FaultDown, FaultSlow, FaultRateLimited:
		return FaultMode(s), nil
	}
	return FaultNone, fmt.Errorf("unknown fault mode %q (use none, down, slow or ratelimited)", s)
}

type simKey struct{}

// WithSim attaches a per-request fault plan (provider name -> mode) to a context.
// It affects only that one request, so a visitor can try a "broken provider" without
// disturbing anyone else.
func WithSim(ctx context.Context, plan map[string]FaultMode) context.Context {
	if len(plan) == 0 {
		return ctx
	}
	return context.WithValue(ctx, simKey{}, plan)
}

func simFrom(ctx context.Context, name string) (FaultMode, bool) {
	plan, _ := ctx.Value(simKey{}).(map[string]FaultMode)
	m, ok := plan[name]
	return m, ok
}

// ParseSimHeader reads "gemini=down,groq=slow" into a plan. Unknown names/modes are ignored.
func ParseSimHeader(v string, known func(string) bool) map[string]FaultMode {
	plan := map[string]FaultMode{}
	for _, part := range strings.Split(v, ",") {
		name, mode, ok := strings.Cut(strings.TrimSpace(part), "=")
		if !ok {
			continue
		}
		name = strings.ToLower(strings.TrimSpace(name))
		m, err := ParseFault(strings.ToLower(strings.TrimSpace(mode)))
		if err != nil || !known(name) {
			continue
		}
		plan[name] = m
	}
	return plan
}

// Faults holds the current fault mode of one provider. It is safe to change at any time.
type Faults struct {
	mode      atomic.Value // FaultMode
	SlowDelay time.Duration
}

// NewFaults creates a Faults switch in the "none" position.
func NewFaults() *Faults {
	f := &Faults{SlowDelay: 8 * time.Second}
	f.mode.Store(FaultNone)
	return f
}

func (f *Faults) Set(m FaultMode) { f.mode.Store(m) }
func (f *Faults) Get() FaultMode  { return f.mode.Load().(FaultMode) }

// Faulty wraps a real provider and applies the current fault mode before each call.
type Faulty struct {
	Provider
	F *Faults
}

func (w *Faulty) inject(ctx context.Context) error {
	mode, sim := w.F.Get(), false
	if m, ok := simFrom(ctx, w.Name()); ok {
		mode, sim = m, true
	}
	switch mode {
	case FaultDown:
		return &Error{Provider: w.Name(), Kind: KindServer, Status: 503, Message: "simulated outage", Simulated: sim}
	case FaultRateLimited:
		return &Error{Provider: w.Name(), Kind: KindRateLimited, Status: 429, Message: "simulated rate limit", RetryAfter: 30 * time.Second, Simulated: sim}
	case FaultSlow:
		delay := w.F.SlowDelay
		if sim {
			delay = 2500 * time.Millisecond // a visitor's own test: slow, but not painful
		}
		t := time.NewTimer(delay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return &Error{Provider: w.Name(), Kind: KindTimeout, Message: "simulated slowness: " + ctx.Err().Error(), Err: ctx.Err(), Simulated: sim}
		}
	}
	return nil
}

// Chat implements Provider.
func (w *Faulty) Chat(ctx context.Context, req *openai.ChatRequest) (*openai.ChatResponse, error) {
	if err := w.inject(ctx); err != nil {
		return nil, err
	}
	return w.Provider.Chat(ctx, req)
}

// ChatStream implements Provider.
func (w *Faulty) ChatStream(ctx context.Context, req *openai.ChatRequest) (Stream, error) {
	if err := w.inject(ctx); err != nil {
		return nil, err
	}
	return w.Provider.ChatStream(ctx, req)
}
