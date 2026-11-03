package gateway

import (
	"context"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/effects"
	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/work"
)

func TestAdminTicketsAndAttention(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	now := time.Now().Unix()
	_ = f.srv.st.TouchPeer("bob", now)

	c, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "t1", "to": "bob", "from": "alice", "kind": "task", "payload": "do the thing",
	}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("send task: %d %+v", c, out)
	}
	w := work.New(f.srv.st, 300)
	if err := w.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.SetWork(w)
	admin := f.adminLogin(t)

	c, out = f.doAuth(t, "GET", "/admin/tickets?assignee=bob", nil, admin)
	if c != 200 {
		t.Fatalf("tickets: %d %+v", c, out)
	}
	items, _ := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("tickets items: %+v", out)
	}
	first := items[0].(map[string]any)
	if first["stage"] != "queued" || first["assignee_online"] != true {
		t.Fatalf("ticket row: %+v", first)
	}

	// A status message advances the ticket without touching the protocol.
	c, out = f.do(t, "POST", "/messages", map[string]any{
		"id": "s1", "to": "alice", "from": "bob", "kind": "status", "status": "started",
		"in_reply_to": "t1", "payload": "started working now",
	}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("send status: %d %+v", c, out)
	}
	if err := w.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, out = f.doAuth(t, "GET", "/admin/tickets?assignee=bob", nil, admin)
	items, _ = out["items"].([]any)
	first = items[0].(map[string]any)
	if first["stage"] != "working" {
		t.Fatalf("working stage: %+v", first)
	}

	// A held task produces a person/approval attention item.
	c, out = f.do(t, "POST", "/messages", map[string]any{
		"id": "t2", "to": "bob", "from": "alice", "kind": "task",
		"payload": "needs a human first", "requires_approval": true,
	}, f.tok["alice"])
	if c != 202 {
		t.Fatalf("held task: %d %+v", c, out)
	}
	if err := w.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	c, out = f.doAuth(t, "GET", "/admin/attention", nil, admin)
	if c != 200 {
		t.Fatalf("attention: %d %+v", c, out)
	}
	counts, _ := out["counts"].(map[string]any)
	if counts["person/approval"] != float64(1) {
		t.Fatalf("attention counts: %+v", out)
	}
	if out["ready"] != true {
		t.Fatalf("ready: %+v", out)
	}

	c, out = f.doAuth(t, "GET", "/admin/tickets?stage=attention", nil, admin)
	if c != 200 || out["total"] != float64(1) {
		t.Fatalf("attention filter: %d %+v", c, out)
	}

	c, out = f.doAuth(t, "GET", "/admin/stats", nil, admin)
	if c != 200 || out["work_ready"] != true || out["open_tickets"] != float64(2) {
		t.Fatalf("stats work fields: %d %+v", c, out)
	}

	if c, _ := f.do(t, "GET", "/admin/tickets", nil, ""); c != 401 {
		t.Fatalf("unauthenticated tickets: %d", c)
	}
}

func TestAdminTicketsUnavailableWithoutWorkService(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	if c, _ := f.doAuth(t, "GET", "/admin/tickets", nil, admin); c != 503 {
		t.Fatalf("nil work service should 503, got %d", c)
	}
}

