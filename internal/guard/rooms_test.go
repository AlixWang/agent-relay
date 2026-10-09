package guard

import (
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// roomLimits is a guard whose room window is small enough to trip in a test.
func roomLimits() Limits {
	l := testLimits()
	l.RoomFuseMaxMessages = 3
	l.RoomFuseWindowSecs = 3600
	return l
}

func roomStore(t *testing.T, members ...string) store.Store {
	t.Helper()
	st := openTestStore(t)
	if err := st.CreateRoom(&store.Room{ID: "grp_x", Name: "x", CreatedAt: 1}); err != nil {
		t.Fatalf("create room: %v", err)
	}
	for _, id := range members {
		if err := st.AddRoomMember("grp_x", id, 0, 1); err != nil {
			t.Fatalf("add member %s: %v", id, err)
		}
	}
	return st
}

func rejectCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		t.Fatal("expected a rejection")
	}
	r, ok := err.(*Rejection)
	if !ok {
		t.Fatalf("not a Rejection: %v", err)
	}
	return r.Code
}

// A room target must be validated: unknown or archived rooms are refused (a
// typo in an alias must not become a silent black hole) and non-members get
// 403 rather than the ability to post into someone else's group.
func TestRoomTargetValidation(t *testing.T) {
	st := roomStore(t, "alice")
	g := New(st, roomLimits())
	now := time.Now().Unix()

	if code := rejectCode(t, mustErr(g.Check(&Envelope{ID: "1", To: "grp_nope", From: "alice", Payload: "x"}, now))); code != "bad_message" {
		t.Fatalf("unknown room: %s", code)
	}
	if code := rejectCode(t, mustErr(g.Check(&Envelope{ID: "2", To: "grp_x", From: "bob", Payload: "x"}, now))); code != "permission_not_authorized" {
		t.Fatalf("non-member: %s", code)
	}
	if v, err := g.Check(&Envelope{ID: "3", To: "grp_x", From: "alice", Payload: "x"}, now); err != nil || v.RootID != "grp_x" {
		t.Fatalf("member send: v=%+v err=%v", v, err)
	}
	// The relay's own identities may speak in any room (console + notices).
	for _, from := range []string{"operator", "system"} {
		v, err := g.Check(&Envelope{ID: "s-" + from, To: "grp_x", From: from, Payload: "x"}, now)
		if err != nil || v.RootID != "grp_x" {
			t.Fatalf("%s send into room: v=%+v err=%v", from, v, err)
		}
	}
	if err := st.SetRoomArchived("grp_x", now); err != nil {
		t.Fatal(err)
	}
	if code := rejectCode(t, mustErr(g.Check(&Envelope{ID: "4", To: "grp_x", From: "alice", Payload: "x"}, now))); code != "bad_message" {
		t.Fatalf("archived room: %s", code)
	}
}

func mustErr(_ *Verdict, err error) error { return err }

