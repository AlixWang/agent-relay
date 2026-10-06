#!/usr/bin/env bash
# task-relay-watch.sh v2 -- lightweight poll of the group task relay.
# Pure shell: costs no tokens. Wakes the worker agent only when there are
# unacked tasks or results for this assistant's identity.
# Identity comes from ~/workspace/task-relay/identity (one line, e.g. muse-a).
set -euo pipefail
source "$HATCH_HOOK_RUNTIME"

RELAY="http://100.71.61.96:18789"
BASE="$HOME/workspace/task-relay"
TOKEN_FILE="$BASE/.token"
ID_FILE="$BASE/identity"
STATE_DIR="$HOME/hooks/state"
STATE_FILE="$STATE_DIR/task-relay.json"
RETRY_SECS=900  # re-wake at most this often for items the worker didn't ack

tunnel_proxy="${HTTPS_PROXY%:*}:3130"

ident="$(tr -d '[:space:]' < "$ID_FILE" 2>/dev/null || true)"
if ! [[ "$ident" =~ ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$ ]]; then
  log "bad or missing identity in $ID_FILE"
  silent "no identity configured"
  exit 0
fi
token="$(tr -d '[:space:]' < "$TOKEN_FILE" 2>/dev/null || true)"
if [[ -z "$token" ]]; then
  log "no relay token at $TOKEN_FILE"
  silent "no relay token configured"
  exit 0
fi

api_get() { # $1 = path; prints body or empty on failure
  curl --fail --silent --max-time 15 \
    --proxy "$tunnel_proxy" \
    -H "Authorization: Bearer $token" \
    "$RELAY$1" 2>/dev/null || true
}

api_post() { # $1 = path, $2 = json body; prints body or empty on failure
  curl --fail --silent --max-time 15 -X POST \
    --proxy "$tunnel_proxy" \
    -H "Authorization: Bearer $token" \
    -H "Content-Type: application/json" \
    -d "$2" "$RELAY$1" 2>/dev/null || true
}

# heartbeat: best effort, never blocks the poll (old relay answers 404: fine)
if [[ -z "$(api_post "/heartbeat" "{\"id\":\"$ident\"}")" ]]; then
  log "heartbeat failed (relay may be pre-v2)"
fi

tasks_json="$(api_get "/tasks?for=$ident")"
results_json="$(api_get "/results?for=$ident")"
if [[ -z "$tasks_json" || -z "$results_json" ]]; then
  log "relay unreachable or auth failed"
  silent "relay unreachable"
  exit 0
fi

mapfile -t task_ids < <(jq -r '.items[]? | select(.acked_by // [] | index("'"$ident"'") | not) | .id' \
  <<<"$tasks_json" 2>/dev/null || true)
mapfile -t result_ids < <(jq -r '.items[]? | select(.acked_by // [] | index("'"$ident"'") | not) | .id' \
  <<<"$results_json" 2>/dev/null || true)
all_ids=("${task_ids[@]}" "${result_ids[@]}")

mkdir -p "$STATE_DIR"
seen_ids=()
last_wake=0
if [[ -f "$STATE_FILE" ]]; then
  mapfile -t seen_ids < <(jq -r '.seen_ids[]?' "$STATE_FILE" 2>/dev/null || true)
  last_wake="$(jq -r '.last_wake // 0' "$STATE_FILE" 2>/dev/null || echo 0)"
fi

new_ids=()
for id in ${all_ids[@]+"${all_ids[@]}"}; do
  skip=0
  for s in ${seen_ids[@]+"${seen_ids[@]}"}; do
    [[ "$id" == "$s" ]] && skip=1 && break
  done
  [[ $skip -eq 0 ]] && new_ids+=("$id")
done

now="$(date +%s)"
should_wake=0
reason="no new relay items"
if [[ ${#new_ids[@]} -gt 0 ]]; then
  should_wake=1
  reason="new relay items: ${new_ids[*]}"
elif [[ ${#all_ids[@]} -gt 0 && $(( now - last_wake )) -ge $RETRY_SECS ]]; then
  should_wake=1
  reason="unacked relay items pending retry: ${all_ids[*]}"
fi

if [[ $should_wake -eq 1 ]]; then
  if [[ "${HATCH_HOOK_DRY_RUN:-0}" != "1" ]]; then
    ids_json="$(printf '%s\n' ${all_ids[@]+"${all_ids[@]}"} | jq -R . | jq -s .)"
    jq -n --argjson ids "$ids_json" --argjson lw "$now" \
      '{seen_ids: $ids, last_wake: $lw}' > "$STATE_FILE"
  fi
  payload="$(jq -n --argjson t "$tasks_json" --argjson r "$results_json" \
    --arg me "$ident" \
    '{tasks: [$t.items[]? | select(.acked_by // [] | index($me) | not)],
      results: [$r.items[]? | select(.acked_by // [] | index($me) | not)]}')"
  wake "$reason" "$payload"
else
  silent "$reason"
fi
