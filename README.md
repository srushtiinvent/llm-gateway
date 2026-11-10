# LLM Gateway

A small server that sits between your apps and several AI providers (Gemini, Groq, OpenRouter).
Your app talks to the gateway with one OpenAI-style request, and the gateway:

- **falls back** to another provider when one fails, is slow, or runs out of free quota
- **retries** temporary failures (with jittered backoff) and **pauses** a broken provider (circuit breaker)
- **tracks each provider's free-tier limits** (per minute, per day, tokens per minute) and steers requests away *before* you get blocked
- **caches answers**, so repeated questions cost nothing
- **protects itself**: per-key rate limits, daily budgets, and a queue that says "busy" instead of melting down
- **records** latency, tokens, cache hits and errors for a dashboard
- keeps **conversation history** moving with the request, so a backup provider knows the whole chat

Built with Go, using only the standard library plus one Postgres driver.

> **Status:** Stage 1 (the backend) is finished and tested. The customer portal and owner console are
> available in [`web/`](web/). Customer pages include a guide, API reference, and playground; owner
> tools use the `/admin/...` endpoints already built here.

## Run the control tower website

The React + TypeScript customer portal and owner console live in `web/`.

```bash
cd web
npm install
npm run dev
```

The Vite dev server proxies `/admin`, `/v1`, and `/healthz` to `http://localhost:8080`. Start the
gateway in another terminal. Visitors start on **Getting started** and can use the Playground with a
gateway key. The owner console appears after the owner connects with the admin token. The backend
currently lets the owner create and revoke gateway keys; public sign-up and self-service accounts
are not implemented. Admin and gateway keys stay in page memory; the non-secret API address is saved
on that device.

The **Getting started** page explains how a customer receives a key and sends a request. The **API
guide** includes a copyable request example. Owner monitoring, provider controls, and key management
stay under **Owner console**.

To have the Go server serve the built website, run `npm run build` in `web/`, then start the gateway
with `WEB_DIST=web/dist`. The API and website will share the same origin.

---

## 1. Install Go (once)

Mac (Homebrew): `brew install go`  ·  or download from https://go.dev/dl/

Check it worked (you need version 1.22 or newer):

```bash
go version
```

## 2. Run it with fake providers (no API keys needed)

Open Terminal in this folder, then:

```bash
cp .env.example .env
go run ./cmd/gateway
```

You should see "LLM Gateway is running" and three `mock-...` providers. Leave this terminal open.

## 3. Take the guided tour

Open a **second** Terminal tab in the same folder:

```bash
ADMIN_TOKEN=dev-admin-token ./scripts/try.sh
```

(If you see "permission denied", run `bash scripts/try.sh` instead, with the same `ADMIN_TOKEN=...` in front.)

It creates a key, asks questions, shows a cache hit, breaks a provider so you can watch the fallback,
and proves that conversation history reaches the backup provider.

## 4. Run the tests

```bash
go test ./...
```

Add `-race` (`go test -race ./...`) to also check for data races. Storage tests against Postgres run
only if you set `TEST_DATABASE_URL`.

## 5. Measure it (numbers for your resume)

Create a key with no limits, then run the load tester:

```bash
curl -s -X POST localhost:8080/admin/keys -H "Authorization: Bearer dev-admin-token" \
     -d '{"name":"load","rpm":0,"daily":0}'
# copy the "key" value (starts with gw_) and then:
go run ./cmd/loadtest -key gw_PASTE_KEY_HERE -n 1000 -c 20 -prompts 40
```

For a fair test of the gateway itself, restart it with unlimited fake providers:

```bash
MOCK_RPM=0 MOCK_RPD=0 MOCK_LATENCY_MS=5 MAX_CONCURRENT=64 QUEUE_MAX_WAITING=500 go run ./cmd/gateway
```

**Always say what you measured.** Numbers against fake providers measure the gateway's own overhead.
Numbers against real providers are dominated by the providers' speed.

