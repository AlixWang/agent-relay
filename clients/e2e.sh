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

# SSE push (§4.4b): backlog replay + live delivery on the same view as poll.
STREAM_OUT="$DATA/stream.out"
(curl -s -N --max-time 6 "${AUTH_B[@]}" \
  "$BASE/messages/stream?for=t-bob&since=0" > "$STREAM_OUT" 2>/dev/null || true) &
STREAM_PID=$!
# Backlog must contain the earlier direct message d1.
sleep 2
if grep -q '"id":"d1"' "$STREAM_OUT" 2>/dev/null && grep -q 'event: message' "$STREAM_OUT" 2>/dev/null; then
  ok "stream backlog replay"
else
  bad "stream backlog replay" "$(head -c 300 "$STREAM_OUT" 2>/dev/null)"
fi
# Live push: send while the stream is held open.
curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
  -H 'Content-Type: application/json' \
  --data '{"id":"stream-live1","to":"t-bob","from":"t-alice","payload":"live via sse"}' > /dev/null
sleep 2
if grep -q '"id":"stream-live1"' "$STREAM_OUT" 2>/dev/null; then
  ok "stream live push"
else
  bad "stream live push" "$(head -c 300 "$STREAM_OUT" 2>/dev/null)"
fi
kill "$STREAM_PID" 2>/dev/null || true
wait "$STREAM_PID" 2>/dev/null || true
# Stream auth parity: spoofed for → 403, no token → 401.
if curl -s -o /dev/null -w '%{http_code}' --max-time 3 "${AUTH_A[@]}" \
    "$BASE/messages/stream?for=t-bob&since=0" | grep -q 403; then
  ok "stream for-spoof 403"
else
  bad "stream for-spoof 403"
fi
if curl -s -o /dev/null -w '%{http_code}' --max-time 3 \
    "$BASE/messages/stream?for=t-bob&since=0" | grep -q 401; then
  ok "stream unauth 401"
else
  bad "stream unauth 401"
fi

# ---- console command center + rooms (DESIGN §6.8/§9.5) ----
# The operator speaks as the reserved identity through POST /admin/messages; a
# room is a routing alias whose members receive one stored message.
jq_ok "room created" '.ok == true and .room.id == "grp_ops" and (.room.members | length) == 2' \
  curl -s -b "$JAR" -X POST "$BASE/admin/rooms" \
    -H 'Content-Type: application/json' \
    --data '{"id":"ops","name":"运维群","members":["t-alice","t-bob"]}'

if curl -s -o /dev/null -w '%{http_code}' -b "$JAR" -X POST "$BASE/admin/rooms" \
    -H 'Content-Type: application/json' \
    --data '{"id":"grp_ops","members":["t-alice"]}' | grep -q 409; then
  ok "duplicate room 409"
else
  bad "duplicate room 409"
fi

OP_SEND="$(curl -s -b "$JAR" -X POST "$BASE/admin/messages" \
  -H 'Content-Type: application/json' \
  --data '{"to":"grp_ops","kind":"task","payload":"@t-alice 控制台指令：回报一次状态"}')"
if echo "$OP_SEND" | jq -e '.ok == true and .thread == "grp_ops"' >/dev/null 2>&1; then
  ok "operator room send"
else
  bad "operator room send" "$OP_SEND"
fi
if curl -s -b "$JAR" -X POST "$BASE/admin/messages" -H 'Content-Type: application/json' \
    --data '{"to":"t-alice","payload":"x","from":"t-alice"}' -o /dev/null -w '%{http_code}' | grep -q 400; then
  ok "operator from-override rejected"
else
  bad "operator from-override rejected"
fi

ROOM_VIEW="$(curl -s "$BASE/messages?for=t-bob&since=0" "${AUTH_B[@]}")"
if echo "$ROOM_VIEW" | jq -e '[.items[] | select(.to == "grp_ops" and .from == "operator")] | length > 0' >/dev/null 2>&1; then
  ok "room message reaches member"
else
  bad "room message reaches member" "$ROOM_VIEW"
fi
# The message author never receives its own room message back. The operator
# authored the one above; for a member-authored message we check after the
# reply below. Here: the member DID get the operator's message (it is a group
# message to the whole room, not a private one).
SELF_VIEW="$(curl -s "$BASE/messages?for=t-alice&since=0" "${AUTH_A[@]}")"
if echo "$SELF_VIEW" | jq -e '[.items[] | select(.payload | test("控制台指令"))] | length == 1' >/dev/null 2>&1; then
  ok "room message delivered to addressed member"
else
  bad "room message delivered to addressed member" "$SELF_VIEW"
