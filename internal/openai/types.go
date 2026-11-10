// Package openai holds the request and response shapes of the OpenAI chat API.
//
// The gateway speaks this format on both sides: apps call the gateway with it,
// and the gateway calls Gemini, Groq and OpenRouter with it (all three offer an
// OpenAI-compatible endpoint). Because it is the de-facto standard, any existing
// OpenAI client library can use the gateway by changing one URL.
package openai

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Message is one turn of a conversation. Role is "system", "user" or "assistant".
type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// ChatRequest is what an app sends to POST /v1/chat/completions.
type ChatRequest struct {
	Model       string    `json:"model"`
	Messages    []Message `json:"messages"`
	Temperature *float64  `json:"temperature,omitempty"`
	MaxTokens   *int      `json:"max_tokens,omitempty"`
	TopP        *float64  `json:"top_p,omitempty"`
	Stream      bool      `json:"stream,omitempty"`
}

// Usage reports how many tokens a request used.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// Choice is one possible answer.
type Choice struct {
	Index        int     `json:"index"`
	Message      Message `json:"message"`
	FinishReason string  `json:"finish_reason"`
}

// GatewayInfo is extra information the gateway adds to every response, so the
// caller (and the website) can see what happened behind the scenes.
type GatewayInfo struct {
	Provider  string   `json:"provider"`            // who produced the answer
	Cache     string   `json:"cache"`               // "hit" or "miss"
	LatencyMs int64    `json:"latency_ms"`          // total time inside the gateway
	Attempts  int      `json:"attempts"`            // calls made to providers
	Tried     []string `json:"tried,omitempty"`     // e.g. ["gemini:rate_limited","groq:ok"]
	Trimmed   int      `json:"trimmed,omitempty"`   // old messages dropped to fit a small context window
	QueuedMs  int64    `json:"queued_ms,omitempty"` // time spent waiting in the queue
}

// ChatResponse is the non-streaming answer.
type ChatResponse struct {
	ID      string       `json:"id"`
	Object  string       `json:"object"`
	Created int64        `json:"created"`
	Model   string       `json:"model"`
	Choices []Choice     `json:"choices"`
	Usage   Usage        `json:"usage"`
	Gateway *GatewayInfo `json:"gateway,omitempty"`
}

// Delta is the piece of text carried by one streaming chunk.
type Delta struct {
	Role    string `json:"role,omitempty"`
	Content string `json:"content,omitempty"`
}

// StreamChoice is one choice inside a streaming chunk.
type StreamChoice struct {
	Index        int     `json:"index"`
	Delta        Delta   `json:"delta"`
	FinishReason *string `json:"finish_reason"`
}

// ChunkError is an error a provider may send in the middle of a stream.
type ChunkError struct {
	Message string `json:"message"`
	Code    any    `json:"code,omitempty"`
}

// StreamChunk is one server-sent event of a streaming answer.
type StreamChunk struct {
	ID      string         `json:"id"`
	Object  string         `json:"object"`
	Created int64          `json:"created"`
	Model   string         `json:"model"`
	Choices []StreamChoice `json:"choices"`
	Usage   *Usage         `json:"usage,omitempty"`
	Error   *ChunkError    `json:"error,omitempty"`
}

// EstimateTokens guesses the token count of a text (about 4 characters per token).
// It is only used for planning quotas; real counts come from the provider when given.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + 3) / 4
}

// EstimatePromptTokens guesses how many tokens the whole conversation uses.
func (r *ChatRequest) EstimatePromptTokens() int {
	n := 0
	for _, m := range r.Messages {
		n += EstimateTokens(m.Content) + 4 // a few tokens of overhead per message
	}
	return n
}

// NewID makes an id like "chatcmpl-gw-3f9a1c2b7d10".
func NewID() string {
	b := make([]byte, 6)
	_, _ = rand.Read(b)
	return "chatcmpl-gw-" + hex.EncodeToString(b)
}

// Now returns the current unix time (used for the "created" field).
func Now() int64 { return time.Now().Unix() }
