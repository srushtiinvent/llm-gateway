package router

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
	"github.com/srushtiinvent/llm-gateway/internal/provider"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
)

// fake is a provider whose behaviour the test controls.
type fake struct {
	name  string
	calls atomic.Int32
	// fn decides the outcome of call number n (1-based).
	fn   func(n int, req *openai.ChatRequest) (*openai.ChatResponse, error)
	last atomic.Value // last request seen
}

func (f *fake) Name() string  { return f.name }
func (f *fake) Model() string { return f.name + "-model" }
func (f *fake) Chat(ctx context.Context, req *openai.ChatRequest) (*openai.ChatResponse, error) {
	n := int(f.calls.Add(1))
	f.last.Store(req)
	return f.fn(n, req)
}
func (f *fake) ChatStream(ctx context.Context, req *openai.ChatRequest) (provider.Stream, error) {
	n := int(f.calls.Add(1))
	f.last.Store(req)
	if _, err := f.fn(n, req); err != nil {
		return nil, err
	}
	return &oneChunk{}, nil
}

type oneChunk struct{ done bool }

func (o *oneChunk) Next() (*openai.StreamChunk, error) {
	if o.done {
		return nil, io.EOF
	}
	o.done = true
	return &openai.StreamChunk{Choices: []openai.StreamChoice{{Delta: openai.Delta{Content: "hi"}}}}, nil
}
func (o *oneChunk) Close() error { return nil }

func okResp(name string) (*openai.ChatResponse, error) {
	return &openai.ChatResponse{Model: name, Choices: []openai.Choice{{Message: openai.Message{Role: "assistant", Content: "from " + name}}},
		Usage: openai.Usage{PromptTokens: 5, CompletionTokens: 5, TotalTokens: 10}}, nil
}

func fail(name string, kind provider.ErrKind, status int) (*openai.ChatResponse, error) {
	return nil, &provider.Error{Provider: name, Kind: kind, Status: status, Message: "boom"}
}

func always(name string) *fake {
	return &fake{name: name, fn: func(int, *openai.ChatRequest) (*openai.ChatResponse, error) { return okResp(name) }}
}

func failing(name string, kind provider.ErrKind, status int) *fake {
	return &fake{name: name, fn: func(int, *openai.ChatRequest) (*openai.ChatResponse, error) { return fail(name, kind, status) }}
}

func entry(f *fake, l quota.Limits, maxIn int) *Entry {
	return NewEntry(EntryConfig{Provider: f, Limits: l, MaxInputTokens: maxIn, BreakerThreshold: 3, BreakerOpenFor: time.Minute})
}

func testRouter(entries ...*Entry) *Router {
	o := DefaultOptions()
	o.BackoffBase, o.BackoffMax = time.Millisecond, 2*time.Millisecond
	return New(entries, o)
}

func userReq(text string) *openai.ChatRequest {
	return &openai.ChatRequest{Messages: []openai.Message{{Role: "user", Content: text}}}
}

func TestFirstProviderAnswers(t *testing.T) {
	a, b := always("a"), always("b")
	r := testRouter(entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0))
	resp, tr, err := r.Chat(context.Background(), userReq("hi"), r.Resolve("auto"))
	if err != nil || tr.Provider != "a" || resp.Choices[0].Message.Content != "from a" {
		t.Fatalf("err=%v trace=%+v", err, tr)
	}
	if b.calls.Load() != 0 {
		t.Fatal("backup must not be called when the first one works")
	}
}

func TestRetryThenFallback(t *testing.T) {
	a, b := failing("a", provider.KindServer, 503), always("b")
	r := testRouter(entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0))
	_, tr, err := r.Chat(context.Background(), userReq("hi"), r.Resolve("auto"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"a:server_error", "a:server_error", "b:ok"} // original try + 1 retry, then fallback
	if !reflect.DeepEqual(tr.Summary(), want) {
		t.Fatalf("summary %v, want %v", tr.Summary(), want)
	}
	if a.calls.Load() != 2 || tr.Calls != 3 {
		t.Fatalf("calls a=%d total=%d", a.calls.Load(), tr.Calls)
	}
}

