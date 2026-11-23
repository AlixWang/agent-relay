package web

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// These tests exercise the 1.1.0 stable core: config file, wake adapters,
// fail-closed RELAY_URL, proxy modes and the tick lock. They run the exact
// embedded artifact against a fake BASE/PATH and a deliberately minimal
// environment: no HATCH_HOOK_RUNTIME, RELAY_URL, proxy vars or BASE leak in
// from the host (a Hatch session would otherwise poison the generic case).

type watchFixture struct {
	root    string
	base    string
	fakebin string
	script  string
}

func newWatchFixture(t *testing.T) *watchFixture {
	t.Helper()
	requireWatchTools(t)
	root := t.TempDir()
	base := filepath.Join(root, "base")
	fakebin := filepath.Join(root, "fakebin")
	for _, dir := range []string{filepath.Join(base, "spool"), fakebin} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestFile(t, filepath.Join(base, "identity"), "muse-test", 0o644)
	writeTestFile(t, filepath.Join(base, ".token"), "test-token", 0o600)
	writeTestFile(t, filepath.Join(base, "relay-tail.pid"), fmt.Sprintf("%d", os.Getpid()), 0o644)
	writeTestFile(t, filepath.Join(base, ".spool_offset"), "0", 0o644)
	// The watchdog only needs a live pid with comm=relay-tail; faking ps keeps
	// the test independent of the process name.
	writeTestFile(t, filepath.Join(fakebin, "ps"), "#!/bin/sh\necho relay-tail\n", 0o755)
	writeTestFile(t, filepath.Join(fakebin, "curl"), "#!/usr/bin/env bash\nprintf '{\"ok\":true}'\n", 0o755)
	script := filepath.Join(root, "relay-watch.sh")
	writeTestFile(t, script, WatchScript(), 0o755)
	return &watchFixture{root: root, base: base, fakebin: fakebin, script: script}
}

func (f *watchFixture) env(extra ...string) []string {
	env := []string{
		"PATH=" + f.fakebin + string(os.PathListSeparator) + os.Getenv("PATH"),
		"HOME=" + f.root,
		"BASE=" + f.base,
		"LC_ALL=C",
		"AGENT_LOG_UPLOAD=0",
		"SCRIPT_AUTO_UPDATE=0",
	}
	return append(env, extra...)
}

func (f *watchFixture) addSpoolTask(t *testing.T, id, payload string) {
	t.Helper()
	line, err := json.Marshal(map[string]any{"tasks": []map[string]any{{
		"id": id, "from": "alice", "to": "muse-test", "kind": "task", "payload": payload,
	}}})
	if err != nil {
		t.Fatalf("marshal task: %v", err)
	}
	path := filepath.Join(f.base, "spool", "wake.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open spool: %v", err)
	}
	defer file.Close()
	if _, err := file.Write(append(line, '\n')); err != nil {
		t.Fatalf("write spool: %v", err)
	}
}

func (f *watchFixture) run(t *testing.T, env []string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "bash", f.script)
	cmd.Dir = f.root
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("relay-watch.sh failed: %v\n%s", err, out)
	}
	return string(out)
}

func requireWatchTools(t *testing.T) {
	t.Helper()
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}
}

func writeFakeHatchRuntime(t *testing.T, path string) {
	t.Helper()
	body := `#!/usr/bin/env bash
_hatch_hook_emit() {
  local decision="$1" reason="${2-}" payload="${3-}" result
  if [ -n "$payload" ]; then
    result="$(jq -cn --arg d "$decision" --arg r "$reason" --argjson p "$payload" \
      '{decision:$d,reason:$r,payload:$p}')" || return 1
  else
    result="$(jq -cn --arg d "$decision" --arg r "$reason" '{decision:$d,reason:$r}')" || return 1
  fi
  printf 'HATCH_HOOK_RESULT:%s\n' "$result"
  exit 0
}
wake() { _hatch_hook_emit "wake" "${1-}" "${2-}"; }
silent() { _hatch_hook_emit "silent" "${1-}" "${2-}"; }
log() {
  local message="${1-}" fields="${2-}"
  [ -n "$fields" ] || fields='{}'
  printf 'HATCH_HOOK_LOG:%s\n' "$(jq -cn --arg m "$message" --argjson f "$fields" '$f + {message:$m}')" >&2
}
`
	writeTestFile(t, path, body, 0o755)
}

