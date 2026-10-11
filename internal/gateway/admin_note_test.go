package gateway

import (
	"testing"
)

// AdminNote is operator-only (e.g. login email): writable via
// PATCH /admin/peers, readable on GET /admin/peers (and searchable via q),
// but never exposed on the agent-facing GET /peers.
func TestAdminPeerNoteIsolation(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "noted", "muse")

	if err := f.srv.st.UpdatePeerAdminNote("noted", "login@example.com"); err != nil {
		t.Fatal(err)
	}
	if p, _ := f.srv.st.GetPeer("noted"); p == nil || p.AdminNote != "login@example.com" {
		t.Fatalf("store round-trip: %+v", p)
	}

	c, _ := f.doAuth(t, "PATCH", "/admin/peers/noted", map[string]any{"admin_note": "login@example.com"}, admin)
	if c != 200 {
		t.Fatalf("patch: %d", c)
	}
	c, out := f.doAuth(t, "GET", "/admin/peers?q=login@example.com", nil, admin)
	if c != 200 {
		t.Fatalf("admin list: %d", c)
	}
	found := false
	for _, x := range out["peers"].([]any) {
		m := x.(map[string]any)
		if m["id"] == "noted" {
			found = true
			if m["admin_note"] != "login@example.com" {
				t.Fatalf("admin list missing note: %v", m)
			}
		}
	}
	if !found {
		t.Fatalf("admin search by note found nothing: %v", out)
	}

	c, out = f.do(t, "GET", "/peers", nil, f.tok["alice"])
	if c != 200 {
		t.Fatalf("peers: %d", c)
	}
	for _, x := range out["peers"].([]any) {
		m := x.(map[string]any)
		if _, ok := m["admin_note"]; ok {
			t.Fatalf("agent /peers leaked admin_note for %v", m["id"])
		}
	}

	c, _ = f.doAuth(t, "PATCH", "/admin/peers/noted", map[string]any{"admin_note": ""}, admin)
	if c != 200 {
		t.Fatalf("clear: %d", c)
	}
	if p, _ := f.srv.st.GetPeer("noted"); p.AdminNote != "" {
		t.Fatalf("clear failed: %q", p.AdminNote)
	}
}
