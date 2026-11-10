package server

import (
	"encoding/json"
	"github.com/srushtiinvent/llm-gateway/internal/config"
	"strings"
	"testing"
)

func chatBody() map[string]any {
	return map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "hello"}}}
}

func TestSignupGivesWorkingKeyAndIsLimited(t *testing.T) {
	h := newHarness(t, hopts{})
	h.srv.Config.PublicSignup, h.srv.Config.PublicKeyRPM, h.srv.Config.PublicKeyDaily = true, 5, 7
	h.srv.Config.SignupPerIPPerDay, h.srv.Config.SignupGlobalPerDay = 2, 100

	var key string
	for i := 0; i < 2; i++ {
		resp, b := h.do("POST", "/v1/signup", "", nil, nil)
		if resp.StatusCode != 201 {
			t.Fatalf("signup %d: %d %s", i, resp.StatusCode, b)
		}
		var out struct {
			Key        string `json:"key"`
			DailyLimit int    `json:"daily_limit"`
		}
		_ = json.Unmarshal(b, &out)
		if !strings.HasPrefix(out.Key, "gw_") || out.DailyLimit != 7 {
			t.Fatalf("bad signup body: %s", b)
		}
		key = out.Key
	}
	if resp, _ := h.do("POST", "/v1/signup", "", nil, nil); resp.StatusCode != 429 {
		t.Fatalf("3rd signup from same IP should be refused, got %d", resp.StatusCode)
	}
	if resp, b := h.do("POST", "/v1/chat/completions", key, chatBody(), nil); resp.StatusCode != 200 {
		t.Fatalf("visitor key should chat: %d %s", resp.StatusCode, b)
	}
	if resp, _ := h.do("GET", "/v1/usage", key, nil, nil); resp.StatusCode != 200 {
		t.Fatalf("usage: %d", resp.StatusCode)
	}
}

func TestSignupCanBeDisabled(t *testing.T) {
	h := newHarness(t, hopts{})
	h.srv.Config.PublicSignup = false
	if resp, _ := h.do("POST", "/v1/signup", "", nil, nil); resp.StatusCode != 403 {
		t.Fatalf("want 403, got %d", resp.StatusCode)
	}
}

func TestSimulateIsPerRequestAndLeavesNoTrace(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("sim", 100, 100)
	hdr := map[string]string{"X-Gateway-Simulate": "alpha=down,beta=ratelimited"}

	for i := 0; i < 5; i++ { // more than the breaker threshold (3)
		resp, b := h.do("POST", "/v1/chat/completions", key, chatBody(), hdr)
		if resp.StatusCode != 200 {
			t.Fatalf("simulated request %d: %d %s", i, resp.StatusCode, b)
		}
		var out struct {
			Gateway struct {
				Provider string   `json:"provider"`
				Tried    []string `json:"tried"`
			} `json:"gateway"`
		}
		_ = json.Unmarshal(b, &out)
		if out.Gateway.Provider != "gamma" {
			t.Fatalf("expected failover to gamma, got %s (%v)", out.Gateway.Provider, out.Gateway.Tried)
		}
	}
	// A normal request right after must still go to alpha: nothing leaked into shared state.
	_, b := h.do("POST", "/v1/chat/completions", key, chatBody(), nil)
	var out struct {
		Gateway struct{ Provider string } `json:"gateway"`
	}
	_ = json.Unmarshal(b, &out)
	if out.Gateway.Provider != "alpha" {
		t.Fatalf("simulation leaked: normal request went to %s", out.Gateway.Provider)
	}
	for _, p := range h.rt.Snapshot() {
		if p.Breaker != "closed" || p.Quota.CooldownSec != 0 || p.RequestsFailed != 0 {
			t.Fatalf("shared state touched for %s: %+v", p.Name, p)
		}
	}
}

