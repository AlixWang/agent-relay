package web

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestWatchScriptLayer0 exercises the Layer 0 decision on the exact embedded
// script: short mechanical results are acked by the script and dropped from
// the wake payload, while anything that might carry real content still wakes
// the worker. It runs the script against a fake BASE/PATH so no relay, token
// or worker is involved.
func TestWatchScriptLayer0(t *testing.T) {
	for _, tool := range []string{"bash", "jq"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not available", tool)
		}
	}

	cases := []struct {
		name      string
		tasks     []map[string]any
		ackOK     bool
		wantAcks  []string
		wantWakes []string
	}{
		{
			name:      "mechanical result is acked without waking",
			tasks:     []map[string]any{{"id": "m1", "from": "alice", "to": "muse-test", "kind": "result", "payload": "done"}},
			ackOK:     true,
			wantAcks:  []string{"m1"},
			wantWakes: nil,
		},
		{
			name: "mixed batch acks only the mechanical result",
			tasks: []map[string]any{
				{"id": "m1", "from": "alice", "to": "muse-test", "kind": "result", "payload": "done"},
				{"id": "t1", "from": "alice", "to": "muse-test", "kind": "task", "payload": "implement X"},
			},
			ackOK:     true,
			wantAcks:  []string{"m1"},
			wantWakes: []string{"t1"},
		},
		{
			name:      "guard blocks ok but failed",
			tasks:     []map[string]any{{"id": "m2", "from": "alice", "to": "muse-test", "kind": "result", "payload": "ok, but failed"}},
			ackOK:     true,
			wantAcks:  nil,
			wantWakes: []string{"m2"},
		},
		{
			name:      "real failure is not acked",
			tasks:     []map[string]any{{"id": "m3", "from": "alice", "to": "muse-test", "kind": "result", "payload": "ERROR: disk full"}},
			ackOK:     true,
			wantAcks:  nil,
			wantWakes: []string{"m3"},
		},
		{
			name:      "guard uses word boundaries",
			tasks:     []map[string]any{{"id": "m4", "from": "alice", "to": "muse-test", "kind": "result", "payload": "button works"}},
			ackOK:     true,
			wantAcks:  []string{"m4"},
			wantWakes: nil,
		},
		{
			name:      "ack failure falls back to waking",
			tasks:     []map[string]any{{"id": "m5", "from": "alice", "to": "muse-test", "kind": "result", "payload": "done"}},
			ackOK:     false,
			wantAcks:  []string{"m5"},
			wantWakes: []string{"m5"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			run := runLayer0Watch(t, tc.tasks, tc.ackOK)

			if got, want := len(run.acks), len(tc.wantAcks); got != want {
				t.Fatalf("ack calls = %d (%v), want %d (%v); stderr:\n%s", got, run.acks, want, tc.wantAcks, run.output)
			}
			for i, want := range tc.wantAcks {
				if got := run.acks[i]; got != want {
					t.Fatalf("ack[%d] = %q, want %q; all acks: %v", i, got, want, run.acks)
				}
			}

			pending := readOptional(t, filepath.Join(run.base, "spool", "pending.log"))
			experiment := readOptional(t, filepath.Join(run.base, "layer0-experiment.log"))
			done := readOptional(t, filepath.Join(run.base, ".done_ids"))

			for _, id := range tc.wantWakes {
				if !strings.Contains(pending, id) {
					t.Fatalf("worker was not woken for %s; pending.log:\n%s\nstderr:\n%s", id, pending, run.output)
				}
				if !strings.Contains(experiment, id+" worker-wake") {
					t.Fatalf("missing worker-wake line for %s; layer0-experiment.log:\n%s", id, experiment)
				}
			}
			for _, id := range tc.wantAcks {
				hasDone := false
				for _, line := range strings.Split(done, "\n") {
					if strings.TrimSpace(line) == id {
						hasDone = true
					}
				}
				if !tc.ackOK {
					if strings.Contains(experiment, id+" layer0-script-ack") {
						t.Fatalf("failed ack %s recorded as a script ack; layer0-experiment.log:\n%s", id, experiment)
					}
					if hasDone {
						t.Fatalf("failed ack id %s must not enter .done_ids:\n%s", id, done)
					}
					continue
				}
				if !strings.Contains(experiment, id+" layer0-script-ack") {
					t.Fatalf("missing layer0-script-ack line for %s; layer0-experiment.log:\n%s", id, experiment)
				}
				if !hasDone {
					t.Fatalf("acked id %s missing from .done_ids:\n%s", id, done)
				}
				if strings.Contains(pending, id) {
					t.Fatalf("mechanical id %s was also handed to the worker; pending.log:\n%s", id, pending)
				}
			}
		})
	}
}

