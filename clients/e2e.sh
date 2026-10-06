#!/usr/bin/env bash
# e2e.sh — end-to-end smoke against a local agent-relay (new protocol only).
# Usage: ./clients/e2e.sh [base_url]   (default http://127.0.0.1:18789)
# Requires: agent-relay binary built, jq, curl. Starts its own server on a
# temp data dir so production is untouched.
set -euo pipefail

BASE="${1:-http://127.0.0.1:18789}"
BIN="${BIN:-./agent-relay}"
DATA="$(mktemp -d)"
CFG="$DATA/config.toml"
PORT="${BASE##*:}"
PORT="${PORT%%/*}"
case "$PORT" in ''|*[!0-9]*) PORT=18789 ;; esac

cat > "$CFG" <<EOF
listen_addr = "127.0.0.1"
port = $PORT
data_dir = "$DATA"
admin_password_hash = ""
verify_timeout_secs = 60
EOF

"$BIN" -config "$CFG" > "$DATA/server.log" 2>&1 &
SRV=$!
trap 'kill $SRV 2>/dev/null; rm -rf "$DATA"' EXIT

# wait for health (max ~10s)
for i in $(seq 1 50); do
  if curl -s --max-time 2 "$BASE/health" | jq -e '.ok == true' >/dev/null 2>&1; then
    break
  fi
  sleep 0.2
done

pass=0; fail=0
ok()   { echo "ok   $1"; pass=$((pass+1)); }
bad()  { echo "FAIL $1${2:+: $2}"; fail=$((fail+1)); }
expect_ok() { # $1 desc, $2... — runs command, expects exit 0
  local desc="$1"; shift
  if "$@" >/dev/null 2>&1; then ok "$desc"; else bad "$desc"; fi
}
jq_ok() { # $1 desc, $2 jq-filter, $3... curl args — fetches JSON, jq -e filter
  local desc="$1" filter="$2"; shift 2
  local out
  if ! out="$("$@" 2>/dev/null)"; then bad "$desc" "curl failed"; return; fi
  if echo "$out" | jq -e "$filter" >/dev/null 2>&1; then ok "$desc"; else bad "$desc" "$out"; fi
}

ADMIN_PW="$(grep -o 'one-time admin password: [^ ]*' "$DATA/server.log" | awk '{print $NF}')"
jq_ok "health ok" '.ok == true and .version == 2' \
  curl -s "$BASE/health"

JAR="$DATA/jar"
jq_ok "admin login" '.ok == true' \
  curl -s -c "$JAR" -X POST "$BASE/admin/login" \
    -H 'Content-Type: application/json' \
    --data "{\"password\":\"$ADMIN_PW\"}"

mk_invite() { # $1 agent_type, $2 peer_id → prints code
  curl -s -b "$JAR" -X POST "$BASE/admin/prompts" \
    -H 'Content-Type: application/json' \
    --data "{\"agent_type\":\"$1\",\"peer_id\":\"$2\",\"create_invite\":true}" \
    | jq -r .code
}
do_register() { # $1 code, $2 id, $3 agent_type → prints token
  curl -s -X POST "$BASE/register" \
    -H 'Content-Type: application/json' \
    --data "{\"code\":\"$1\",\"id\":\"$2\",\"agent_type\":\"$3\",\"protocol_version\":1,\"capabilities\":\"{}\"}" \
    | jq -r .token
}

CODE_A="$(mk_invite muse t-alice)"
CODE_B="$(mk_invite claw t-bob)"
if [ -n "$CODE_A" ] && [ "$CODE_A" != "null" ] && [ -n "$CODE_B" ] && [ "$CODE_B" != "null" ]; then
  ok "invites minted"
else
  bad "invites minted" "$CODE_A / $CODE_B"
fi

TOK_A="$(do_register "$CODE_A" t-alice muse)"
TOK_B="$(do_register "$CODE_B" t-bob claw)"
if [ -n "$TOK_A" ] && [ "$TOK_A" != "null" ] && [ -n "$TOK_B" ] && [ "$TOK_B" != "null" ]; then
  ok "both registered"
else
  bad "both registered" "$TOK_A / $TOK_B"
fi

AUTH_A=(-H "Authorization: Bearer $TOK_A")
AUTH_B=(-H "Authorization: Bearer $TOK_B")