fi
jq_ok "member acks room message" '.ok == true' \
  curl -s -X POST "$BASE/ack" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"message_id":"'"$(echo "$SELF_VIEW" | jq -r '[.items[] | select(.to=="grp_ops")][0].id // "none"')"'","by":"t-alice"}'

# Batch ack (§7.8): one wake can carry a dozen chat messages, so the worker acks
# them in one call. Ids it cannot see come back under "skipped" — never a silent
# success for the caller, never a failed call for the rest of the batch.
jq_ok "batch seed 1" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" -H 'Content-Type: application/json' \
    --data '{"id":"batch-1","to":"grp_ops","from":"t-alice","kind":"chat","payload":"批量确认用例第一条：这条消息专门用来验证一次 ack 多条的行为是否生效。"}'
jq_ok "batch seed 2" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" -H 'Content-Type: application/json' \
    --data '{"id":"batch-2","to":"grp_ops","from":"t-alice","kind":"chat","payload":"Second batch-ack fixture with deliberately different wording and length from the first one."}'
BATCH_OUT="$(curl -s -X POST "$BASE/ack" "${AUTH_B[@]}" -H 'Content-Type: application/json' \
  --data '{"ids":["batch-1","batch-2","no-such-batch-id"],"by":"t-bob"}')"
if echo "$BATCH_OUT" | jq -e '.ok == true and .acked == 2 and ((.skipped // []) | index("no-such-batch-id")) != null' >/dev/null 2>&1; then
  ok "batch ack accepts ids"
else
  bad "batch ack accepts ids" "$BATCH_OUT"
fi
if curl -s "$BASE/messages?for=t-bob&since=0" "${AUTH_B[@]}" | jq -e '(.items | map(.id) | index("batch-1")) | not' >/dev/null 2>&1; then
  ok "batch ack stops redelivery"
else
  bad "batch ack stops redelivery"
fi

# The read-back is the opposite of the pull queue: after acking, a member must
# still be able to see what the room just said (§7.8 发言前对表).
if curl -s "$BASE/messages/room?room=grp_ops&limit=20" "${AUTH_B[@]}" | jq -e '(.items | map(.id) | index("batch-1")) != null' >/dev/null 2>&1; then
  ok "room read-back keeps acked messages"
else
  bad "room read-back keeps acked messages"
fi
if curl -s -o /dev/null -w '%{http_code}' "$BASE/messages/room?room=grp_ops" | grep -q 401; then
  ok "room read-back rejects anonymous"
else
  bad "room read-back rejects anonymous"
fi

# A non-member cannot post into the room (403, not a silent black hole).
UNINVITED_CODE="$(mk_invite muse t-carol)"
UNINVITED_TOK="$(do_register "$UNINVITED_CODE" t-carol muse)"
if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/messages" \
    -H "Authorization: Bearer $UNINVITED_TOK" -H 'Content-Type: application/json' \
    --data '{"id":"outsider-1","to":"grp_ops","kind":"chat","payload":"outsider"}' | grep -q 403; then
  ok "room non-member 403"
else
  bad "room non-member 403"
fi
# …and cannot read the room's history back either.
if curl -s -o /dev/null -w '%{http_code}' "$BASE/messages/room?room=grp_ops"     -H "Authorization: Bearer $UNINVITED_TOK" | grep -q 403; then
  ok "room read-back non-member 403"
else
  bad "room read-back non-member 403"
fi

# A member replies to the group; the operator's inbox collects direct reports.
jq_ok "member room reply" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_A[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"room-reply-1","to":"grp_ops","kind":"result","payload":"状态：一切正常"}'
AFTER_REPLY="$(curl -s "$BASE/messages?for=t-alice&since=0" "${AUTH_A[@]}")"
if echo "$AFTER_REPLY" | jq -e '[.items[] | select(.payload | test("一切正常"))] | length == 0' >/dev/null 2>&1; then
  ok "own room message not echoed to sender"
else
  bad "own room message not echoed to sender" "$AFTER_REPLY"
fi
if echo "$AFTER_REPLY" | jq -e '[.items[] | select(.from == "operator")] | length == 1' >/dev/null 2>&1; then
  ok "sender still sees other room messages"
else
  bad "sender still sees other room messages"
fi
jq_ok "agent reports to operator" '.ok == true' \
  curl -s -X POST "$BASE/messages" "${AUTH_B[@]}" \
    -H 'Content-Type: application/json' \
    --data '{"id":"dm-report-1","to":"operator","kind":"result","payload":"回执：无需处理"}'

INBOX="$(curl -s -b "$JAR" "$BASE/admin/inbox")"
if echo "$INBOX" | jq -e '.unread >= 1 and ([.items[] | select(.from == "t-bob")] | length) >= 1' >/dev/null 2>&1; then
  ok "operator inbox unread"
