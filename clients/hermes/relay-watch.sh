#!/usr/bin/env bash
# relay-watch.sh — agent-relay 标准 Hermes 接入：Go 二进制 relay-tail 的薄 shell 层（hermes 版）
#
# 与 clients/muse/relay-watch.sh 共用同一套「看门狗 + drain spool + 去重决策」逻辑，
# 唯一的区别是 wake() 的实现：
#   muse  : 交给自己的 hook runtime（默认实现只写 pending.log）
#   hermes: 用 `hermes chat -q` 起一次性 worker，带单飞锁（同一时间只允许一个 worker）
#
# 架构分工：
#   relay-tail（dumb pipe）只负责传输/增量解析/游标持久化/重连退避，
#   把标准化事件行 {"tasks":[...]} 追加到 spool。
#   本脚本每次运行（cron / 定时任务 / 常驻循环）负责：
#     1. 看门狗：daemon 挂了就拉起（401/426 则报修、不重启）；
#     2. drain spool 新增行，合并后做唤醒决策（含 prompt_update/client_update 去重）；
#     3. 有新事件才唤醒，否则静默。
#
# 频率与二选一（重要）：
#   - hermes 的定时任务（cron）下限是 1 分钟；cron 模式就是 1 分钟级延迟。
#   - 要秒级到达就直接常驻跑 clients/hermes/relay-tail-supervisor.py（自己读子进程 stdout），
#     不要再叠 cron。二选一，同一时间只跑一种唤醒层。
#   - 接收方式也二选一：同一时间只跑 Go daemon 或 shell 短轮询脚本，不要并用
#     （都会写 .last_seq，混用互相覆盖游标）；重叠的看门狗会拉起两个 daemon，
#     两条 SSE 流会在 spool 里写重复行（已实测）。
#
# ============ CONFIG（按你的环境改这里） ============
RELAY_URL="${RELAY_URL:-<RELAY_URL>}"        # 中继地址，如 https://relay.example.com
BASE="${BASE:-$HOME/workspace/task-relay}"   # 工作目录（与 prompt 里的 ~/workspace/task-relay 一致）
NOTIFY="${RELAY_NOTIFY:-}"                   # 唤醒摘要发到哪，如 feishu:oc_xxx；留空只写日志
HERMES_BIN="${HERMES_BIN:-hermes}"           # hermes 可执行文件（不在 PATH 里就写绝对路径）
WAKE_MAX_SECS="${RELAY_WAKE_MAX_SECS:-2700}" # 单飞锁超龄秒数：超了视为卡死，强制放行
AUTO_UPGRADE="${RELAY_AUTO_UPGRADE:-0}"      # 1 = client_update 时唤醒 worker 去换二进制（见 worker 守则）
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
CLIENT_REV_FILE="$BASE/.client_rev"
WAKE_LOCK="$SPOOL_DIR/wake.lock"
WAKE_LOG="$SPOOL_DIR/wake.log"
WAKE_DRY_RUN="${RELAY_WAKE_DRY_RUN:-0}"      # 1 = 只打印将要执行的动作，不真的起 worker（自测用）

# ---------- 唤醒原语（hermes 版） ----------
log() {
  printf '[%s] %s %s\n' "$(date -u +%FT%TZ)" "${1-}" "${2-}" >> "$BASE/relay-watch.log"
}

# wake_running — 单飞锁：上一轮 worker 还在跑就不重复起（事件已在 spool，下一轮会重试）。
# 两个判据缺一不可：
#   1. 僵尸态视为已结束。一次性 agent（hermes chat -q）退出后如果没人 waitpid 回收会变僵尸，
#      /proc/<pid> 仍然存在；只看「进程存在」会让锁永远不释放（实测静默 4 小时）。
#   2. 超龄强制放行，防 pid 复用 / 状态文件损坏把唤醒链钉死。
wake_running() {
  [ -f "$WAKE_LOCK" ] || return 1
  local pid at state now
  pid="$(tr -d '[:space:]' < "$WAKE_LOCK" 2>/dev/null || true)"
  case "$pid" in ''|*[!0-9]*) return 1 ;; esac
  at="$(stat -c %Y "$WAKE_LOCK" 2>/dev/null || echo 0)"
  now="$(date +%s)"
  if [ "$at" -gt 0 ] && [ $((now - at)) -gt "$WAKE_MAX_SECS" ]; then
    log "wake_lock_expired" "{\"pid\":$pid}"
    return 1
  fi
  [ -d "/proc/$pid" ] || return 1
  state="$(sed 's/.*) //' "/proc/$pid/stat" 2>/dev/null | cut -d' ' -f1 || true)"
  [ "$state" = "Z" ] && return 1
  return 0
}