## 6. Use real providers

1. Get free keys: Gemini (https://aistudio.google.com/apikey) and Groq (https://console.groq.com/keys).
2. In `.env`: set `MOCK_PROVIDERS=false` and remove the `#` before `GEMINI_API_KEY=` and `GROQ_API_KEY=`, then paste the keys right after the `=` (no spaces, no quotes).
3. Restart the gateway. The start-up banner lists the providers it will use.
4. **Check model names and limits** on each provider's website. Free-tier rules change often, and
   the defaults in `.env.example` are only starting points. If a provider's real limit is lower
   than what you configured, the gateway still protects you: a `429` from the provider pauses it
   automatically (and honours its `Retry-After` header).

Keys live only in `.env` (ignored by git) or in your host's environment settings. Never paste them into code or chat.

## 7. Keep keys and logs (optional database)

Without `DATABASE_URL`, API keys and logs live in memory and disappear when you stop the gateway.
For a permanent store, create a free database at https://neon.com , copy its connection string into
`DATABASE_URL`, and restart. The tables are created automatically.

---

## Using the gateway from code

It speaks the OpenAI chat format, so existing OpenAI client libraries work if you change the base URL
to `http://localhost:8080/v1` and use a gateway key (`gw_...`) as the API key.

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Authorization: Bearer gw_YOUR_KEY" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Explain a circuit breaker."}]}'
```

The `model` field chooses the routing:

| model | meaning |
|---|---|
| `auto` (or anything unknown, like `gpt-4o`) | try providers in priority order, with fallback |
| `fastest` | try the provider with the lowest recent latency first |
| a provider name (`gemini`, `groq`...) | use only that provider, no fallback |

Every answer includes a `gateway` block (and `X-Gateway-*` headers) showing what happened:
which provider answered, `cache` hit or miss, how many attempts, and the `tried` list such as
`["gemini:rate_limited","groq:ok"]`.

Send `"stream": true` for server-sent events. Send header `X-Gateway-Cache: bypass` to skip the cache.

### Endpoints

| Endpoint | Who | What |
|---|---|---|
| `POST /v1/chat/completions` | app (gateway key) | ask a question |
| `GET /v1/models` | anyone | list routing choices |
| `GET /healthz` · `GET /readyz` | anyone | light check · check including the database |
| `GET /admin/overview?window=1h\|6h\|24h\|7d` | admin | all dashboard numbers in one call |
| `GET /admin/providers` | admin | each provider: breaker state, quota used, latency |
| `POST /admin/providers/{name}/fault` | admin | failure simulator: `{"mode":"down\|slow\|ratelimited\|none"}` |
| `GET /admin/logs?limit=50` | admin | recent requests (metadata only) |
| `GET/POST /admin/keys`, `POST /admin/keys/{id}/revoke` | admin | manage gateway keys |
| `POST /admin/cache/clear` | admin | forget cached answers |

Admin endpoints need `Authorization: Bearer <ADMIN_TOKEN>`.

### Errors you may see

| Status | `error.code` | Meaning |
|---|---|---|
| 401 | `missing_api_key` / `invalid_api_key` | no key, wrong key, or revoked key |
| 429 | `rate_limit_exceeded` | this key sent too many requests per minute |
| 429 | `daily_budget_exceeded` | this key used its daily budget (cache hits are free) |
| 429 | `free_quota_exhausted` | every provider's free limit is used up right now (`Retry-After` tells you when) |
| 503 | `overloaded` | the gateway queue is full, try again in a few seconds |
| 503 | `all_providers_failed` | no provider could answer |
| 400 | `provider_rejected_request` | the providers rejected the request itself |

---

## Settings

Everything is configured by environment variables or the `.env` file; see `.env.example` for the full,
commented list.

## Honest limitations (good to know, good to say in interviews)

- **Free-tier numbers and model names are starting points.** They change; check the providers' docs.
- **Tested with fake providers and a protocol-faithful stand-in server, not live Gemini/Groq.** Your
  first real call is the first live test of those adapters. Gemini and Groq are reached through their
  OpenAI-compatible endpoints.
- **The cache is exact-match and in memory** (lost on restart). A semantic cache (matching questions with the same
  meaning) is planned next.
- **One server instance.** Rate limits, quotas, the queue and the cache are in-process. Running several
  instances would need shared state (for example Redis).
- **Streaming can fall back only before the first word is sent.** If a provider dies mid-answer, the stream ends with an error event.
- **Token counts are estimated** (about 4 characters per token) when a provider does not report usage, and for planning quotas.
- **Messages are plain text.** Images, tool calls and function calling are not supported.
- **Only metadata is stored** (who, which provider, timing, token counts), never the text of questions or answers.

## Deploying on Render

Create a Go Web Service from the repository. The gateway reads Render's `PORT` automatically.
Use `go build -o gateway ./cmd/gateway` as the build command and `./gateway` as the start command.
In Render's environment settings, add a strong `ADMIN_TOKEN`, set `MOCK_PROVIDERS=false`, and add
the provider keys you want to use (`GEMINI_API_KEY`, `GROQ_API_KEY`, or both). Add `DATABASE_URL`
to keep API keys and request logs across restarts. The service address Render assigns, such as
`https://your-gateway.onrender.com`, is the public gateway URL.

