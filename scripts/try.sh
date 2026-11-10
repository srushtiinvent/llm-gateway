#!/usr/bin/env bash
# A guided tour of the gateway. Run it while the gateway is running in another terminal:
#
#     ADMIN_TOKEN=dev-admin-token ./scripts/try.sh
#
# (use the same ADMIN_TOKEN that is in your .env file)

set -u
URL="${GATEWAY_URL:-http://localhost:8080}"
ADMIN="${ADMIN_TOKEN:-dev-admin-token}"

pp() { # pretty-print JSON if python3 is available
  if command -v python3 >/dev/null 2>&1; then python3 -m json.tool 2>/dev/null || cat; else cat; fi
}
say() { printf '\n\033[1m== %s\033[0m\n' "$1"; }
chat() { # chat "model" "text"
  curl -s "$URL/v1/chat/completions" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" \
    -d "{\"model\":\"$1\",\"messages\":[{\"role\":\"user\",\"content\":\"$2\"}]}"
}
fault() { curl -s -X POST "$URL/admin/providers/$1/fault" -H "Authorization: Bearer $ADMIN" -d "{\"mode\":\"$2\"}" >/dev/null; }

say "1. Is the gateway alive?"
curl -s "$URL/healthz" | pp

say "2. Create an API key for an app (shown only once)"
RESP=$(curl -s -X POST "$URL/admin/keys" -H "Authorization: Bearer $ADMIN" -d '{"name":"demo-app"}')
echo "$RESP" | pp
KEY=$(echo "$RESP" | grep -o '"key": *"[^"]*"' | sed 's/.*"\(gw_[^"]*\)"/\1/')
if [ -z "$KEY" ]; then
  echo "Could not create a key. Is ADMIN_TOKEN correct? (it must match the gateway's ADMIN_TOKEN)"; exit 1
fi

Q="What is a circuit breaker? ($(date +%s))"
say "3. Ask a question: the gateway picks a provider"
chat auto "$Q" | pp

say "4. Ask the SAME question again: answered from the cache (cache = hit, attempts = 0)"
chat auto "$Q" | pp

say "5. Break the first provider, then ask something new: the gateway falls back (look at 'tried')"
FIRST=$(curl -s "$URL/admin/providers" -H "Authorization: Bearer $ADMIN" | grep -o '"name": *"[^"]*"' | head -1 | sed 's/.*: *"\(.*\)"/\1/')
fault "$FIRST" down
chat auto "Hello after the outage ($(date +%s))" | pp
fault "$FIRST" none

say "6. Conversation history moves with the request (works with any provider)"
fault "$FIRST" ratelimited
curl -s "$URL/v1/chat/completions" -H "Authorization: Bearer $KEY" -H "Content-Type: application/json" -d '{
  "model":"auto",
  "messages":[
    {"role":"user","content":"my name is Srushti"},
    {"role":"assistant","content":"Nice to meet you!"},
    {"role":"user","content":"what is my name?"}
  ]}' | pp
fault "$FIRST" none

say "7. What does each provider look like right now? (quota used, circuit breaker, latency)"
curl -s "$URL/admin/providers" -H "Authorization: Bearer $ADMIN" | pp

say "8. The numbers behind the dashboard"
curl -s "$URL/admin/overview?window=1h" -H "Authorization: Bearer $ADMIN" | pp | head -60
echo "..."
echo
echo "Done. Your demo key was: $KEY"
