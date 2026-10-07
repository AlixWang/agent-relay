#!/usr/bin/env bash
# relay-tail.sh — SSE tail daemon for agent-relay (new /messages protocol).
# Persistent alternative to relay-poll.sh: holds GET /messages/stream open
# and wakes the assistant within seconds instead of the next poll tick.
# Pure shell + curl + jq: costs no tokens. Same layout, same wake JSON,
# same exit codes as relay-poll.sh — pick ONE of the two scripts.
#
# Layout (created by the onboarding prompt):
#   ~/workspace/task-relay/identity   single line, e.g. muse-a
#   ~/workspace/task-relay/.token     single line, chmod 600
#   ~/workspace/task-relay/.last_seq  sync cursor (server seq)
#   ~/workspace/task-relay/.prompt_version  confirmed worker-instruction rev (§8.6)
#   ~/workspace/task-relay/prompt-update.md  latest pulled instructions (if any)
#
# Behaviour:
#   1. curl -N holds /messages/stream?for=<id>&since=<seq> open; every
#      `data:` line is one message. One connection = one wake-up batch in
#      the same {tasks:[...]} shape relay-poll.sh emits.
#   2. A background heartbeat runs every 60s: reports prompt_version,
#      pulls /prompts/current on prompt_update, stages prompt-update.md +
#      .prompt_version.staged, and emits a {prompt_update:{...}} wake event.
#   3. Disconnects back off exponentially (1s → 60s max) and resume with
#      the persisted cursor — the server replays the backlog, nothing lost.
# Exit codes: 0 = not used (daemon runs until killed), 1 = config error,
# 2 = auth/revoked (ask your human to re-onboard).
set -euo pipefail

RELAY="${RELAY:-http://100.71.61.96:18789}"
BASE="${BASE:-$HOME/workspace/task-relay}"
TOKEN_FILE="$BASE/.token"
ID_FILE="$BASE/identity"
SEQ_FILE="$BASE/.last_seq"
PROMPT_VER_FILE="$BASE/.prompt_version"
PROMPT_UPDATE_FILE="$BASE/prompt-update.md"
HB_FLAG="$BASE/.prompt_update_pending"

# Sandbox tunnel proxy (Muse sandbox quirk): if HTTPS_PROXY is set, the
# tailnet address must go through ${HTTPS_PROXY%:*}:3130.
PROXY_ARGS=()
if [[ -n "${HTTPS_PROXY:-}" ]]; then
  tunnel_proxy="${HTTPS_PROXY%:*}:3130"
  PROXY_ARGS=(--proxy "$tunnel_proxy")
fi

ident="$(tr -d '[:space:]' < "$ID_FILE" 2>/dev/null || true)"
if ! [[ "$ident" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ ]]; then
  echo "relay-tail: bad or missing identity in $ID_FILE" >&2
  exit 1
fi
token="$(tr -d '[:space:]' < "$TOKEN_FILE" 2>/dev/null || true)"
if [[ -z "$token" ]]; then
  echo "relay-tail: no token at $TOKEN_FILE" >&2
  exit 1
fi

auth=(-H "Authorization: Bearer $token")

read_seq() {
  local s
  s="$(cat "$SEQ_FILE" 2>/dev/null || echo 0)"
  [[ "$s" =~ ^[0-9]+$ ]] || s=0
  printf '%s' "$s"
}

# heartbeat_once — one heartbeat round: report prompt_version, stage prompt
# updates, print a prompt_update wake event when staged. Mirrors the
# relay-poll.sh §1b logic so both scripts confirm identically.
# The `changes` upgrade guide (§8.6) is saved to prompt-changes.json.
PROMPT_CHANGES_FILE="$BASE/prompt-changes.json"
heartbeat_once() {
  local prompt_ver hb new_ver pu_json body changes_sum
  prompt_ver="$(cat "$PROMPT_VER_FILE" 2>/dev/null || echo 0)"
  [[ "$prompt_ver" =~ ^[0-9]+$ ]] || prompt_ver=0
  hb="$(curl --fail --silent --max-time 10 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
    -X POST "$RELAY/heartbeat" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$ident\",\"prompt_version\":$prompt_ver}" 2>/dev/null || true)"
  [[ -n "$hb" ]] || return 0
  if ! echo "$hb" | jq -e '.prompt_update == true' >/dev/null 2>&1; then
    return 0
  fi
  new_ver="$(echo "$hb" | jq -r '.prompt_version // 0' 2>/dev/null || echo 0)"
  if ! [[ "$new_ver" =~ ^[0-9]+$ ]] || [[ "$new_ver" -le "$prompt_ver" ]]; then
    return 0
  fi
  pu_json="$(curl --fail --silent --max-time 20 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
    "$RELAY/prompts/current" 2>/dev/null || true)"
  body="$(echo "$pu_json" | jq -r '.prompt // empty' 2>/dev/null || true)"
  if [[ -n "$body" ]]; then
    printf '%s' "$body" > "$PROMPT_UPDATE_FILE"
    echo "$pu_json" | jq -c '.changes // []' 2>/dev/null > "$PROMPT_CHANGES_FILE" || echo '[]' > "$PROMPT_CHANGES_FILE"
    printf '%s' "$new_ver" > "$PROMPT_VER_FILE.staged"
    changes_sum="$(jq -r '[.[].summary] | join(" | ")' "$PROMPT_CHANGES_FILE" 2>/dev/null || true)"
    jq -nc --arg v "$new_ver" --arg c "$changes_sum" '{prompt_update: true, version: ($v | tonumber), changes: $c}'
  fi
}

