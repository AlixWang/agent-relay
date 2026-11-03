package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/config"
)

func TestAgentLogsIngestAndAdminQuery(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	token := f.tok["alice"]

	batch := map[string]any{
		"version": 1,
		"client":  map[string]any{"version": "v1", "rev": "r1"},
		"events": []any{
			map[string]any{
				"id": "e1", "level": "error", "source": "relay-watch", "event": "worker.timeout",
				"message": "worker timeout", "task_id": "op-1", "thread": "op/alice",
				"fields": map[string]any{"age_secs": 1800},
			},
			map[string]any{
				"id": "e2", "level": "info", "source": "relay-watch", "event": "wake.requested",
				"message": "wake", "task_id": "op-1", "thread": "op/alice",
			},
		},
	}
	c, out := f.do(t, "POST", "/logs", batch, token)
	if c != 200 || out["accepted"] != float64(2) {
		t.Fatalf("ingest: %d %+v", c, out)
	}
	// Same event ids are idempotent.
	c, out = f.do(t, "POST", "/logs", batch, token)
	if c != 200 || out["duplicates"] != float64(2) {
		t.Fatalf("dedupe: %d %+v", c, out)
	}
	// No auth must fail.
	if c, _ := f.do(t, "POST", "/logs", batch, ""); c != 401 {
		t.Fatalf("unauthenticated status = %d", c)
	}

	admin := f.adminLogin(t)
	c, out = f.doAuth(t, "GET", "/admin/logs?peer=alice&level=error&facets=1", nil, admin)
	if c != 200 {
		t.Fatalf("admin logs: %d %+v", c, out)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 || out["total"] != float64(1) {
		t.Fatalf("filtered entries: %+v", out)
	}
	first, _ := entries[0].(map[string]any)
	if first["event"] != "worker.timeout" || first["peer_id"] != "alice" {
		t.Fatalf("entry: %+v", first)
	}
	if _, ok := first["fields"].(map[string]any); !ok {
		t.Fatalf("fields not an object: %T %v", first["fields"], first["fields"])
	}
	facets, _ := out["facets"].(map[string]any)
	if facets["sources"] == nil || facets["levels"] == nil {
		t.Fatalf("facets: %+v", out)
	}
}

func TestAgentLogsRejectInvalidBatch(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	c, _ := f.do(t, "POST", "/logs", map[string]any{"events": []any{}}, f.tok["alice"])
	if c != 400 {
		t.Fatalf("empty batch status = %d", c)
	}
	c, _ = f.do(t, "POST", "/logs", map[string]any{"version": 99, "events": []any{
		map[string]any{"id": "e1", "level": "info", "source": "relay-watch", "event": "x"},
	}}, f.tok["alice"])
	if c != 400 {
		t.Fatalf("bad version status = %d", c)
	}
}

func TestAgentLogsPeerIDComesFromToken(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")
	c, out := f.do(t, "POST", "/logs", map[string]any{"events": []any{
		map[string]any{"id": "spoof", "level": "info", "source": "relay-watch", "event": "wake.requested", "peer_id": "bob"},
	}}, f.tok["alice"])
	if c != 200 || out["accepted"] != float64(1) {
		t.Fatalf("ingest: %d %+v", c, out)
	}
	admin := f.adminLogin(t)
	_, out = f.doAuth(t, "GET", "/admin/logs?peer=alice", nil, admin)
	if out["total"] != float64(1) {
		t.Fatalf("alice logs: %+v", out)
	}
	_, out = f.doAuth(t, "GET", "/admin/logs?peer=bob", nil, admin)
	if out["total"] != float64(0) {
		t.Fatalf("bob logs: %+v", out)
	}
}

func TestAgentLogsRejectIndividualInvalidEvents(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	c, out := f.do(t, "POST", "/logs", map[string]any{"events": []any{
		map[string]any{"id": "bad-level", "level": "verbose", "source": "relay-watch", "event": "wake.requested"},
		map[string]any{"id": "good", "level": "info", "source": "relay-watch", "event": "wake.requested"},
	}}, f.tok["alice"])
	if c != 200 || out["accepted"] != float64(1) || out["rejected"] != float64(1) {
		t.Fatalf("mixed batch: %d %+v", c, out)
	}
}

func TestAgentLogsRateLimit(t *testing.T) {
	f := newFixtureWithConfig(t, func(c *config.Config) { c.AgentLogRatePerMinute = 1 })
	f.registerPeer(t, "alice", "muse")
	batch := map[string]any{"events": []any{
		map[string]any{"id": "rate-1", "level": "info", "source": "relay-watch", "event": "wake.requested"},
	}}
	if c, out := f.do(t, "POST", "/logs", batch, f.tok["alice"]); c != 200 {
		t.Fatalf("first: %d %+v", c, out)
	}
	batch["events"] = []any{map[string]any{"id": "rate-2", "level": "info", "source": "relay-watch", "event": "wake.requested"}}
	if c, _ := f.do(t, "POST", "/logs", batch, f.tok["alice"]); c != 429 {
		t.Fatalf("second status = %d, want 429", c)
	}
}

func TestAgentLogsOversizeBatch(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	events := make([]any, 0, 100)
	for i := 0; i < 100; i++ {
		events = append(events, map[string]any{
			"id":    "big-" + string(rune('A'+i%26)) + "-" + string(rune('0'+i/26)),
			"level": "info", "source": "relay-watch", "event": "wake.requested",
			"fields": map[string]any{"pad": strings.Repeat("x", 3000)},
		})
	}
	c, _ := f.do(t, "POST", "/logs", map[string]any{"events": events}, f.tok["alice"])
	if c != 413 {
		t.Fatalf("oversize status = %d, want 413", c)
	}
}

func TestAgentLogsPaginationExportAndAuditSeparation(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	events := make([]any, 0, 3)
	for i := 0; i < 3; i++ {
		events = append(events, map[string]any{
			"id": "page-" + string(rune('a'+i)), "level": "info", "source": "relay-watch",
			"event": "wake.requested", "message": "m",
		})
	}
	if c, out := f.do(t, "POST", "/logs", map[string]any{"events": events}, f.tok["alice"]); c != 200 {
		t.Fatalf("ingest: %d %+v", c, out)
	}
	admin := f.adminLogin(t)
	_, out := f.doAuth(t, "GET", "/admin/logs?peer=alice&page=1&page_size=1", nil, admin)
	if out["total"] != float64(3) {
		t.Fatalf("total: %+v", out)
	}
	entries, _ := out["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("page len: %+v", entries)
	}
	req := httptest.NewRequest("GET", "/admin/logs/export?peer=alice", nil)
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("export status: %d %s", rec.Code, rec.Body.String())
	}
	if lines := strings.Count(strings.TrimSpace(rec.Body.String()), "\n") + 1; lines != 3 {
		t.Fatalf("export lines: %q", rec.Body.String())
	}
	audit, err := f.srv.st.ListAudit("", "", 0, 100)
	if err != nil {
		t.Fatalf("audit: %v", err)
	}
	for _, a := range audit {
		if strings.Contains(a.Action, "agent") {
			t.Fatalf("agent log leaked into audit: %+v", a)
		}
	}
}
