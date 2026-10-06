#!/usr/bin/env bash
# relay-poll.sh — zero-token watcher for agent-relay (new /messages protocol).
# Pure shell: costs no tokens. Wakes the assistant only on real work.
#
# Layout (created by the onboarding prompt):
#   ~/workspace/task-relay/identity   single line, e.g. muse-a
#   ~/workspace/task-relay/.token     single line, chmod 600
#   ~/workspace/task-relay/.last_seq  sync cursor (server seq)
#
# Behaviour per poll:
#   1. POST /heartbeat (best-effort)
#   2. GET /messages?for=<id>&since=<seq>
#   3. Empty → exit silently, persisting next_since.
#      Non-empty → print items as JSON to stdout (the hook/cron wakes the
#      assistant with this payload) and advance the cursor.
# Exit codes: 0 = ok (with or without work), 1 = config error, 2 = auth/revoked.
set -euo pipefail

RELAY="${RELAY:-http://100.71.61.96:18789}"
BASE="${BASE:-$HOME/workspace/task-relay}"
TOKEN_FILE="$BASE/.token"
ID_FILE="$BASE/identity"
SEQ_FILE="$BASE/.last_seq"

# Sandbox tunnel proxy (Muse sandbox quirk): if HTTPS_PROXY is set, the
# tailnet address must go through ${HTTPS_PROXY%:*}:3130.
PROXY_ARGS=()
if [[ -n "${HTTPS_PROXY:-}" ]]; then
  tunnel_proxy="${HTTPS_PROXY%:*}:3130"
  PROXY_ARGS=(--proxy "$tunnel_proxy")
fi

ident="$(tr -d '[:space:]' < "$ID_FILE" 2>/dev/null || true)"
if ! [[ "$ident" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ ]]; then
  echo "relay-poll: bad or missing identity in $ID_FILE" >&2
  exit 1
fi
token="$(tr -d '[:space:]' < "$TOKEN_FILE" 2>/dev/null || true)"
if [[ -z "$token" ]]; then
  echo "relay-poll: no token at $TOKEN_FILE" >&2
  exit 1
fi
since="$(cat "$SEQ_FILE" 2>/dev/null || echo 0)"
[[ "$since" =~ ^[0-9]+$ ]] || since=0

auth=(-H "Authorization: Bearer $token")

# 1. heartbeat (best-effort, never blocks the poll)
curl --fail --silent --max-time 10 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
  -X POST "$RELAY/heartbeat" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$ident\"}" >/dev/null 2>&1 || true

# 2. incremental pull
http_code="000"
resp_file="$(mktemp)"
trap 'rm -f "$resp_file"' EXIT
http_code="$(curl --silent --max-time 15 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
  -o "$resp_file" -w '%{http_code}' \
  "$RELAY/messages?for=$ident&since=$since" 2>/dev/null || echo 000)"

case "$http_code" in
  200) ;;
  401)
    echo "relay-poll: 401 unauthorized — token revoked or peer suspended. Stopping poll; ask your human to re-onboard." >&2
    exit 2
    ;;
  426)
    echo "relay-poll: 426 upgrade required — ask your human to re-run the current onboarding prompt." >&2
    exit 2
    ;;
  *)
    echo "relay-poll: relay unreachable (http $http_code), backing off" >&2
    exit 0
    ;;
esac

# 3. advance cursor + emit work if any
next_since="$(jq -r '.next_since // 0' "$resp_file" 2>/dev/null || echo "$since")"
[[ "$next_since" =~ ^[0-9]+$ ]] || next_since="$since"
printf '%s' "$next_since" > "$SEQ_FILE"

count="$(jq -r '.items | length' "$resp_file" 2>/dev/null || echo 0)"
if [[ "$count" == "0" ]]; then
  exit 0
fi

# Work present: hand the full items to the waker.
jq -c '{tasks: [.items[]?]}' "$resp_file"