func TestRetrySucceedsOnSecondTry(t *testing.T) {
	a := &fake{name: "a", fn: func(n int, _ *openai.ChatRequest) (*openai.ChatResponse, error) {
		if n == 1 {
			return fail("a", provider.KindNetwork, 0)
		}
		return okResp("a")
	}}
	b := always("b")
	r := testRouter(entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0))
	_, tr, err := r.Chat(context.Background(), userReq("hi"), r.Resolve("auto"))
	if err != nil || tr.Provider != "a" || b.calls.Load() != 0 {
		t.Fatalf("a temporary glitch should be retried on the same provider: %v %v", err, tr.Summary())
	}
}

func TestRateLimitedFallsBackWithoutRetryAndSetsCooldown(t *testing.T) {
	a, b := failing("a", provider.KindRateLimited, 429), always("b")
	ea := entry(a, quota.Limits{}, 0)
	r := testRouter(ea, entry(b, quota.Limits{}, 0))

	_, tr, err := r.Chat(context.Background(), userReq("one"), r.Resolve("auto"))
	if err != nil || !reflect.DeepEqual(tr.Summary(), []string{"a:rate_limited", "b:ok"}) {
		t.Fatalf("err=%v %v", err, tr.Summary())
	}
	if a.calls.Load() != 1 {
		t.Fatal("a 429 must not be retried on the same provider")
	}
	_, tr, _ = r.Chat(context.Background(), userReq("two"), r.Resolve("auto"))
	if !reflect.DeepEqual(tr.Summary(), []string{"a:skipped_quota_cooldown", "b:ok"}) {
		t.Fatalf("second request should skip a during its cooldown, got %v", tr.Summary())
	}
	if a.calls.Load() != 1 {
		t.Fatal("a must not be called during cooldown")
	}
}

func TestCircuitBreakerStopsCallingBrokenProvider(t *testing.T) {
	a, b := failing("a", provider.KindServer, 500), always("b")
	o := DefaultOptions()
	o.MaxRetries = 0
	r := New([]*Entry{entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0)}, o)
	for i := 0; i < 3; i++ { // threshold is 3
		r.Chat(context.Background(), userReq("x"), r.Resolve("auto"))
	}
	if a.calls.Load() != 3 {
		t.Fatalf("a calls = %d", a.calls.Load())
	}
	_, tr, _ := r.Chat(context.Background(), userReq("x"), r.Resolve("auto"))
	if !reflect.DeepEqual(tr.Summary(), []string{"a:skipped_circuit_open", "b:ok"}) {
		t.Fatalf("got %v", tr.Summary())
	}
	if a.calls.Load() != 3 {
		t.Fatal("open breaker must prevent the call")
	}
	if got := r.Snapshot()[0]; got.Breaker != "open" {
		t.Fatalf("snapshot should show open breaker, got %q", got.Breaker)
	}
}

func TestQuotaSkipsProviderBeforeCalling(t *testing.T) {
	a, b := always("a"), always("b")
	r := testRouter(entry(a, quota.Limits{RPM: 1}, 0), entry(b, quota.Limits{}, 0))
	r.Chat(context.Background(), userReq("one"), r.Resolve("auto"))
	_, tr, _ := r.Chat(context.Background(), userReq("two"), r.Resolve("auto"))
	if !reflect.DeepEqual(tr.Summary(), []string{"a:skipped_quota_rpm", "b:ok"}) {
		t.Fatalf("got %v", tr.Summary())
	}
	if a.calls.Load() != 1 {
		t.Fatal("a must not be called once its free limit is used")
	}
}

func TestPinnedProviderHasNoFallback(t *testing.T) {
	a, b := failing("a", provider.KindServer, 500), always("b")
	r := testRouter(entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0))
	st := r.Resolve("a")
	if st.Mode != "pinned" {
		t.Fatalf("model 'a' should pin provider a, got %+v", st)
	}
	_, _, err := r.Chat(context.Background(), userReq("x"), st)
	var fe *FailedError
	if !errors.As(err, &fe) || !fe.Pinned || b.calls.Load() != 0 {
		t.Fatalf("pinned requests must not fall back: err=%v b=%d", err, b.calls.Load())
	}
}

func TestUnknownModelMeansAuto(t *testing.T) {
	r := testRouter(entry(always("a"), quota.Limits{}, 0))
	if got := r.Resolve("gpt-4o"); got.Mode != "auto" {
		t.Fatalf("unknown model names must map to auto, got %+v", got)
	}
	if got := r.Resolve(""); got.Mode != "auto" {
		t.Fatal("empty model is auto")
	}
	if got := r.Resolve("FASTEST"); got.Mode != "fastest" {
		t.Fatal("fastest is case-insensitive")
	}
}

