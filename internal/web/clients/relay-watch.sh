#!/usr/bin/env bash
# relay-watch.sh — agent-relay 标准 Muse 接入：Go 二进制 relay-tail 的薄 shell 层
#
# 架构分工：
#   relay-tail（dumb pipe）只负责传输/增量解析/游标持久化/重连退避，
#   把标准化事件行 {"tasks":[...]} 追加到 spool。
#   本脚本每 5 秒运行一次（cron / hook / 定时任务），负责：
#     1. 看门狗：daemon 挂了就拉起（401/426 则报修、不重启）；
#     2. drain spool 新增行，合并后做唤醒决策（含 prompt_update/client_update 去重）；
#     3. 有新事件才唤醒，否则静默。
#   二选一：同一时间只跑这一种接收方式，不要同时再跑 relay-poll.sh
#   （两者都会写 .last_seq，混用会互相覆盖游标）。
#
# ============ CONFIG（按你的环境改这里） ============
RELAY_URL="${RELAY_URL:-<RELAY_URL>}"      # 中继地址，如 https://relay.example.com
BASE="${BASE:-$HOME/workspace/task-relay}" # 工作目录
# 身份与 token 由 daemon 直接读取，本脚本不碰 token：
#   $BASE/identity   单行身份，如 <YOUR_IDENTITY>
#   $BASE/.token     个人 token，chmod 600
# ====================================================
#
set -euo pipefail

BIN="$BASE/bin/relay-tail"
SPOOL_DIR="$BASE/spool"
SPOOL="$SPOOL_DIR/wake.jsonl"
SPOOL_ERR="$SPOOL_DIR/relay-tail.err"
PIDFILE="$BASE/relay-tail.pid"
OFFSET_FILE="$BASE/.spool_offset"
AUTH_FAIL_MARKER="$BASE/.relay_auth_failed"
PROMPT_VER_FILE="$BASE/.prompt_version"
CLIENT_VER_FILE="$BASE/.client_version"

# ---------- 本地适配：唤醒/日志原语 ----------
# 默认实现：wake 只把事件记到 pending.log，等人工或你自己的调度来处理。
# 如果你有 hook/agent 调度体系，把 wake() 换成调你的唤醒命令
#（如 Hatch 的 hook runtime：source "$HATCH_HOOK_RUNTIME" 后直接调 wake）。
wake() {
  printf '%s\n' "${2-}" >> "$SPOOL_DIR/pending.log"
}
log() {
  printf '[%s] %s %s\n' "$(date -u +%FT%TZ)" "${1-}" "${2-}" >> "$BASE/relay-watch.log"
}
silent() { :; }  # 无事发生；调用处随后 exit 0
# --------------------------------------------

mkdir -p "$SPOOL_DIR"
touch "$SPOOL"

# ---- 1. 看门狗 ----
daemon_alive() {
  [ -f "$PIDFILE" ] || return 1
  local pid
  pid="$(tr -d '[:space:]' < "$PIDFILE" 2>/dev/null || true)"
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  kill -0 "$pid" 2>/dev/null
}

