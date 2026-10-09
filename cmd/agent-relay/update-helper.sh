#!/usr/bin/env bash
# agent-relay-update — web one-click update worker for systemd installs.
#
# Canonical copy: this file is embedded in the agent-relay binary and extracted
# by deploy/install.sh (`agent-relay --print-update-helper`), so the worker and
# the binary can never drift apart. The worker also refreshes itself from the
# freshly installed binary after every successful update, which is how helper
# improvements ship with a release instead of needing another install.sh run.
#
# Runs as root, triggered by agent-relay-update.timer (clean mount namespace, no
# ProtectSystem). Scans $DATA_DIR/update-jobs/pending for <ver>.req, processes
# the oldest, deletes the req. Nothing about the job lives in the gateway's
# memory: the gateway restarts itself here.
#
# Protocol with the gateway (internal/gateway/update.go). The .req content is
# the job id; every line this script writes is appended to
# $DATA_DIR/update-jobs/<ver>.log, which the console tails live. Machine lines:
#   STEP <phase> <detail>    queue|download|checksum|backup|install|helper|
#                            restart|health|result
#   UPDATE_RESULT <ok|rolled_back|failed> <detail>     terminal line, exactly one
set -uo pipefail

REPO="${AGENT_RELAY_REPO:-AlixWang/agent-relay}"
BIN_PATH="${AGENT_RELAY_BIN:-/usr/local/bin/agent-relay}"
CONFIG_PATH="${AGENT_RELAY_CONFIG:-/etc/agent-relay/config.toml}"
DATA_DIR="${AGENT_RELAY_DATA:-/var/lib/agent-relay}"
HELPER="${AGENT_RELAY_HELPER:-/usr/local/sbin/agent-relay-update}"
KEEP_BACKUPS=3
KEEP_LOGS=10

JOB_DIR="$DATA_DIR/update-jobs"
PENDING_DIR="$JOB_DIR/pending"

ts() { date -u '+%H:%M:%S'; }
logf() { printf '%s [update] %s\n' "$(ts)" "$*"; }
step() { printf '%s STEP %s %s\n' "$(ts)" "$1" "${2:-}"; }
result() { printf '%s UPDATE_RESULT %s %s\n' "$(ts)" "$1" "$2"; }
die() { # die <phase> <detail>
  logf "$1 failed: $2"
  step "$1" "failed: $2"
  result failed "$1"
  exit 1
}

# ---- pick up the oldest pending request --------------------------------
mkdir -p "$PENDING_DIR" 2>/dev/null || true
REQ="$(ls "$PENDING_DIR"/*.req 2>/dev/null | head -1 || true)"
[ -n "$REQ" ] || exit 0
ver="$(basename "$REQ" .req)"
job="$(head -1 "$REQ" 2>/dev/null | tr -d '[:space:]')"
rm -f "$REQ"
case "$ver" in v[0-9]*) ;; *) logf "ignoring malformed version '$ver'"; exit 0 ;; esac
case "$ver" in *[^A-Za-z0-9._-]*) logf "ignoring malformed version '$ver'"; exit 0 ;; esac

ARCH_RAW="$(uname -m)"
case "$ARCH_RAW" in
  x86_64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *)
    logf "unsupported arch $ARCH_RAW"
    exit 1
    ;;
esac

mkdir -p "$JOB_DIR"
JOB="$JOB_DIR/$ver.log"
: >"$JOB"