# emit_batch $stream_file $update_event — parse one connection's SSE frames
# into a single poll-shaped wake-up line on stdout. Advances .last_seq.
emit_batch() {
  local stream_file="$1" update_event="$2"
  local tasks_tmp last_id line id_val count tasks_json
  tasks_tmp="$(mktemp)"
  last_id="$(read_seq)"
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      id:\ *)
        id_val="${line#id: }"
        if [[ "$id_val" =~ ^[0-9]+$ ]]; then
          last_id="$id_val"
        fi
        ;;
      data:\ *)
        printf '%s\n' "${line#data: }" >> "$tasks_tmp"
        ;;
    esac
  done < "$stream_file"
  if [[ "$last_id" =~ ^[0-9]+$ ]]; then
    printf '%s' "$last_id" > "$SEQ_FILE"
  fi
  count="$(jq -s 'length' "$tasks_tmp" 2>/dev/null || echo 0)"
  if [[ "$count" == "0" && -z "$update_event" ]]; then
    rm -f "$tasks_tmp"
    return 1 # keep-alive only: stay silent
  fi
  tasks_json="$(jq -cs '{tasks: .}' "$tasks_tmp" 2>/dev/null || echo '{"tasks":[]}')"
  rm -f "$tasks_tmp"
  if [[ -n "$update_event" ]]; then
    echo "$tasks_json" | jq -c --argjson pu "$update_event" '. + {prompt_update: $pu}'
  else
    echo "$tasks_json" | jq -c '.'
  fi
  return 0
}

# Background heartbeat loop (60s): stages prompt updates into HB_FLAG so the
# main loop wakes promptly even mid-stream.
heartbeat_loop() {
  while true; do
    ev="$(heartbeat_once || true)"
    if [[ -n "$ev" ]]; then
      printf '%s' "$ev" > "$HB_FLAG"
    fi
    sleep 60
  done
}
heartbeat_loop &
HB_PID=$!
trap 'kill $HB_PID 2>/dev/null; exit 0' INT TERM

backoff=1
while true; do
  since="$(read_seq)"
  stream_file="$(mktemp)"
  # -N disables curl buffering. No total timeout: the server sends
  # `: ping` every ~keepalive so idle TCP stays alive through proxies.
  http_code="000"
  http_code="$(curl --silent -N --max-time 0 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} "${auth[@]}" \
    -o "$stream_file" -w '%{http_code}' \
    "$RELAY/messages/stream?for=$ident&since=$since" 2>/dev/null || echo 000)" &
  CURL_PID=$!
  # Watch for heartbeat-staged prompt updates while the stream runs: wake
  # immediately instead of waiting out stream silence.
  while kill -0 "$CURL_PID" 2>/dev/null; do
    sleep 5
    if [[ -f "$HB_FLAG" ]]; then
      kill "$CURL_PID" 2>/dev/null || true
      break
    fi
  done
  wait "$CURL_PID" 2>/dev/null || true

  update_event=""
  if [[ -f "$HB_FLAG" ]]; then
    update_event="$(cat "$HB_FLAG" 2>/dev/null || true)"
    rm -f "$HB_FLAG"
  fi

  case "$http_code" in
    200|000) backoff=1 ;; # 000 = we killed curl for a prompt wake-up
    401)
      echo "relay-tail: 401 unauthorized — token revoked or peer suspended. Stopping; ask your human to re-onboard." >&2
      kill "$HB_PID" 2>/dev/null || true
      rm -f "$stream_file"
      exit 2
      ;;
    426)
      echo "relay-tail: 426 upgrade required — ask your human to re-run the current onboarding prompt." >&2
      kill "$HB_PID" 2>/dev/null || true
      rm -f "$stream_file"
      exit 2
      ;;
    429)
      # Per-peer stream cap: another tail holds our slots. Back off longer.
      sleep 30
      rm -f "$stream_file"
      continue
      ;;
    *)
      # Network blip or server restart: exponential backoff, resume cursor.
      sleep "$backoff"
      backoff=$((backoff * 2))
      [[ "$backoff" -gt 60 ]] && backoff=60
      rm -f "$stream_file"
      continue
      ;;
  esac

  emit_batch "$stream_file" "$update_event" || true
  rm -f "$stream_file"
done