# reusing an invite must fail with 400 (current protocol: tests used_code, not the 426 gate)
if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/register" \
    -H 'Content-Type: application/json' \
    --data "{\"code\":\"$CODE_A\",\"id\":\"t-alice\",\"agent_type\":\"muse\",\"protocol_version\":1}" | grep -q 400; then
  ok "duplicate invite rejected"
else
  bad "duplicate invite rejected"
fi

# verify flow: smoke → poll → result → ack → active
SMOKE="$(curl -s -X POST "$BASE/verify/smoke" "${AUTH_A[@]}" | jq -r .smoke_id)"
if [ -n "$SMOKE" ] && [ "$SMOKE" != "null" ]; then ok "smoke issued"; else bad "smoke issued" "$SMOKE"; fi

SMOKE_JSON="$(curl -s "$BASE/messages?for=t-alice&since=0" "${AUTH_A[@]}")"
if echo "$SMOKE_JSON" | jq -e --arg s "$SMOKE" '.items | map(.id) | index($s)' >/dev/null 2>&1; then ok "smoke visible"; else bad "smoke visible" "$SMOKE_JSON"; fi

jq_ok "result posted" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data "{\"id\":\"r1\",\"to\":\"system\",\"from\":\"t-alice\",\"kind\":\"result\",\"in_reply_to\":\"$SMOKE\",\"payload\":\"收到\"}"

jq_ok "smoke acked" '.ok == true' \
  curl -s -X POST "$BASE/ack" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data "{\"message_id\":\"$SMOKE\",\"by\":\"t-alice\"}"

jq_ok "alice active" '.peers | map(select(.id=="t-alice"))[0].status == "active"' \
  curl -s "$BASE/peers" "${AUTH_A[@]}"

# direct + broadcast + approval hold
jq_ok "direct send" '.seq > 0' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"d1","to":"t-bob","from":"t-alice","payload":"hello bob"}'

jq_ok "direct visible" '.items | map(.id) | index("d1")' \
  curl -s "$BASE/messages?for=t-bob&since=0" "${AUTH_B[@]}"

jq_ok "broadcast held" '.held == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"b1","to":"*","from":"t-alice","payload":"all hands","requires_approval":true}'

jq_ok "held invisible" '(.items | map(.id) | index("b1")) | not' \
  curl -s "$BASE/messages?for=t-bob&since=0" "${AUTH_B[@]}"

SEQ_B="$(curl -s -b "$JAR" "$BASE/admin/messages?thread=t-alice/b1" | jq -r '.items[0].seq')"
jq_ok "admin approve" '.ok == true' \
  curl -s -b "$JAR" -X POST "$BASE/admin/messages/approve" \
    -H 'Content-Type: application/json' \
    --data "{\"seq\":$SEQ_B,\"approve\":true}"

jq_ok "approved visible" '.items | map(.id) | index("b1")' \
  curl -s "$BASE/messages?for=t-bob&since=0" "${AUTH_B[@]}"

# guard: duplicate + spoof + cross-ack
if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"d1","to":"t-bob","from":"t-alice","payload":"again"}' | grep -q 409; then
  ok "dup rejected 409"
else
  bad "dup rejected 409"
fi

if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"sp1","to":"t-bob","from":"t-bob","payload":"spoof"}' | grep -q 403; then
  ok "from-spoof 403"
else
  bad "from-spoof 403"
fi

if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/ack" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"message_id":"d1","by":"t-bob"}' | grep -q 403; then
  ok "cross-ack 403"
else
  bad "cross-ack 403"
fi

jq_ok "heartbeat" '.ok == true' \
  curl -s -X POST "$BASE/heartbeat" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"t-bob"}'

# permission handshake: B started → request → A allow → B resumed → result → ack
jq_ok "perm task" '.seq > 0' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-t1","to":"t-bob","from":"t-alice","payload":"list /tmp/x for me please with distinct wording entirely xxxxx"}'

jq_ok "perm started" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-s1","to":"t-alice","from":"t-bob","kind":"status","in_reply_to":"p-t1","status":"started","payload":"B started executing the listing task now in detail xxxxx"}'

jq_ok "perm request" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-pr1","to":"t-alice","from":"t-bob","kind":"permission_request","in_reply_to":"p-t1","op":"shell.exec","target":"/tmp/x","detail":"list it","payload":"B needs approval to run the listing command in detail xxxxx"}'

jq_ok "perm fields surfaced" '.items | map(select(.kind=="permission_request"))[0].op == "shell.exec"' \
  curl -s "$BASE/messages?for=t-alice&since=0" "${AUTH_A[@]}"

jq_ok "perm allow" '.seq > 0' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-pd1","to":"t-bob","from":"t-alice","kind":"permission_decision","in_reply_to":"p-pr1","decision":"allow","payload":"A approves this one listing operation only xxxx"}'

if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-pd2","to":"t-bob","from":"t-alice","kind":"permission_decision","in_reply_to":"p-pr1","decision":"deny","payload":"A changes mind and denies now in detail xxxxxx"}' | grep -q 409; then
  ok "perm flip 409"
else
  bad "perm flip 409"
fi

jq_ok "perm resumed" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-s2","to":"t-alice","from":"t-bob","kind":"status","in_reply_to":"p-t1","status":"resumed","payload":"B resumed after approval and continues working xxxxx"}'

jq_ok "perm result" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"p-r1","to":"t-alice","from":"t-bob","kind":"result","in_reply_to":"p-t1","payload":"listing done a.txt b.txt with full output xxxxxxxxx"}'

jq_ok "perm task acked" '.ok == true' \
  curl -s -X POST "$BASE/ack" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"message_id":"p-t1","by":"t-bob"}'

echo "== pass=$pass fail=$fail =="
test "$fail" -eq 0
