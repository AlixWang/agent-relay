package store

import (
	"fmt"
	"path/filepath"
	"testing"
)

func openTestStore(t *testing.T) *sqliteStore {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s.(*sqliteStore)
}

var testMsgN int

func insert(t *testing.T, s *sqliteStore, m *Message) *Message {
	t.Helper()
	if m.ApprovalState == "" {
		m.ApprovalState = "n/a"
	}
	if m.ID == "" {
		testMsgN++
		m.ID = fmt.Sprintf("m%d", testMsgN)
	}
	if m.CreatedAt == 0 {
		m.CreatedAt = int64(1000 + testMsgN)
	}
	seq, err := s.InsertMessage(m)
	if err != nil {
		t.Fatal(err)
	}
	m.Seq = seq
	return m
}

// ScopeKey is the single definition of "one conversation". If thread, room
// and DM fall back in the wrong order, FIFO and watermarks both scope
// wrongly.
func TestScopeKeyFallback(t *testing.T) {
	cases := []struct {
		name string
		m    Message
		want string
	}{
		{"thread wins", Message{RootID: "R1", Recipient: "grp_x", Sender: "a"}, "thread:R1"},
		{"room root wins", Message{RootID: "grp_ops", Recipient: "grp_ops", Sender: "a"}, "room:grp_ops"},
		{"room next", Message{Recipient: "grp_ops", Sender: "a"}, "room:grp_ops"},
		{"dm pair is symmetric", Message{Recipient: "bob", Sender: "alice"}, "dm:alice|bob"},
		{"dm other way", Message{Recipient: "alice", Sender: "bob"}, "dm:alice|bob"},
		{"control plane has none", Message{Kind: "status", Recipient: "bob", Sender: "alice"}, ""},
		{"system notice has none", Message{Kind: "system", Recipient: "grp_x", Sender: "system"}, ""},
	}
	for _, c := range cases {
		if got := ScopeKey(&c.m); got != c.want {
			t.Errorf("%s: got %q want %q", c.name, got, c.want)
		}
	}
}

// The watermark is the read count. Out-of-order acks must not let a later
// message jump ahead of an earlier one the peer never processed.
func TestWatermarkPinsAtHole(t *testing.T) {
	s := openTestStore(t)
	insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "1"})
	insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "2"})
	insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "3"})

	// Ack 1 and 3, skip 2 — legal with per-message acks.
	for _, seq := range []int64{1, 3} {
		ok, err := s.AckIfVisible(seq, "bob", 100)
		if err != nil || !ok {
			t.Fatalf("ack %d: ok=%v err=%v", seq, ok, err)
		}
	}
	wm, err := s.ReadWatermark("dm:alice|bob", "bob")
	if err != nil {
		t.Fatal(err)
	}
	if wm != 1 {
		t.Fatalf("watermark must pin below the hole: got %d want 1", wm)
	}
	stats, err := s.ReadStats([]int64{1, 2, 3})
	if err != nil {
		t.Fatal(err)
	}
	if stats[2].Read != 0 {
		t.Fatalf("message 2 was never processed, must read 0: %+v", stats[2])
	}
	if stats[3].Read != 0 {
		t.Fatalf("message 3 cannot be read while 2 is not: %+v", stats[3])
	}

	// Closing the hole releases both.
	if _, err := s.AckIfVisible(2, "bob", 100); err != nil {
		t.Fatal(err)
	}
	wm, _ = s.ReadWatermark("dm:alice|bob", "bob")
	if wm != 3 {
		t.Fatalf("after closing the hole: got %d want 3", wm)
	}
}