if ! daemon_alive; then
  if [ ! -x "$BIN" ]; then
    log "relay_binary_missing" '{"detail":"bin/relay-tail 不可执行"}'
    wake "中继 Go 二进制丢失或不可执行" '{"binary_missing":true}'
    exit 0
  fi
  if [ -f "$AUTH_FAIL_MARKER" ]; then
    silent "daemon 认证失败待人工处理，暂不拉起"
    exit 0
  fi
  if tail -n 20 "$SPOOL_ERR" 2>/dev/null | grep -qE "relay-tail: (401|426)"; then
    log "relay_auth_error" '{"detail":"daemon 报 401/426，需要重新 onboarding"}'
    touch "$AUTH_FAIL_MARKER"
    wake "中继认证或协议失败，需要重新跑 onboarding" '{"auth_error":true}'
    exit 0
  fi
  # 拉起 daemon。注意两点：
  #  1. 不要用 setsid：它会 fork，$! 拿到的是中间进程 PID，pidfile 失准后
  #     看门狗会误判 daemon 已死、重复拉起（实测：两个 daemon 各持一条 SSE 流，
  #     spool 出现逐字节相同的重复行）。用 nohup（不 fork，$! 准确）。
  #  2. 代理：relay-tail v0.9.0+ 已修复旧版"HTTPS_PROXY 非空就强制改写 :3130"
  #     的 bug（只对 tailnet/私网地址走 tunnel，公网按环境代理原样走）。
  #     用旧版二进制才需要在这里 unset 代理变量。
  (
    export RELAY_URL
    RELAY="$RELAY_URL"
    export RELAY
    cd "$BASE" || exit 1
    nohup "$BIN" --transport sse >>"$SPOOL" 2>>"$SPOOL_ERR" < /dev/null &
    echo $! > "$PIDFILE"
  )
  rm -f "$AUTH_FAIL_MARKER"
  log "relay_daemon_started" '{"transport":"sse"}'
fi

