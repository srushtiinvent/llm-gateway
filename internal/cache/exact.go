// Package cache remembers answers so repeated questions do not use up free-tier quota.
package cache

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sync"
	"sync/atomic"
	"time"

	"github.com/srushtiinvent/llm-gateway/internal/openai"
)

// Entry is a remembered answer.
type Entry struct {
	Response *openai.ChatResponse
	Provider string
	Created  time.Time
}

// Cache is an exact-match cache: the same conversation with the same settings returns
// the stored answer. It is an LRU (least recently used entries are dropped first when it
// is full) and entries expire after a time limit (TTL).
type Cache struct {
	mu    sync.Mutex
	ttl   time.Duration
	max   int
	ll    *list.List // front = most recently used
	items map[string]*list.Element
	now   func() time.Time

	hits      atomic.Int64
	misses    atomic.Int64
	evictions atomic.Int64
}

type item struct {
	key     string
	entry   Entry
	expires time.Time
}

// New creates a cache.
func New(max int, ttl time.Duration) *Cache {
	if max < 1 {
		max = 1
	}
	return &Cache{ttl: ttl, max: max, ll: list.New(), items: map[string]*list.Element{}, now: time.Now}
}

// Key makes the cache key for a request. `route` is the routing choice ("auto", "fastest" or a provider
// name) so that asking for a specific provider does not return another provider's answer.
func Key(req *openai.ChatRequest, route string) string {
	b, _ := json.Marshal(struct {
		Route       string           `json:"r"`
		Messages    []openai.Message `json:"m"`
		Temperature *float64         `json:"t,omitempty"`
		MaxTokens   *int             `json:"x,omitempty"`
		TopP        *float64         `json:"p,omitempty"`
	}{route, req.Messages, req.Temperature, req.MaxTokens, req.TopP})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func clone(r *openai.ChatResponse) *openai.ChatResponse {
	c := *r
	c.Choices = append([]openai.Choice(nil), r.Choices...)
	c.Gateway = nil
	return &c
}

// Get returns a copy of the stored answer if it exists and has not expired.
func (c *Cache) Get(key string) (*Entry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[key]
	if !ok {
		c.misses.Add(1)
		return nil, false
	}
	it := el.Value.(*item)
	if c.now().After(it.expires) {
		c.ll.Remove(el)
		delete(c.items, key)
		c.misses.Add(1)
		return nil, false
	}
	c.ll.MoveToFront(el)
	c.hits.Add(1)
	e := it.entry
	e.Response = clone(e.Response)
	return &e, true
}

// Put stores an answer.
func (c *Cache) Put(key string, resp *openai.ChatResponse, provider string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e := Entry{Response: clone(resp), Provider: provider, Created: c.now()}
	if el, ok := c.items[key]; ok {
		it := el.Value.(*item)
		it.entry, it.expires = e, c.now().Add(c.ttl)
		c.ll.MoveToFront(el)
		return
	}
	c.items[key] = c.ll.PushFront(&item{key: key, entry: e, expires: c.now().Add(c.ttl)})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*item).key)
		c.evictions.Add(1)
	}
}

// Clear forgets everything.
func (c *Cache) Clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.ll.Init()
	c.items = map[string]*list.Element{}
}

// Stats is a snapshot for the dashboard.
type Stats struct {
	Entries   int   `json:"entries"`
	MaxSize   int   `json:"max_entries"`
	Hits      int64 `json:"hits"`
	Misses    int64 `json:"misses"`
	Evictions int64 `json:"evictions"`
}

// Stats returns the current numbers (hits and misses since the server started).
func (c *Cache) Stats() Stats {
	c.mu.Lock()
	n := c.ll.Len()
	c.mu.Unlock()
	return Stats{Entries: n, MaxSize: c.max, Hits: c.hits.Load(), Misses: c.misses.Load(), Evictions: c.evictions.Load()}
}