// A pending-approval message is a hole: it is not deliverable yet, so it
// must pin the watermark rather than let a later ack skip over it. After the
// human approves it, it becomes the scope head and can be acked normally.
func TestApprovalHoleDeliversAfterApprove(t *testing.T) {
	s := openTestStore(t)
	held := insert(t, s, &Message{
		Sender: "alice", Recipient: "bob", Kind: "task", Payload: "held",
		RequiresApproval: true, ApprovalState: "pending",
	})
	after := insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "after"})

	if _, err := s.AckIfVisible(after.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	wm, _ := s.ReadWatermark("dm:alice|bob", "bob")
	if wm != 0 {
		t.Fatalf("held message must pin the watermark: got %d want 0", wm)
	}
	stats, _ := s.ReadStats([]int64{held.Seq, after.Seq})
	if stats[held.Seq].Read != 0 || stats[after.Seq].Read != 0 {
		t.Fatalf("nothing behind the held message is read: %+v", stats)
	}
	head, err := s.ScopeHead("bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != 0 {
		t.Fatalf("nothing may be delivered while the approval hole is open: %+v", head)
	}

	if _, err := s.db.Exec(`UPDATE messages SET approval_state='approved' WHERE seq=?`, held.Seq); err != nil {
		t.Fatal(err)
	}
	head, err = s.ScopeHead("bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != 1 || head[0].Seq != held.Seq {
		t.Fatalf("approved message must become the scope head, got %+v", head)
	}
}

