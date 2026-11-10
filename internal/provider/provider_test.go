package provider

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

func req(text string) *openai.ChatRequest {
	return &openai.ChatRequest{Model: "auto", Messages: []openai.Message{{Role: "user", Content: text}}}
}

func TestOpenAICompatChatSuccess(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","created":1,"model":"real-model","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":2}}`)
	}))
	defer srv.Close()

	p := NewOpenAICompat("groq", srv.URL+"/openai/v1/", "secret", "llama-x", nil)
	resp, err := p.Chat(context.Background(), req("hi"))
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/openai/v1/chat/completions" || gotAuth != "Bearer secret" {
		t.Fatalf("path=%q auth=%q", gotPath, gotAuth)
	}
	if gotBody["model"] != "llama-x" {
		t.Fatalf("the gateway's model name must be replaced by the real one, got %v", gotBody["model"])
	}
	if resp.Choices[0].Message.Content != "hello" || resp.Usage.TotalTokens != 5 {
		t.Fatalf("unexpected response %+v", resp)
	}
}

func TestOpenAICompatErrorMapping(t *testing.T) {
	cases := []struct {
		status int
		body   string
		kind   ErrKind
		msg    string
	}{
		{429, `{"error":{"message":"quota exceeded"}}`, KindRateLimited, "quota exceeded"},
		{500, `oops`, KindServer, "oops"},
		{503, `[{"error":{"message":"overloaded"}}]`, KindServer, "overloaded"},
		{401, `{"error":{"message":"bad key"}}`, KindAuth, "bad key"},
		{400, `{"error":{"message":"bad input"}}`, KindBadRequest, "bad input"},
	}
	for _, c := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if c.status == 429 {
				w.Header().Set("Retry-After", "7")
			}
			w.WriteHeader(c.status)
			io.WriteString(w, c.body)
		}))
		p := NewOpenAICompat("x", srv.URL, "k", "m", nil)
		_, err := p.Chat(context.Background(), req("hi"))
		srv.Close()
		var pe *Error
		if !errors.As(err, &pe) || pe.Kind != c.kind || pe.Status != c.status || !strings.Contains(pe.Message, c.msg) {
			t.Errorf("status %d: got %v", c.status, err)
			continue
		}
		if c.status == 429 && pe.RetryAfter != 7*time.Second {
			t.Errorf("Retry-After not parsed: %v", pe.RetryAfter)
		}
	}
}

func TestOpenAICompatTimeoutAndNetworkErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond)
	}))
	p := NewOpenAICompat("x", srv.URL, "k", "m", nil)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, req("hi"))
	srv.Close()
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindTimeout || !pe.Retryable() {
		t.Fatalf("want timeout, got %v", err)
	}

	p2 := NewOpenAICompat("x", "http://127.0.0.1:1", "k", "m", nil) // nothing listens there
	_, err = p2.Chat(context.Background(), req("hi"))
	if !errors.As(err, &pe) || pe.Kind != KindNetwork {
		t.Fatalf("want network error, got %v", err)
	}
}

func TestOpenAICompatEmptyChoicesIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"choices":[]}`)
	}))
	defer srv.Close()
	_, err := NewOpenAICompat("x", srv.URL, "k", "m", nil).Chat(context.Background(), req("hi"))
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindServer {
		t.Fatalf("got %v", err)
	}
}

func TestOpenAICompatStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var b map[string]any
		json.NewDecoder(r.Body).Decode(&b)
		if b["stream"] != true {
			t.Errorf("stream flag must be forwarded")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, ": keep-alive\n\n")
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"role":"assistant"}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"Hel"}}]}`+"\n\n")
		io.WriteString(w, `data: {"id":"1","choices":[{"index":0,"delta":{"content":"lo"}}]}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()
	s, err := NewOpenAICompat("x", srv.URL, "k", "m", nil).ChatStream(context.Background(), req("hi"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var text strings.Builder
	for {
		c, err := s.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, ch := range c.Choices {
			text.WriteString(ch.Delta.Content)
		}
	}
	if text.String() != "Hello" {
		t.Fatalf("assembled %q", text.String())
	}
}

func TestOpenAICompatStreamErrorChunkAndHTTPError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `data: {"choices":[{"delta":{"content":"a"}}]}`+"\n\n")
		io.WriteString(w, `data: {"error":{"message":"upstream died"}}`+"\n\n")
	}))
	s, err := NewOpenAICompat("x", srv.URL, "k", "m", nil).ChatStream(context.Background(), req("hi"))
	if err != nil {
		t.Fatal(err)
	}
	s.Next()
	_, err = s.Next()
	var pe *Error
	if !errors.As(err, &pe) || !strings.Contains(pe.Message, "upstream died") {
		t.Fatalf("mid-stream error not surfaced: %v", err)
	}
	s.Close()
	srv.Close()

	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv2.Close()
	_, err = NewOpenAICompat("x", srv2.URL, "k", "m", nil).ChatStream(context.Background(), req("hi"))
	if !errors.As(err, &pe) || pe.Kind != KindRateLimited {
		t.Fatalf("stream open should map 429: %v", err)
	}
}

func TestMockRemembersNameFromHistory(t *testing.T) {
	m := NewMock("m", "mm", 0)
	resp, _ := m.Chat(context.Background(), &openai.ChatRequest{Messages: []openai.Message{
		{Role: "user", Content: "my name is Srushti"},
		{Role: "assistant", Content: "hi"},
		{Role: "user", Content: "What is my name?"},
	}})
	if !strings.Contains(resp.Choices[0].Message.Content, "Srushti") {
		t.Fatalf("mock should answer from history: %q", resp.Choices[0].Message.Content)
	}
}

func TestFaultInjection(t *testing.T) {
	f := NewFaults()
	p := &Faulty{Provider: NewMock("m", "mm", 0), F: f}
	if _, err := p.Chat(context.Background(), req("x")); err != nil {
		t.Fatal("no fault: should work")
	}
	f.Set(FaultDown)
	_, err := p.Chat(context.Background(), req("x"))
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != KindServer {
		t.Fatalf("down: %v", err)
	}
	f.Set(FaultRateLimited)
	_, err = p.ChatStream(context.Background(), req("x"))
	if !errors.As(err, &pe) || pe.Kind != KindRateLimited {
		t.Fatalf("ratelimited: %v", err)
	}
	f.Set(FaultSlow)
	f.SlowDelay = 500 * time.Millisecond
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = p.Chat(ctx, req("x"))
	if !errors.As(err, &pe) || pe.Kind != KindTimeout {
		t.Fatalf("slow with short deadline should time out: %v", err)
	}
	if _, err := ParseFault("explode"); err == nil {
		t.Fatal("unknown mode must be rejected")
	}
}