// Console-routed targets have canonical thread keys: one DM thread per peer
// with the operator, one thread per room — never a new thread per message.
func TestConsoleThreadKeysAreCanonical(t *testing.T) {
	st := openTestStore(t)
	g := New(st, roomLimits())
	now := time.Now().Unix()

	v, err := g.Check(&Envelope{ID: "d1", To: "operator", From: "alice", Payload: "需要你确认一下部署窗口，回复即可"}, now)
	if err != nil || v.RootID != "op/alice" {
		t.Fatalf("operator DM root: %+v err=%v", v, err)
	}
	// A reply to a specific message stays in the same DM thread.
	parent := v.RootID
	if _, err := st.InsertMessage(&store.Message{ID: "d1", Sender: "alice", Recipient: "operator",
		Kind: "chat", RootID: parent, Payload: "需要你确认一下部署窗口，回复即可", CreatedAt: now, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	v2, err := g.Check(&Envelope{ID: "d2", To: "operator", From: "alice", InReplyTo: "d1", Payload: "另外补充一点：窗口改到明天下午"}, now)
	if err != nil || v2.RootID != "op/alice" {
		t.Fatalf("reply root: %+v err=%v", v2, err)
	}
	// The operator's own send to that peer lands in the same thread.
	v3, err := g.Check(&Envelope{ID: "d3", To: "alice", From: "operator", Payload: "确认收到，我这就开始处理部署"}, now)
	if err != nil || v3.RootID != "op/alice" {
		t.Fatalf("operator → peer root: %+v err=%v", v3, err)
	}
}

// Rooms use a rolling window instead of the whole-thread fuse: a long-lived
// room must not be permanently fused, but burst chatter is still bounded.
func TestRoomWindowedFuse(t *testing.T) {
	st := roomStore(t, "alice")
	g := New(st, roomLimits()) // 3 messages / 3600s
	now := time.Now().Unix()

	for i := 0; i < 3; i++ {
		env := &Envelope{ID: "m" + itoaInt(i), To: "grp_x", From: "alice", Payload: "第 " + itoaInt(i) + " 条群消息：进度同步一下"}
		sendOK(t, g, st, env, now)
	}
	code := rejectCode(t, mustErr(g.Check(&Envelope{ID: "m9", To: "grp_x", From: "alice", Payload: "再来一条就该触发窗口上限了"}, now)))
	if code != "loop_fuse_tripped" {
		t.Fatalf("window cap: %s", code)
	}

	// Outside the window the count resets: old chatter does not wedge the room.
	later := now + 3601
	if _, err := g.Check(&Envelope{ID: "m10", To: "grp_x", From: "alice", Payload: "新的一小时，重新开始计数"}, later); err != nil {
		t.Fatalf("room should accept after the window: %v", err)
	}
}

// Rooms are exempt from the whole-thread age fuse: with a 1h age limit a room
// would otherwise be permanently fused the day after it was created.
func TestRoomIgnoresThreadAgeFuse(t *testing.T) {
	st := roomStore(t, "alice")
	l := roomLimits()
	l.FuseMaxAgeSecs = 60
	g := New(st, l)
	now := time.Now().Unix()

	if _, err := st.InsertMessage(&store.Message{ID: "old", Sender: "alice", Recipient: "grp_x",
		Kind: "chat", RootID: "grp_x", Payload: "很久以前的群聊消息", CreatedAt: now - 100000, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Check(&Envelope{ID: "new", To: "grp_x", From: "alice", Payload: "隔了很久继续说一句正事"}, now); err != nil {
		t.Fatalf("room wrongly hit the age fuse: %v", err)
	}
	// A normal 1:1 thread still gets the age fuse.
	if _, err := st.InsertMessage(&store.Message{ID: "old2", Sender: "alice", Recipient: "bob",
		Kind: "task", RootID: "alice/old2", Payload: "很久以前的群聊消息", CreatedAt: now - 100000, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	v, err := g.Check(&Envelope{ID: "n1", To: "bob", From: "alice", Payload: "hi"}, now)
	if err == nil {
		// A fresh thread is fine; the fuse must apply to the aged thread key.
		_ = v
	}
	if _, err := st.InsertMessage(&store.Message{ID: "t1", Sender: "alice", Recipient: "bob",
		Kind: "task", RootID: "alice/old2", Payload: "x", CreatedAt: now - 100000, ApprovalState: "n/a"}); err != nil {
		t.Fatal(err)
	}
	if code := rejectCode(t, mustErr(g.Check(&Envelope{ID: "n2", To: "bob", From: "alice",
		InReplyTo: "t1", Payload: "reply"}, now))); code != "loop_fuse_tripped" {
		t.Fatalf("1:1 age fuse should still apply, got %s", code)
	}
}

// A task addressed to a room has no 1:1 handshake; the rejection must say what
// to do instead, because the direction rules cannot apply.
func TestRoomHandshakeRefusedWithGuidance(t *testing.T) {
	st := roomStore(t, "alice")
	g := New(st, roomLimits())
	now := time.Now().Unix()

	root := sendOK(t, g, st, &Envelope{ID: "rt1", To: "grp_x", From: "operator",
		Kind: "task", Payload: "alice 部署一下"}, now)
	if root != "grp_x" {
		t.Fatalf("room task root: %s", root)
	}
	err := mustErr(g.Check(&Envelope{ID: "pr1", To: "operator", From: "alice",
		Kind: "permission_request", InReplyTo: "rt1", Op: "deploy", Target: "staging", Payload: "?"}, now))
	code := rejectCode(t, err)
	if code != "bad_permission_ref" {
		t.Fatalf("room handshake: %s", code)
	}
	if !strings.Contains(err.Error(), "DM the operator") {
		t.Fatalf("rejection should point at the DM path: %v", err)
	}
	// The same request in a 1:1 operator task still works.
	st2 := openTestStore(t)
	g2 := New(st2, roomLimits())
	sendOK(t, g2, st2, &Envelope{ID: "dt1", To: "alice", From: "operator", Kind: "task", Payload: "部署"}, now)
	if _, err := g2.Check(&Envelope{ID: "pr2", To: "operator", From: "alice",
		Kind: "permission_request", InReplyTo: "dt1", Op: "deploy", Target: "staging", Payload: "?"}, now); err != nil {
		t.Fatalf("1:1 handshake should work: %v", err)
	}
}

func itoaInt(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}
