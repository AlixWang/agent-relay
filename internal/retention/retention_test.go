package retention

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
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

func TestReducedDefaultWindows(t *testing.T) {
	st := openTest(t)
	now := int64(200 * 86400)
	// Audit: 31 days old is out under 30, 29 days old stays.
	_ = st.AppendAudit("x", "old.evt", "", now-31*86400)
	_ = st.AppendAudit("x", "fresh.evt", "", now-29*86400)
	// Agent logs: 8 days old is out under 7, 6 days old stays.
	if _, _, err := st.InsertAgentLogs([]*store.AgentLog{
		{EventID: "old", Ts: now - 8*86400, ReceivedAt: now - 8*86400, PeerID: "alice", Level: "info", Source: "relay-watch", Event: "wake.requested"},
		{EventID: "fresh", Ts: now - 6*86400, ReceivedAt: now - 6*86400, PeerID: "alice", Level: "info", Source: "relay-watch", Event: "wake.requested"},
	}); err != nil {
		t.Fatal(err)
	}
	r := New(st, Policy{AuditRetentionDays: 30, AgentLogRetentionDays: 7, DataDir: t.TempDir()})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["audit_pruned"] != 1 || stats["agent_logs_pruned"] != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	entries, _ := st.ListAudit("", "", 0, 10)
	if len(entries) != 1 || entries[0].Action != "fresh.evt" {
		t.Fatalf("audit after prune: %+v", entries)
	}
	if _, total, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0); err != nil || total != 1 {
		t.Fatalf("agent logs after prune: total=%d err=%v", total, err)
	}
}

func TestPurgeLegacyAuditNoiseOnce(t *testing.T) {
	st := openTest(t)
	now := int64(1000)
	success := "peer=alice account=ma1 ok=true vm=UNKNOWN wake_ok=true wake_state=noise_confirmed error= wake_error="
	failure := "peer=alice account=ma1 ok=false error=session failed"
	wakeFailure := "peer=alice account=ma1 ok=true vm=RUNNING wake_ok=false wake_state=unconfirmed error= wake_error=noise timeout"
	for _, e := range []struct {
		action, detail string
		ts             int64
	}{
		{"muse.auto_wake", success, now - 100},
		{"muse.auto_wake", failure, now - 90},
		{"muse.auto_wake", wakeFailure, now - 80},
		{"peer.offline", "peer=alice offline_secs=600", now - 70},
		{"peer.offline", "peer=bob offline_secs=300", now - 60},
		{"peer.status", "peer=alice status=active", now - 50},
	} {
		if err := st.AppendAudit("system", e.action, e.detail, e.ts); err != nil {
			t.Fatal(err)
		}
	}
	r := New(st, Policy{AuditRetentionDays: 30, DataDir: t.TempDir()})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["audit_noise_purged"] != 3 || stats["audit_auto_wake_purged"] != 1 ||
		stats["audit_peer_offline_purged"] != 2 {
		t.Fatalf("stats: %+v", stats)
	}
	entries, _ := st.ListAudit("", "", 0, 20)
	counts := map[string]int{}
	for _, e := range entries {
		counts[e.Action]++
		if e.Action == "retention.purged" &&
			(!strings.Contains(e.Detail, "muse_auto_wake_success=1") ||
				!strings.Contains(e.Detail, "peer_offline=2") ||
				!strings.Contains(e.Detail, "muse_auto_wake_kept=2")) {
			t.Fatalf("purge audit detail: %q", e.Detail)
		}
	}
	if counts["muse.auto_wake"] != 2 || counts["peer.offline"] != 0 ||
		counts["peer.status"] != 1 || counts["retention.purged"] != 1 || len(entries) != 4 {
		t.Fatalf("audit after purge: %+v", counts)
	}
	// The failed and error-bearing wake rows survive; the routine success is gone.
	for _, e := range entries {
		if e.Action == "muse.auto_wake" && e.Detail == success {
			t.Fatalf("success row survived: %+v", e)
		}
	}
	// Second pass is a no-op: marker set, zero deletes, no second purge audit.
	stats, err = r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["audit_noise_purged"] != 0 {
		t.Fatalf("second run purged: %+v", stats)
	}
	entries2, _ := st.ListAudit("", "", 0, 20)
	if len(entries2) != len(entries) {
		t.Fatalf("second run changed audit rows: %d -> %d", len(entries), len(entries2))
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

func TestPermissionsPrunedWithTTL(t *testing.T) {
	st := openTest(t)
	r := New(st, Policy{MessageTTLDays: 30, DataDir: t.TempDir()})
	now := int64(100 * 86400)
	st.CreatePermissionRequest(mkPerm("old-decided", "a/t1", "allowed", now-40*86400, now-40*86400))
	st.CreatePermissionRequest(mkPerm("fresh-decided", "a/t1", "denied", now-86400, now-86400))
	st.CreatePermissionRequest(mkPerm("pending", "a/t1", "pending", now-100, now+3600))
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["permissions_pruned"] != 1 {
		t.Fatalf("stats: %+v", stats)
	}
	if pr, _ := st.GetPermissionRequest("old-decided"); pr != nil {
		t.Fatal("old row kept")
	}
	if pr, _ := st.GetPermissionRequest("fresh-decided"); pr == nil {
		t.Fatal("fresh row pruned")
	}
	if pr, _ := st.GetPermissionRequest("pending"); pr == nil {
		t.Fatal("pending row pruned")
	}
}

func mkPerm(id, thread, status string, created, expires int64) *store.PermissionRequest {
	return &store.PermissionRequest{RequestID: id, Thread: thread, Requester: "b", Approver: "a",
		Status: status, CreatedAt: created, ExpiresAt: expires}
}

func TestOpenTicketPinsMessages(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	old := now - 31*86400
	for _, id := range []string{"a", "b"} {
		_ = st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: old})
		_ = st.TouchPeer(id, now)
	}
	seq, _ := st.InsertMessage(&store.Message{ID: "t1", Sender: "a", Recipient: "b",
		RootID: "a/t1", Kind: "task", Payload: "old task", CreatedAt: old})
	_ = st.Ack(seq, "b", old+1)
	_ = st.CreateTicket(&store.Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: old, UpdatedAt: old, LastSeq: seq})
	_ = st.SetMessageTicket(seq, seq)

	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["messages_archived"] != 0 {
		t.Fatalf("open ticket pinned message archived: %+v", stats)
	}
	if got, _ := st.GetBySenderID("a", "t1"); got == nil {
		t.Fatal("open ticket task message deleted")
	}
	// Closing the ticket releases both the pin and the projection row.
	_ = st.UpdateTicket(&store.Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "done", Stage: "done", CreatedAt: old, UpdatedAt: old, LastSeq: seq})
	stats, err = r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["messages_archived"] != 1 || stats["tickets_pruned"] != 1 {
		t.Fatalf("closed ticket not archived/pruned: %+v", stats)
	}
}