func TestFailureKinds(t *testing.T) {
	cases := []struct {
		name string
		a, b *fake
		want string
	}{
		{"all bad requests", failing("a", provider.KindBadRequest, 400), failing("b", provider.KindBadRequest, 422), "bad_request"},
		{"all rate limited", failing("a", provider.KindRateLimited, 429), failing("b", provider.KindRateLimited, 429), "quota_exhausted"},
		{"servers down", failing("a", provider.KindServer, 500), failing("b", provider.KindNetwork, 0), "unavailable"},
		{"mixed", failing("a", provider.KindRateLimited, 429), failing("b", provider.KindServer, 500), "unavailable"},
	}
	for _, c := range cases {
		r := testRouter(entry(c.a, quota.Limits{}, 0), entry(c.b, quota.Limits{}, 0))
		_, _, err := r.Chat(context.Background(), userReq("x"), r.Resolve("auto"))
		var fe *FailedError
		if !errors.As(err, &fe) || fe.Kind != c.want {
			t.Errorf("%s: got %v, want kind %s", c.name, err, c.want)
		}
	}
}

func TestQuotaExhaustedSuggestsRetryAfter(t *testing.T) {
	a := entry(always("a"), quota.Limits{RPM: 1}, 0)
	r := testRouter(a)
	r.Chat(context.Background(), userReq("1"), r.Resolve("auto"))
	_, _, err := r.Chat(context.Background(), userReq("2"), r.Resolve("auto"))
	var fe *FailedError
	if !errors.As(err, &fe) || fe.Kind != "quota_exhausted" || fe.RetryAfter <= 0 || fe.RetryAfter > time.Minute {
		t.Fatalf("expected quota_exhausted with RetryAfter, got %+v err=%v", fe, err)
	}
}

func TestConversationHistoryIsHandedToTheBackup(t *testing.T) {
	a := failing("a", provider.KindRateLimited, 429)
	b := always("b")
	r := testRouter(entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0))
	req := &openai.ChatRequest{Messages: []openai.Message{
		{Role: "system", Content: "be nice"},
		{Role: "user", Content: "my name is Srushti"},
		{Role: "assistant", Content: "Hi Srushti!"},
		{Role: "user", Content: "what is my name?"},
	}}
	if _, _, err := r.Chat(context.Background(), req, r.Resolve("auto")); err != nil {
		t.Fatal(err)
	}
	got := b.last.Load().(*openai.ChatRequest)
	if !reflect.DeepEqual(got.Messages, req.Messages) {
		t.Fatalf("backup provider must receive the full history, got %+v", got.Messages)
	}
}

func TestTrimForSmallContextWindow(t *testing.T) {
	long := strings.Repeat("word ", 400) // about 500 tokens each
	req := &openai.ChatRequest{Messages: []openai.Message{
		{Role: "system", Content: "rules"},
		{Role: "user", Content: long},
		{Role: "assistant", Content: long},
		{Role: "user", Content: long},
		{Role: "assistant", Content: long},
		{Role: "user", Content: "final question"},
	}}
	small := always("small")
	r := testRouter(entry(small, quota.Limits{}, 900))
	_, tr, err := r.Chat(context.Background(), req, r.Resolve("auto"))
	if err != nil {
		t.Fatal(err)
	}
	sent := small.last.Load().(*openai.ChatRequest)
	if tr.Trimmed == 0 || len(sent.Messages) >= len(req.Messages) {
		t.Fatalf("expected trimming, trimmed=%d sent=%d", tr.Trimmed, len(sent.Messages))
	}
	if sent.Messages[0].Role != "system" || sent.Messages[len(sent.Messages)-1].Content != "final question" {
		t.Fatal("system message and the last question must always be kept")
	}
	if sent.EstimatePromptTokens() > 900 {
		t.Fatalf("trimmed request still too long: %d tokens", sent.EstimatePromptTokens())
	}
	if len(req.Messages) != 6 {
		t.Fatal("the caller's request must not be modified")
	}
}

