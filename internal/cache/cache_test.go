package cache

import (
	"testing"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

func resp(text string) *openai.ChatResponse {
	return &openai.ChatResponse{Model: "m", Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: text}}}}
}

func req(text string) *openai.ChatRequest {
	return &openai.ChatRequest{Messages: []openai.Message{{Role: "user", Content: text}}}
}

func TestHitMissAndKey(t *testing.T) {
	c := New(10, time.Hour)
	k := Key(req("hello"), "auto")
	if _, ok := c.Get(k); ok {
		t.Fatal("empty cache must miss")
	}
	c.Put(k, resp("hi"), "groq")
	e, ok := c.Get(k)
	if !ok || e.Provider != "groq" || e.Response.Choices[0].Message.Content != "hi" {
		t.Fatalf("expected a hit, got %+v", e)
	}
	if Key(req("hello"), "auto") != k {
		t.Fatal("same input must give same key")
	}
	if Key(req("hello"), "groq") == k {
		t.Fatal("asking a specific provider must not share the 'auto' entry")
	}
	if Key(req("hello!"), "auto") == k {
		t.Fatal("different text, different key")
	}
	temp := 0.2
	r := req("hello")
	r.Temperature = &temp
	if Key(r, "auto") == k {
		t.Fatal("different temperature, different key")
	}
	s := c.Stats()
	if s.Hits != 1 || s.Misses != 1 || s.Entries != 1 {
		t.Fatalf("stats %+v", s)
	}
}

func TestReturnedCopyIsIndependent(t *testing.T) {
	c := New(10, time.Hour)
	c.Put("k", resp("original"), "p")
	e, _ := c.Get("k")
	e.Response.Choices[0].Message.Content = "changed by caller"
	e2, _ := c.Get("k")
	if e2.Response.Choices[0].Message.Content != "original" {
		t.Fatal("callers must not be able to modify the stored answer")
	}
}

func TestTTLExpiry(t *testing.T) {
	c := New(10, time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	c.Put("k", resp("x"), "p")
	now = now.Add(61 * time.Second)
	if _, ok := c.Get("k"); ok {
		t.Fatal("entry should have expired")
	}
}

func TestLRUEviction(t *testing.T) {
	c := New(2, time.Hour)
	c.Put("a", resp("1"), "p")
	c.Put("b", resp("2"), "p")
	c.Get("a")                 // a is now the most recently used
	c.Put("c", resp("3"), "p") // evicts b
	if _, ok := c.Get("b"); ok {
		t.Fatal("b should have been evicted")
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a should still be there")
	}
	if c.Stats().Evictions != 1 {
		t.Fatalf("evictions = %d", c.Stats().Evictions)
	}
	c.Clear()
	if c.Stats().Entries != 0 {
		t.Fatal("Clear should empty the cache")
	}
}
