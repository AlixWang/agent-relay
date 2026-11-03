package store

import "testing"

func TestTicketSubscriptionVisibility(t *testing.T) {
	st := openTest(t)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := st.CreatePeer(&Peer{ID: id, Status: "active", CreatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	taskSeq := mustMsg(t, st, &Message{ID: "t1", Sender: "a", Recipient: "b", Kind: "task",
		RootID: "a/t1", Payload: "task", CreatedAt: 1000})
	if err := st.CreateTicket(&Ticket{ID: taskSeq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "open", Stage: "working", CreatedAt: 1000, UpdatedAt: 1000, LastSeq: taskSeq}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetMessageTicket(taskSeq, taskSeq)
	if err := st.SubscribeTicket(taskSeq, "a/t1", "own", 1000); err != nil {
		t.Fatal(err)
	}
	// b participates in the thread.
	_ = mustMsg(t, st, &Message{ID: "m1", Sender: "b", Recipient: "a", RootID: "a/t1", Payload: "reply", CreatedAt: 1010})
	// a -> c in the same thread, not addressed to b.
	_ = mustMsg(t, st, &Message{ID: "m2", Sender: "a", Recipient: "c", RootID: "a/t1", Payload: "for c", CreatedAt: 1020})

	vis, err := st.VisibleTo("b", 0, 50)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, m := range vis {
		if m.ID == "m2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("subscribed participant b should see m2: %+v", vis)
	}
	visD, _ := st.VisibleTo("d", 0, 50)
	for _, m := range visD {
		if m.ID == "m2" {
			t.Fatal("non-participant d must not see m2")
		}
	}
	// A closed ticket releases the subscription delivery.
	if err := st.UpdateTicket(&Ticket{ID: taskSeq, ThreadRoot: "a/t1", Requester: "a", Assignee: "b",
		State: "done", Stage: "done", CreatedAt: 1000, UpdatedAt: 1030, LastSeq: taskSeq}); err != nil {
		t.Fatal(err)
	}
	_ = mustMsg(t, st, &Message{ID: "m3", Sender: "a", Recipient: "c", RootID: "a/t1", Payload: "after", CreatedAt: 1030})
	vis, _ = st.VisibleTo("b", 0, 50)
	for _, m := range vis {
		if m.ID == "m3" {
			t.Fatal("closed ticket must not deliver subscribed thread messages")
		}
	}
}