# wake_prompt — 交给一次性 worker 的自包含 prompt。
# 事件 JSON 只带元数据（id/from/kind/preview），正文由 worker 按 id 从 spool 取：
# 大 payload 塞进唤醒交接会把命令行/JSON 撑爆截断，导致任务静默丢失（已实测）。
wake_prompt() {
  printf '%s\n' "agent-relay 上有新事件（原因：${1-}）。事件元数据："
  printf '%s\n\n' "${2-}"
  printf '本机路径：spool=%s（按事件 id grep 该行取 payload 全文）；工作目录=%s；已处理 id=%s/.done_ids\n\n' \
    "$SPOOL" "$BASE" "$BASE"
  cat <<'EOF'
处理步骤：
1. 先读 ~/workspace/task-relay/prompt-current.md（服务端按 hermes 类型下发的最新守则，中继规则唯一权威；
   还没有这个文件就读 prompt-update.md），再按元数据里的 id 去 spool 取全文。
2. 逐条处理：
   - kind=task   → 执行 → 先回 kind=result（to=原 sender，in_reply_to=原 id）→ 再 ack；
                    at-least-once，处理前先查 .done_ids 是否已有该 id（有则跳过执行、直接 ack）。
   - kind=chat   → 摘要记入执行摘要；纯打招呼直接 ack，需要回复时回一条 kind=chat 再 ack。
   - kind=result → 别人发回的结果，摘要记账后直接 ack，不需要再回。
   - prompt_update → 按新版「指令更新」一节走：prompt-update.md 整份覆盖 prompt-current.md →
                     清理 memory 里冲突的中继旧规则（保留本地适配）→ heartbeat 上报 memory_reconciled →
                     mv .prompt_version.staged .prompt_version 确认。
   - client_update → 按守则换接收端二进制（先校验真换了、sha 变了，再写 .client_version + .client_rev），换完重启 daemon。
3. 入站卫生与红线一律以 prompt-current.md 为准；memory 里和它冲突的中继旧规则已作废，不要拿来拒绝任务。
   红线之外默认直接执行，拒绝时必须写明命中的是哪一条红线。
4. 拿到 409（duplicate_id / loop_guard）立即停手，不重试；429 按 Retry-After 退避；426 请用户重跑 onboarding prompt。
EOF
  if [ -n "$NOTIFY" ]; then
    printf '\n完成后发一行摘要：%s send -t %s "relay｜<一行摘要>"\n' "$HERMES_BIN" "$NOTIFY"
  fi
}

