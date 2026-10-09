package store

import "testing"

func roomTestCreate(t *testing.T, st Store, id, name string, ts int64) {
	t.Helper()
	if err := st.CreateRoom(&Room{ID: id, Name: name, CreatedAt: ts}); err != nil {
		t.Fatalf("create room: %v", err)
	}
}

func roomTestPeer(t *testing.T, st Store, id string) {
	t.Helper()
	if err := st.CreatePeer(&Peer{ID: id, DisplayName: id, AgentType: "muse", Status: "active", CreatedAt: 1, LastSeen: 1}); err != nil {
		t.Fatalf("create peer %s: %v", id, err)
	}
}

func roomMsg(t *testing.T, st Store, id, from, room, payload string, ts int64) int64 {
	t.Helper()
	seq, err := st.InsertMessage(&Message{
		ID: id, Sender: from, Recipient: room, Kind: "chat", RootID: room,
		Payload: payload, CreatedAt: ts, ApprovalState: "n/a",
	})
	if err != nil {
		t.Fatalf("insert %s: %v", id, err)
	}
	return seq
}

func TestRoomCRUDAndStats(t *testing.T) {
	st := openTest(t)
	roomTestPeer(t, st, "alice")
	roomTestPeer(t, st, "bob")
	roomTestCreate(t, st, "grp_ops", "运维", 1000)
	st.AddRoomMember("grp_ops", "alice", 0, 1000)
	st.AddRoomMember("grp_ops", "bob", 0, 1000)
	roomMsg(t, st, "m1", "operator", "grp_ops", "大家好", 1100)

	rooms, err := st.ListRoomsWithStats(false)
	if err != nil || len(rooms) != 1 {
		t.Fatalf("list: %v %+v", err, rooms)
	}
	rs := rooms[0]
	if rs.ID != "grp_ops" || rs.Name != "运维" || rs.Count != 2 {
		t.Fatalf("room stats: %+v", rs)
	}
	if len(rs.Members) != 2 || rs.Members[0] != "alice" || rs.Members[1] != "bob" {
		t.Fatalf("members: %+v", rs.Members)
	}
	if rs.LastSender != "operator" || rs.LastPreview != "大家好" || rs.LastSeq != 1 {
		t.Fatalf("last message: %+v", rs)
	}

	// Re-adding an existing member must not move start_seq (that would hide
	// history the member already had).
	st.AddRoomMember("grp_ops", "alice", 999, 1200)
	members, _ := st.ListRoomMembers("grp_ops")
	for _, m := range members {
		if m.PeerID == "alice" && m.StartSeq != 0 {
			t.Fatalf("re-add moved start_seq: %+v", m)
		}
	}

	// Archive: hidden from the default list, still visible on request.
	if err := st.SetRoomArchived("grp_ops", 2000); err != nil {
		t.Fatal(err)
	}
	if rooms, _ := st.ListRoomsWithStats(false); len(rooms) != 0 {
		t.Fatalf("archived room still listed: %+v", rooms)
	}
	if rooms, _ := st.ListRoomsWithStats(true); len(rooms) != 1 {
		t.Fatalf("archived room missing from includeArchived: %+v", rooms)
	}

	// Restore + rename.
	st.SetRoomArchived("grp_ops", 0)
	if err := st.UpdateRoom("grp_ops", "Ops", "note"); err != nil {
		t.Fatal(err)
	}
	room, err := st.GetRoom("grp_ops")
	if err != nil || room == nil || room.Name != "Ops" || room.Note != "note" || room.ArchivedAt != 0 {
		t.Fatalf("room after update: %+v err=%v", room, err)
	}
	if missing, _ := st.GetRoom("grp_nope"); missing != nil {
		t.Fatalf("unknown room should be nil, got %+v", missing)
	}
}

