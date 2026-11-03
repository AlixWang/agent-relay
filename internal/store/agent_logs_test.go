package store

import (
	"testing"
)

func TestAgentLogsInsertDedupeAndQuery(t *testing.T) {
	st := openTest(t)
	events := []*AgentLog{
		{EventID: "e1", Ts: 100, ReceivedAt: 100, PeerID: "alice", Level: "error", Source: "relay-watch",
			Event: "worker.timeout", Message: "timeout", TaskID: "t1", Thread: "op/alice", Fields: `{"age":1800}`},
		{EventID: "e2", Ts: 101, ReceivedAt: 101, PeerID: "alice", Level: "info", Source: "relay-watch",
			Event: "wake.requested", Message: "wake", TaskID: "t1", Thread: "op/alice", Fields: `{}`},
		{EventID: "e3", Ts: 102, ReceivedAt: 102, PeerID: "bob", Level: "warn", Source: "relay-tail",
			Event: "receiver.stderr", Message: "backoff", Fields: `{"line":"retry"}`},
	}
	accepted, duplicates, err := st.InsertAgentLogs(events)
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if accepted != 3 || duplicates != 0 {
		t.Fatalf("insert counts: accepted=%d duplicates=%d", accepted, duplicates)
	}
	accepted, duplicates, err = st.InsertAgentLogs(events)
	if err != nil {
		t.Fatalf("duplicate insert: %v", err)
	}
	if accepted != 0 || duplicates != 3 {
		t.Fatalf("duplicate counts: accepted=%d duplicates=%d", accepted, duplicates)
	}

	logs, total, err := st.ListAgentLogsPage(AgentLogFilter{PeerID: "alice", Level: []string{"error"}, Event: "worker."}, 10, 0)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if total != 1 || len(logs) != 1 || logs[0].EventID != "e1" {
		t.Fatalf("filtered list: total=%d logs=%+v", total, logs)
	}
	if logs[0].Fields != `{"age":1800}` {
		t.Fatalf("fields lost: %q", logs[0].Fields)
	}

	sources, eventsList, levels, err := st.AgentLogFacets(AgentLogFilter{})
	if err != nil {
		t.Fatalf("facets: %v", err)
	}
	if len(sources) != 2 || len(eventsList) != 3 || levels["error"] != 1 || levels["info"] != 1 || levels["warn"] != 1 {
		t.Fatalf("facets: sources=%v events=%v levels=%v", sources, eventsList, levels)
	}

	stats, err := st.AgentLogStats(0, 200)
	if err != nil {
		t.Fatalf("stats: %v", err)
	}
	if stats.Errors != 1 || stats.Warnings != 1 || stats.Peers != 2 || stats.LastReceivedAt != 102 {
		t.Fatalf("stats: %+v", stats)
	}

	pruned, err := st.PruneAgentLogs(101)
	if err != nil || pruned != 1 {
		t.Fatalf("prune: n=%d err=%v", pruned, err)
	}
}

func TestTrimAgentLogs(t *testing.T) {
	st := openTest(t)
	for i := 0; i < 5; i++ {
		_, _, err := st.InsertAgentLogs([]*AgentLog{{
			EventID: "e" + itoaTest(i), Ts: int64(i), ReceivedAt: int64(i),
			PeerID: "alice", Level: "info", Source: "relay-watch", Event: "tick",
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	n, err := st.TrimAgentLogs("alice", 2)
	if err != nil {
		t.Fatalf("trim: %v", err)
	}
	if n != 3 {
		t.Fatalf("trim removed %d, want 3", n)
	}
	_, total, err := st.ListAgentLogsPage(AgentLogFilter{PeerID: "alice"}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("after trim total=%d err=%v", total, err)
	}
}

func itoaTest(i int) string {
	return string(rune('0' + i))
}

func TestAgentLogFacetsDoNotFilterTheirOwnDimension(t *testing.T) {
	st := openTest(t)
	events := []*AgentLog{
		{EventID: "f1", Ts: 1, ReceivedAt: 1, PeerID: "alice", Level: "error", Source: "relay-watch", Event: "worker.timeout"},
		{EventID: "f2", Ts: 2, ReceivedAt: 2, PeerID: "alice", Level: "info", Source: "relay-tail", Event: "receiver.stderr"},
	}
	if _, _, err := st.InsertAgentLogs(events); err != nil {
		t.Fatal(err)
	}
	sources, eventsList, levels, err := st.AgentLogFacets(AgentLogFilter{Source: []string{"relay-watch"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("source facet collapsed by source filter: %v", sources)
	}
	if len(eventsList) != 1 || eventsList[0] != "worker.timeout" {
		t.Fatalf("event facet: %v", eventsList)
	}
	if levels["error"] != 1 {
		t.Fatalf("level facet: %v", levels)
	}
}
