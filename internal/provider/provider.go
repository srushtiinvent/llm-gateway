// Package provider contains the "adapters" that talk to AI providers.
//
// Every provider implements the same small interface, so the router can treat
// Gemini, Groq, OpenRouter (or a fake mock) in exactly the same way. Adding a new
// provider means writing one more adapter, nothing else changes.
package provider

import (
	"context"
	"fmt"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

// Provider is one AI backend.
type Provider interface {
	Name() string  // short id, e.g. "gemini"
	Model() string // the real model name used, e.g. "gemini-2.5-flash"

	// Chat sends the conversation and waits for the full answer.
	Chat(ctx context.Context, req *openai.ChatRequest) (*openai.ChatResponse, error)

	// ChatStream starts a streaming answer. The caller must Close the stream.
	ChatStream(ctx context.Context, req *openai.ChatRequest) (Stream, error)
}

// Stream yields the answer piece by piece.
type Stream interface {
	// Next returns the next chunk, or io.EOF when the answer is complete.
	Next() (*openai.StreamChunk, error)
	Close() error
}

// ErrKind says what went wrong, because the gateway reacts differently to each kind.
type ErrKind int

const (
	KindNetwork     ErrKind = iota + 1 // could not reach the provider
	KindTimeout                        // provider took too long
	KindServer                         // provider answered 5xx
	KindRateLimited                    // provider answered 429 (free limit reached)
	KindAuth                           // 401/403: bad or missing API key
	KindBadRequest                     // 400/404/413/422: provider rejected the request
	KindOther
)

func (k ErrKind) String() string {
	switch k {
	case KindNetwork:
		return "network_error"
	case KindTimeout:
		return "timeout"
	case KindServer:
		return "server_error"
	case KindRateLimited:
		return "rate_limited"
	case KindAuth:
		return "auth_error"
	case KindBadRequest:
		return "bad_request"
	default:
		return "error"
	}
}

// Error is an error from a provider, with enough detail for the router to decide what to do.
type Error struct {
	Provider   string
	Kind       ErrKind
	Status     int
	Message    string
	RetryAfter time.Duration // set when the provider told us how long to wait
	Err        error
}

func (e *Error) Error() string {
	if e.Status != 0 {
		return fmt.Sprintf("%s: %s (HTTP %d): %s", e.Provider, e.Kind, e.Status, e.Message)
	}
	return fmt.Sprintf("%s: %s: %s", e.Provider, e.Kind, e.Message)
}

func (e *Error) Unwrap() error { return e.Err }

// Retryable reports whether trying the same provider again might work.
func (e *Error) Retryable() bool {
	return e.Kind == KindNetwork || e.Kind == KindTimeout || e.Kind == KindServer
}

// CountsAgainstBreaker reports whether this error should push the circuit breaker toward "open".
// A 429 or a 400 is not a sign the provider is broken, so those do not count.
func (e *Error) CountsAgainstBreaker() bool {
	return e.Kind == KindNetwork || e.Kind == KindTimeout || e.Kind == KindServer || e.Kind == KindAuth
}

// Refundable reports whether the failed call probably did not use up free-tier quota.
func (e *Error) Refundable() bool {
	return e.Kind == KindNetwork || e.Kind == KindTimeout || e.Kind == KindServer
}

// KindFromStatus turns an HTTP status code into an ErrKind.
func KindFromStatus(status int) ErrKind {
	switch {
	case status == 429:
		return KindRateLimited
	case status == 401 || status == 403:
		return KindAuth
	case status >= 500:
		return KindServer
	case status == 408:
		return KindTimeout
	case status >= 400:
		return KindBadRequest
	default:
		return KindOther
	}
}