// The delivery rule for rooms is the whole point of the design: one stored
// message, membership decides who sees it, joiners only see what is new, and
// the sender never gets its own message back (no self-wake loop).
func TestRoomVisibilityJoinSeqAndAcks(t *testing.T) {
	st := openTest(t)
	roomTestCreate(t, st, "grp_x", "x", 1)
	st.AddRoomMember("grp_x", "alice", 0, 1)

	old := roomMsg(t, st, "old", "operator", "grp_x", "before bob joined", 10)

	vis := func(peer string) []*Message {
		t.Helper()
		msgs, err := st.VisibleTo(peer, 0, 50)
		if err != nil {
			t.Fatalf("visible %s: %v", peer, err)
		}
		return msgs
	}

	if n := len(vis("alice")); n != 1 {
		t.Fatalf("member should see the room message, got %d", n)
	}
	if n := len(vis("bob")); n != 0 {
		t.Fatalf("non-member must not see room messages, got %d", n)
	}
	if n := len(vis("operator")); n != 0 {
		t.Fatalf("sender must not see its own room message, got %d", n)
	}

	// bob joins now: start_seq is the current max, so the old message stays out.
	maxSeq, _ := st.MaxSeq()
	if err := st.AddRoomMember("grp_x", "bob", maxSeq, 20); err != nil {
		t.Fatal(err)
	}
	if n := len(vis("bob")); n != 0 {
		t.Fatalf("joiner must not see pre-join history, got %d", n)
	}
	fresh := roomMsg(t, st, "fresh", "operator", "grp_x", "after bob joined", 21)
	if n := len(vis("bob")); n != 1 {
		t.Fatalf("joiner must see new messages, got %d", n)
	}

	// Acking goes through the same rule: a member can, a non-member cannot.
	if ok, err := st.AckIfVisible(fresh, "alice", 22); err != nil || !ok {
		t.Fatalf("member ack: ok=%v err=%v", ok, err)
	}
	if ok, _ := st.AckIfVisible(fresh, "carol", 22); ok {
		t.Fatal("non-member acked a room message")
	}
	visibleTo := func(peer, id string) bool {
		t.Helper()
		for _, m := range vis(peer) {
			if m.ID == id {
				return true
			}
		}
		return false
	}
	if visibleTo("alice", "fresh") {
		t.Fatal("acked member still sees the message")
	}
	if !visibleTo("bob", "fresh") {
		t.Fatal("bob must still see the message after alice acked")
	}
	acks, err := st.AcksForSeqs([]int64{old, fresh})
	if err != nil || len(acks[fresh]) != 1 || acks[fresh][0] != "alice" {
		t.Fatalf("acks: %+v err=%v", acks, err)
	}

	// A removed member stops receiving.
	st.RemoveRoomMember("grp_x", "bob")
	if n := len(vis("bob")); n != 0 {
		t.Fatalf("removed member still sees room messages, got %d", n)
	}
}

func TestRoomFuseWindowCountsOnlyTheWindow(t *testing.T) {
	st := openTest(t)
	roomTestCreate(t, st, "grp_x", "x", 1)
	st.AddRoomMember("grp_x", "alice", 0, 1)
	roomMsg(t, st, "a", "alice", "grp_x", "one", 100)
	roomMsg(t, st, "b", "alice", "grp_x", "two", 200)
	roomMsg(t, st, "c", "alice", "grp_x", "three", 9000)

	if n, _ := st.CountRoomMessagesSince("grp_x", 0, 0); n != 3 {
		t.Fatalf("all messages: %d", n)
	}
	if n, _ := st.CountRoomMessagesSince("grp_x", 0, 500); n != 1 {
		t.Fatalf("windowed count should be 1, got %d", n)
	}
	if n, _ := st.CountRoomMessagesSince("grp_x", 2, 0); n != 1 {
		t.Fatalf("post-watermark count should be 1, got %d", n)
	}
}

func TestSettingsAndOperatorInbox(t *testing.T) {
	st := openTest(t)
	if v, err := st.GetSetting("nope"); err != nil || v != "" {
		t.Fatalf("missing setting: %q err=%v", v, err)
	}
	if err := st.SetSetting("k", "1", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("k", "2", 11); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.GetSetting("k"); v != "2" {
		t.Fatalf("setting overwrite failed: %q", v)
	}

	// Two replies to the operator, plus noise that must not show up.
	if _, err := st.InsertMessage(&Message{ID: "r1", Sender: "alice", Recipient: "operator", Kind: "result", Payload: "done", CreatedAt: 20, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&Message{ID: "r2", Sender: "bob", Recipient: "operator", Kind: "result", Payload: "also done", CreatedAt: 21, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&Message{ID: "other", Sender: "alice", Recipient: "bob", Kind: "chat", Payload: "not for the console", CreatedAt: 22, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}

	items, total, err := st.InboxMessages(10, 0)
	if err != nil || total != 2 || len(items) != 2 {
		t.Fatalf("inbox: total=%d items=%d err=%v", total, len(items), err)
	}
	if items[0].ID != "r2" { // newest first
		t.Fatalf("inbox order: %+v", items[0])
	}
	if n, _ := st.CountInboxSince(0); n != 2 {
		t.Fatalf("unread count: %d", n)
	}
	if n, _ := st.CountInboxSince(items[0].Seq); n != 0 {
		t.Fatalf("unread after watermark: %d", n)
	}
}