For the frontend, either deploy `web/` as a Render Static Site (`npm ci && npm run build`, publish
`dist`) and enter the Go service URL as the API base in **Getting started**, or build the frontend
into `web/dist` and set `WEB_DIST=web/dist` so the Go service serves the website and API together.
When hosting the frontend separately, set `ALLOWED_ORIGINS` on the Go service to the exact Static
Site URL. The owner then enters the Go service URL and admin token in the owner console. Customers
need only the public gateway URL and a gateway key issued by the owner. Set `MOCK_PROVIDERS=false`
and configure real provider keys for real AI responses. The dashboard reads real telemetry after the
owner connects; a new gateway starts with zero traffic until the first API requests arrive.

## Learn how it works

See [docs/HOW-IT-WORKS.md](docs/HOW-IT-WORKS.md) for the life of a request and a tour of the code.


## Public website (new frontend in `web/`)

The site works for any visitor, not just the owner:

- **Play**: the first "Take off" creates a personal free key (`POST /v1/signup`, rate-limited per IP) and stores it in that visitor's browser. Every request is real and goes through the router.
- **Break / Fix**: a visitor's failure test is sent as the header `X-Gateway-Simulate: gemini=down` on that one request. The router really fails over, but the test never touches the shared circuit breakers, cooldowns or counters, so one visitor cannot break the site for others (see `internal/server/public_test.go`).
- **Radar**: reads `GET /v1/public/stats`, which has totals and provider health only (no keys, prompts or error text).
- **Simulator**: runs in the browser, so it never spends free-tier quota.
- **Keys**: visitors see their own key and allowance. The owner pastes `ADMIN_TOKEN` to unlock custom keys and revoke.

Run it:

```bash
cd web && npm ci && npm run build && cd ..
WEB_DIST=web/dist go run ./cmd/gateway     # open http://localhost:8080
```

For live frontend work use `cd web && npm run dev` (proxies `/v1`, `/admin`, `/healthz` to :8080).

### Bring your own provider keys

On the Keys page a visitor can paste their own Gemini / Groq / OpenRouter keys. They stay in that visitor's browser and are sent as `X-Provider-Key-Gemini` (etc.) headers with each request. The gateway builds temporary providers from them for that one request, so the flight runs on the visitor's own provider quota, is not counted against the site's daily budget, uses a cache namespace of its own (nobody else can be served their answers), and the keys are never stored or logged. `POST /v1/byok/check` tests keys with a one-token call. Tests: `internal/server/public_test.go`.