{
  logf "job ${job:-?} target $ver arch $ARCH"
  step queue "picked up $ver (job ${job:-?}, arch $ARCH)"

  # Running release, so a stale queue entry can never downgrade a server.
  RUNNING="$("$BIN_PATH" --version 2>/dev/null | sed -n 's/.*release \(v[0-9][A-Za-z0-9._-]*\).*/\1/p' | head -1)"
  if [ -n "$RUNNING" ] && [ "$RUNNING" != "$ver" ]; then
    newest="$(printf '%s\n%s\n' "$RUNNING" "$ver" | sort -V 2>/dev/null | tail -1)"
    if [ -n "$newest" ] && [ "$newest" = "$RUNNING" ]; then
      logf "refusing $ver: not newer than running $RUNNING"
      step result "skip $ver (running $RUNNING is not older)"
      result failed "not-newer-than-$RUNNING"
      exit 1
    fi
  fi
  [ -n "$RUNNING" ] && logf "running release $RUNNING → $ver"

  TMP="$(mktemp -d)"
  trap 'rm -rf "$TMP"' EXIT
  URL="https://github.com/$REPO/releases/download/$ver/agent-relay-linux-$ARCH"
  SUMS="https://github.com/$REPO/releases/download/$ver/SHA256SUMS"

  step download "GET $URL"
  t0=$SECONDS
  curl -fSL --retry 3 --retry-delay 2 --connect-timeout 15 -o "$TMP/agent-relay" "$URL" ||
    die download "curl exit $? for $URL"
  logf "downloaded $(wc -c <"$TMP/agent-relay") bytes in $((SECONDS - t0))s"

  step checksum "verify agent-relay-linux-$ARCH against SHA256SUMS"
  curl -fSL --connect-timeout 15 -o "$TMP/SHA256SUMS" "$SUMS" ||
    die checksum "SHA256SUMS download failed"
  # Match by asset name: the release lists several files.
  EXPECTED="$(grep " agent-relay-linux-$ARCH\$" "$TMP/SHA256SUMS" | awk '{print $1}' | head -1)"
  ACTUAL="$(sha256sum "$TMP/agent-relay" | awk '{print $1}')"
  if [ -z "$EXPECTED" ]; then
    logf "no 'agent-relay-linux-$ARCH' line in SHA256SUMS"
    step checksum "asset name not listed in SHA256SUMS"
    result failed checksum-missing
    exit 1
  fi
  if [ "$EXPECTED" != "$ACTUAL" ]; then
    logf "checksum mismatch expected=$EXPECTED actual=$ACTUAL"
    step checksum "mismatch expected=$EXPECTED actual=$ACTUAL"
    result failed checksum
    exit 1
  fi
  step checksum "ok $ACTUAL"

  TS="$(date -u +%Y%m%d%H%M%S)"
  BAK="$BIN_PATH.bak.$TS"
  step backup "cp $BIN_PATH $BAK"
  cp "$BIN_PATH" "$BAK" || die backup "cannot back up $BIN_PATH"

  step install "install $TMP/agent-relay -> $BIN_PATH"
  install -m 755 "$TMP/agent-relay" "$BIN_PATH" || die install "cannot write $BIN_PATH"
  if ! "$BIN_PATH" --version >/dev/null 2>&1; then
    logf "new binary does not run, restoring $BAK"
    cp "$BAK" "$BIN_PATH"
    step install "new binary is not runnable, restored the backup"
    result failed binary-not-runnable
    exit 1
  fi
  logf "installed $ver ($("$BIN_PATH" --version 2>/dev/null | head -1))"

  # Refresh this worker from the binary just installed: one source of truth.
  # Written to a temp file and renamed so the running script keeps its inode.
  step helper "refresh $HELPER from the new binary"
  if "$BIN_PATH" --print-update-helper >"$TMP/helper" 2>/dev/null && [ -s "$TMP/helper" ]; then
    chmod 700 "$TMP/helper"
    chown root:root "$TMP/helper" 2>/dev/null || true
    if mv -f "$TMP/helper" "$HELPER"; then
      logf "helper refreshed ($(wc -l <"$HELPER") lines)"
    else
      logf "helper refresh failed (kept the current worker)"
    fi
  else
    logf "helper refresh skipped: binary has no --print-update-helper"
  fi

  step restart "systemctl restart agent-relay"
  if ! systemctl restart agent-relay; then
    logf "restart failed, rolling back to $BAK"
    cp "$BAK" "$BIN_PATH"
    systemctl restart agent-relay || true
    result rolled_back "restart-failed"
    exit 1
  fi

  PORT="$(sed -n 's/^ *port *= *//p' "$CONFIG_PATH" | tr -d '"[:space:]' | head -1)"
  [ -n "$PORT" ] || PORT=18789
  step health "poll http://127.0.0.1:$PORT/health"
  healthy=0
  for i in 1 2 3 4 5 6 7 8; do
    sleep 2
    body="$(curl -sk --max-time 5 "http://127.0.0.1:$PORT/health" || true)"
    case "$body" in
      *'"ok":true'*)
        healthy=1
        logf "health ok after attempt $i"
        break
        ;;
    esac
    step health "attempt $i: no ok yet"
  done

  if [ "$healthy" = "1" ]; then
    step health "ok"
    ls -t "$BIN_PATH".bak.* 2>/dev/null | tail -n +$((KEEP_BACKUPS + 1)) | xargs -r rm -f
    ls -t "$JOB_DIR"/*.log 2>/dev/null | tail -n +$((KEEP_LOGS + 1)) | xargs -r rm -f
    step result "finished in ${SECONDS}s, now running $ver"
    result ok "$ver"
    exit 0
  fi

  logf "health failed after 8 attempts, rolling back to $BAK"
  step health "failed, rolling back to $(basename "$BAK")"
  cp "$BAK" "$BIN_PATH"
  systemctl restart agent-relay || true
  sleep 3
  if curl -sk --max-time 5 "http://127.0.0.1:$PORT/health" | grep -q '"ok":true'; then
    logf "rolled back to $RUNNING"
    step result "rolled back to $RUNNING"
    result rolled_back "health-failed"
  else
    logf "rollback uncertain: /health is not answering"
    result failed "health-failed-rollback-uncertain"
  fi
  exit 1
} 2>&1 | tee -a "$JOB"

# tee would hide the worker's own exit status; pipefail (set above) keeps it.
exit "${PIPESTATUS[0]}"
