package agentlog

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func openLogStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func validEvent(id string) Event {
	return Event{
		ID: id, Level: "info", Source: "relay-watch", Event: "wake.requested",
		Message: "wake", TaskID: "t1", Thread: "op/alice",
		Fields: map[string]any{"count": float64(1)},
	}
}

func TestIngestValidAndSanitized(t *testing.T) {
	st := openLogStore(t)
	ing := New(st, 100, 0)
	b := Batch{Version: 1, Client: Client{Version: "v1", Rev: "r1"}, Events: []Event{
		validEvent("e1"),
		{ID: "e2", Level: "warning", Source: "relay-tail", Event: "receiver.stderr",
			Message: "retry",
			Fields:  map[string]any{"token": "secret", "nested": map[string]any{"Authorization": "x", "ok": "yes"}}},
	}}
	res, err := ing.Ingest("alice", b, 1000)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Accepted != 2 || res.Rejected != 0 {
		t.Fatalf("result: %+v", res)
	}
	logs, total, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("list: total=%d err=%v", total, err)
	}
	if logs[0].Level != "warn" {
		t.Fatalf("warning not normalized: %q", logs[0].Level)
	}
	var fields map[string]any
	if err := json.Unmarshal([]byte(logs[0].Fields), &fields); err != nil {
		t.Fatal(err)
	}
	if _, ok := fields["token"]; ok {
		t.Fatalf("token key leaked: %v", fields)
	}
	nested, _ := fields["nested"].(map[string]any)
	if nested == nil {
		t.Fatalf("nested fields lost: %v", fields)
	}
	if _, ok := nested["Authorization"]; ok {
		t.Fatalf("Authorization key leaked: %v", nested)
	}
	if nested["ok"] != "yes" {
		t.Fatalf("safe nested field lost: %v", nested)
	}
}

func TestIngestRejectsInvalidIndividualEvents(t *testing.T) {
	st := openLogStore(t)
	ing := New(st, 100, 0)
	bad := validEvent("bad id with spaces")
	b := Batch{Events: []Event{validEvent("e1"), bad}}
	res, err := ing.Ingest("alice", b, 1000)
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if res.Accepted != 1 || res.Rejected != 1 {
		t.Fatalf("result: %+v", res)
	}
}

func TestIngestRateLimit(t *testing.T) {
	st := openLogStore(t)
	ing := New(st, 2, 0)
	b := Batch{Events: []Event{validEvent("e1"), validEvent("e2")}}
	if _, err := ing.Ingest("alice", b, 1000); err != nil {
		t.Fatalf("first: %v", err)
	}
	_, err := ing.Ingest("alice", Batch{Events: []Event{validEvent("e3")}}, 1001)
	var re *RateError
	if !errors.As(err, &re) {
		t.Fatalf("expected RateError, got %v", err)
	}
	if re.Retry < 1 {
		t.Fatalf("retry: %d", re.Retry)
	}
}

func TestIngestStructuralValidation(t *testing.T) {
	ing := New(openLogStore(t), 100, 0)
	if _, err := ing.Ingest("alice", Batch{Version: 2, Events: []Event{validEvent("e1")}}, 1000); err == nil {
		t.Fatal("expected version error")
	}
	if _, err := ing.Ingest("alice", Batch{Events: nil}, 1000); err == nil {
		t.Fatal("expected empty batch error")
	}
}

func TestIngestCorrectsClientClock(t *testing.T) {
	st := openLogStore(t)
	ing := New(st, 100, 0)
	ev := validEvent("clock")
	ev.Ts = 0
	if _, err := ing.Ingest("alice", Batch{Events: []Event{ev}}, 1000); err != nil {
		t.Fatal(err)
	}
	logs, _, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0)
	if err != nil || len(logs) != 1 {
		t.Fatalf("list: %v %+v", err, logs)
	}
	if logs[0].Ts != 1000 {
		t.Fatalf("ts = %d, want 1000", logs[0].Ts)
	}
}

func TestIngestFoldsNoisyDaemonStarted(t *testing.T) {
	st := openLogStore(t)
	ing := New(st, 100, 0)
	ev := func(id, level, event string) Event {
		return Event{ID: id, Level: level, Source: "relay-watch", Event: event}
	}

	res, err := ing.Ingest("alice", Batch{Events: []Event{
		ev("d1", "info", "watchdog.daemon_started"),
		ev("d2", "info", "watchdog.daemon_started"),
		ev("w1", "warn", "watchdog.daemon_crash_loop"),
	}}, 1000)
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted != 2 || res.Duplicates != 1 {
		t.Fatalf("first batch: %+v", res)
	}

	// Second batch within 1h containing only daemon_started folds cleanly without DB write.
	res2, err := ing.Ingest("alice", Batch{Events: []Event{ev("d3", "info", "watchdog.daemon_started")}}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if res2.Accepted != 0 || res2.Duplicates != 1 {
		t.Fatalf("folded batch: %+v", res2)
	}

	// After 1h window, one more daemon_started is accepted.
	res3, err := ing.Ingest("alice", Batch{Events: []Event{ev("d4", "info", "watchdog.daemon_started")}}, 1000+3601)
	if err != nil {
		t.Fatal(err)
	}
	if res3.Accepted != 1 || res3.Duplicates != 0 {
		t.Fatalf("post-window batch: %+v", res3)
	}

	_, total, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 20, 0)
	if err != nil || total != 3 {
		t.Fatalf("stored total=%d err=%v, want 3", total, err)
	}
}
