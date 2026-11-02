package provider

import (
	"context"
	"fmt"
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
	switch w.F.Get() {
	case FaultDown:
		return &Error{Provider: w.Name(), Kind: KindServer, Status: 503, Message: "simulated outage"}
	case FaultRateLimited:
		return &Error{Provider: w.Name(), Kind: KindRateLimited, Status: 429, Message: "simulated rate limit", RetryAfter: 30 * time.Second}
	case FaultSlow:
		t := time.NewTimer(w.F.SlowDelay)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return &Error{Provider: w.Name(), Kind: KindTimeout, Message: "simulated slowness: " + ctx.Err().Error(), Err: ctx.Err()}
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