func TestPruneAgentLogs(t *testing.T) {
	st := openTest(t)
	now := int64(100 * 86400)
	_, _, err := st.InsertAgentLogs([]*store.AgentLog{
		{EventID: "old", Ts: now - 2*86400, ReceivedAt: now - 2*86400, PeerID: "alice", Level: "error", Source: "relay-watch", Event: "worker.timeout"},
		{EventID: "new", Ts: now, ReceivedAt: now, PeerID: "alice", Level: "info", Source: "relay-watch", Event: "wake.requested"},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := New(st, Policy{AgentLogRetentionDays: 1, DataDir: t.TempDir()})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats["agent_logs_pruned"] != 1 {
		t.Fatalf("pruned=%d want 1", stats["agent_logs_pruned"])
	}
	_, total, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0)
	if err != nil || total != 1 {
		t.Fatalf("remaining total=%d err=%v", total, err)
	}
}

func TestTrimAgentLogsPerPeerCap(t *testing.T) {
	st := openTest(t)
	now := int64(1000)
	for i := 0; i < 3; i++ {
		_, _, err := st.InsertAgentLogs([]*store.AgentLog{{
			EventID: "cap-" + string(rune('a'+i)), Ts: now, ReceivedAt: now,
			PeerID: "alice", Level: "info", Source: "relay-watch", Event: "wake.requested",
		}})
		if err != nil {
			t.Fatal(err)
		}
	}
	r := New(st, Policy{AgentLogMaxRowsPerPeer: 2, DataDir: t.TempDir()})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if stats["agent_logs_trimmed"] != 1 {
		t.Fatalf("trimmed=%d want 1", stats["agent_logs_trimmed"])
	}
	_, total, err := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0)
	if err != nil || total != 2 {
		t.Fatalf("remaining=%d err=%v", total, err)
	}
}

// A thread with recent traffic is pinned: its older fully-acked messages must
// not be archived while the assistant may still need the conversation.
func TestActiveThreadIsPinned(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	for _, id := range []string{"a", "b"} {
		if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: now - 31*86400}); err != nil {
			t.Fatal(err)
		}
	}
	old := now - 31*86400
	seq1, _ := st.InsertMessage(&store.Message{ID: "old", Sender: "a", Recipient: "b",
		RootID: "a/thread", Payload: "old", CreatedAt: old})
	seq2, _ := st.InsertMessage(&store.Message{ID: "fresh", Sender: "a", Recipient: "b",
		RootID: "a/thread", Payload: "fresh", CreatedAt: now - 86400})
	_ = st.Ack(seq1, "b", old+1)
	_ = st.Ack(seq2, "b", now)
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["messages_archived"] != 0 {
		t.Fatalf("active thread was archived: %+v", stats)
	}
}

