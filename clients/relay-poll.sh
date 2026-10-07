#!/usr/bin/env bash
# relay-poll.sh — zero-token watcher for agent-relay (new /messages protocol).
# Pure shell: costs no tokens. Wakes the assistant only on real work.
#
# Layout (created by the onboarding prompt):
#   ~/workspace/task-relay/identity   single line, e.g. muse-a
#   ~/workspace/task-relay/.token     single line, chmod 600
#   ~/workspace/task-relay/.last_seq  sync cursor (server seq)
#   ~/workspace/task-relay/.prompt_version  confirmed worker-instruction rev (§8.6)
#   ~/workspace/task-relay/prompt-update.md  latest pulled instructions (if any)
#
# Behaviour per poll:
#   1. POST /heartbeat (best-effort) carrying prompt_version;
#      prompt_update=true → GET /prompts/current, save to prompt-update.md,
#      and wake the assistant with a prompt_update event.
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
PROMPT_VER_FILE="$BASE/.prompt_version"
PROMPT_UPDATE_FILE="$BASE/prompt-update.md"

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
prompt_ver="$(cat "$PROMPT_VER_FILE" 2>/dev/null || echo 0)"
[[ "$prompt_ver" =~ ^[0-9]+$ ]] || prompt_ver=0

auth=(-H "Authorization: Bearer $token")

# 1. heartbeat (best-effort, never blocks the poll). Carries our confirmed
# prompt_version; the server answers prompt_update=true when newer worker
# instructions exist (§8.6).
hb_file="$(mktemp)"
trap 'rm -f "$hb_file" "$resp_file"' EXIT
curl --fail --silent --max-time 10 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
  -X POST "$RELAY/heartbeat" \
  -H 'Content-Type: application/json' \
  -d "{\"id\":\"$ident\",\"prompt_version\":$prompt_ver}" -o "$hb_file" 2>/dev/null || true

# 1b. prompt update available → pull full current instructions and stage
# them for the assistant. The assistant applies them and confirms by
# reporting the new version in its next heartbeat (UpdatePeerPrompt).
# The `changes` array is the upgrade guide (§8.6): only the entries newer
# than our version, so the assistant patches incrementally. Saved to
# prompt-changes.json; summarized into the wake event.
PROMPT_CHANGES_FILE="$BASE/prompt-changes.json"
update_event=""
# Dedup: already-staged version wakes only once. Without this a peer that
# hasn't confirmed yet gets re-woken every poll (per-round spam).
if jq -e '.prompt_update == true' "$hb_file" >/dev/null 2>&1; then
  new_ver="$(jq -r '.prompt_version // 0' "$hb_file" 2>/dev/null || echo 0)"
  staged_ver="$(cat "$PROMPT_VER_FILE.staged" 2>/dev/null || echo 0)"
  if [[ "$new_ver" =~ ^[0-9]+$ ]] && [[ "$new_ver" -gt "$prompt_ver" ]] && [[ "$staged_ver" != "$new_ver" ]]; then
    pu_file="$(mktemp)"
    if curl --fail --silent --max-time 20 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
        -o "$pu_file" "$RELAY/prompts/current" 2>/dev/null; then
      body="$(jq -r '.prompt // empty' "$pu_file" 2>/dev/null || true)"
      if [[ -n "$body" ]]; then
        printf '%s' "$body" > "$PROMPT_UPDATE_FILE"
        jq -c '.changes // []' "$pu_file" 2>/dev/null > "$PROMPT_CHANGES_FILE" || echo '[]' > "$PROMPT_CHANGES_FILE"
        printf '%s' "$new_ver" > "$PROMPT_VER_FILE.staged"
        changes_sum="$(jq -r '[.[].summary] | join(" | ")' "$PROMPT_CHANGES_FILE" 2>/dev/null || true)"
        update_event="$(jq -nc --arg v "$new_ver" --arg c "$changes_sum" '{prompt_update: true, version: ($v | tonumber), changes: $c}')"
      fi
    fi
    rm -f "$pu_file"
  fi
fi

# 2. incremental pull
http_code="000"
resp_file="$(mktemp)"
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
# Staged prompt update with no message traffic still needs to wake the
# assistant — otherwise new instructions sit unread until the next task.
if [[ "$count" == "0" && -z "$update_event" ]]; then
  exit 0
fi

# Work present: hand the full items to the waker.
if [[ -n "$update_event" ]]; then
  jq -c --argjson pu "$update_event" '{tasks: [.items[]?], prompt_update: $pu}' "$resp_file"
else
  jq -c '{tasks: [.items[]?]}' "$resp_file"
fi
