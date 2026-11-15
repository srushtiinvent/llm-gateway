package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/srushtiinvent/llm-gateway/internal/auth"
	"github.com/srushtiinvent/llm-gateway/internal/quota"
)

func (h *harness) usage(key string) (used, remaining int) {
	h.t.Helper()
	_, b := h.do("GET", "/v1/usage", key, nil, nil)
	var u struct{ DailyUsed, DailyRemaining int }
	var raw map[string]any
	json.Unmarshal(b, &raw)
	u.DailyUsed, u.DailyRemaining = int(raw["daily_used"].(float64)), int(raw["daily_remaining"].(float64))
	return u.DailyUsed, u.DailyRemaining
}

func tempBody(model, text string, temp *float64) map[string]any {
	b := map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": text}}}
	if temp != nil {
		b["temperature"] = *temp
	}
	return b
}

// The daily budget counts exactly the requests a provider answered: not cache hits, not requests
// rejected by a limit, not requests that never reached a provider. And it agrees after a restart.
func TestDailyBudgetCountsExactlyProviderAnswers(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("text-to-sql", 0, 1200)
	for i := 0; i < 10; i++ {
		if resp, _ := h.chat(key, "auto", fmt.Sprintf("question %d", i), nil); resp.StatusCode != 200 {
			t.Fatalf("call %d: %d", i, resp.StatusCode)
		}
	}
	for i := 0; i < 5; i++ { // cache hits are free
		h.chat(key, "auto", "question 0", nil)
	}
	for _, n := range []string{"alpha", "beta", "gamma"} { // a request no provider could answer
		h.fault(n, "down")
	}
	if resp, _ := h.chat(key, "auto", "nobody home", nil); resp.StatusCode != 503 {
		t.Fatalf("all down: %d", resp.StatusCode)
	}
	if used, left := h.usage(key); used != 10 || left != 1190 {
		t.Fatalf("used=%d left=%d, want 10 and 1190", used, left)
	}

	// After a restart the count is rebuilt from the log and must be the same.
	h.srv.Close() // flush the log
	k, _ := h.srv.Auth.Authenticate(context.Background(), mustReq(key))
	if n, _ := auth.NewBudget(h.st).Usage(context.Background(), k); n != 10 {
		t.Fatalf("after restart: used=%d, want 10", n)
	}
}

// A key that used up its budget, then kept getting rejected, must not count the rejections:
// the old log-based count did, which left keys stuck at "0 left" after a restart.
func TestBudgetRejectionsAreNotCounted(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("small", 0, 2)
	h.chat(key, "auto", "a", nil)
	h.chat(key, "auto", "b", nil)
	for i := 0; i < 5; i++ {
		if resp, _ := h.chat(key, "auto", fmt.Sprintf("rejected %d", i), nil); resp.StatusCode != 429 {
			t.Fatalf("over budget: %d", resp.StatusCode)
		}
	}
	h.srv.Close()
	k, _ := h.srv.Auth.Authenticate(context.Background(), mustReq(key))
	if n, _ := auth.NewBudget(h.st).Usage(context.Background(), k); n != 2 {
		t.Fatalf("after restart: used=%d, want 2", n)
	}
}

func TestAdminKeyListShowsUsage(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 50)
	h.chat(key, "auto", "one", nil)
	h.chat(key, "auto", "two", nil)
	_, b := h.do("GET", "/admin/keys", adminTok, nil, nil)
	var out struct {
		Keys []struct {
			Name                      string
			DailyUsed, DailyRemaining int
		}
	}
	json.Unmarshal(fixKeys(b), &out)
	if len(out.Keys) != 1 || out.Keys[0].DailyUsed != 2 || out.Keys[0].DailyRemaining != 48 {
		t.Fatalf("keys: %s", b)
	}
}

