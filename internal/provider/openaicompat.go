package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

// OpenAICompat talks to any provider that offers an OpenAI-style
// "/chat/completions" endpoint. Gemini, Groq and OpenRouter all do, so one adapter
// serves all three; only the base URL, key and model differ.
type OpenAICompat struct {
	name    string
	baseURL string
	apiKey  string
	model   string
	headers map[string]string
	client  *http.Client
}

// One connection pool for every provider adapter. Visitor ("bring your own key") requests create
// short-lived adapters, so they must not each bring their own pool.
var sharedTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	MaxIdleConnsPerHost:   10,
	IdleConnTimeout:       90 * time.Second,
	TLSHandshakeTimeout:   10 * time.Second,
	ResponseHeaderTimeout: 20 * time.Second, // how long to wait for the provider to start answering
}

// NewOpenAICompat creates the adapter.
func NewOpenAICompat(name, baseURL, apiKey, model string, extraHeaders map[string]string) *OpenAICompat {
	return &OpenAICompat{
		name:    name,
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		headers: extraHeaders,
		client:  &http.Client{Transport: sharedTransport}, // no overall timeout: the context controls it
	}
}

func (p *OpenAICompat) Name() string  { return p.name }
func (p *OpenAICompat) Model() string { return p.model }

// body is what we send to the provider (the gateway's model name is replaced by the real one).
type body struct {
	Model       string           `json:"model"`
	Messages    []openai.Message `json:"messages"`
	Temperature *float64         `json:"temperature,omitempty"`
	MaxTokens   *int             `json:"max_tokens,omitempty"`
	TopP        *float64         `json:"top_p,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

func (p *OpenAICompat) newRequest(ctx context.Context, req *openai.ChatRequest, stream bool) (*http.Request, error) {
	b, err := json.Marshal(body{
		Model: p.model, Messages: req.Messages, Temperature: req.Temperature,
		MaxTokens: req.MaxTokens, TopP: req.TopP, Stream: stream,
	})
	if err != nil {
		return nil, err
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/chat/completions", bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	hr.Header.Set("Authorization", "Bearer "+p.apiKey)
	if stream {
		hr.Header.Set("Accept", "text/event-stream")
	}
	for k, v := range p.headers {
		hr.Header.Set(k, v)
	}
	return hr, nil
}

// netError converts a low-level network error into our Error type.
func (p *OpenAICompat) netError(err error) *Error {
	kind := KindNetwork
	var ne net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		kind = KindTimeout
	}
	return &Error{Provider: p.name, Kind: kind, Message: err.Error(), Err: err}
}

// httpError builds an Error from a non-200 response.
func (p *OpenAICompat) httpError(resp *http.Response) *Error {
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	msg := strings.TrimSpace(string(raw))
	var parsed struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal(raw, &parsed) == nil && parsed.Error.Message != "" {
		msg = parsed.Error.Message
	} else {
		// Some providers return a JSON array with the error inside.
		var arr []struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &arr) == nil && len(arr) > 0 && arr[0].Error.Message != "" {
			msg = arr[0].Error.Message
		}
	}
	if len(msg) > 300 {
		msg = msg[:300] + "…"
	}
	e := &Error{Provider: p.name, Kind: KindFromStatus(resp.StatusCode), Status: resp.StatusCode, Message: msg}
	if ra := resp.Header.Get("Retry-After"); ra != "" {
		if secs, err := strconv.Atoi(strings.TrimSpace(ra)); err == nil && secs > 0 {
			e.RetryAfter = time.Duration(secs) * time.Second
		}
	}
	return e
}

// Chat implements Provider.
func (p *OpenAICompat) Chat(ctx context.Context, req *openai.ChatRequest) (*openai.ChatResponse, error) {
	hr, err := p.newRequest(ctx, req, false)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindOther, Message: err.Error(), Err: err}
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return nil, p.netError(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, p.httpError(resp)
	}
	var out openai.ChatResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(&out); err != nil {
		return nil, &Error{Provider: p.name, Kind: KindServer, Status: resp.StatusCode, Message: "unreadable response: " + err.Error(), Err: err}
	}
	if len(out.Choices) == 0 {
		return nil, &Error{Provider: p.name, Kind: KindServer, Status: resp.StatusCode, Message: "provider returned no choices"}
	}
	if out.Usage.TotalTokens == 0 {
		out.Usage.TotalTokens = out.Usage.PromptTokens + out.Usage.CompletionTokens
	}
	return &out, nil
}

// ChatStream implements Provider.
func (p *OpenAICompat) ChatStream(ctx context.Context, req *openai.ChatRequest) (Stream, error) {
	hr, err := p.newRequest(ctx, req, true)
	if err != nil {
		return nil, &Error{Provider: p.name, Kind: KindOther, Message: err.Error(), Err: err}
	}
	resp, err := p.client.Do(hr)
	if err != nil {
		return nil, p.netError(err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, p.httpError(resp)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	return &sseStream{provider: p.name, body: resp.Body, sc: sc}, nil
}

// sseStream reads "server-sent events": lines like `data: {...}` ending with `data: [DONE]`.
type sseStream struct {
	provider string
	body     io.ReadCloser
	sc       *bufio.Scanner
}

func (s *sseStream) Next() (*openai.StreamChunk, error) {
	for s.sc.Scan() {
		line := strings.TrimSpace(s.sc.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue // blank lines and comments (keep-alives)
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			return nil, io.EOF
		}
		var chunk openai.StreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			return nil, &Error{Provider: s.provider, Kind: KindServer, Message: "unreadable stream chunk: " + err.Error(), Err: err}
		}
		if chunk.Error != nil { // provider failed in the middle of the answer
			return nil, &Error{Provider: s.provider, Kind: KindServer, Message: chunk.Error.Message}
		}
		return &chunk, nil
	}
	if err := s.sc.Err(); err != nil {
		kind := KindNetwork
		if errors.Is(err, context.DeadlineExceeded) {
			kind = KindTimeout
		}
		return nil, &Error{Provider: s.provider, Kind: kind, Message: err.Error(), Err: err}
	}
	return nil, io.EOF // some providers just close the connection after the last chunk
}

func (s *sseStream) Close() error { return s.body.Close() }

var _ Provider = (*OpenAICompat)(nil)
