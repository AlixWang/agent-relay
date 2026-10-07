#!/usr/bin/env bash
# task-relay-watch.sh — Hatch hook wrapper around relay-poll.sh (new protocol).
# 注：本脚本只适配 relay-poll.sh 短轮询；Go 二进制方案请用 clients/muse/relay-watch.sh。
# Calls the zero-token poller; wakes the worker only when it emits work JSON.
set -euo pipefail
source "$HATCH_HOOK_RUNTIME"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
POLLER="$SCRIPT_DIR/relay-poll.sh"

if [[ ! -x "$POLLER" ]]; then
  chmod +x "$POLLER" 2>/dev/null || true
fi

# shellcheck disable=SC2154
if [[ "${HATCH_HOOK_DRY_RUN:-0}" == "1" ]]; then
  log "dry_run: would poll relay"
  silent "dry run"
  exit 0
fi

set +e
payload="$("$POLLER" 2>"$HOME/hooks/state/relay-poll.err")"
code=$?
set -e

case "$code" in
  0) ;;
  1)
    log "relay-poll config error: $(cat "$HOME/hooks/state/relay-poll.err" 2>/dev/null)"
    silent "relay not configured"
    exit 0
    ;;
  2)
    # Auth revoked / protocol outdated → must surface to the human.
    err="$(cat "$HOME/hooks/state/relay-poll.err" 2>/dev/null)"
    log "relay-poll needs attention: $err"
    wake "relay needs re-onboarding: $err" "{\"error\": \"$err\"}"
    exit 0
    ;;
  *)
    log "relay-poll failed (code $code)"
    silent "relay poll failed"
    exit 0
    ;;
esac

if [[ -z "$payload" ]]; then
  silent "no new relay items"
  exit 0
fi

count="$(echo "$payload" | jq -r '.tasks | length' 2>/dev/null || echo 1)"
wake "new relay items ($count)" "$payload"
