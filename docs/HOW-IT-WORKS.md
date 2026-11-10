# How the gateway works

## The life of one request

```
App ──POST /v1/chat/completions──▶ 1. Who are you?        auth       (API key, hashed)
                                   2. Too many requests?   limiter    (token bucket per key)
                                   3. Seen this before?    cache      (exact match, LRU + TTL)
                                         │ hit ──▶ answer immediately, no provider used
                                         ▼ miss
                                   4. Daily budget left?   auth.Budget
                                   5. Is there room now?   queue      (max N at once, short waiting line)
                                         │ full ──▶ 503 "busy, try again"
                                         ▼
                                   6. Router picks a provider, for each candidate:
                                        circuit breaker open?  ─▶ skip
                                        free quota used up?    ─▶ skip
                                        call it (timeout)
                                          ok            ─▶ done
                                          temporary err ─▶ wait (jittered backoff), retry once
                                          429           ─▶ pause provider, fall back (no retry)
                                          other error   ─▶ fall back
                                   7. Save answer in cache, write a log line (in the background)
                                   8. Reply with the answer + a "gateway" block
```

If every provider fails the caller gets a clear error: `429` when free quotas are used up (with `Retry-After`),
`503` when providers are down, `400` when the providers rejected the request itself.

## Code tour

| Folder | Job | Key idea |
|---|---|---|
| `cmd/gateway` | start-up and wiring | builds every piece and connects them; graceful shutdown |
| `cmd/loadtest` | load tester | many goroutines, percentiles (p50/p95/p99) |
| `internal/openai` | request/response shapes | the OpenAI chat format is the common language |
| `internal/provider` | talk to AI providers | one `Provider` interface; real adapter, mock, and a fault-injection wrapper |
| `internal/resilience` | circuit breaker, backoff | closed → open → half-open; exponential backoff with full jitter |
| `internal/quota` | free-tier limits | rolling windows of recent calls: RPM, RPD, TPM; reservation you can settle or cancel |
| `internal/limiter` | protect the gateway | token bucket per key; bounded queue (backpressure) |
| `internal/cache` | remember answers | LRU with expiry; copies so callers can't corrupt stored data |
| `internal/router` | decide who answers | the loop that combines breaker + quota + retries + fallback + trimming |
| `internal/store` | keys and logs | one interface, two versions: memory and Postgres |
| `internal/auth` | keys and budgets | keys stored as hashes; small in-memory cache for lookups |
| `internal/server` | HTTP layer | handlers, errors, streaming (SSE), admin API, middleware |

## Go ideas you will meet

- **Interfaces** (`Provider`, `Store`, `Stream`): the router never knows whether a provider is real or fake. This is why tests are easy.
- **Goroutines and channels**: the background log writer receives logs over a channel; the queue is a channel used as a pool of slots.
- **`sync.Mutex` and `sync/atomic`**: shared counters (quota windows, cache, stats) are protected from data races. Run `go test -race ./...` to check.
- **`context.Context`**: carries timeouts and cancellation. If the user disconnects, work stops and the provider is *not* blamed.
- **Errors as values**: `provider.Error` has a *kind*, and the router reacts differently to each kind.
- **Table-driven tests**: many cases, one loop (see `router_test.go`).

## Why it is built this way (interview answers)

**Why a circuit breaker?** If a provider is down, retrying every request wastes time and quota. After a few failures in a row the breaker stops calling it for a while, then lets one trial request through ("half-open") to test recovery.

**Why jitter in retries?** If many requests fail together and all retry after exactly the same delay, they hit the provider together again. A random delay spreads them out.

**Why not retry a 429?** A 429 means "you used your allowance". Retrying immediately makes it worse. The gateway pauses that provider (using `Retry-After` if given) and moves to the next one.

**Why track quotas ourselves?** Learning about a limit from a 429 costs a failed call. Counting our own usage lets the gateway avoid the call. Provider 429s still teach it the real limit if our numbers were wrong.

**Why a queue with a maximum length?** Without a limit, a traffic spike makes everyone slow. With one, extra requests get a fast, honest "busy" answer (backpressure).

**Why are cache hits free for the daily budget?** They cost the providers nothing, so charging the key for them would punish good behaviour. The budget check therefore comes after the cache.

**Why store only metadata?** Prompts can contain personal data. Logs hold who, which provider, timing and token counts, never the text.

**What happens when a streamed answer fails halfway?** Text has already reached the user, so we cannot switch providers invisibly. Fallback works only before the first piece is sent; after that the stream ends with an error event and the failure counts against the provider.

**How does a backup provider know the conversation?** AI APIs are stateless: every call carries the full message history. The gateway forwards the same history to whichever provider answers (trimming the oldest messages if the backup has a smaller context window).

## What to build next

1. **Website** (React): playground, dashboard, failure simulator, keys page (uses the `/admin/...` endpoints).
2. **Semantic cache**: match questions with the same meaning using embeddings, and measure hit rate vs wrong-answer rate at different similarity thresholds.
3. **Server-side conversations**: send a `conversation_id` instead of the whole history.
4. **Deploy** on Render + Neon, then use it from your other projects.