// Only the oldest unacked message per scope is exposed; other scopes are
// unaffected by a blocked one.
func TestScopeHeadSerializesWithinScopeOnly(t *testing.T) {
	s := openTestStore(t)
	a1 := insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", RootID: "T1", Payload: "a1"})
	a2 := insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", RootID: "T1", Payload: "a2"})
	b1 := insert(t, s, &Message{Sender: "carol", Recipient: "bob", Kind: "task", RootID: "T2", Payload: "b1"})

	head, err := s.ScopeHead("bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int64]bool{}
	for _, m := range head {
		got[m.Seq] = true
	}
	if !got[a1.Seq] || !got[b1.Seq] {
		t.Fatalf("each scope must expose its head: %+v", got)
	}
	if got[a2.Seq] {
		t.Fatalf("second message of a scope must stay hidden until the first is acked: %+v", got)
	}

	// Acking T1's head releases T1's second message and nothing else.
	if _, err := s.AckIfVisible(a1.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	head, _ = s.ScopeHead("bob", 10)
	got = map[int64]bool{}
	for _, m := range head {
		got[m.Seq] = true
	}
	if !got[a2.Seq] || !got[b1.Seq] || got[a1.Seq] {
		t.Fatalf("after acking the head: %+v", got)
	}
}

// A roster notice that nobody acks must never pin a watermark at zero.
func TestSystemNoticeDoesNotBlockScope(t *testing.T) {
	s := openTestStore(t)
	insert(t, s, &Message{Sender: "system", Recipient: "bob", Kind: "system", Payload: "roster"})
	task := insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "work"})
	if _, err := s.AckIfVisible(task.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	stats, _ := s.ReadStats([]int64{task.Seq})
	if stats[task.Seq].Read != 1 {
		t.Fatalf("system notice must not starve the watermark: %+v", stats[task.Seq])
	}
}

// A room thread contains both room-wide and one-to-one messages. A private
// reply must only count its addressee, not every member whose watermark
// advanced past the same sequence.
func TestReadStatsRoomPrivateReply(t *testing.T) {
	s := openTestStore(t)
	if err := s.CreateRoom(&Room{ID: "grp_x", Name: "x", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"alice", "bob", "carol"} {
		if err := s.AddRoomMember("grp_x", p, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	g1 := insert(t, s, &Message{Sender: "alice", Recipient: "grp_x", Kind: "chat", RootID: "grp_x", Payload: "hello"})
	priv := insert(t, s, &Message{Sender: "alice", Recipient: "bob", Kind: "chat", RootID: "grp_x", Payload: "secret"})
	g2 := insert(t, s, &Message{Sender: "alice", Recipient: "grp_x", Kind: "chat", RootID: "grp_x", Payload: "later"})
	for _, p := range []string{"bob", "carol"} {
		for _, m := range []*Message{g1, g2} {
			if _, err := s.AckIfVisible(m.Seq, p, 100); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := s.AckIfVisible(priv.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ReadStats([]int64{priv.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if stats[priv.Seq].Read != 1 || stats[priv.Seq].Total != 1 {
		t.Fatalf("private reply read stats: %+v want 1/1", stats[priv.Seq])
	}
	peers, err := s.ReadPeers([]int64{priv.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if len(peers[priv.Seq]) != 1 || peers[priv.Seq][0] != "bob" {
		t.Fatalf("private reply readers: %v want [bob]", peers[priv.Seq])
	}
}

// A member who joined after a message cannot be counted as a reader of it,
// even if they later acked a newer message in the same room.
func TestReadStatsLateJoinerAndDeparture(t *testing.T) {
	s := openTestStore(t)
	if err := s.CreateRoom(&Room{ID: "grp_x", Name: "x", CreatedAt: 1}); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"alice", "bob"} {
		if err := s.AddRoomMember("grp_x", p, 0, 1); err != nil {
			t.Fatal(err)
		}
	}
	old := insert(t, s, &Message{Sender: "alice", Recipient: "grp_x", Kind: "chat", RootID: "grp_x", Payload: "old"})
	if err := s.AddRoomMember("grp_x", "late", old.Seq, 1); err != nil {
		t.Fatal(err)
	}
	newer := insert(t, s, &Message{Sender: "alice", Recipient: "grp_x", Kind: "chat", RootID: "grp_x", Payload: "new"})
	if _, err := s.AckIfVisible(old.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AckIfVisible(newer.Seq, "late", 100); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ReadStats([]int64{old.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if stats[old.Seq].Read != 1 || stats[old.Seq].Total != 1 {
		t.Fatalf("late joiner must not be counted on old message: %+v", stats[old.Seq])
	}
	if err := s.RemoveRoomMember("grp_x", "late"); err != nil {
		t.Fatal(err)
	}
	stats, _ = s.ReadStats([]int64{newer.Seq})
	if stats[newer.Seq].Read != 0 || stats[newer.Seq].Total != 1 {
		t.Fatalf("departed member must not remain a reader: %+v", stats[newer.Seq])
	}
}

// Broadcast has every registered peer as its audience, while operator and
// system messages have no per-message audience at all.
func TestReadStatsBroadcastAndOperator(t *testing.T) {
	s := openTestStore(t)
	for _, p := range []string{"alice", "bob", "carol"} {
		if err := s.CreatePeer(&Peer{ID: p, Status: "active", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	m := insert(t, s, &Message{Sender: "alice", Recipient: "*", Kind: "chat", RootID: "alice/b1", Payload: "all"})
	if _, err := s.AckIfVisible(m.Seq, "bob", 100); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AckIfVisible(m.Seq, "carol", 100); err != nil {
		t.Fatal(err)
	}
	stats, err := s.ReadStats([]int64{m.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if stats[m.Seq].Read != 2 || stats[m.Seq].Total != 2 {
		t.Fatalf("broadcast stats: %+v want 2/2", stats[m.Seq])
	}
	op := insert(t, s, &Message{Sender: "alice", Recipient: "operator", Kind: "chat", RootID: "op/alice", Payload: "hi"})
	stats, _ = s.ReadStats([]int64{op.Seq})
	if stats[op.Seq].Read != 0 || stats[op.Seq].Total != 0 {
		t.Fatalf("operator reply must have no per-message audience: %+v", stats[op.Seq])
	}
}

// A database created before watermark tracking has ack rows but no
// peer_read_watermarks. Opening it must reconstruct the historical frontier.
func TestBackfillHistoricalAcks(t *testing.T) {
	path := filepath.Join(t.TempDir(), "historical.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st1 := s1.(*sqliteStore)
	m := insert(t, st1, &Message{Sender: "alice", Recipient: "bob", Kind: "task", Payload: "work"})
	if err := st1.Ack(m.Seq, "bob", 10); err != nil { // legacy low-level ack: no watermark
		t.Fatal(err)
	}
	st1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	stats, err := s2.ReadStats([]int64{m.Seq})
	if err != nil {
		t.Fatal(err)
	}
	if stats[m.Seq].Read != 1 {
		t.Fatalf("historical ack not backfilled: %+v", stats[m.Seq])
	}
}

// Control-plane rows must never be promoted to a real scope by a reopen.
func TestBackfillControlRowsStayUnscoped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "reopen.db")
	s1, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st1 := s1.(*sqliteStore)
	insert(t, st1, &Message{Sender: "system", Recipient: "bob", Kind: "system", Payload: "roster"})
	st1.Close()

	s2, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	head, err := s2.ScopeHead("bob", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(head) != 0 {
		t.Fatalf("system notice leaked into scope-head: %+v", head)
	}
}
