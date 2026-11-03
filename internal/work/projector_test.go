package work

import (
	"context"
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func workFixture(t *testing.T) (store.Store, *Service) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{"a", "b"} {
		if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: 900}); err != nil {
			t.Fatalf("peer %s: %v", id, err)
		}
	}
	s := New(st, 300)
	s.now = func() int64 { return 1000 }
	return st, s
}

func insert(t *testing.T, st store.Store, m *store.Message) int64 {
	t.Helper()
	seq, err := st.InsertMessage(m)
	if err != nil {
		t.Fatalf("insert %+v: %v", m, err)
	}
	return seq
}

func TestProjectorStageLifecycle(t *testing.T) {
	st, s := workFixture(t)
	task := insert(t, st, &store.Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "do it", CreatedAt: 1000})
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if m, _ := st.GetMandate(task); m == nil {
		t.Fatal("ticket has no mandate")
	}
	if a, _ := st.LatestAssignment(task); a == nil || a.Status != "pending" {
		t.Fatalf("ticket has no pending assignment: %+v", a)
	}
	if subs, _ := st.TicketSubscriptions(task); len(subs) != 1 || subs[0].Mode != "own" {
		t.Fatalf("ticket subscription: %+v", subs)
	}
	items, _, err := s.List(store.TicketFilter{}, 10, 0)
	if err != nil || len(items) != 1 || items[0].Stage != "queued" {
		t.Fatalf("queued: %+v err=%v", items, err)
	}
	insert(t, st, &store.Message{ID: "s1", Sender: "b", Recipient: "a", Kind: "status", Status: "started",
		RootID: "a/t1", InReplyTo: "t1", Payload: "started", CreatedAt: 1010})
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _, _ = s.List(store.TicketFilter{}, 10, 0)
	if len(items) != 1 || items[0].Stage != "working" {
		t.Fatalf("working: %+v", items)
	}
	insert(t, st, &store.Message{ID: "s2", Sender: "b", Recipient: "a", Kind: "status", Status: "blocked",
		RootID: "a/t1", InReplyTo: "t1", Payload: "blocked", CreatedAt: 1020})
	_ = s.Pass(context.Background())
	items, _, _ = s.List(store.TicketFilter{}, 10, 0)
	if len(items) != 1 || items[0].Stage != "person/blocked" {
		t.Fatalf("blocked: %+v", items)
	}
	insert(t, st, &store.Message{ID: "r1", Sender: "b", Recipient: "a", Kind: "result",
		RootID: "a/t1", InReplyTo: "t1", Payload: "done", CreatedAt: 1030})
	_ = s.Pass(context.Background())
	items, _, _ = s.List(store.TicketFilter{}, 10, 0)
	if len(items) != 1 || items[0].Stage != "done" || items[0].State != "done" {
		t.Fatalf("done: %+v", items)
	}
	if items[0].ID != task {
		t.Fatalf("ticket id=%d want task seq=%d", items[0].ID, task)
	}
	if as, _ := st.ListAssignments(task); len(as) == 0 || as[len(as)-1].Status != "ended" {
		t.Fatalf("done ticket assignment should be ended: %+v", as)
	}
	msgs, err := st.TicketMessages(task, 10)
	if err != nil || len(msgs) != 4 {
		t.Fatalf("ticket messages=%d err=%v", len(msgs), err)
	}
}

func TestProjectorLeaseExpiryNeedsReassign(t *testing.T) {
	st, s := workFixture(t)
	task := insert(t, st, &store.Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "do it", CreatedAt: 1000})
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReassignTicket(store.AssignmentCommand{
		TicketID: task, Assignee: "b", LeaseSecs: 60, Now: 1000,
	}); err != nil {
		t.Fatal(err)
	}
	s.now = func() int64 { return 1061 }
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _, err := s.List(store.TicketFilter{}, 10, 0)
	if err != nil || len(items) != 1 {
		t.Fatalf("list after lease: %+v err=%v", items, err)
	}
	if !items[0].LeaseExpired || items[0].EffectiveStage != "person/offline" || !items[0].Attention {
		t.Fatalf("expired lease not surfaced: %+v", items[0])
	}
	if latest, _ := st.LatestAssignment(task); latest == nil || latest.Status != store.AssignmentEnded {
		t.Fatalf("expired assignment not ended: %+v", latest)
	}
	// A later projector pass must not silently re-arm the same lease.
	s.now = func() int64 { return 1200 }
	if err := s.Pass(context.Background()); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.LatestAssignment(task); latest == nil || latest.Status != store.AssignmentEnded || latest.EndedReason != store.AssignmentReasonLease {
		t.Fatalf("projector re-armed expired lease: %+v", latest)
	}
	// Explicit reassignment is the only renewal path.
	if _, err := s.Reassign(task, "b", 60); err != nil {
		t.Fatal(err)
	}
	if latest, _ := st.LatestAssignment(task); latest == nil || latest.Status != store.AssignmentPending {
		t.Fatalf("reassign did not create pending assignment: %+v", latest)
	}
}