func TestActiveLargeThreadNotReportedSkipped(t *testing.T) {
	st := openTest(t)
	r := New(st, Policy{MessageTTLDays: 30, DataDir: t.TempDir()})
	now := int64(100 * 86400)
	for i := 0; i < scanCap+5; i++ {
		ts := now - 40*86400
		if i == scanCap+4 {
			ts = now - 60 // newest message inside TTL keeps the thread active
		}
		_, err := st.InsertMessage(&store.Message{
			ID: "m-" + string(rune('a'+i/26)) + string(rune('a'+i%26)) + "-" + string(rune('0'+i%10)),
			Sender: "operator", Recipient: "alice", RootID: "op/alice",
			Kind: "status", Payload: "ok", CreatedAt: ts,
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["threads_skipped"] != 0 {
		t.Fatalf("active large thread reported skipped: %+v", stats)
	}
	audits, _ := st.ListAudit("system", "retention.skipped", 0, 10)
	if len(audits) != 0 {
		t.Fatalf("unexpected retention.skipped audit: %+v", audits)
	}
}

func TestArchivedRoomMessagesArchivedAfterTTL(t *testing.T) {
	st := openTest(t)
	dir := t.TempDir()
	r := New(st, Policy{MessageTTLDays: 30, DataDir: dir})
	now := int64(100 * 86400)
	old := now - 31*86400

	_ = st.CreateRoom(&store.Room{ID: "grp_old", Name: "Old", CreatedAt: old, ArchivedAt: old + 100})
	_, err := st.InsertMessage(&store.Message{
		ID: "rm1", Sender: "operator", Recipient: "grp_old",
		RootID: "grp_old", Kind: "chat", Payload: "bye", CreatedAt: old,
	})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["messages_archived"] != 1 || stats["messages_deleted"] != 1 {
		t.Fatalf("archived room stats: %+v", stats)
	}
	if got, _ := st.GetBySenderID("operator", "rm1"); got != nil {
		t.Fatal("archived room message was not deleted")
	}
}

func TestPurgeLogNoiseV2Once(t *testing.T) {
	st := openTest(t)
	now := int64(1000)
	_ = st.AppendAudit("alice", "thread.fuse_tripped", "thread=op/alice count=60 window=3600s", now-50)
	_ = st.AppendAudit("system", "retention.skipped", "threads=1 cap=1000", now-40)
	_ = st.AppendAudit("system", "muse.auto_wake", "peer=alice account=ma1 ok=true vm=UNKNOWN wake_ok=false wake_state=unconfirmed error= wake_error=noise dial: 502 (attempt 1/3, 0.8s)", now-30)
	_ = st.AppendAudit("system", "muse.auto_wake", "peer=alice account=ma1 ok=false error=session failed", now-20)
	_ = st.AppendAudit("system", "peer.status", "peer=alice status=active", now-10)
	if _, _, err := st.InsertAgentLogs([]*store.AgentLog{
		{EventID: "ds1", Ts: now - 20, ReceivedAt: now - 20, PeerID: "alice", Level: "info", Source: "relay-watch", Event: "watchdog.daemon_started"},
		{EventID: "err1", Ts: now - 10, ReceivedAt: now - 10, PeerID: "alice", Level: "error", Source: "relay-watch", Event: "worker.timeout"},
	}); err != nil {
		t.Fatal(err)
	}

	r := New(st, Policy{AuditRetentionDays: 30, AgentLogRetentionDays: 7, DataDir: t.TempDir()})
	stats, err := r.RunOnce(now)
	if err != nil {
		t.Fatal(err)
	}
	if stats["log_noise_purged"] != 4 || stats["audit_fuse_tripped_purged"] != 1 ||
		stats["audit_auto_wake_502_purged"] != 1 || stats["audit_retention_skipped_purged"] != 1 ||
		stats["agent_daemon_started_purged"] != 1 {
		t.Fatalf("unexpected v2 purge stats: %+v", stats)
	}
	// Real auto_wake failure + peer.status + retention.purged remain.
	aw, _ := st.ListAudit("system", "muse.auto_wake", 0, 10)
	if len(aw) != 1 || !strings.Contains(aw[0].Detail, "session failed") {
		t.Fatalf("remaining muse.auto_wake: %+v", aw)
	}
	_, total, _ := st.ListAgentLogsPage(store.AgentLogFilter{PeerID: "alice"}, 10, 0)
	if total != 1 {
		t.Fatalf("remaining agent logs = %d, want 1", total)
	}
	// Second run is a no-op.
	stats2, err := r.RunOnce(now + 10)
	if err != nil {
		t.Fatal(err)
	}
	if stats2["log_noise_purged"] != 0 {
		t.Fatalf("second run purged again: %+v", stats2)
	}
}