# ---- 2. drain spool ----
offset="$(cat "$OFFSET_FILE" 2>/dev/null || echo 0)"
case "$offset" in ''|*[!0-9]*) offset=0 ;; esac
total="$(wc -l < "$SPOOL" 2>/dev/null || echo 0)"
case "$total" in ''|*[!0-9]*) total=0 ;; esac
out=""
if [ "$total" -gt "$offset" ]; then
  newlines="$(mktemp)"
  tail -n +"$((offset + 1))" "$SPOOL" > "$newlines"
  # 合并多行事件：tasks 拼接；prompt_update / client_update 只取最后一个
  out="$(jq -cs '
    {tasks: [.[] | .tasks // [] | .[]]}
    + ((([.[] | select(has("prompt_update"))] | last) as $pu
        | if $pu == null then {} else {prompt_update: {prompt_update: true, version: ($pu.version // "?"), changes: ($pu.changes // "")}} end)
       + (([.[] | select(has("client_update"))] | last) as $cu
        | if $cu == null then {} else {client_update: {client_update: true, version: ($cu.version // "?"), download: ($cu.download // "")}} end))
  ' "$newlines" 2>/dev/null || true)"
  rm -f "$newlines"
  # offset 只在解析成功后推进：坏行不推进，下一轮重跑（at-least-once）
  if [ -n "$out" ] && [ "$out" != "null" ]; then
    printf '%s' "$total" > "$OFFSET_FILE"
  else
    out=""
  fi
fi

# spool 过大且已排空则截断，防止无限增长
spool_size="$(wc -c < "$SPOOL" 2>/dev/null || echo 0)"
case "$spool_size" in ''|*[!0-9]*) spool_size=0 ;; esac
if [ "$spool_size" -gt 2097152 ]; then
  cur_total="$(wc -l < "$SPOOL" 2>/dev/null || echo 0)"
  cur_offset="$(cat "$OFFSET_FILE" 2>/dev/null || echo 0)"
  case "$cur_offset" in ''|*[!0-9]*) cur_offset=0 ;; esac
  if [ "$cur_offset" -ge "$cur_total" ]; then
    : > "$SPOOL"
    printf '0' > "$OFFSET_FILE"
    log "spool_truncated" '{"reason":"size>2MB drained"}'
  fi
fi

# ---- 3. 唤醒决策 ----
msg_count=0
has_pu="false"
has_cu="false"
if [ -n "$out" ]; then
  msg_count="$(printf '%s' "$out" | jq -r '.tasks | length' 2>/dev/null || echo 0)"
  # prompt_update 去重：已确认版本且无 staged 文件 = 重复推送，不唤醒
  has_pu="$(printf '%s' "$out" | jq -r 'has("prompt_update")' 2>/dev/null || echo false)"
  if [ "$has_pu" = "true" ]; then
    pu_ver_chk="$(printf '%s' "$out" | jq -r '.prompt_update.version // "?"' 2>/dev/null || echo "?")"
    confirmed_ver="$(cat "$PROMPT_VER_FILE" 2>/dev/null || echo "?")"
    if [ "$pu_ver_chk" != "?" ] && [ "$pu_ver_chk" = "$confirmed_ver" ] && [ ! -f "$PROMPT_VER_FILE.staged" ]; then
      has_pu="false"
      log "prompt_update_duplicate" "$(printf '{"version":%s}' "$(printf '%s' "$pu_ver_chk" | jq -Rs .)")"
    fi
  fi
  # client_update 去重：同理
  has_cu="$(printf '%s' "$out" | jq -r 'has("client_update")' 2>/dev/null || echo false)"
  if [ "$has_cu" = "true" ]; then
    cu_ver_chk="$(printf '%s' "$out" | jq -r '.client_update.version // "?"' 2>/dev/null || echo "?")"
    confirmed_cu="$(cat "$CLIENT_VER_FILE" 2>/dev/null || echo "?")"
    if [ "$cu_ver_chk" != "?" ] && [ "$cu_ver_chk" = "$confirmed_cu" ] && [ ! -f "$CLIENT_VER_FILE.staged" ]; then
      has_cu="false"
      log "client_update_duplicate" "$(printf '{"version":%s}' "$(printf '%s' "$cu_ver_chk" | jq -Rs .)")"
    fi
  fi
fi
case "$msg_count" in ''|*[!0-9]*) msg_count=0 ;; esac

# wake payload 只带元数据（id/from/kind/in_reply_to + payload 前 120 字预览），
# 全文 worker 从 spool/wake.jsonl 按 id 取。
# 原因：大 payload 会把唤醒交接的 JSON 撑爆 → 输出被截断 → 解析失败 → wake 丢失，
# 而 spool offset 已推进，任务会静默卡死（实测：一条 1KB+ 的任务触发过此问题）。
final_payload="$(printf '%s' "$out" | jq -c '{
  tasks: [.tasks[]? | {id, from, kind, in_reply_to,
    payload_preview: ((.payload // "" | tostring)[0:120])}],
  prompt_update: .prompt_update,
  client_update: .client_update
}' 2>/dev/null || echo '{}')"
if [ -z "$final_payload" ] || [ "$final_payload" = "null" ]; then
  final_payload='{"tasks":[]}'
fi
if [ "$msg_count" = "0" ] && [ "$has_pu" != "true" ] && [ "$has_cu" != "true" ]; then
  silent "无新消息"
  exit 0
fi

reasons=()
if [ "$has_pu" = "true" ]; then
  pu_ver="$(printf '%s' "$out" | jq -r '.prompt_update.version // "?"' 2>/dev/null || echo "?")"
  reasons+=("服务端下发新版工作指令 v$pu_ver")
  log "prompt_update_pending" "$(printf '{"version":%s}' "$(printf '%s' "$pu_ver" | jq -Rs .)")"
fi
if [ "$has_cu" = "true" ]; then
  cu_ver="$(printf '%s' "$out" | jq -r '.client_update.version // "?"' 2>/dev/null || echo "?")"
  reasons+=("服务端有新版 Go 接收端 v$cu_ver")
  log "client_update_pending" "$(printf '{"version":%s}' "$(printf '%s' "$cu_ver" | jq -Rs .)")"
fi
if [ "$msg_count" != "0" ]; then
  reasons+=("收到 $msg_count 条新消息")
  log "messages_pending" "$(printf '%s' "$out" | jq -c '{count: (.tasks|length), ids: [.tasks[].id]}')"
fi

reason="$(IFS='；'; echo "${reasons[*]}")"
wake "$reason" "$final_payload"
exit 0