// fixKeys maps snake_case to the field names json.Unmarshal matches case-insensitively.
func fixKeys(b []byte) []byte {
	return []byte(strings.NewReplacer(`"daily_used"`, `"dailyused"`, `"daily_remaining"`, `"dailyremaining"`).Replace(string(b)))
}

func TestTemperatureAboveZeroSkipsCache(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	zero, warm := 0.0, 0.7
	for i, want := range []string{"miss", "hit"} {
		_, b := h.do("POST", "/v1/chat/completions", key, tempBody("auto", "same", &zero), nil)
		var out chatOut
		json.Unmarshal(b, &out)
		if out.Gateway.Cache != want {
			t.Fatalf("temperature 0, call %d: cache=%s want %s", i, out.Gateway.Cache, want)
		}
	}
	for i := 0; i < 2; i++ {
		resp, b := h.do("POST", "/v1/chat/completions", key, tempBody("auto", "same", &warm), nil)
		var out chatOut
		json.Unmarshal(b, &out)
		if out.Gateway.Cache != "bypass" || resp.Header.Get("X-Gateway-Cache") != "bypass" {
			t.Fatalf("temperature 0.7 must not be cached (call %d): %s", i, out.Gateway.Cache)
		}
	}
}

func TestStrictModel(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("app", 0, 0)
	strict := map[string]string{"X-Gateway-Strict-Model": "true"}

	// without strict mode a model name is a preference: the first provider answers
	_, out := h.chat(key, "beta-model", "q1", nil)
	if out.Gateway.Provider != "alpha" || out.Model != "alpha-model" {
		t.Fatalf("non-strict: %+v model=%s", out.Gateway, out.Model)
	}
	// strict: only the provider serving that model
	_, out = h.chat(key, "beta-model", "q2", strict)
	if out.Gateway.Provider != "beta" || out.Model != "beta-model" {
		t.Fatalf("strict: %+v model=%s", out.Gateway, out.Model)
	}
	// strict and that provider is down: an error, never another model
	h.fault("beta", "down")
	resp, out := h.chat(key, "beta-model", "q3", strict)
	if resp.StatusCode != 503 || out.Error.Type != "no_provider_available" || strings.Contains(strings.Join(out.Gateway.Tried, ","), "alpha") {
		t.Fatalf("strict with provider down: %d %+v %v", resp.StatusCode, out.Error, out.Gateway.Tried)
	}
	// strict and nobody serves it
	resp, out = h.chat(key, "gpt-4o", "q4", strict)
	if resp.StatusCode != 400 || out.Error.Code != "model_not_available" {
		t.Fatalf("unknown model, strict: %d %+v", resp.StatusCode, out.Error)
	}
	// "auto" stays "any model" even in strict mode
	if resp, _ := h.chat(key, "auto", "q5", strict); resp.StatusCode != 200 {
		t.Fatalf("auto + strict: %d", resp.StatusCode)
	}
}

func TestQuotaEndpoint(t *testing.T) {
	one := quota.Limits{RPM: 1, RPD: 5}
	h := newHarness(t, hopts{limits: []quota.Limits{one, one, one}})
	key := h.newKey("app", 10, 100)

	read := func() map[string]any {
		resp, b := h.do("GET", "/v1/quota", key, nil, nil)
		if resp.StatusCode != 200 {
			t.Fatalf("quota: %d %s", resp.StatusCode, b)
		}
		var m map[string]any
		json.Unmarshal(b, &m)
		return m
	}
	q := read()
	if q["can_call_now"] != true || len(q["providers"].([]any)) != 3 {
		t.Fatalf("fresh: %v", q)
	}
	for i := 0; i < 3; i++ { // use each provider's one request this minute
		h.chat(key, "auto", fmt.Sprintf("q%d", i), nil)
	}
	q = read()
	if q["can_call_now"] != false || q["reason"] != "no_provider_available" || q["next_available_at"] == nil {
		t.Fatalf("exhausted: %v", q)
	}
	p := q["providers"].([]any)[0].(map[string]any)
	if p["available"] != false || p["blocked_by"] != "rpm" || p["available_at"] == nil {
		t.Fatalf("provider: %v", p)
	}
	day := q["key"].(map[string]any)["per_day"].(map[string]any)
	if day["used"].(float64) != 3 || day["remaining"].(float64) != 97 {
		t.Fatalf("key per_day: %v", day)
	}
	if resp, _ := h.do("GET", "/v1/quota", "", nil, nil); resp.StatusCode != 401 {
		t.Fatal("quota needs a key")
	}
}

