package store

import "testing"

func TestTicketBasicsAndList(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	seq := mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "do the thing", CreatedAt: 1000})
	if err := st.CreateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: 1000, UpdatedAt: 1010, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMessageTicket(seq, seq); err != nil {
		t.Fatal(err)
	}
	if got, err := st.MessageTicket(seq); err != nil || got != seq {
		t.Fatalf("MessageTicket=%d err=%v", got, err)
	}
	tk, err := st.GetTicket(seq)
	if err != nil || tk == nil || tk.Stage != "working" {
		t.Fatalf("GetTicket: %+v err=%v", tk, err)
	}
	items, total, err := st.ListTickets(TicketFilter{State: "open"}, 10, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != seq {
		t.Fatalf("ListTickets: total=%d len=%d err=%v", total, len(items), err)
	}
	// Attention filter is stage LIKE person/%.
	if err := st.UpdateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "person/blocked", CreatedAt: 1000, UpdatedAt: 1010, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	items, total, _ = st.ListTickets(TicketFilter{Stage: "attention"}, 10, 0)
	if total != 1 || len(items) != 1 {
		t.Fatalf("attention list: total=%d len=%d", total, len(items))
	}
}

func TestTicketMessagesAndOpenPins(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	taskSeq := mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	statusSeq := mustMsg(t, st, &Message{ID: "s1", Sender: "b", Recipient: "a", Kind: "status", Status: "started",
		RootID: "a/t1", InReplyTo: "t1", Payload: "started", CreatedAt: 1010})
	if err := st.CreateTicket(&Ticket{ID: taskSeq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: 1000, UpdatedAt: 1010, LastSeq: statusSeq}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetMessageTicket(taskSeq, taskSeq)
	_ = st.SetMessageTicket(statusSeq, taskSeq)
	msgs, err := st.TicketMessages(taskSeq, 10)
	if err != nil || len(msgs) != 2 {
		t.Fatalf("TicketMessages len=%d err=%v", len(msgs), err)
	}
	pinned, err := st.OpenTicketMessageSeqs([]int64{taskSeq, statusSeq})
	if err != nil || !pinned[taskSeq] || !pinned[statusSeq] {
		t.Fatalf("OpenTicketMessageSeqs: %v %v", pinned, err)
	}
	// Ending the ticket releases the pin.
	if err := st.UpdateTicket(&Ticket{ID: taskSeq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "done", Stage: "done", CreatedAt: 1000, UpdatedAt: 1020, LastSeq: statusSeq}); err != nil {
		t.Fatal(err)
	}
	pinned, _ = st.OpenTicketMessageSeqs([]int64{taskSeq})
	if pinned[taskSeq] {
		t.Fatal("done ticket must not pin messages")
	}
}

func TestTicketCountsAndPrune(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	if err := st.TouchPeer("b", 1000); err != nil {
		t.Fatal(err)
	}
	mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	seq := int64(1)
	if err := st.CreateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetMessageTicket(seq, seq)
	counts, err := st.TicketCounts(1100, 300)
	if err != nil || counts.Open != 1 || counts.Attention != 0 {
		t.Fatalf("counts online: %+v err=%v", counts, err)
	}
	counts, _ = st.TicketCounts(2000, 300)
	if counts.Attention != 1 {
		t.Fatalf("offline working ticket must be attention: %+v", counts)
	}
	stages, err := st.TicketStageCounts(2000, 300)
	if err != nil || stages["person/offline"] != 1 {
		t.Fatalf("stage counts: %+v err=%v", stages, err)
	}
	// Ended ticket older than cutoff is pruned with its mapping.
	if err := st.UpdateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "done", Stage: "done", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneTickets(1500)
	if err != nil || n != 1 {
		t.Fatalf("PruneTickets n=%d err=%v", n, err)
	}
	if got, _ := st.MessageTicket(seq); got != 0 {
		t.Fatalf("mapping not pruned: %d", got)
	}
}

func TestTicketStats(t *testing.T) {
	st := openTest(t)
	tickets, mappings, err := st.TicketStats()
	if err != nil || tickets != 0 || mappings != 0 {
		t.Fatalf("empty stats: %d %d %v", tickets, mappings, err)
	}
}

func TestTicketAssignments(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	mustPeer(t, st, "c")
	seq := mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	if err := st.CreateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "queued", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	a, err := st.EnsureAssignment(AssignmentCommand{TicketID: seq, Assignee: "b", Now: 1000})
	if err != nil || a == nil || a.Status != "pending" {
		t.Fatalf("ensure assignment: %+v err=%v", a, err)
	}
	if err := st.MarkAssignmentRunning(seq, 1010); err != nil {
		t.Fatal(err)
	}
	a, _ = st.LatestAssignment(seq)
	if a.Status != "running" || a.StartedAt != 1010 {
		t.Fatalf("running: %+v", a)
	}
	b, err := st.ReassignTicket(AssignmentCommand{TicketID: seq, Assignee: "c", LeaseSecs: 60, Now: 1100})
	if err != nil || b == nil || b.Assignee != "c" || b.LeaseExpiresAt != 1160 {
		t.Fatalf("reassign: %+v err=%v", b, err)
	}
	if old, _ := st.GetAssignment(a.ID); old.Status != "ended" || old.EndedReason != AssignmentReasonReassign {
		t.Fatalf("old assignment not ended by reassign: %+v", old)
	}
	tk, _ := st.GetTicket(seq)
	if tk.Assignee != "c" {
		t.Fatalf("ticket assignee=%s", tk.Assignee)
	}
}

// A lease is a real deadline: expiry ends the binding, a plain refresh must not
// silently re-arm it, and only an explicit reassignment starts a new one.
func TestAssignmentLeaseExpiry(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "a")
	mustPeer(t, st, "b")
	seq := mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	if err := st.CreateTicket(&Ticket{ID: seq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.ReassignTicket(AssignmentCommand{TicketID: seq, Assignee: "b", LeaseSecs: 30, Now: 1000}); err != nil {
		t.Fatal(err)
	}
	n, err := st.ExpireAssignmentLeases(1030)
	if err != nil || n != 1 {
		t.Fatalf("expire: n=%d err=%v", n, err)
	}
	latest, err := st.LatestAssignment(seq)
	if err != nil || latest == nil || latest.Status != AssignmentEnded || latest.EndedReason != AssignmentReasonLease {
		t.Fatalf("latest after expiry: %+v err=%v", latest, err)
	}
	if latest, _ := st.LatestAssignment(seq); latest == nil || latest.Status != AssignmentEnded {
		t.Fatalf("expired lease still active: %+v", latest)
	}
	// EnsureAssignment must not resurrect the expired binding.
	if again, err := st.EnsureAssignment(AssignmentCommand{TicketID: seq, Assignee: "b", Now: 1040}); err != nil || again != nil {
		t.Fatalf("expired lease re-armed: %+v err=%v", again, err)
	}
	// Explicit reassignment is the renewal path.
	if next, err := st.ReassignTicket(AssignmentCommand{TicketID: seq, Assignee: "b", LeaseSecs: 60, Now: 1050}); err != nil || next == nil || next.Status != AssignmentPending {
		t.Fatalf("explicit renewal: %+v err=%v", next, err)
	}
}

func TestPolicyVersionAndMandate(t *testing.T) {
	st := openTest(t)
	if err := st.UpsertPolicyVersion("v1", "allow_peer_tasks=true", 1000); err != nil {
		t.Fatal(err)
	}
	p, err := st.GetPolicyVersion("v1")
	if err != nil || p == nil || p.Rules == "" {
		t.Fatalf("policy version: %+v err=%v", p, err)
	}
	seq := mustMsg(t, st, &Message{ID: "t7", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t7", Payload: "task", CreatedAt: 1000})
	if err := st.CreateTicket(&Ticket{ID: seq, ThreadRoot: "a/t7", Requester: "a", Assignee: "b",
		State: "open", Stage: "queued", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: seq}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMandate(&Mandate{TicketID: seq, PolicyVersion: "v1", Scope: "task",
		Requester: "operator", RequesterKind: "operator", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	m, err := st.GetMandate(seq)
	if err != nil || m == nil || m.PolicyVersion != "v1" || m.RequesterKind != "operator" {
		t.Fatalf("mandate: %+v err=%v", m, err)
	}
}
