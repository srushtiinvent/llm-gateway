package auth

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/srushtiinvent/llm-gateway/internal/store"
)

func TestGenerateKey(t *testing.T) {
	plain, prefix, hash := GenerateKey()
	if !strings.HasPrefix(plain, "gw_") || len(plain) != 3+48 {
		t.Fatalf("unexpected key shape %q", plain)
	}
	if prefix != plain[:9] || hash != HashKey(plain) || hash == plain {
		t.Fatal("prefix/hash wrong")
	}
	p2, _, _ := GenerateKey()
	if p2 == plain {
		t.Fatal("keys must be random")
	}
}

func req(token string) *http.Request {
	r, _ := http.NewRequest("GET", "/", nil)
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	return r
}

func TestAuthenticate(t *testing.T) {
	s := store.NewMemory()
	plain, prefix, hash := GenerateKey()
	k, _ := s.CreateKey(context.Background(), "app", prefix, hash, 0, 0)
	a := New(s, "adm")

	if got, err := a.Authenticate(context.Background(), req(plain)); err != nil || got.ID != k.ID {
		t.Fatalf("valid key: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), req("")); err != ErrMissingKey {
		t.Fatalf("missing: %v", err)
	}
	if _, err := a.Authenticate(context.Background(), req("gw_nope")); err != ErrInvalidKey {
		t.Fatalf("invalid: %v", err)
	}
	_ = s.RevokeKey(context.Background(), k.ID)
	if _, err := a.Authenticate(context.Background(), req(plain)); err != nil {
		t.Fatal("a cached key keeps working briefly until Forget is called")
	}
	a.Forget()
	if _, err := a.Authenticate(context.Background(), req(plain)); err != ErrInvalidKey {
		t.Fatalf("revoked key should fail after Forget: %v", err)
	}
}

func TestIsAdmin(t *testing.T) {
	a := New(store.NewMemory(), "secret")
	if !a.IsAdmin(req("secret")) || a.IsAdmin(req("wrong")) || a.IsAdmin(req("")) {
		t.Fatal("admin token check wrong")
	}
	r := req("")
	r.Header.Set("X-Admin-Token", "secret")
	if !a.IsAdmin(r) {
		t.Fatal("X-Admin-Token header should work too")
	}
	if New(store.NewMemory(), "").IsAdmin(req("")) {
		t.Fatal("with no admin token configured nobody is admin")
	}
}

func TestBudget(t *testing.T) {
	s := store.NewMemory()
	b := NewBudget(s)
	k := &store.Key{ID: 1, DailyLimit: 2}
	ctx := context.Background()
	if !b.Allow(ctx, k) {
		t.Fatal("fresh key has budget")
	}
	b.Used(k)
	b.Used(k)
	if b.Allow(ctx, k) {
		t.Fatal("budget of 2 is used up")
	}
	if !b.Allow(ctx, &store.Key{ID: 2, DailyLimit: 0}) {
		t.Fatal("0 = unlimited")
	}
	// after a restart the count is rebuilt from the saved log: only billed requests count
	// (not rejected ones, not cache hits), so a key that hit its limit is not stuck at zero.
	s.InsertLog(ctx, store.RequestLog{KeyID: 7, Status: "ok", Billed: true})
	for i := 0; i < 5; i++ {
		s.InsertLog(ctx, store.RequestLog{KeyID: 7, Status: "error", Error: "daily_budget_exceeded"})
		s.InsertLog(ctx, store.RequestLog{KeyID: 7, Status: "ok", CacheHit: true})
	}
	if n, _ := NewBudget(s).Usage(ctx, &store.Key{ID: 7, DailyLimit: 2}); n != 1 {
		t.Fatalf("after a restart: used = %d, want 1 (only billed requests count)", n)
	}
	s.InsertLog(ctx, store.RequestLog{KeyID: 7, Status: "ok", Billed: true})
	if NewBudget(s).Allow(ctx, &store.Key{ID: 7, DailyLimit: 2}) {
		t.Fatal("usage must survive a restart (read back from the log)")
	}
}

func TestBudgetReserveAndRefund(t *testing.T) {
	ctx := context.Background()
	b := NewBudget(store.NewMemory())
	k := &store.Key{ID: 42, DailyLimit: 2}
	if !b.Reserve(ctx, k) || !b.Reserve(ctx, k) {
		t.Fatal("two reservations fit a budget of 2")
	}
	if b.Reserve(ctx, k) {
		t.Fatal("third reservation must be refused")
	}
	b.Refund(k)
	if !b.Reserve(ctx, k) {
		t.Fatal("a refunded slot can be reserved again")
	}
	if n, _ := b.Usage(ctx, k); n != 2 {
		t.Fatalf("usage %d, want 2", n)
	}
	unlimited := &store.Key{ID: 43}
	b.Refund(unlimited)
	if !b.Reserve(ctx, unlimited) {
		t.Fatal("no limit means always allowed")
	}
}
