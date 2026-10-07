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
#   ~/workspace/task-relay/prompt-changes.json  upgrade guide (§8.6 changes)
#
# Behaviour:
#   1. curl -N holds /messages/stream?for=<id>&since=<seq> open; the stream is
#      parsed incrementally — every `data:` frame is emitted as one wake-JSON
#      line on stdout immediately, in the same {tasks:[...]} shape
#      relay-poll.sh emits. `id:` lines advance the persisted cursor.
#      (The stream must be parsed while it is held open: buffering it to a
#      file and parsing only on disconnect would delay live messages until
#      the next reconnect.)
#   2. A background heartbeat runs every 60s: reports prompt_version, pulls
#      /prompts/current on prompt_update, stages prompt-update.md +
#      prompt-changes.json + .prompt_version.staged, and prints a
#      {prompt_update:{...}} wake line (with the changes summary).
#   3. Disconnects back off exponentially (1s → 60s max) and resume with the
#      persisted cursor — the server replays the backlog, nothing lost.
#      A stream that delivered wakes reconnects promptly (no backoff).
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
PROMPT_CHANGES_FILE="$BASE/prompt-changes.json"
EMITTED_FLAG="$BASE/.tail_emitted"

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

# read_seq — persisted sync cursor (server seq). The stream parser advances
# it on every id: line, so a reconnect resumes without replays.
read_seq() {
  local s
  s="$(cat "$SEQ_FILE" 2>/dev/null || echo 0)"
  [[ "$s" =~ ^[0-9]+$ ]] || s=0
  printf '%s' "$s"
}

# heartbeat_once — one heartbeat round: report prompt_version, stage prompt
# updates, print a prompt_update wake event when staged. Mirrors the
# relay-poll.sh logic so both scripts confirm identically.
# The `changes` upgrade guide (§8.6) is saved to prompt-changes.json and
# summarized into the wake event.
heartbeat_once() {
  local prompt_ver hb new_ver pu_json body changes_sum
  prompt_ver="$(cat "$PROMPT_VER_FILE" 2>/dev/null || echo 0)"
  [[ "$prompt_ver" =~ ^[0-9]+$ ]] || prompt_ver=0
  hb="$(curl --fail --silent --max-time 10 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} \
    -H "Authorization: Bearer $token" \
    -X POST "$RELAY/heartbeat" \
    -H 'Content-Type: application/json' \
    -d "{\"id\":\"$ident\",\"prompt_version\":$prompt_ver}" 2>/dev/null || true)"
  [[ -n "$hb" ]] || return 0
  if ! echo "$hb" | jq -e '.prompt_update == true' >/dev/null 2>&1; then
    return 0
  fi
  new_ver="$(echo "$hb" | jq -r '.prompt_version // 0' 2>/dev/null || echo 0)"
  staged_ver="$(cat "$PROMPT_VER_FILE.staged" 2>/dev/null || echo 0)"
  # Dedup: already-staged version wakes only once, not every 60s round.
  if ! [[ "$new_ver" =~ ^[0-9]+$ ]] || [[ "$new_ver" -le "$prompt_ver" ]] || [[ "$staged_ver" == "$new_ver" ]]; then
    return 0
  fi
  pu_json="$(curl --fail --silent --max-time 20 ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} \
    -H "Authorization: Bearer $token" \
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

# parse_stream — incremental SSE parser. Reads frames from stdin; for each
# frame carrying data, prints one wake-JSON line ({tasks:[...]}) to stdout
# immediately and persists the cursor on id: lines. The HTTP status is
# delivered by curl's -w trailer as a final __HTTP_CODE__ line.
# Must parse while the connection is open: buffering to a file and parsing
# only on disconnect delays live messages until the next reconnect.
# Exit codes: 0 stream ended, 2 auth/outdated (401/426), 3 rate-limited (429).
parse_stream() {
  local line id_val tasks_tmp code tasks_json
  tasks_tmp="$(mktemp)"
  code=""
  while IFS= read -r line || [[ -n "$line" ]]; do
    case "$line" in
      "__HTTP_CODE__:"*)
        code="${line#__HTTP_CODE__:}"
        ;;
      "id: "*)
        id_val="${line#id: }"
        if [[ "$id_val" =~ ^[0-9]+$ ]]; then
          printf '%s' "$id_val" > "$SEQ_FILE"
        fi
        ;;
      "data: "*)
        printf '%s\n' "${line#data: }" >> "$tasks_tmp"
        ;;
      "")
        # Blank line = end of one SSE frame: emit immediately.
        if [[ -s "$tasks_tmp" ]]; then
          if tasks_json="$(jq -cs '{tasks: .}' "$tasks_tmp" 2>/dev/null)"; then
            printf '%s\n' "$tasks_json"
            touch "$EMITTED_FLAG"
          fi
          : > "$tasks_tmp"
        fi
        ;;
    esac
  done
  rm -f "$tasks_tmp"
  case "$code" in
    401|426) return 2 ;;
    429) return 3 ;;
  esac
  return 0
}

# Background heartbeat loop (60s). Wake events go straight to stdout (the
# launcher appends stdout to the spool), so instruction updates don't wait
# for stream activity and never kill the held connection.
heartbeat_loop() {
  local ev
  while true; do
    ev="$(heartbeat_once || true)"
    if [[ -n "$ev" ]]; then
      printf '%s\n' "$ev"
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
  rm -f "$EMITTED_FLAG"
  # -w trailer carries the HTTP status through the pipe (parse_stream reads
  # it as the final line). bash builtins don't buffer, so each wake line
  # hits stdout immediately.
  set +e
  curl --silent -N --max-time 0 -w '\n__HTTP_CODE__:%{http_code}\n' \
    ${PROXY_ARGS[@]+"${PROXY_ARGS[@]}"} \
    -H "Authorization: Bearer $token" \
    "$RELAY/messages/stream?for=$ident&since=$since" 2>/dev/null | parse_stream
  pcode=$?
  set -e
  case "$pcode" in
    2)
      echo "relay-tail: 401 unauthorized or 426 upgrade required — token revoked/suspended or protocol outdated. Stopping; ask your human to re-onboard." >&2
      kill "$HB_PID" 2>/dev/null || true
      exit 2
      ;;
    3)
      # Per-peer stream cap: another tail holds our slots. Back off longer.
      sleep 30
      continue
      ;;
  esac
  if [[ -f "$EMITTED_FLAG" ]]; then
    backoff=1 # healthy stream that delivered: reconnect promptly
  else
    # Silent disconnect (or keep-alive-only connection): back off so a
    # flapping network doesn't spin.
    sleep "$backoff"
    backoff=$((backoff * 2))
    [[ "$backoff" -gt 60 ]] && backoff=60
  fi
done