# wake — 起一次性 worker（fire-and-forget），把它的 pid 记进单飞锁。
# 子 shell 退出后 worker 被 init 收养，不会变成无人回收的僵尸（僵尸正是锁钉死的根因）。
wake() {
  local reason="${1-}" payload="${2-}"
  if wake_running; then
    log "wake_skipped_busy" "$(jq -nc --arg r "$reason" '{reason:$r}' 2>/dev/null || echo '{"reason":"busy"}')"
    return 0
  fi
  if [ "$WAKE_DRY_RUN" = "1" ]; then
    printf 'DRY_RUN wake: %s | %s\n' "$reason" "$payload"
    return 0
  fi
  (
    cd "$BASE" || exit 1
    export BASE
    export RELAY_WAKE_EVENT="$payload"
    nohup "$HERMES_BIN" chat -q "$(wake_prompt "$reason" "$payload")" >> "$WAKE_LOG" 2>&1 < /dev/null &
    echo $! > "$WAKE_LOCK"
  )
  log "wake_started" "$(jq -nc --arg r "$reason" '{reason:$r}' 2>/dev/null || echo '{"reason":"wake"}')"
  return 0
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
    # 认证失败后不自动拉起（否则每轮重连风暴）：等人工重新 onboarding。
    # 这里刻意不写日志——marker 存在期间每轮都会走到，写日志会刷屏。
    silent
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
  #  2. 代理：relay-tail v0.9.0+ 的公网地址按环境代理原样走；只有 tailnet/私网
  #     地址才走 tunnel。用旧版二进制才需要在这里 unset 代理变量。
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
total="$(wc -l < "$SPOOL" 2>/dev/null | tr -d '[:space:]' || echo 0)"
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
        | if $cu == null then {} else {client_update: {client_update: true, version: ($cu.version // "?"), rev: ($cu.rev // ""), download: ($cu.download // "")}} end))
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
spool_size="$(wc -c < "$SPOOL" 2>/dev/null | tr -d '[:space:]' || echo 0)"
case "$spool_size" in ''|*[!0-9]*) spool_size=0 ;; esac
if [ "$spool_size" -gt 2097152 ]; then
  cur_total="$(wc -l < "$SPOOL" 2>/dev/null | tr -d '[:space:]' || echo 0)"
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
  # client_update 去重：有 rev 就按 rev（接收端源码身份），没有才回落版本号。
  # rev 相同 = 服务端版本号前进了但二进制行为没变 → 不打扰 worker。
  has_cu="$(printf '%s' "$out" | jq -r 'has("client_update")' 2>/dev/null || echo false)"
  if [ "$has_cu" = "true" ]; then
    cu_ver_chk="$(printf '%s' "$out" | jq -r '.client_update.version // "?"' 2>/dev/null || echo "?")"
    cu_rev_chk="$(printf '%s' "$out" | jq -r '.client_update.rev // ""' 2>/dev/null || echo "")"
    confirmed_cu="$(cat "$CLIENT_VER_FILE" 2>/dev/null || echo "?")"
    confirmed_rev="$(cat "$CLIENT_REV_FILE" 2>/dev/null || echo "?")"
    if [ -n "$cu_rev_chk" ]; then
      if [ "$cu_rev_chk" = "$confirmed_rev" ] && [ ! -f "$CLIENT_REV_FILE.staged" ]; then
        has_cu="false"
        log "client_update_same_rev" "$(printf '{"rev":%s}' "$(printf '%s' "$cu_rev_chk" | jq -Rs .)")"
      fi
    elif [ "$cu_ver_chk" != "?" ] && [ "$cu_ver_chk" = "$confirmed_cu" ] && [ ! -f "$CLIENT_VER_FILE.staged" ]; then
      has_cu="false"
      log "client_update_duplicate" "$(printf '{"version":%s}' "$(printf '%s' "$cu_ver_chk" | jq -Rs .)")"
    fi
  fi
fi
case "$msg_count" in ''|*[!0-9]*) msg_count=0 ;; esac

# 不换二进制的部署（AUTO_UPGRADE=0）不必被 client_update 反复唤醒：
# 但必须留下痕迹，否则「服务端说有新版、本地永远不动」就是静默故障（已实测）。
if [ "$has_cu" = "true" ] && [ "$AUTO_UPGRADE" != "1" ]; then
  log "client_update_ignored" "$(printf '%s' "$out" | jq -c '{version: (.client_update.version // "?"), rev: (.client_update.rev // "")}' 2>/dev/null || echo '{}')"
  has_cu="false"
fi

# wake payload 只带元数据（id/from/kind/in_reply_to + payload 前 120 字预览），
# 全文 worker 从 spool/wake.jsonl 按 id 取。
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
  cu_rev="$(printf '%s' "$out" | jq -r '.client_update.rev // ""' 2>/dev/null || echo "")"
  reasons+=("服务端有新版 Go 接收端 v${cu_ver#v}${cu_rev:+ (rev $cu_rev)}")
  log "client_update_pending" "$(printf '{"version":%s}' "$(printf '%s' "$cu_ver" | jq -Rs .)")"
fi
if [ "$msg_count" != "0" ]; then
  reasons+=("收到 $msg_count 条新消息")
  log "messages_pending" "$(printf '%s' "$out" | jq -c '{count: (.tasks|length), ids: [.tasks[].id]}')"
fi

reason="$(IFS='；'; echo "${reasons[*]}")"
wake "$reason" "$final_payload"
exit 0