func TestProjectorMultipleTicketsPerThread(t *testing.T) {
	st, s := workFixture(t)
	t1 := insert(t, st, &store.Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "first", CreatedAt: 1000})
	t2 := insert(t, st, &store.Message{ID: "t2", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", InReplyTo: "t1", Payload: "second", CreatedAt: 1001})
	insert(t, st, &store.Message{ID: "r1", Sender: "b", Recipient: "a", Kind: "result",
		RootID: "a/t1", InReplyTo: "t1", Payload: "first done", CreatedAt: 1010})
	insert(t, st, &store.Message{ID: "r2", Sender: "b", Recipient: "a", Kind: "result",
		RootID: "a/t1", InReplyTo: "t2", Payload: "second done", CreatedAt: 1011})
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, total, _ := s.List(store.TicketFilter{}, 10, 0)
	if total != 2 || len(items) != 2 {
		t.Fatalf("tickets total=%d len=%d", total, len(items))
	}
	byID := map[int64]*TicketView{items[0].ID: items[0], items[1].ID: items[1]}
	if byID[t1].Stage != "done" || byID[t2].Stage != "done" {
		t.Fatalf("stages: t1=%+v t2=%+v", byID[t1], byID[t2])
	}
	// Rebuild is idempotent.
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	items2, total2, _ := s.List(store.TicketFilter{}, 10, 0)
	if total2 != 2 || len(items2) != 2 || items2[0].ID != items[0].ID || items2[1].ID != items[1].ID {
		t.Fatalf("rebuild drift: %+v vs %+v", items, items2)
	}
}

func TestProjectorExcludesNonWorkMessages(t *testing.T) {
	st, s := workFixture(t)
	insert(t, st, &store.Message{ID: "b1", Sender: "a", Recipient: "*", Kind: "task",
		RootID: "a/b1", Payload: "broadcast", CreatedAt: 1000})
	insert(t, st, &store.Message{ID: "r1", Sender: "a", Recipient: "grp_ops", Kind: "task",
		RootID: "grp_ops", Payload: "room", CreatedAt: 1001})
	insert(t, st, &store.Message{ID: "sys1", Sender: "system", Recipient: "a", Kind: "task",
		RootID: "verify/a", Payload: "smoke", CreatedAt: 1002})
	insert(t, st, &store.Message{ID: "v1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "verify/a", Payload: "verify", CreatedAt: 1003})
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, total, _ := s.List(store.TicketFilter{}, 10, 0)
	if total != 0 || len(items) != 0 {
		t.Fatalf("non-work messages created tickets: %+v", items)
	}
}

func TestProjectorPermissionStageAndExpiry(t *testing.T) {
	st, s := workFixture(t)
	insert(t, st, &store.Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	insert(t, st, &store.Message{ID: "s1", Sender: "b", Recipient: "a", Kind: "status", Status: "started",
		RootID: "a/t1", InReplyTo: "t1", Payload: "started", CreatedAt: 1010})
	permSeq := insert(t, st, &store.Message{ID: "pr1", Sender: "b", Recipient: "a", Kind: "permission_request",
		RootID: "a/t1", InReplyTo: "t1", Payload: "need approval", Op: "shell.exec", ExpiresAt: 2000, CreatedAt: 1020})
	if err := st.CreatePermissionRequest(&store.PermissionRequest{RequestID: "pr1", Thread: "a/t1",
		Requester: "b", Approver: "a", Status: "pending", Op: "shell.exec", CreatedAt: 1020, ExpiresAt: 2000}); err != nil {
		t.Fatal(err)
	}
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _, _ := s.List(store.TicketFilter{}, 10, 0)
	if len(items) != 1 || items[0].Stage != "person/answer" {
		t.Fatalf("permission stage: %+v", items)
	}
	// After expiry the request no longer counts; the status returns to working.
	s.now = func() int64 { return 3000 }
	if err := s.RefreshOpen(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _, _ = s.List(store.TicketFilter{}, 10, 0)
	if len(items) != 1 || items[0].Stage != "working" {
		t.Fatalf("expired permission stage: %+v", items)
	}
	_ = permSeq
}

func TestProjectorAttentionIncludesOffline(t *testing.T) {
	st, s := workFixture(t)
	insert(t, st, &store.Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	s.now = func() int64 { return 2000 } // b never heartbeated
	if err := s.Rebuild(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, total, err := s.List(store.TicketFilter{Stage: "attention"}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 {
		t.Fatalf("attention=%+v total=%d err=%v", items, total, err)
	}
	if items[0].EffectiveStage != "person/offline" || !items[0].Attention {
		t.Fatalf("expected offline attention: %+v", items[0])
	}
	counts, err := s.Counts()
	if err != nil || counts.Open != 1 || counts.Attention != 1 {
		t.Fatalf("counts=%+v err=%v", counts, err)
	}
}