func TestPerKeyProviderRulesAndPaidProvider(t *testing.T) {
	h := newHarness(t, hopts{paid: []bool{false, false, true}}) // gamma costs money
	plain := h.newKey("visitor-ish", 0, 0)

	// a key without rules never reaches the paid provider, even when every free one is down
	h.fault("alpha", "down")
	h.fault("beta", "down")
	resp, out := h.chat(plain, "auto", "help", nil)
	if resp.StatusCode != 503 || strings.Contains(strings.Join(out.Gateway.Tried, ","), "gamma") {
		t.Fatalf("paid provider must be off limits: %d %v", resp.StatusCode, out.Gateway.Tried)
	}
	if _, out := h.chat(plain, "gamma", "pin it", nil); out.Gateway.Provider == "gamma" {
		t.Fatal("pinning a paid provider by name must not work for a key without rules")
	}

	// a key whose rules list it can use it, in the order given
	resp, b := h.do("POST", "/admin/keys", adminTok, map[string]any{"name": "eval", "providers": []string{"beta", "alpha", "gamma"}}, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, b)
	}
	var created struct{ Key string }
	json.Unmarshal(b, &created)
	_, out = h.chat(created.Key, "auto", "help", nil)
	if out.Gateway.Provider != "gamma" || strings.Join(out.Gateway.Tried, ",") != "beta:server_error,alpha:server_error,gamma:ok" {
		t.Fatalf("rules order: %+v", out.Gateway)
	}

	// rules can be changed later; unknown names are refused
	h.fault("alpha", "none")
	h.fault("beta", "none")
	var id struct{ ID int64 }
	json.Unmarshal(b, &id)
	if resp, b := h.do("POST", fmt.Sprintf("/admin/keys/%d/providers", id.ID), adminTok, map[string]any{"providers": []string{"beta"}}, nil); resp.StatusCode != 200 {
		t.Fatalf("set rules: %d %s", resp.StatusCode, b)
	}
	if _, out := h.chat(created.Key, "auto", "again", nil); out.Gateway.Provider != "beta" {
		t.Fatalf("new rules: %+v", out.Gateway)
	}
	if resp, _ := h.do("POST", fmt.Sprintf("/admin/keys/%d/providers", id.ID), adminTok, map[string]any{"providers": []string{"nope"}}, nil); resp.StatusCode != 400 {
		t.Fatal("unknown provider must be refused")
	}
}

// Bring-your-own-key works for any API caller, not only the website.
func TestByokOverAPIForAnyKey(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("text-to-sql-demo", 0, 1) // a budget of one
	h.chat(key, "auto", "uses the budget", nil)
	hdr := map[string]string{"X-Provider-Key-Beta": "visitor-own-key-123456"}
	for i := 0; i < 3; i++ {
		resp, out := h.chat(key, "auto", fmt.Sprintf("own key %d", i), hdr)
		if resp.StatusCode != 200 || out.Gateway.Provider != "beta" {
			t.Fatalf("byok call %d: %d %+v", i, resp.StatusCode, out.Error)
		}
	}
	if used, _ := h.usage(key); used != 1 {
		t.Fatalf("byok calls must not use the key's budget: used=%d", used)
	}
}

func mustReq(key string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	r.Header.Set("Authorization", "Bearer "+key)
	return r
}