type layer0Run struct {
	base   string
	acks   []string
	output string
}

func runLayer0Watch(t *testing.T, tasks []map[string]any, ackOK bool) layer0Run {
	t.Helper()

	root := t.TempDir()
	base := filepath.Join(root, "base")
	fakebin := filepath.Join(root, "fakebin")
	curlDir := filepath.Join(root, "curl")
	for _, dir := range []string{filepath.Join(base, "spool"), fakebin, curlDir} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	writeTestFile(t, filepath.Join(base, "identity"), "muse-test", 0o644)
	writeTestFile(t, filepath.Join(base, ".token"), "test-token", 0o600)
	writeTestFile(t, filepath.Join(base, "relay-tail.pid"), fmt.Sprintf("%d", os.Getpid()), 0o644)
	writeTestFile(t, filepath.Join(base, ".spool_offset"), "0", 0o644)

	var spool strings.Builder
	for _, task := range tasks {
		line, err := json.Marshal(map[string]any{"tasks": []map[string]any{task}})
		if err != nil {
			t.Fatalf("marshal task: %v", err)
		}
		spool.Write(line)
		spool.WriteByte('\n')
	}
	writeTestFile(t, filepath.Join(base, "spool", "wake.jsonl"), spool.String(), 0o644)

	// The watchdog only needs to see a live pid with comm=relay-tail; faking ps
	// is enough and keeps the test independent of process names.
	writeTestFile(t, filepath.Join(fakebin, "ps"), "#!/bin/sh\necho relay-tail\n", 0o755)

	ackResult := `{"ok":true}`
	if !ackOK {
		ackResult = `{"ok":false}`
	}
	curlScript := `#!/usr/bin/env bash
all="$*"
body=""
while [ "$#" -gt 0 ]; do
  case "$1" in
    -d) body="$2"; shift 2 ;;
    -d*) body="${1#-d}"; shift ;;
    *) shift ;;
  esac
done
if [ -n "$body" ] && printf '%s' "$all" | grep -q '/ack'; then
  printf '%s\n' "$body" >> "$FAKE_CURL_DIR/acks.jsonl"
  printf '%s' "$FAKE_ACK_RESULT"
  exit 0
fi
printf '{"ok":true}'
`
	writeTestFile(t, filepath.Join(fakebin, "curl"), curlScript, 0o755)

	watchPath := filepath.Join(root, "relay-watch.sh")
	writeTestFile(t, watchPath, WatchScript(), 0o755)

	cmd := exec.Command("bash", watchPath)
	cmd.Dir = root
	cmd.Env = append(os.Environ(),
		"BASE="+base,
		"RELAY_URL=http://relay.invalid",
		"AGENT_LOG_UPLOAD=0",
		"SCRIPT_AUTO_UPDATE=0",
		"FAKE_CURL_DIR="+curlDir,
		"FAKE_ACK_RESULT="+ackResult,
		"PATH="+fakebin+string(os.PathListSeparator)+os.Getenv("PATH"),
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("relay-watch.sh failed: %v\n%s", err, out)
	}

	run := layer0Run{base: base, output: string(out)}
	ackLog := readOptional(t, filepath.Join(curlDir, "acks.jsonl"))
	for _, line := range strings.Split(ackLog, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var body struct {
			MessageID string `json:"message_id"`
			By        string `json:"by"`
		}
		if err := json.Unmarshal([]byte(line), &body); err != nil {
			t.Fatalf("bad ack body %q: %v", line, err)
		}
		if body.By != "muse-test" {
			t.Fatalf("ack by = %q, want muse-test", body.By)
		}
		run.acks = append(run.acks, body.MessageID)
	}
	return run
}

func writeTestFile(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func readOptional(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}