func TestFastestPrefersLowerLatency(t *testing.T) {
	slow := &fake{name: "slow", fn: func(int, *openai.ChatRequest) (*openai.ChatResponse, error) {
		time.Sleep(40 * time.Millisecond)
		return okResp("slow")
	}}
	quick := always("quick")
	r := testRouter(entry(slow, quota.Limits{}, 0), entry(quick, quota.Limits{}, 0))
	// pinned calls teach the router each provider's speed
	r.Chat(context.Background(), userReq("x"), r.Resolve("slow"))
	r.Chat(context.Background(), userReq("x"), r.Resolve("quick"))
	_, tr, _ := r.Chat(context.Background(), userReq("x"), r.Resolve("fastest"))
	if tr.Provider != "quick" {
		t.Fatalf("fastest should pick quick, got %s", tr.Provider)
	}
	_, tr, _ = r.Chat(context.Background(), userReq("x"), r.Resolve("auto"))
	if tr.Provider != "slow" {
		t.Fatal("auto keeps the configured priority order")
	}
}

func TestClientCancelIsNotAProviderFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	a := &fake{name: "a", fn: func(int, *openai.ChatRequest) (*openai.ChatResponse, error) {
		cancel()
		return nil, &provider.Error{Provider: "a", Kind: provider.KindNetwork, Message: "context canceled", Err: context.Canceled}
	}}
	b := always("b")
	ea := entry(a, quota.Limits{RPM: 5}, 0)
	r := testRouter(ea, entry(b, quota.Limits{}, 0))
	_, _, err := r.Chat(ctx, userReq("x"), r.Resolve("auto"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
	if b.calls.Load() != 0 {
		t.Fatal("no fallback when the user already left")
	}
	if s := r.Snapshot()[0]; s.RequestsFailed != 0 || s.ConsecutiveFailures != 0 {
		t.Fatalf("user cancellation must not count against the provider: %+v", s)
	}
	if s := ea.Quota.Snapshot(); s.RPMUsed != 0 {
		t.Fatalf("quota should be refunded, used=%d", s.RPMUsed)
	}
}

func TestAuthErrorFallsBackAndCountsAgainstBreaker(t *testing.T) {
	a, b := failing("a", provider.KindAuth, 401), always("b")
	o := DefaultOptions()
	o.MaxRetries = 0
	r := New([]*Entry{entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0)}, o)
	for i := 0; i < 3; i++ {
		_, tr, err := r.Chat(context.Background(), userReq("x"), r.Resolve("auto"))
		if err != nil || tr.Provider != "b" {
			t.Fatalf("bad key on a must fall back to b: %v", err)
		}
	}
	if r.Snapshot()[0].Breaker != "open" {
		t.Fatal("repeated auth errors should open the breaker so we stop hitting the provider")
	}
}

func TestStreamFallbackBeforeFirstByte(t *testing.T) {
	a, b := failing("a", provider.KindServer, 503), always("b")
	o := DefaultOptions()
	o.MaxRetries = 0
	r := New([]*Entry{entry(a, quota.Limits{}, 0), entry(b, quota.Limits{}, 0)}, o)
	sr, tr, err := r.ChatStream(context.Background(), userReq("x"), r.Resolve("auto"))
	if err != nil || tr.Provider != "b" {
		t.Fatalf("stream should fall back before any text is sent: err=%v", err)
	}
	if _, err := sr.Stream.Next(); err != nil {
		t.Fatal(err)
	}
	sr.Finish(nil, false)
}

func TestStreamMidwayFailureCountsAgainstProvider(t *testing.T) {
	a := always("a")
	o := DefaultOptions()
	r := New([]*Entry{entry(a, quota.Limits{}, 0)}, o)
	sr, _, err := r.ChatStream(context.Background(), userReq("x"), r.Resolve("auto"))
	if err != nil {
		t.Fatal(err)
	}
	sr.Finish(&provider.Error{Provider: "a", Kind: provider.KindNetwork, Message: "reset"}, false)
	if s := r.Snapshot()[0]; s.RequestsFailed != 1 || s.LastError == "" {
		t.Fatalf("mid-stream failure should be recorded: %+v", s)
	}
	sr.Finish(errors.New("again"), false) // calling twice must be harmless
	if r.Snapshot()[0].RequestsFailed != 1 {
		t.Fatal("Finish must only count once")
	}
}
