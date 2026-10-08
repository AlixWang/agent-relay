package store

import (
	"testing"
)

func openTest(t *testing.T) Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func mustPeer(t *testing.T, st Store, id string) {
	t.Helper()
	if err := st.CreatePeer(&Peer{ID: id, Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatalf("peer %s: %v", id, err)
	}
}

func mustMsg(t *testing.T, st Store, m *Message) int64 {
	t.Helper()
	seq, err := st.InsertMessage(m)
	if err != nil {
		t.Fatalf("insert %+v: %v", m, err)
	}
	return seq
}

func TestVisibleToDirectAndBroadcast(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	mustMsg(t, st, &Message{ID: "d1", Sender: "a", Recipient: "b", Kind: "task", RootID: "a/d1", Payload: "hi", CreatedAt: 1001})
	mustMsg(t, st, &Message{ID: "b1", Sender: "a", Recipient: "*", Kind: "chat", RootID: "a/b1", Payload: "all", CreatedAt: 1002})

	// Direct visible to recipient only.
	vis, err := st.VisibleTo("b", 0, 50)
	if err != nil || len(vis) != 2 {
		t.Fatalf("b should see 2, got %d (%v)", len(vis), err)
	}
	vis, _ = st.VisibleTo("a", 0, 50)
	if len(vis) != 0 {
		t.Fatalf("sender should see 0, got %d", len(vis))
	}
	// Pending approval excluded.
	mustMsg(t, st, &Message{ID: "h1", Sender: "a", Recipient: "b", RootID: "a/h1",
		Payload: "held", CreatedAt: 1003, ApprovalState: "pending"})
	vis, _ = st.VisibleTo("b", 0, 50)
	if len(vis) != 2 {
		t.Fatalf("held must be hidden, got %d", len(vis))
	}
	// Ack hides for that peer only.
	if err := st.Ack(vis[0].Seq, "b", 1004); err != nil {
		t.Fatal(err)
	}
	vis, _ = st.VisibleTo("b", 0, 50)
	if len(vis) != 1 {
		t.Fatalf("after ack b should see 1, got %d", len(vis))
	}
	// since cursor.
	vis, _ = st.VisibleTo("b", 1<<62, 50)
	if len(vis) != 0 {
		t.Fatalf("since cursor broken, got %d", len(vis))
	}
}

func TestUniqueSenderID(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustMsg(t, st, &Message{ID: "x", Sender: "a", Recipient: "b", RootID: "a/x", Payload: "1", CreatedAt: 1})
	if _, err := st.InsertMessage(&Message{ID: "x", Sender: "a", Recipient: "b", RootID: "a/x", Payload: "2", CreatedAt: 2}); err == nil {
		t.Fatal("expected UNIQUE violation")
	}
	// Same id from a different sender is fine.
	mustMsg(t, st, &Message{ID: "x", Sender: "b", Recipient: "a", RootID: "b/x", Payload: "3", CreatedAt: 3})
}

func TestThreadAndFuseWatermark(t *testing.T) {
	st := openTest(t)
	for i := int64(0); i < 3; i++ {
		mustMsg(t, st, &Message{ID: string(rune('a' + i)), Sender: "a", Recipient: "b",
			RootID: "r1", Payload: "p", CreatedAt: 100 + i})
	}
	msgs, err := st.ThreadMessages("r1", 10)
	if err != nil || len(msgs) != 3 {
		t.Fatalf("thread: %d (%v)", len(msgs), err)
	}
	if msgs[0].Seq > msgs[1].Seq {
		t.Fatal("thread must be ascending")
	}
	last, err := st.LastNInThread("r1", 2)
	if err != nil || len(last) != 2 || last[0].Seq > last[1].Seq {
		t.Fatalf("lastN order: %+v (%v)", last, err)
	}
	n, err := st.CountThreadSince("r1", 0)
	if err != nil || n != 3 {
		t.Fatalf("count: %d (%v)", n, err)
	}
	// Watermark round-trip.
	if wm, _ := st.FuseWatermark("r1"); wm != 0 {
		t.Fatalf("fresh watermark: %d", wm)
	}
	if err := st.SetFuseWatermark("r1", msgs[1].Seq); err != nil {
		t.Fatal(err)
	}
	if wm, _ := st.FuseWatermark("r1"); wm != msgs[1].Seq {
		t.Fatalf("watermark: %d", wm)
	}
	if err := st.ClearFuseWatermark("r1"); err != nil {
		t.Fatal(err)
	}
	if wm, _ := st.FuseWatermark("r1"); wm != 0 {
		t.Fatalf("cleared watermark: %d", wm)
	}
	ts, err := st.ThreadOldestTs("r1")
	if err != nil || ts != 100 {
		t.Fatalf("oldest: %d (%v)", ts, err)
	}
	if ts, _ := st.ThreadOldestTs("nope"); ts != 0 {
		t.Fatalf("missing thread oldest: %d", ts)
	}
}

func TestSearchEscapesLike(t *testing.T) {
	st := openTest(t)
	mustMsg(t, st, &Message{ID: "m1", Sender: "a", Recipient: "b", RootID: "r", Payload: "100% real_task", CreatedAt: 1})
	mustMsg(t, st, &Message{ID: "m2", Sender: "a", Recipient: "b", RootID: "r", Payload: "100X realZtask", CreatedAt: 2})
	got, err := st.SearchMessages("100% real_task", 10)
	if err != nil || len(got) != 1 || got[0].ID != "m1" {
		t.Fatalf("LIKE escaping broken: %+v (%v)", got, err)
	}
}

func TestTokenLifecycle(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	id, err := st.CreateToken("a", "hash1", "initial", 100)
	if err != nil || id == 0 {
		t.Fatalf("create token: %v", err)
	}
	p, tok, err := st.FindPeerByTokenHash("hash1")
	if err != nil || p == nil || tok == nil || p.ID != "a" {
		t.Fatalf("find: %+v %+v (%v)", p, tok, err)
	}
	if err := st.TouchToken(id, 200, "100.1.2.3"); err != nil {
		t.Fatal(err)
	}
	toks, _ := st.ListTokensByPeer("a")
	if len(toks) != 1 || toks[0].LastUsedAt != 200 || toks[0].LastIP != "100.1.2.3" {
		t.Fatalf("touch: %+v", toks)
	}
	if err := st.RevokeToken(id, 300); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := st.FindPeerByTokenHash("hash1"); p != nil {
		t.Fatal("revoked token still resolves")
	}
	// Suspended peer: token stops resolving.
	id2, _ := st.CreateToken("a", "hash2", "second", 400)
	_ = id2
	if err := st.UpdatePeerStatus("a", "suspended"); err != nil {
		t.Fatal(err)
	}
	if p, _, _ := st.FindPeerByTokenHash("hash2"); p != nil {
		t.Fatal("suspended peer still resolves")
	}
}

func TestInviteAndSmokeAndAudit(t *testing.T) {
	st := openTest(t)
	if err := st.CreateInvite(&Invite{CodeHash: "h", IntendedID: "n1", AgentType: "muse", CreatedAt: 1, ExpiresAt: 9999}); err != nil {
		t.Fatal(err)
	}
	inv, err := st.GetInvite("h")
	if err != nil || inv == nil || inv.IntendedID != "n1" {
		t.Fatalf("invite: %+v (%v)", inv, err)
	}
	if err := st.MarkInviteUsed("h", "n1", 5); err != nil {
		t.Fatal(err)
	}
	inv, _ = st.GetInvite("h")
	if inv.UsedBy != "n1" || inv.UsedAt != 5 {
		t.Fatalf("used: %+v", inv)
	}
	if err := st.SetSmoke("n1", "smoke-1", 10); err != nil {
		t.Fatal(err)
	}
	if id, ts, _ := st.GetSmoke("n1"); id != "smoke-1" || ts != 10 {
		t.Fatalf("smoke: %s %d", id, ts)
	}
	if err := st.ClearSmoke("n1"); err != nil {
		t.Fatal(err)
	}
	if id, _, _ := st.GetSmoke("n1"); id != "" {
		t.Fatal("smoke not cleared")
	}
	if err := st.AppendAudit("admin", "test.evt", "d", 20); err != nil {
		t.Fatal(err)
	}
	entries, err := st.ListAudit("", "", 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != "test.evt" {
		t.Fatalf("audit: %+v (%v)", entries, err)
	}
	n, err := st.PruneAudit(21)
	if err != nil || n != 1 {
		t.Fatalf("prune audit: %d (%v)", n, err)
	}
	if size, err := st.DBSize(); err != nil || size <= 0 {
		t.Fatalf("dbsize: %d (%v)", size, err)
	}
	if err := st.Vacuum(); err != nil {
		t.Fatalf("vacuum: %v", err)
	}
}

func TestPeerPruneKeepsNeverSeen(t *testing.T) {
	st := openTest(t)
	now := int64(10 * 86400) // day 10: stale (last_seen=10s) is past the 7-day window
	mustPeer(t, st, "fresh") // last_seen=0
	if err := st.CreatePeer(&Peer{ID: "stale", Status: "active", CreatedAt: 1, LastSeen: 10}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PrunePeers(now - 7*86400)
	if err != nil || n != 1 {
		t.Fatalf("prune: %d (%v)", n, err)
	}
	if p, _ := st.GetPeer("fresh"); p == nil {
		t.Fatal("never-seen peer pruned")
	}
	if p, _ := st.GetPeer("stale"); p != nil {
		t.Fatal("stale peer kept")
	}
}

func TestPermissionRequestCAS(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	now := int64(1000)
	mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "work", CreatedAt: now})
	// New fields round-trip through insert + read.
	mustMsg(t, st, &Message{ID: "pr1", Sender: "b", Recipient: "a", Kind: "permission_request",
		InReplyTo: "t1", RootID: "a/t1", Payload: "need approval", CreatedAt: now,
		Op: "shell.exec", Target: "/tmp/x", Detail: "list it", ExpiresAt: now + 600})
	got, err := st.GetBySenderID("b", "pr1")
	if err != nil || got == nil || got.Op != "shell.exec" || got.Target != "/tmp/x" ||
		got.Detail != "list it" || got.ExpiresAt != now+600 {
		t.Fatalf("fields lost: %+v %v", got, err)
	}
	// Permission row create + read + count.
	if err := st.CreatePermissionRequest(&PermissionRequest{
		RequestID: "pr1", Thread: "a/t1", Requester: "b", Approver: "a",
		Status: "pending", Op: "shell.exec", Target: "/tmp/x", Detail: "list it",
		CreatedAt: now, ExpiresAt: now + 600,
	}); err != nil {
		t.Fatalf("create perm: %v", err)
	}
	if n, _ := st.CountOpenPermissions("a/t1", now); n != 1 {
		t.Fatalf("open count: %d", n)
	}
	// CAS win: decision message inserted, row flipped.
	dec := &Message{ID: "pd1", Sender: "a", Recipient: "b", Kind: "permission_decision",
		InReplyTo: "pr1", RootID: "a/t1", Payload: "ok once", CreatedAt: now, Decision: "allow"}
	won, err := st.DecidePermission(dec, "pr1", "allow", now+5)
	if err != nil || !won {
		t.Fatalf("CAS win: %v %v", won, err)
	}
	pr, _ := st.GetPermissionRequest("pr1")
	if pr.Status != "allowed" || pr.DecidedAt != now+5 || pr.DecisionID != "pd1" {
		t.Fatalf("row after CAS: %+v", pr)
	}
	// CAS loss on a decided row: no insert... (insert of the losing
	// decision still happens inside DecidePermission — that path is only
	// reached from queue.sendDecision which re-checks first; here assert
	// the row itself does not flip).
	dec2 := &Message{ID: "pd2", Sender: "a", Recipient: "b", Kind: "permission_decision",
		InReplyTo: "pr1", RootID: "a/t1", Payload: "changed mind", CreatedAt: now + 6, Decision: "deny"}
	won, err = st.DecidePermission(dec2, "pr1", "deny", now+6)
	if err != nil || won {
		t.Fatalf("CAS loss: %v %v", won, err)
	}
	pr, _ = st.GetPermissionRequest("pr1")
	if pr.Status != "allowed" {
		t.Fatalf("row flipped on loss: %+v", pr)
	}
	// Expired pending row: MarkPermissionExpired flips it.
	if err := st.CreatePermissionRequest(&PermissionRequest{
		RequestID: "pr-old", Thread: "a/t1", Requester: "b", Approver: "a",
		Status: "pending", CreatedAt: now - 900, ExpiresAt: now - 100,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkPermissionExpired("pr-old", now); err != nil {
		t.Fatal(err)
	}
	if pr, _ := st.GetPermissionRequest("pr-old"); pr.Status != "expired" {
		t.Fatalf("not expired: %+v", pr)
	}
	if n, _ := st.CountOpenPermissions("a/t1", now); n != 0 {
		t.Fatalf("expired still open: %d", n)
	}
	// Prune drops decided rows past TTL, keeps pending and unexpired.
	if n, err := st.PrunePermissions(now); err != nil || n != 1 {
		t.Fatalf("prune old: %d %v", n, err)
	}
	if n, err := st.PrunePermissions(now + 7200); err != nil || n != 1 {
		t.Fatalf("prune later: %d %v", n, err)
	}
}

func TestStoreAdminPages(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "alice")
	mustPeer(t, st, "bob")

	// Create tokens
	if _, err := st.CreateToken("alice", "hash1", "tok1", 100); err != nil {
		t.Fatal(err)
	}
	tok2ID, err := st.CreateToken("bob", "hash2", "tok2", 200)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RevokeToken(tok2ID, 300); err != nil {
		t.Fatal(err)
	}

	// Create messages across threads
	mustMsg(t, st, &Message{ID: "m1", Sender: "alice", Recipient: "bob", Kind: "task",
		RootID: "alice/t1", Payload: "do homework", CreatedAt: 1000})
	mustMsg(t, st, &Message{ID: "m2", Sender: "bob", Recipient: "alice", Kind: "progress",
		RootID: "alice/t1", Payload: "working on it", CreatedAt: 1010})
	mustMsg(t, st, &Message{ID: "m3", Sender: "alice", Recipient: "bob", Kind: "task",
		RootID: "alice/t2", Payload: "research something", CreatedAt: 1020,
		RequiresApproval: true, ApprovalState: "pending"})

	// Audit logs
	_ = st.AppendAudit("admin", "peer.status", "peer=alice status=active", 1000)
	_ = st.AppendAudit("system", "verify.passed", "peer=alice", 1010)

	// Test AdminCounts
	counts, err := st.AdminCounts()
	if err != nil {
		t.Fatalf("AdminCounts: %v", err)
	}
	if counts.Threads != 2 {
		t.Errorf("threads count: got %d, want 2", counts.Threads)
	}
	if counts.PendingApprovals != 1 {
		t.Errorf("pending approvals: got %d, want 1", counts.PendingApprovals)
	}
	if counts.TokensActive != 1 {
		t.Errorf("active tokens: got %d, want 1", counts.TokensActive)
	}
	if counts.AuditTotal != 2 {
		t.Errorf("audit total: got %d, want 2", counts.AuditTotal)
	}

	// Test ThreadSummaries
	sums, total, err := st.ThreadSummaries(ThreadFilter{}, 10, 0)
	if err != nil || total != 2 || len(sums) != 2 {
		t.Fatalf("ThreadSummaries: %v, total=%d, len=%d", err, total, len(sums))
	}
	if sums[0].RootID != "alice/t2" {
		t.Errorf("expected latest thread first: got %s, want alice/t2", sums[0].RootID)
	}
	if sums[0].Held != 1 {
		t.Errorf("expected held=1 on t2, got %d", sums[0].Held)
	}

	// Test ThreadSummaries with Held filter
	heldSums, heldTotal, err := st.ThreadSummaries(ThreadFilter{Held: true}, 10, 0)
	if err != nil || heldTotal != 1 || len(heldSums) != 1 || heldSums[0].RootID != "alice/t2" {
		t.Fatalf("ThreadSummaries held filter failed: %v, total=%d", err, heldTotal)
	}

	// Test SearchMessagesPage
	searchResults, searchTotal, err := st.SearchMessagesPage("homework", 10, 0)
	if err != nil || searchTotal != 1 || len(searchResults) != 1 {
		t.Fatalf("SearchMessagesPage: %v, total=%d", err, searchTotal)
	}
	if searchResults[0].Payload != "do homework" {
		t.Errorf("search result payload mismatch: %s", searchResults[0].Payload)
	}

	// Test ListAuditPage
	auditEntries, auditTotal, err := st.ListAuditPage(AuditFilter{}, 10, 0)
	if err != nil || auditTotal != 2 || len(auditEntries) != 2 {
		t.Fatalf("ListAuditPage: %v, total=%d", err, auditTotal)
	}
	if auditEntries[0].Action != "verify.passed" { // newest first
		t.Errorf("expected newest audit first: got %s", auditEntries[0].Action)
	}

	// Test AuditFacets
	actors, actions, err := st.AuditFacets()
	if err != nil || len(actors) != 2 || len(actions) != 2 {
		t.Fatalf("AuditFacets: %v, actors=%v, actions=%v", err, actors, actions)
	}
}

// The receiver source revision is the update predicate (§8.9) and must
// round-trip like any other peer column.
func TestPeerClientRevRoundTrip(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&Peer{ID: "p1", DisplayName: "p1", AgentType: "muse", Status: "active", CreatedAt: 1, LastSeen: 1}); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPeer("p1"); p == nil || p.ClientRev != "" {
		t.Fatalf("fresh peer should have no rev: %+v", p)
	}
	if err := st.UpdatePeerClientRev("p1", "abc123def456", 42); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetPeer("p1")
	if err != nil || p.ClientRev != "abc123def456" || p.ClientUpdatedAt != 42 {
		t.Fatalf("rev not stored: %+v err=%v", p, err)
	}
	all, err := st.ListPeers()
	if err != nil || len(all) != 1 || all[0].ClientRev != "abc123def456" {
		t.Fatalf("rev not listed: %+v err=%v", all, err)
	}
}