func TestPublicStatsHidesPrivateData(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("x", 100, 100)
	h.do("POST", "/v1/chat/completions", key, chatBody(), nil)
	resp, b := h.do("GET", "/v1/public/stats?window=1h", "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	s := string(b)
	for _, bad := range []string{"key_id", "last_error", "gw_", "prefix"} {
		if strings.Contains(s, bad) {
			t.Fatalf("public stats leaks %q: %s", bad, s)
		}
	}
	if !strings.Contains(s, `"providers"`) || !strings.Contains(s, `"recent"`) {
		t.Fatalf("missing fields: %s", s)
	}
	if resp, _ := h.do("GET", "/admin/overview", "", nil, nil); resp.StatusCode != 401 {
		t.Fatalf("admin must stay private, got %d", resp.StatusCode)
	}
}

func byokHarness(t *testing.T) *harness {
	h := newHarness(t, hopts{})
	h.srv.Config.Providers = []config.ProviderConfig{
		{Name: "alpha", Model: "alpha-model", Mock: true},
		{Name: "beta", Model: "beta-model", Mock: true},
	}
	return h
}

func cacheState(t *testing.T, b []byte) string {
	var out struct {
		Gateway struct {
			Cache string `json:"cache"`
		} `json:"gateway"`
	}
	_ = json.Unmarshal(b, &out)
	return out.Gateway.Cache
}

func TestByokSkipsSiteBudgetAndNeverStoresKeys(t *testing.T) {
	h := byokHarness(t)
	key := h.newKey("v", 100, 1) // site budget: 1 provider call a day
	hdr := map[string]string{"X-Provider-Key-Alpha": "my-own-key-123456"}
	for i := 0; i < 4; i++ {
		body := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "q" + string(rune('a'+i))}}}
		if resp, b := h.do("POST", "/v1/chat/completions", key, body, hdr); resp.StatusCode != 200 {
			t.Fatalf("byok request %d should not hit the site budget: %d %s", i, resp.StatusCode, b)
		}
	}
	// without own keys the 1-call budget applies: first ok, second refused
	n := func(c string) map[string]any {
		return map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": c}}}
	}
	h.do("POST", "/v1/chat/completions", key, n("site1"), nil)
	if resp, _ := h.do("POST", "/v1/chat/completions", key, n("site2"), nil); resp.StatusCode != 429 {
		t.Fatalf("site budget should still apply without own keys, got %d", resp.StatusCode)
	}
	// the key must not appear anywhere the gateway exposes
	_, stats := h.do("GET", "/v1/public/stats", "", nil, nil)
	_, logs := h.do("GET", "/admin/logs", adminTok, nil, nil)
	if strings.Contains(string(stats), "my-own-key") || strings.Contains(string(logs), "my-own-key") {
		t.Fatal("provider key leaked into stats or logs")
	}
}

func TestByokCacheIsPerVisitor(t *testing.T) {
	h := byokHarness(t)
	key := h.newKey("v", 100, 100)
	body := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "same question"}}}
	a := map[string]string{"X-Provider-Key-Alpha": "visitor-a-key-1234"}
	b := map[string]string{"X-Provider-Key-Alpha": "visitor-b-key-5678"}
	_, r1 := h.do("POST", "/v1/chat/completions", key, body, a)
	_, r2 := h.do("POST", "/v1/chat/completions", key, body, a)
	_, r3 := h.do("POST", "/v1/chat/completions", key, body, b)
	_, r4 := h.do("POST", "/v1/chat/completions", key, body, nil)
	if cacheState(t, r1) != "miss" || cacheState(t, r2) != "hit" {
		t.Fatalf("same visitor should get a cache hit: %s / %s", cacheState(t, r1), cacheState(t, r2))
	}
	if cacheState(t, r3) != "miss" || cacheState(t, r4) != "miss" {
		t.Fatalf("another visitor (or the site's own keys) must not see that cached answer: %s / %s", cacheState(t, r3), cacheState(t, r4))
	}
}

func TestByokIgnoresMalformedKeysAndCheckWorks(t *testing.T) {
	h := byokHarness(t)
	resp, b := h.do("POST", "/v1/byok/check", "", nil, map[string]string{"X-Provider-Key-Alpha": "good-key-1234567", "X-Provider-Key-Beta": "bad key with spaces"})
	if resp.StatusCode != 200 {
		t.Fatalf("check: %d %s", resp.StatusCode, b)
	}
	s := string(b)
	if !strings.Contains(s, `"alpha":{"ok":true`) || strings.Contains(s, "beta") {
		t.Fatalf("expected only alpha to be checked: %s", s)
	}
}

const alphaKey = "X-Provider-Key-Alpha"

func TestByokSkipsSiteBudgetAndIsolatesCache(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("tiny", 100, 1) // site budget: 1 provider call a day
	mine := map[string]string{alphaKey: "my-own-alpha-key-123456"}
	other := map[string]string{alphaKey: "someone-elses-key-abcdef"}

	for i := 0; i < 4; i++ { // far over the site budget: fine, it is the visitor's own quota
		body := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "q" + string(rune('a'+i))}}}
		if resp, b := h.do("POST", "/v1/chat/completions", key, body, mine); resp.StatusCode != 200 {
			t.Fatalf("byok request %d: %d %s", i, resp.StatusCode, b)
		}
	}
	// Without their own keys the same gw key is held to the site budget.
	h.do("POST", "/v1/chat/completions", key, chatBody(), nil)
	body2 := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "different question"}}}
	h.do("POST", "/v1/chat/completions", key, body2, nil)
	if resp, _ := h.do("POST", "/v1/chat/completions", key, map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "third"}}}, nil); resp.StatusCode != 429 {
		t.Fatalf("site budget should apply without own keys, got %d", resp.StatusCode)
	}

	// Cache: my answer must not be served to another visitor with a different key.
	q := map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": "cache isolation"}}}
	_, b1 := h.do("POST", "/v1/chat/completions", key, q, mine)
	_, b2 := h.do("POST", "/v1/chat/completions", key, q, mine)
	_, b3 := h.do("POST", "/v1/chat/completions", key, q, other)
	hit := func(b []byte) string {
		var o struct{ Gateway struct{ Cache string } }
		_ = json.Unmarshal(b, &o)
		return o.Gateway.Cache
	}
	if hit(b1) != "miss" || hit(b2) != "hit" || hit(b3) != "miss" {
		t.Fatalf("cache isolation broken: %s %s %s", hit(b1), hit(b2), hit(b3))
	}
}

