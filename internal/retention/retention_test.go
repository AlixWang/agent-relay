package retention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func openTest(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestArchiveOldFullyAcked(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	old := now - 31*86400

	for _, id := range []string{"a", "b"} {
		if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: old}); err != nil {
			t.Fatal(err)
		}
		if err := st.TouchPeer(id, now); err != nil {
			t.Fatal(err)
		}
	}
	// Old direct message, acked → archived.
	s1, _ := st.InsertMessage(&store.Message{ID: "d1", Sender: "a", Recipient: "b",
		RootID: "a/d1", Payload: "old", CreatedAt: old})
	_ = st.Ack(s1, "b", old+1)
	// Old direct message, NOT acked → kept.
	_, _ = st.InsertMessage(&store.Message{ID: "d2", Sender: "a", Recipient: "b",
		RootID: "a/d2", Payload: "unacked", CreatedAt: old})
	// Fresh message, acked → kept (TTL).
	s3, _ := st.InsertMessage(&store.Message{ID: "d3", Sender: "a", Recipient: "b",
		RootID: "a/d3", Payload: "fresh", CreatedAt: now - 86400})
	_ = st.Ack(s3, "b", now)
	// Old held message → kept even if acked (never delivered, never ackable via API).
	s4, _ := st.InsertMessage(&store.Message{ID: "h1", Sender: "a", Recipient: "b",
		RootID: "a/h1", Payload: "held", CreatedAt: old, ApprovalState: "pending"})
	_ = st.Ack(s4, "b", old+1)

	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats["messages_archived"] != 1 || stats["messages_deleted"] != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	// Archive file landed with the right month + content.
	entries, _ := os.ReadDir(filepath.Join(dir, "archive"))
	if len(entries) != 1 {
		t.Fatalf("archive files: %v", entries)
	}
	data, _ := os.ReadFile(filepath.Join(dir, "archive", entries[0].Name()))
	var m store.Message
	if err := json.Unmarshal(data, &m); err != nil || m.ID != "d1" {
		t.Fatalf("archive content: %s (%v)", data, err)
	}
	// DB state: d1 gone, rest kept.
	if got, _ := st.GetBySenderID("a", "d1"); got != nil {
		t.Fatal("d1 not deleted")
	}
	for _, id := range []string{"d2", "d3", "h1"} {
		if got, _ := st.GetBySenderID("a", id); got == nil {
			t.Fatalf("%s wrongly deleted", id)
		}
	}
	audits, _ := st.ListAudit("system", "retention.archived", 0, 10)
	if len(audits) != 1 {
		t.Fatal("missing retention.archived audit")
	}
}

func TestBroadcastNeedsEveryActiveAck(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	old := now - 31*86400
	for _, id := range []string{"a", "b", "c"} {
		_ = st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: old})
		_ = st.TouchPeer(id, now)
	}
	seq, _ := st.InsertMessage(&store.Message{ID: "b1", Sender: "a", Recipient: "*",
		RootID: "a/b1", Payload: "all", CreatedAt: old})
	_ = st.Ack(seq, "b", old+1) // c hasn't acked
	stats, _ := r.RunOnce(now)
	if stats["messages_archived"] != 0 {
		t.Fatalf("partial broadcast archived: %+v", stats)
	}
	_ = st.Ack(seq, "c", old+2)
	stats, _ = r.RunOnce(now)
	if stats["messages_archived"] != 1 {
		t.Fatalf("full broadcast not archived: %+v", stats)
	}
}

func TestPeerAndAuditPrune(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{PeerPruneAfterDays: 7, AuditRetentionDays: 90, DataDir: dir})
	now := int64(200 * 86400)
	_ = st.CreatePeer(&store.Peer{ID: "stale", Status: "active", CreatedAt: 1, LastSeen: now - 8*86400})
	_ = st.CreatePeer(&store.Peer{ID: "fresh", Status: "active", CreatedAt: 1, LastSeen: now - 86400})
	_, _ = st.CreateToken("stale", "h1", "t", 1) // tokens survive peer prune (DESIGN §10.2)
	_ = st.AppendAudit("x", "old.evt", "", now-91*86400)
	_ = st.AppendAudit("x", "new.evt", "", now-86400)

	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["peers_pruned"] != 1 || stats["audit_pruned"] != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if p, _ := st.GetPeer("stale"); p != nil {
		t.Fatal("stale kept")
	}
	if toks, _ := st.ListTokens(); len(toks) != 1 {
		t.Fatalf("token wrongly pruned: %d", len(toks))
	}
	entries, _ := st.ListAudit("", "", 0, 10)
	if len(entries) != 1 || entries[0].Action != "new.evt" {
		t.Fatalf("audit: %+v", entries)
	}
}

func TestRejectedHoldArchivedAfterTTL(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	old := now - 31*86400
	for _, id := range []string{"a", "b"} {
		_ = st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: old})
		_ = st.TouchPeer(id, now)
	}
	// Rejected hold, never acked: terminal, archived after TTL.
	_, _ = st.InsertMessage(&store.Message{ID: "rej", Sender: "a", Recipient: "b",
		RootID: "a/rej", Payload: "no", CreatedAt: old, ApprovalState: "rejected"})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["messages_archived"] != 1 {
		t.Fatalf("rejected not archived: %+v", stats)
	}
	if got, _ := st.GetBySenderID("a", "rej"); got != nil {
		t.Fatal("rejected not deleted")
	}
}

func TestDisabledWindowsSkip(t *testing.T) {
	st := openTest(t)
	r := New(st, Policy{DataDir: t.TempDir()}) // all TTLs zero
	stats, err := r.RunOnce(99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 0 {
		t.Fatalf("disabled should no-op: %+v", stats)
	}
}