func TestAgentTicketsAndReassign(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	_ = f.srv.st.TouchPeer("bob", time.Now().Unix())
	c, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "t1", "to": "bob", "from": "alice", "kind": "task", "payload": "do it",
	}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("send: %d %+v", c, out)
	}
	w := work.New(f.srv.st, 300)
	if err := w.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.srv.SetWork(w)

	c, out = f.do(t, "GET", "/tickets", nil, f.tok["bob"])
	if c != 200 || out["total"] != float64(1) {
		t.Fatalf("bob tickets: %d %+v", c, out)
	}
	items, _ := out["items"].([]any)
	ticketID := int64(items[0].(map[string]any)["id"].(float64))

	c, out = f.do(t, "GET", "/tickets/"+itoa(ticketID), nil, f.tok["bob"])
	if c != 200 || out["ticket"].(map[string]any)["assignee"] != "bob" {
		t.Fatalf("bob ticket: %d %+v", c, out)
	}
	if c, _ := f.do(t, "GET", "/tickets/"+itoa(ticketID), nil, f.tok["alice"]); c != 403 {
		t.Fatalf("foreign ticket should 403, got %d", c)
	}
	if c, out := f.do(t, "POST", "/messages", map[string]any{
		"id": "ctx1", "to": "alice", "from": "bob", "kind": "chat", "payload": "context thread",
	}, f.tok["bob"]); c != 200 {
		t.Fatalf("context seed: %d %+v", c, out)
	}
	c, out = f.do(t, "POST", "/tickets/"+itoa(ticketID)+"/subscriptions",
		map[string]any{"thread_root": "bob/ctx1", "mode": "context"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("subscribe: %d %+v", c, out)
	}
	if subs, _ := out["ticket"].(map[string]any)["subscriptions"].([]any); len(subs) != 2 {
		t.Fatalf("subscriptions: %+v", out)
	}

	admin := f.adminLogin(t)
	c, out = f.doAuth(t, "POST", "/admin/tickets/"+itoa(ticketID)+"/reassign",
		map[string]any{"assignee": "alice", "lease_secs": 60}, admin)
	if c != 200 {
		t.Fatalf("reassign: %d %+v", c, out)
	}
	tk := out["ticket"].(map[string]any)
	if tk["assignee"] != "alice" || tk["assignment"].(map[string]any)["status"] != "pending" {
		t.Fatalf("reassign shape: %+v", tk)
	}
	if c, out := f.do(t, "GET", "/tickets", nil, f.tok["bob"]); c != 200 || out["total"] != float64(0) {
		t.Fatalf("bob should have no tickets after reassign: %d %+v", c, out)
	}
	if c, out := f.do(t, "GET", "/tickets", nil, f.tok["alice"]); c != 200 || out["total"] != float64(1) {
		t.Fatalf("alice should own the ticket: %d %+v", c, out)
	}
}

func TestAdminHealthAndPromptVersions(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/health", nil, admin)
	if c != 200 || out["policy_version"] == "" {
		t.Fatalf("health: %d %+v", c, out)
	}
	c, out = f.doAuth(t, "GET", "/admin/prompts/versions", nil, admin)
	if c != 200 || out["current"] == nil {
		t.Fatalf("prompt versions: %d %+v", c, out)
	}
}

func TestAdminHealthEffectsPerKindAndPromptDiffBase(t *testing.T) {
	f := newFixture(t)
	fx := effects.New(f.srv.st)
	f.srv.SetEffects(fx)
	now := time.Now().Unix()
	if err := f.srv.st.EnqueueEffect("webhook.offline", "due", "{}", now-10); err != nil {
		t.Fatal(err)
	}
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/health", nil, admin)
	if c != 200 {
		t.Fatalf("health: %d %+v", c, out)
	}
	eff, _ := out["effects"].(map[string]any)
	kinds, _ := eff["kinds"].(map[string]any)
	if eff["pending"] != float64(1) || eff["due"] != float64(1) || kinds["webhook.offline"] == nil {
		t.Fatalf("per-kind health: %+v", out)
	}

	if err := prompts.RecordTemplateRevisions(f.srv.st, now); err != nil {
		t.Fatal(err)
	}
	current := prompts.TemplateRevision("muse")
	if err := f.srv.st.UpsertPromptRevision("test-next-muse", "muse", current, "new prompt line\n", now+1); err != nil {
		t.Fatal(err)
	}
	c, out = f.doAuth(t, "GET", "/admin/prompts/diff?agent_type=muse&to=test-next-muse", nil, admin)
	if c != 200 || out["from"] != current || out["to"] != "test-next-muse" {
		t.Fatalf("prompt diff base: %d %+v", c, out)
	}
	if added, _ := out["added"].([]any); len(added) == 0 {
		t.Fatalf("prompt diff added: %+v", out)
	}
}
