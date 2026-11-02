// Package auth checks API keys and admin access, and enforces each key's daily budget.
package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/store"
)

var (
	ErrMissingKey = errors.New("missing API key: send the header 'Authorization: Bearer <your gateway key>'")
	ErrInvalidKey = errors.New("invalid or revoked API key")
)

// GenerateKey makes a new random key like "gw_3f9a...". The plain text is shown to the user
// exactly once; only its hash is stored, so a leaked database does not leak working keys.
func GenerateKey() (plain, prefix, hash string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	plain = "gw_" + hex.EncodeToString(b)
	return plain, plain[:9], HashKey(plain)
}

// HashKey returns the SHA-256 hash of a key, as hex.
func HashKey(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

type cached struct {
	key     *store.Key
	expires time.Time
}

// Authenticator validates keys against the store (with a short in-memory cache so
// that most requests do not need a database lookup).
type Authenticator struct {
	store      store.Store
	adminToken string
	ttl        time.Duration

	mu    sync.Mutex
	cache map[string]cached
}

// New creates an Authenticator.
func New(s store.Store, adminToken string) *Authenticator {
	return &Authenticator{store: s, adminToken: adminToken, ttl: 30 * time.Second, cache: map[string]cached{}}
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

// Authenticate returns the key making the request.
func (a *Authenticator) Authenticate(ctx context.Context, r *http.Request) (*store.Key, error) {
	plain := bearer(r)
	if plain == "" {
		return nil, ErrMissingKey
	}
	hash := HashKey(plain)

	a.mu.Lock()
	if c, ok := a.cache[hash]; ok && time.Now().Before(c.expires) {
		a.mu.Unlock()
		return c.key, nil
	}
	a.mu.Unlock()

	k, err := a.store.FindKeyByHash(ctx, hash)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, ErrInvalidKey
		}
		return nil, err
	}
	if k.Revoked {
		return nil, ErrInvalidKey
	}
	a.mu.Lock()
	a.cache[hash] = cached{key: k, expires: time.Now().Add(a.ttl)}
	a.mu.Unlock()
	return k, nil
}

// Forget clears cached keys (called after a key is revoked).
func (a *Authenticator) Forget() {
	a.mu.Lock()
	a.cache = map[string]cached{}
	a.mu.Unlock()
}

// IsAdmin reports whether the request carries the admin token.
func (a *Authenticator) IsAdmin(r *http.Request) bool {
	if a.adminToken == "" {
		return false
	}
	tok := bearer(r)
	if tok == "" {
		tok = r.Header.Get("X-Admin-Token")
	}
	return tok != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.adminToken)) == 1
}

// Budget enforces each key's daily limit on requests that actually reach a provider.
// (Cache hits are free, so they do not count.) The day is the UTC calendar day.
type Budget struct {
	store store.Store
	mu    sync.Mutex
	days  map[int64]*day
}

type day struct {
	date string
	n    int
}

// NewBudget creates a Budget.
func NewBudget(s store.Store) *Budget { return &Budget{store: s, days: map[int64]*day{}} }

func today() (string, time.Time) {
	now := time.Now().UTC()
	return now.Format("2006-01-02"), now.Truncate(24 * time.Hour)
}

// Allow reports whether the key still has budget today.
func (b *Budget) Allow(ctx context.Context, k *store.Key) bool {
	if k.DailyLimit <= 0 {
		return true
	}
	date, start := today()
	b.mu.Lock()
	d := b.days[k.ID]
	b.mu.Unlock()
	if d == nil || d.date != date {
		// First time today (or after a restart): load what was already used from the log.
		n, err := b.store.CountProviderCalls(ctx, k.ID, start)
		if err != nil {
			return true // do not block users because the database hiccuped
		}
		b.mu.Lock()
		d = &day{date: date, n: n}
		b.days[k.ID] = d
		b.mu.Unlock()
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return d.n < k.DailyLimit
}

// Used records one provider call for the key.
func (b *Budget) Used(k *store.Key) {
	date, _ := today()
	b.mu.Lock()
	defer b.mu.Unlock()
	d := b.days[k.ID]
	if d == nil || d.date != date {
		d = &day{date: date}
		b.days[k.ID] = d
	}
	d.n++
}