func TestWatchScriptHatchAdapter(t *testing.T) {
	f := newWatchFixture(t)
	runtime := filepath.Join(f.root, "hatch_hook_runtime.sh")
	writeFakeHatchRuntime(t, runtime)
	f.addSpoolTask(t, "t1", "do the thing")

	env := f.env("RELAY_URL=http://relay.invalid", "HATCH_HOOK_RUNTIME="+runtime, "WATCH_RUNTIME_LOG=1")
	out := f.run(t, env)
	if !strings.Contains(out, `"decision":"wake"`) {
		t.Fatalf("first run did not emit a Hatch wake:\n%s", out)
	}
	if !strings.Contains(out, "HATCH_HOOK_LOG:") {
		t.Fatalf("WATCH_RUNTIME_LOG=1 did not mirror a Hatch log:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "1" {
		t.Fatalf("offset after wake = %q, want 1 (the Hatch exit-0 bug regressed)", got)
	}
	if pending := readOptional(t, filepath.Join(f.base, "spool", "pending.log")); pending != "" {
		t.Fatalf("hatch adapter wrote pending.log: %s", pending)
	}
	if log := readOptional(t, filepath.Join(f.base, "relay-watch.log")); !strings.Contains(log, "script.mode=hatch") {
		t.Fatalf("missing script.mode=hatch in relay-watch.log:\n%s", log)
	}

	// Second run over the same spool must be silent and must not re-wake.
	out = f.run(t, env)
	if !strings.Contains(out, `"decision":"silent"`) {
		t.Fatalf("second run did not emit a Hatch silent:\n%s", out)
	}
	if strings.Contains(out, `"decision":"wake"`) {
		t.Fatalf("same spool page woke twice:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "1" {
		t.Fatalf("offset after second run = %q, want 1", got)
	}
}

func TestWatchScriptGenericAdapter(t *testing.T) {
	f := newWatchFixture(t)
	f.addSpoolTask(t, "g1", "generic wake")

	out := f.run(t, f.env("RELAY_URL=http://relay.invalid"))
	if strings.Contains(out, "HATCH_HOOK_RESULT") {
		t.Fatalf("generic adapter unexpectedly emitted a Hatch result:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "1" {
		t.Fatalf("offset after generic wake = %q, want 1", got)
	}
	pending := readOptional(t, filepath.Join(f.base, "spool", "pending.log"))
	if !strings.Contains(pending, "g1") {
		t.Fatalf("generic wake missing from pending.log:\n%s", pending)
	}
	if log := readOptional(t, filepath.Join(f.base, "relay-watch.log")); !strings.Contains(log, "script.mode=generic") {
		t.Fatalf("missing script.mode=generic in relay-watch.log:\n%s", log)
	}
}

func TestWatchScriptOverlayAdapter(t *testing.T) {
	f := newWatchFixture(t)
	f.addSpoolTask(t, "o1", "overlay wake")
	overlay := filepath.Join(f.base, "relay-watch.local.sh")
	writeTestFile(t, overlay, `watch_wake() { printf 'OVERLAY_WAKE:%s\n' "$1"; return 0; }
watch_silent() { printf 'OVERLAY_SILENT:%s\n' "$1"; return 0; }
`, 0o644)

	out := f.run(t, f.env("RELAY_URL=http://relay.invalid"))
	if !strings.Contains(out, "OVERLAY_WAKE:") {
		t.Fatalf("overlay adapter was not used:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "1" {
		t.Fatalf("offset after overlay wake = %q, want 1", got)
	}
	if log := readOptional(t, filepath.Join(f.base, "relay-watch.log")); !strings.Contains(log, "script.mode=overlay") {
		t.Fatalf("missing script.mode=overlay in relay-watch.log:\n%s", log)
	}
}

func TestWatchScriptOverlayRejectsGroupWritable(t *testing.T) {
	f := newWatchFixture(t)
	f.addSpoolTask(t, "o2", "must fall back to generic")
	overlay := filepath.Join(f.base, "relay-watch.local.sh")
	writeTestFile(t, overlay, `watch_wake() { printf 'OVERLAY_WAKE:%s\n' "$1"; return 0; }
watch_silent() { return 0; }
`, 0o644)
	if err := os.Chmod(overlay, 0o666); err != nil {
		t.Fatalf("chmod overlay: %v", err)
	}

	out := f.run(t, f.env("RELAY_URL=http://relay.invalid"))
	if strings.Contains(out, "OVERLAY_WAKE:") {
		t.Fatalf("group-writable overlay must be ignored:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "1" {
		t.Fatalf("offset after fallback = %q, want 1", got)
	}
	if pending := readOptional(t, filepath.Join(f.base, "spool", "pending.log")); !strings.Contains(pending, "o2") {
		t.Fatalf("fallback did not write pending.log:\n%s", pending)
	}
	log := readOptional(t, filepath.Join(f.base, "relay-watch.log"))
	if !strings.Contains(log, "config.overlay_rejected") || !strings.Contains(log, "script.mode=generic") {
		t.Fatalf("missing rejection/fallback log:\n%s", log)
	}
}

func TestWatchScriptMissingRelayURL(t *testing.T) {
	f := newWatchFixture(t)
	f.addSpoolTask(t, "m1", "must not be drained")

	env := f.env() // deliberately no RELAY_URL, no relay-watch.env
	out1 := f.run(t, env)
	if got := strings.Count(out1, "RELAY_URL 未配置"); got != 1 {
		t.Fatalf("first missing-url run printed %d errors, want 1:\n%s", got, out1)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "0" {
		t.Fatalf("fail-closed run advanced offset to %q", got)
	}
	if pending := readOptional(t, filepath.Join(f.base, "spool", "pending.log")); pending != "" {
		t.Fatalf("fail-closed run wrote pending.log: %s", pending)
	}
	log := readOptional(t, filepath.Join(f.base, "relay-watch.log"))
	if got := strings.Count(log, "config.missing_relay_url"); got != 1 {
		t.Fatalf("missing-url log count = %d, want 1:\n%s", got, log)
	}

	// Within the hour the error/log must stay rate-limited (no 5s-tick spam).
	out2 := f.run(t, env)
	if strings.Contains(out2, "RELAY_URL 未配置") {
		t.Fatalf("second missing-url run spammed stderr:\n%s", out2)
	}
	log = readOptional(t, filepath.Join(f.base, "relay-watch.log"))
	if got := strings.Count(log, "config.missing_relay_url"); got != 1 {
		t.Fatalf("missing-url log count after second run = %d, want 1:\n%s", got, log)
	}
}

func TestWatchScriptProxyModes(t *testing.T) {
	const (
		unset = "__unset__"
		hp    = "http://hp.invalid:3128"
		hpl   = "http://hpl.invalid:3128"
		p     = "http://p.invalid:3128"
		pl    = "http://pl.invalid:3128"
		ap    = "socks5://ap.invalid:1080"
		apl   = "socks5://apl.invalid:1080"
	)
	cases := []struct {
		name string
		mode string
		want map[string]string
	}{
		{
			name: "inherit keeps the environment",
			mode: "inherit",
			want: map[string]string{"HTTPS_PROXY": hp, "https_proxy": hpl, "HTTP_PROXY": p, "http_proxy": pl, "ALL_PROXY": ap, "all_proxy": apl},
		},
		{
			name: "strip removes only HTTP(S) proxies",
			mode: "strip",
			want: map[string]string{"HTTPS_PROXY": unset, "https_proxy": unset, "HTTP_PROXY": unset, "http_proxy": unset, "ALL_PROXY": ap, "all_proxy": apl},
		},
		{
			name: "direct removes every proxy",
			mode: "direct",
			want: map[string]string{"HTTPS_PROXY": unset, "https_proxy": unset, "HTTP_PROXY": unset, "http_proxy": unset, "ALL_PROXY": unset, "all_proxy": unset},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newWatchFixture(t)
			if err := os.Remove(filepath.Join(f.base, "relay-tail.pid")); err != nil {
				t.Fatalf("remove pidfile: %v", err)
			}
			if err := os.Remove(filepath.Join(f.base, ".token")); err != nil {
				t.Fatalf("remove token: %v", err)
			}
			envOut := filepath.Join(f.root, "daemon-env.txt")
			if err := os.MkdirAll(filepath.Join(f.base, "bin"), 0o755); err != nil {
				t.Fatalf("mkdir bin: %v", err)
			}
			writeTestFile(t, filepath.Join(f.base, "bin", "relay-tail"), `#!/usr/bin/env bash
{
  echo "HTTPS_PROXY=${HTTPS_PROXY-__unset__}"
  echo "https_proxy=${https_proxy-__unset__}"
  echo "HTTP_PROXY=${HTTP_PROXY-__unset__}"
  echo "http_proxy=${http_proxy-__unset__}"
  echo "ALL_PROXY=${ALL_PROXY-__unset__}"
  echo "all_proxy=${all_proxy-__unset__}"
} > "$FAKE_ENV_OUT.tmp"
mv "$FAKE_ENV_OUT.tmp" "$FAKE_ENV_OUT"
exit 0
`, 0o755)
			// Unknown keys must be ignored, not executed; env must win over config.
			writeTestFile(t, filepath.Join(f.base, "relay-watch.env"), fmt.Sprintf(
				"RELAY_URL=http://relay.invalid\nRELAY_DAEMON_PROXY=%s\nAGENT_LOG_UPLOAD=0\nSCRIPT_AUTO_UPDATE=0\nEVIL=$(touch %s)\n",
				tc.mode, filepath.Join(f.root, "evil-executed")), 0o644)

			env := f.env(
				"HTTPS_PROXY="+hp, "https_proxy="+hpl,
				"HTTP_PROXY="+p, "http_proxy="+pl,
				"ALL_PROXY="+ap, "all_proxy="+apl,
				"FAKE_ENV_OUT="+envOut,
			)
			f.run(t, env)
			waitForFile(t, envOut, 3*time.Second)
			got := parseKeyValues(t, readOptional(t, envOut))
			for key, want := range tc.want {
				if got[key] != want {
					t.Fatalf("%s: %s = %q, want %q (all: %v)", tc.name, key, got[key], want, got)
				}
			}
			if _, err := os.Stat(filepath.Join(f.root, "evil-executed")); err == nil {
				t.Fatalf("config file executed an unknown key")
			}
		})
	}
}

func TestWatchScriptEnvOverridesConfig(t *testing.T) {
	f := newWatchFixture(t)
	if err := os.Remove(filepath.Join(f.base, "relay-tail.pid")); err != nil {
		t.Fatalf("remove pidfile: %v", err)
	}
	if err := os.Remove(filepath.Join(f.base, ".token")); err != nil {
		t.Fatalf("remove token: %v", err)
	}
	envOut := filepath.Join(f.root, "daemon-env.txt")
	if err := os.MkdirAll(filepath.Join(f.base, "bin"), 0o755); err != nil {
		t.Fatalf("mkdir bin: %v", err)
	}
	writeTestFile(t, filepath.Join(f.base, "bin", "relay-tail"), `#!/usr/bin/env bash
echo "HTTPS_PROXY=${HTTPS_PROXY-__unset__}" > "$FAKE_ENV_OUT.tmp"
mv "$FAKE_ENV_OUT.tmp" "$FAKE_ENV_OUT"
exit 0
`, 0o755)
	// Config says direct, environment says inherit: environment must win.
	writeTestFile(t, filepath.Join(f.base, "relay-watch.env"),
		"RELAY_URL=http://relay.invalid\nRELAY_DAEMON_PROXY=direct\nAGENT_LOG_UPLOAD=0\nSCRIPT_AUTO_UPDATE=0\n", 0o644)
	env := f.env("RELAY_DAEMON_PROXY=inherit", "HTTPS_PROXY=http://kept.invalid:3128", "FAKE_ENV_OUT="+envOut)
	f.run(t, env)
	waitForFile(t, envOut, 3*time.Second)
	got := parseKeyValues(t, readOptional(t, envOut))
	if got["HTTPS_PROXY"] != "http://kept.invalid:3128" {
		t.Fatalf("environment did not override config: %v", got)
	}
}

func TestWatchScriptTickLock(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("flock not available")
	}
	f := newWatchFixture(t)
	f.addSpoolTask(t, "locked", "must not drain")

	lockPath := filepath.Join(f.base, ".relay-watch.tick.lock")
	ready := filepath.Join(f.root, "lock-ready")
	holder := exec.Command("flock", "-x", lockPath, "-c", `touch "$READY"; exec sleep 10`)
	holder.Env = []string{"PATH=" + os.Getenv("PATH"), "READY=" + ready}
	if err := holder.Start(); err != nil {
		t.Fatalf("start lock holder: %v", err)
	}
	defer func() {
		_ = holder.Process.Kill()
		_, _ = holder.Process.Wait()
	}()
	waitForFile(t, ready, 3*time.Second)

	out := f.run(t, f.env("RELAY_URL=http://relay.invalid"))
	if out != "" {
		t.Fatalf("lock-contended tick should be silent, got:\n%s", out)
	}
	if got := readOptional(t, filepath.Join(f.base, ".spool_offset")); got != "0" {
		t.Fatalf("lock-contended tick advanced offset to %q", got)
	}
	if pending := readOptional(t, filepath.Join(f.base, "spool", "pending.log")); pending != "" {
		t.Fatalf("lock-contended tick wrote pending.log: %s", pending)
	}
	if log := readOptional(t, filepath.Join(f.base, "relay-watch.log")); strings.Contains(log, "script.mode=") {
		t.Fatalf("lock-contended tick logged a mode line:\n%s", log)
	}
}

func parseKeyValues(t *testing.T, raw string) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("bad key=value line %q", line)
		}
		out[key] = value
	}
	return out
}

func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func TestWatchScriptDaemonStartQuietAndCrashLoop(t *testing.T) {
	f := newWatchFixture(t)
	if err := os.Remove(filepath.Join(f.base, "relay-tail.pid")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(f.base, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Fake relay-tail exits immediately so each tick observes a dead daemon and restarts it.
	writeTestFile(t, filepath.Join(f.base, "bin", "relay-tail"), "#!/usr/bin/env bash\nexit 0\n", 0o755)
	// Fake ps reports a different comm so daemon_alive always treats the pid as dead/reused.
	writeTestFile(t, filepath.Join(f.fakebin, "ps"), "#!/bin/sh\necho not-relay-tail\n", 0o755)

	env := f.env("RELAY_URL=http://relay.invalid", "AGENT_LOG_UPLOAD=1", "AGENT_LOG_LEVEL=info")
	// Disable actual curl failure blocking or point curl to echo ok (already echoes {"ok":true}).
	// To inspect agent-log.jsonl before flush clears it (it only clears >1MB), run 1 tick first.
	f.run(t, env)
	agentLog := readOptional(t, filepath.Join(f.base, "spool", "agent-log.jsonl"))
	if strings.Contains(agentLog, "watchdog.daemon_started") {
		t.Fatalf("routine daemon_started should be debug-only and not enqueued at info:\n%s", agentLog)
	}
	// Two more rapid restarts -> 3 starts within 5m triggers warn watchdog.daemon_crash_loop.
	f.run(t, env)
	f.run(t, env)
	agentLog = readOptional(t, filepath.Join(f.base, "spool", "agent-log.jsonl"))
	if count := strings.Count(agentLog, "watchdog.daemon_crash_loop"); count != 1 {
		t.Fatalf("watchdog.daemon_crash_loop count = %d, want 1:\n%s", count, agentLog)
	}
}

func TestWatchScriptFailedWakeDedupesStatusAndLogs(t *testing.T) {
	f := newWatchFixture(t)
	f.addSpoolTask(t, "dup-task-1", "please do work")
	curlCalls := filepath.Join(f.root, "curl-calls.log")
	writeTestFile(t, filepath.Join(f.fakebin, "curl"), fmt.Sprintf(`#!/usr/bin/env bash
printf 'CALL:%%s\n' "$*" >> %q
printf '{"ok":true}'
`, curlCalls), 0o755)
	writeTestFile(t, filepath.Join(f.base, "relay-watch.local.sh"),
		"watch_wake() { return 1; }\nwatch_silent() { return 0; }\n", 0o644)

	env := f.env("RELAY_URL=http://relay.invalid", "AGENT_LOG_UPLOAD=1", "AGENT_LOG_LEVEL=info")
	f.run(t, env)
	f.run(t, env)

	callsRaw := readOptional(t, curlCalls)
	if n := strings.Count(callsRaw, "/messages"); n != 1 {
		t.Fatalf("status/started POST count = %d, want 1:\n%s", n, callsRaw)
	}
	pendingRaw := strings.TrimSpace(readOptional(t, filepath.Join(f.base, ".worker_pending")))
	if lines := strings.Split(pendingRaw, "\n"); len(lines) != 1 {
		t.Fatalf(".worker_pending lines = %d, want 1:\n%s", len(lines), pendingRaw)
	}
	agentLog := readOptional(t, filepath.Join(f.base, "spool", "agent-log.jsonl"))
	if n := strings.Count(agentLog, `"event":"wake.requested"`); n != 1 {
		t.Fatalf("wake.requested count = %d, want 1:\n%s", n, agentLog)
	}
	if n := strings.Count(agentLog, `"event":"wake.failed"`); n != 1 {
		t.Fatalf("wake.failed count = %d, want 1:\n%s", n, agentLog)
	}
}
