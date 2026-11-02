package provider

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

// Mock is a pretend AI. It lets you try every gateway feature (fallback, caching,
// quotas, streaming) without any API key. Its answers are simple and predictable.
type Mock struct {
	name    string
	model   string
	latency time.Duration
}

// NewMock creates a mock provider that takes roughly `latency` to answer.
func NewMock(name, model string, latency time.Duration) *Mock {
	return &Mock{name: name, model: model, latency: latency}
}

func (m *Mock) Name() string  { return m.name }
func (m *Mock) Model() string { return m.model }

var (
	askName = regexp.MustCompile(`(?i)what('?s| is) my name`)
	isName  = regexp.MustCompile(`(?i)my name is (\w+)`)
)

// reply builds the mock answer. It also shows how conversation history is handed
// from one provider to the next: if an earlier message said "my name is X" and the
// last message asks for the name, the mock answers from the history it was given.
func (m *Mock) reply(msgs []openai.Message) string {
	last := ""
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			last = msgs[i].Content
			break
		}
	}
	if askName.MatchString(last) {
		for i := len(msgs) - 1; i >= 0; i-- {
			if msgs[i].Role != "user" {
				continue
			}
			if g := isName.FindStringSubmatch(msgs[i].Content); g != nil {
				return fmt.Sprintf("[%s] Your name is %s (I read that from the conversation history the gateway sent me).", m.name, g[1])
			}
		}
		return fmt.Sprintf("[%s] I don't know your name yet. Tell me with: my name is ...", m.name)
	}
	return fmt.Sprintf("[%s] You said: %q. I can see %d message(s) of conversation history.", m.name, last, len(msgs))
}

func (m *Mock) wait(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return &Error{Provider: m.name, Kind: KindTimeout, Message: ctx.Err().Error(), Err: ctx.Err()}
	}
}

// Chat implements Provider.
func (m *Mock) Chat(ctx context.Context, req *openai.ChatRequest) (*openai.ChatResponse, error) {
	if err := m.wait(ctx, m.latency); err != nil {
		return nil, err
	}
	text := m.reply(req.Messages)
	pt, ct := req.EstimatePromptTokens(), openai.EstimateTokens(text)
	return &openai.ChatResponse{
		ID: openai.NewID(), Object: "chat.completion", Created: openai.Now(), Model: m.model,
		Choices: []openai.Choice{{Index: 0, Message: openai.Message{Role: "assistant", Content: text}, FinishReason: "stop"}},
		Usage:   openai.Usage{PromptTokens: pt, CompletionTokens: ct, TotalTokens: pt + ct},
	}, nil
}

// ChatStream implements Provider.
func (m *Mock) ChatStream(ctx context.Context, req *openai.ChatRequest) (Stream, error) {
	if err := m.wait(ctx, m.latency/3); err != nil { // time to first token
		return nil, err
	}
	words := strings.SplitAfter(m.reply(req.Messages), " ")
	return &mockStream{ctx: ctx, m: m, words: words, id: openai.NewID()}, nil
}

type mockStream struct {
	ctx   context.Context
	m     *Mock
	words []string
	i     int
	id    string
	state int // 0 = role chunk, 1 = words, 2 = finish chunk, 3 = done
}

func (s *mockStream) Next() (*openai.StreamChunk, error) {
	switch s.state {
	case 0:
		s.state = 1
		return s.chunk(openai.Delta{Role: "assistant"}, nil), nil
	case 1:
		if s.i < len(s.words) {
			if err := s.m.wait(s.ctx, 25*time.Millisecond); err != nil {
				return nil, err
			}
			w := s.words[s.i]
			s.i++
			return s.chunk(openai.Delta{Content: w}, nil), nil
		}
		s.state = 2
		fallthrough
	case 2:
		s.state = 3
		stop := "stop"
		return s.chunk(openai.Delta{}, &stop), nil
	}
	return nil, io.EOF
}

func (s *mockStream) chunk(d openai.Delta, finish *string) *openai.StreamChunk {
	return &openai.StreamChunk{
		ID: s.id, Object: "chat.completion.chunk", Created: openai.Now(), Model: s.m.model,
		Choices: []openai.StreamChoice{{Index: 0, Delta: d, FinishReason: finish}},
	}
}

func (s *mockStream) Close() error { return nil }

var _ Provider = (*Mock)(nil)