else
  bad "operator inbox unread" "$INBOX"
fi
INBOX_SEQ="$(echo "$INBOX" | jq -r '.items[0].seq')"
jq_ok "inbox marked read" '.ok == true' \
  curl -s -b "$JAR" -X POST "$BASE/admin/inbox/read" \
    -H 'Content-Type: application/json' --data "{\"seq\":$INBOX_SEQ}"
jq_ok "inbox unread cleared" '.unread == 0' \
  curl -s -b "$JAR" "$BASE/admin/inbox"
jq_ok "stats exposes inbox_unread" '.inbox_unread == 0' \
  curl -s -b "$JAR" "$BASE/admin/stats"

# ---- read receipts + 解散群聊 (DESIGN §9.5) ----
# The room thread carries per-member ack detail: the console turns it into
# "谁已读/谁未读" (members who joined after a message are not counted).
RT="$(curl -s -b "$JAR" "$BASE/admin/messages?thread=grp_ops")"
if echo "$RT" | jq -e '[.items[] | select(.recipient == "grp_ops") | select(((.acked_by // []) | index("t-alice")) != null)] | length >= 1' >/dev/null 2>&1; then
  ok "room thread exposes acked_by per message"
else
  bad "room thread exposes acked_by per message" "$RT"
fi
if echo "$RT" | jq -e '.room.members | length >= 2' >/dev/null 2>&1; then
  ok "room thread carries its member list"
else
  bad "room thread carries its member list" "$RT"
fi

jq_ok "room dissolved" '.ok == true and .id == "grp_ops"' \
  curl -s -b "$JAR" -X POST "$BASE/admin/rooms/grp_ops/dissolve" \
    -H 'Content-Type: application/json' --data '{}'
if curl -s -b "$JAR" "$BASE/admin/rooms" | jq -e '[.rooms[] | select(.id == "grp_ops")] | length == 0' >/dev/null 2>&1; then
  ok "dissolved room leaves the active list"
else
  bad "dissolved room leaves the active list"
fi
if curl -s -b "$JAR" "$BASE/admin/rooms?archived=1" | jq -e '[.rooms[] | select(.id == "grp_ops" and .archived == true)] | length == 1' >/dev/null 2>&1; then
  ok "dissolved room listed under archived"
else
  bad "dissolved room listed under archived"
fi
if curl -s -b "$JAR" -X POST "$BASE/admin/messages" -H 'Content-Type: application/json' \
    --data '{"id":"room-after-dissolve","to":"grp_ops","kind":"chat","payload":"解散后"}' \
    -o /dev/null -w '%{http_code}' | grep -q 400; then
  ok "send into dissolved room 400"
else
  bad "send into dissolved room 400"
fi
if curl -s -b "$JAR" -X POST "$BASE/admin/rooms/grp_ops/members" -H 'Content-Type: application/json' \
    --data '{"peer_id":"t-alice"}' -o /dev/null -w '%{http_code}' | grep -q 400; then
  ok "add member to dissolved room 400"
else
  bad "add member to dissolved room 400"
fi
RT2="$(curl -s -b "$JAR" "$BASE/admin/messages?thread=grp_ops")"
if echo "$RT2" | jq -e '.room.archived == true and (.items[-1].kind == "system") and (.items[-1].payload | test("解散"))' >/dev/null 2>&1; then
  ok "dissolve notice posted into the room"
else
  bad "dissolve notice posted into the room" "$RT2"
fi
jq_ok "dissolve is idempotent" '.ok == true and .already == true' \
  curl -s -b "$JAR" -X POST "$BASE/admin/rooms/grp_ops/dissolve" \
    -H 'Content-Type: application/json' --data '{}'

# Room thread view carries per-member ack state and the member list.
ROOM_THREAD="$(curl -s -b "$JAR" "$BASE/admin/messages?thread=grp_ops")"
if echo "$ROOM_THREAD" | jq -e '.room.members | length == 2' >/dev/null 2>&1; then
  ok "room thread members"
else
  bad "room thread members" "$ROOM_THREAD"
fi

# Reserved identities cannot be registered, so nobody can speak as the human.
if curl -s -o /dev/null -w '%{http_code}' -X POST "$BASE/register" \
    -H 'Content-Type: application/json' \
    --data '{"code":"whatever","id":"operator","agent_type":"muse","protocol_version":1}' | grep -q 400; then
  ok "reserved id rejected at register"
else
  bad "reserved id rejected at register"
fi

echo "== pass=$pass fail=$fail =="
test "$fail" -eq 0
