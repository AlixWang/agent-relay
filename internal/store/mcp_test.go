package store

import (
	"testing"
)

func openMCPStore(t *testing.T) Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestMCPKeysAndPeerGrants(t *testing.T) {
	st := openMCPStore(t)
	key := &MCPKey{
		ID: "mk1", PrincipalType: "key", Name: "readonly", KeyPrefix: "rmk_abcdefgh",
		KeyHash: "hash-one", Scopes: []string{"mcp:adsense:read:*"}, Status: "active",
		CreatedAt: 1, ExpiresAt: 100,
	}
	if err := st.CreateMCPKey(key); err != nil {
		t.Fatalf("create key: %v", err)
	}
	got, err := st.GetMCPKeyByHash("hash-one")
	if err != nil || got == nil || got.Name != "readonly" || len(got.Scopes) != 1 {
		t.Fatalf("get by hash: %+v err=%v", got, err)
	}
	if err := st.UpdateMCPKeyUsage("mk1", 50, "10.0.0.1"); err != nil {
		t.Fatalf("usage: %v", err)
	}
	got, _ = st.GetMCPKey("mk1")
	if got.UseCount != 1 || got.LastUsedAt != 50 || got.LastUsedIP != "10.0.0.1" {
		t.Fatalf("usage not recorded: %+v", got)
	}
	if ok, _ := st.RevokeMCPKey("mk1", 60); !ok {
		t.Fatal("first revoke should win")
	}
	if ok, _ := st.RevokeMCPKey("mk1", 61); ok {
		t.Fatal("second revoke must be a no-op")
	}

	peer := &MCPKey{
		ID: "mp1", PrincipalType: "peer", PeerID: "alice", Name: "alice",
		Scopes: []string{"mcp:bing-webmaster:read:*"}, Status: "active", CreatedAt: 1,
	}
	saved, err := st.SaveMCPPeerGrant(peer)
	if err != nil || saved == nil || saved.PeerID != "alice" {
		t.Fatalf("save peer: %+v err=%v", saved, err)
	}
	peer.ID = "ignored-on-update"
	peer.Scopes = []string{"mcp:bing-webmaster:write:submit_url"}
	saved, err = st.SaveMCPPeerGrant(peer)
	if err != nil || saved.ID != "mp1" || len(saved.Scopes) != 1 {
		t.Fatalf("update peer: %+v err=%v", saved, err)
	}
	grants, err := st.ListMCPPeerGrants()
	if err != nil || len(grants) != 1 {
		t.Fatalf("list peer grants: %+v err=%v", grants, err)
	}
}

func TestMCPApprovalCAS(t *testing.T) {
	st := openMCPStore(t)
	a := &MCPApproval{
		ID: "ap1", Principal: "peer:alice", AuthKind: "relay_token", Endpoint: "/mcp/bing-webmaster",
		ServiceID: "bing-webmaster", Tool: "add_blocked_url", Risk: "high",
		ParamsJSON: `{"site":"https://x.test/"}`, ParamsHash: "h1", Status: "pending",
		RequestedAt: 1, ExpiresAt: 100,
	}
	if err := st.CreateMCPApproval(a); err != nil {
		t.Fatalf("create: %v", err)
	}
	if open, _ := st.GetOpenMCPApproval("peer:alice", "add_blocked_url", "h1"); open == nil || open.ID != "ap1" {
		t.Fatalf("open lookup: %+v", open)
	}
	if ok, _ := st.ApproveMCPApproval("ap1", 10, 20, "admin", "ok"); !ok {
		t.Fatal("approve should win")
	}
	if ok, _ := st.ApproveMCPApproval("ap1", 11, 21, "admin", "again"); ok {
		t.Fatal("second approve must fail CAS")
	}
	if ok, _ := st.ClaimMCPApprovalExecution("ap1", 15); !ok {
		t.Fatal("claim inside window should win")
	}
	if ok, _ := st.ClaimMCPApprovalExecution("ap1", 16); ok {
		t.Fatal("replay claim must fail")
	}
	if err := st.UpdateMCPApprovalOutcome("ap1", "upstream_error", "upstream_error", 503); err != nil {
		t.Fatalf("outcome: %v", err)
	}
	got, _ := st.GetMCPApproval("ap1")
	if got.Status != "executed" || got.Outcome != "upstream_error" || got.UpstreamStatus != 503 {
		t.Fatalf("terminal row: %+v", got)
	}

	pending := &MCPApproval{
		ID: "ap2", Principal: "key:k", AuthKind: "mcp_key", Endpoint: "/mcp", ServiceID: "bing-webmaster",
		Tool: "add_blocked_url", ParamsHash: "h2", Status: "pending", RequestedAt: 1, ExpiresAt: 5,
	}
	if err := st.CreateMCPApproval(pending); err != nil {
		t.Fatalf("create pending: %v", err)
	}
	expired, err := st.ExpireDueMCPApprovals(10)
	if err != nil || len(expired) != 1 || expired[0].ID != "ap2" {
		t.Fatalf("expire due: %+v err=%v", expired, err)
	}
	got, _ = st.GetMCPApproval("ap2")
	if got.Status != "expired" {
		t.Fatalf("pending not expired: %+v", got)
	}
	if n, _ := st.CountMCPApprovals("pending"); n != 0 {
		t.Fatalf("pending count = %d", n)
	}
}

func TestMCPAuditListPruneTrim(t *testing.T) {
	st := openMCPStore(t)
	entries := []*MCPAuditEntry{
		{Ts: 100, Actor: "peer:alice", ServiceID: "bing-webmaster", Tool: "query_stats", Risk: "read", Outcome: "ok"},
		{Ts: 200, Actor: "admin", ServiceID: "bing-webmaster", Tool: "add_blocked_url", Risk: "high", Outcome: "approved", ApprovalID: "ap1"},
		{Ts: 300, Actor: "peer:alice", ServiceID: "adsense", Tool: "list_accounts", Risk: "read", Outcome: "error", ErrorCode: "upstream_error"},
	}
	for _, e := range entries {
		if err := st.AppendMCPAudit(e); err != nil {
			t.Fatalf("append: %v", err)
		}
	}
	rows, total, err := st.ListMCPAudit(MCPAuditFilter{Actor: "peer:alice"}, 10, 0)
	if err != nil || total != 2 || len(rows) != 2 {
		t.Fatalf("filter actor: total=%d rows=%d err=%v", total, len(rows), err)
	}
	if rows[0].Ts != 300 {
		t.Fatalf("newest first expected, got %d", rows[0].Ts)
	}
	if n, err := st.PruneMCPAudit(1000, 150); err != nil || n != 2 {
		t.Fatalf("prune: n=%d err=%v", n, err)
	}
	// The write row at ts=200 survives the write cutoff 150.
	if _, total, _ := st.ListMCPAudit(MCPAuditFilter{}, 10, 0); total != 1 {
		t.Fatalf("after prune total=%d", total)
	}
	for i := 0; i < 5; i++ {
		_ = st.AppendMCPAudit(&MCPAuditEntry{Ts: int64(1000 + i), Actor: "a", Risk: "high", Outcome: "ok"})
	}
	if n, err := st.TrimMCPAudit(3); err != nil || n != 3 {
		t.Fatalf("trim: n=%d err=%v", n, err)
	}
}