func TestByokKeysNeverStoredOrEchoedAndBadOnesIgnored(t *testing.T) {
	h := newHarness(t, hopts{})
	key := h.newKey("k", 100, 100)
	secret := "super-secret-provider-key-999"
	resp, b := h.do("POST", "/v1/chat/completions", key, chatBody(), map[string]string{alphaKey: secret})
	if resp.StatusCode != 200 || strings.Contains(string(b), secret) {
		t.Fatalf("status %d / key echoed: %s", resp.StatusCode, b)
	}
	for _, hdr := range []string{"X-Gateway-Provider", "X-Gateway-Tried"} {
		if strings.Contains(resp.Header.Get(hdr), secret) {
			t.Fatalf("key leaked in header %s", hdr)
		}
	}
	// A malformed key (spaces, too short) is ignored and the request runs normally on the site's providers.
	resp, _ = h.do("POST", "/v1/chat/completions", key, chatBody(), map[string]string{alphaKey: "bad key!"})
	if resp.StatusCode != 200 {
		t.Fatalf("malformed key should be ignored, got %d", resp.StatusCode)
	}
	// Check endpoint reports per-provider results.
	resp, b = h.do("POST", "/v1/byok/check", "", nil, map[string]string{alphaKey: secret})
	if resp.StatusCode != 200 || !strings.Contains(string(b), `"alpha":{"ok":true`) || strings.Contains(string(b), secret) {
		t.Fatalf("check: %d %s", resp.StatusCode, b)
	}
}

func TestRequireOwnKeysBlocksVisitorsOnSiteQuotaOnly(t *testing.T) {
	h := newHarness(t, hopts{})
	h.srv.Config.RequireOwnKeys = true
	h.srv.Config.PublicSignup, h.srv.Config.PublicKeyRPM, h.srv.Config.PublicKeyDaily = true, 5, 7
	h.srv.Config.SignupPerIPPerDay, h.srv.Config.SignupGlobalPerDay = 5, 100

	_, b := h.do("POST", "/v1/signup", "", nil, nil)
	var out struct{ Key string }
	_ = json.Unmarshal(b, &out)

	resp, body := h.do("POST", "/v1/chat/completions", out.Key, chatBody(), nil)
	if resp.StatusCode != 403 || !strings.Contains(string(body), "own_keys_required") {
		t.Fatalf("visitor without own keys should be refused: %d %s", resp.StatusCode, body)
	}
	if resp, body := h.do("POST", "/v1/chat/completions", out.Key, chatBody(), map[string]string{alphaKey: "my-own-alpha-key-123456"}); resp.StatusCode != 200 {
		t.Fatalf("visitor with own key should fly: %d %s", resp.StatusCode, body)
	}
	// the owner's own keys (not made by sign-up) still use the site's providers
	owner := h.newKey("owner", 100, 100)
	if resp, body := h.do("POST", "/v1/chat/completions", owner, chatBody(), nil); resp.StatusCode != 200 {
		t.Fatalf("owner key should work: %d %s", resp.StatusCode, body)
	}
}

func TestVisitorGetsFreeFlightThenMustBringOwnKey(t *testing.T) {
	h := newHarness(t, hopts{})
	h.srv.Config.PublicSignup, h.srv.Config.PublicKeyRPM, h.srv.Config.PublicKeyDaily = true, 5, 1
	h.srv.Config.SignupPerIPPerDay, h.srv.Config.SignupGlobalPerDay = 5, 100
	_, b := h.do("POST", "/v1/signup", "", nil, nil)
	var out struct{ Key string }
	_ = json.Unmarshal(b, &out)

	q := func(s string) map[string]any {
		return map[string]any{"model": "auto", "messages": []map[string]string{{"role": "user", "content": s}}}
	}
	if resp, body := h.do("POST", "/v1/chat/completions", out.Key, q("first"), nil); resp.StatusCode != 200 {
		t.Fatalf("free flight: %d %s", resp.StatusCode, body)
	}
	resp, body := h.do("POST", "/v1/chat/completions", out.Key, q("second"), nil)
	if resp.StatusCode != 429 || !strings.Contains(string(body), "free_flights_used") {
		t.Fatalf("second flight on site keys should point to own keys: %d %s", resp.StatusCode, body)
	}
	if resp, body := h.do("POST", "/v1/chat/completions", out.Key, q("second"), map[string]string{alphaKey: "my-own-alpha-key-123456"}); resp.StatusCode != 200 {
		t.Fatalf("with own key it should fly: %d %s", resp.StatusCode, body)
	}
}