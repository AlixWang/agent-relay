package guard

import (
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func testLimits() Limits {
	return Limits{FuseMaxMessages: 5, FuseMaxAgeSecs: 3600, RatePerMinute: 60}
}

func sendOK(t *testing.T, g *Guard, st store.Store, env *Envelope, now int64) string {
	t.Helper()
	v, err := g.Check(env, now)
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	seq, err := st.InsertMessage(&store.Message{
		ID: env.ID, Sender: env.From, Recipient: env.To, Kind: env.Kind,
		RootID: v.RootID, Payload: env.Payload, CreatedAt: now,
		ApprovalState: "n/a",
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	g.RecordHit(env.From, now)
	_ = seq
	return v.RootID
}

func TestDuplicateID(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	env := &Envelope{ID: "a1", To: "muse-b", From: "muse-a", Payload: "hello"}
	sendOK(t, g, st, env, now)
	if _, err := g.Check(&Envelope{ID: "a1", To: "muse-b", From: "muse-a", Payload: "hello again"}, now); err == nil {
		t.Fatal("expected duplicate_id rejection")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "duplicate_id" {
		t.Fatalf("wrong rejection: %v", err)
	}
}

func TestHardFuseCount(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits()) // 5 max
	now := time.Now().Unix()
	root := ""
	for i := 0; i < 5; i++ {
		env := &Envelope{ID: string(rune('a'+i)) + "x", To: "muse-b", From: "muse-a",
			InReplyTo: map[bool]string{true: "a", false: ""}[i > 0], Payload: "step number something different each time to avoid dup detection entirely here"}
		if i > 0 {
			env.InReplyTo = "ax"
			env.ID = string(rune('a'+i)) + "x"
			// keep same thread by replying to first message's id chain root
			env.Payload = "payload variant number with sufficient length difference padding " + string(rune('0'+i)) + " ........................................"
		}
		_ = root
		if i == 0 {
			root = sendOK(t, g, st, env, now)
		} else {
			// force same thread: root derives from reply parent
			v, err := g.Check(env, now)
			if err != nil {
				t.Fatalf("check %d: %v", i, err)
			}
			if _, err := st.InsertMessage(&store.Message{ID: env.ID, Sender: "muse-a", Recipient: "muse-b",
				RootID: v.RootID, Payload: env.Payload, CreatedAt: now, ApprovalState: "n/a"}); err != nil {
				t.Fatalf("insert %d: %v", i, err)
			}
			g.RecordHit("muse-a", now)
		}
	}
	// 6th message in the same thread must trip the fuse. Payload is long and
	// distinct so the loop heuristic cannot fire first.
	_, err := g.Check(&Envelope{ID: "zz", To: "muse-b", From: "muse-a", InReplyTo: "ax",
		Payload: "this is a completely fresh long task description that shares nothing with earlier ones, padding 1234567890 abcdefghijklmnopqrstuvwxyz"}, now)
	if r, ok := err.(*Rejection); !ok || r.Code != "loop_fuse_tripped" {
		t.Fatalf("expected loop_fuse_tripped, got %v", err)
	}
}

func TestLoopGuardDuplicate(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	sendOK(t, g, st, &Envelope{ID: "m1", To: "muse-b", From: "muse-a", Payload: "请统计 token"}, now)
	if _, err := g.Check(&Envelope{ID: "m2", To: "muse-a", From: "muse-b", InReplyTo: "m1", Payload: "请统计token!!"}, now); err == nil {
		t.Fatal("expected loop_guard on normalized duplicate")
	}
}

func TestLoopGuardAckOnly(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	sendOK(t, g, st, &Envelope{ID: "m1", To: "muse-b", From: "muse-a", Payload: "收到"}, now)
	if _, err := g.Check(&Envelope{ID: "m2", To: "muse-a", From: "muse-b", InReplyTo: "m1", Payload: "收到"}, now); err == nil {
		t.Fatal("expected loop_guard on ack-only chain")
	}
}

func TestRateLimit(t *testing.T) {
	st := openTestStore(t)
	g := New(st, Limits{FuseMaxMessages: 1000, FuseMaxAgeSecs: 3600, RatePerMinute: 12})
	now := time.Now().Unix()
	for i := 0; i < 12; i++ {
		env := &Envelope{ID: "r" + string(rune('a'+i)), To: "muse-b", From: "muse-a",
			Payload: "distinct long task payload number with padding to dodge dup " + string(rune('A'+i)) + " xxxxxxxxxxxxxxxxxxxx"}
		if _, err := g.Check(env, now); err != nil {
			t.Fatalf("check %d: %v", i, err)
		}
		g.RecordHit("muse-a", now)
	}
	if _, err := g.Check(&Envelope{ID: "rz", To: "muse-b", From: "muse-a", Payload: "one more distinct long payload zzzzzzzzzzzzzzzzzzz"}, now); err == nil {
		t.Fatal("expected rate_limited")
	}
}

func TestApprovalHeld(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	v, err := g.Check(&Envelope{ID: "h1", To: "muse-b", From: "muse-a", Payload: "rm -rf /", RequiresApproval: true}, time.Now().Unix())
	if err != nil {
		t.Fatalf("check: %v", err)
	}
	if !v.Held {
		t.Fatal("expected held verdict")
	}
}

// ---- permission handshake (§6.5) ----

// seedTask inserts a task a→b and returns its root. Payloads are long and
// distinct so the loop heuristic never fires inside handshake tests.
func seedTask(t *testing.T, g *Guard, st store.Store, id string, now int64) string {
	t.Helper()
	return sendOK(t, g, st, &Envelope{ID: id, To: "muse-b", From: "muse-a",
		Payload: "seed task " + id + " with enough distinct wording to dodge dup detection entirely " + string(rune('A'+len(id))) + " xxxxxxxxxxxxxxxxxxxx"}, now)
}

func seedPermissionRow(t *testing.T, st store.Store, thread, reqID string, now int64) {
	t.Helper()
	if err := st.CreatePermissionRequest(&store.PermissionRequest{
		RequestID: reqID, Thread: thread, Requester: "muse-b", Approver: "muse-a",
		Status: "pending", Op: "shell.exec", Target: "/tmp/x", Detail: "list it", CreatedAt: now, ExpiresAt: now + 600,
	}); err != nil {
		t.Fatalf("perm row: %v", err)
	}
}

func TestHandshakeStatusDirection(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	seedTask(t, g, st, "t1", now)
	// B→A started passes.
	if _, err := g.Check(&Envelope{ID: "s1", To: "muse-a", From: "muse-b", Kind: "status",
		InReplyTo: "t1", Status: "started", Payload: "B reports task t1 has started executing in detail here xxxxx"}, now); err != nil {
		t.Fatalf("B started: %v", err)
	}
	// A→B started is reversed: 403-class.
	if _, err := g.Check(&Envelope{ID: "s2", To: "muse-b", From: "muse-a", Kind: "status",
		InReplyTo: "t1", Status: "started", Payload: "A fakes a start signal with padding xxxxxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected direction rejection")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "permission_not_authorized" {
		t.Fatalf("wrong code: %v", err)
	}
	// Bad status value.
	if _, err := g.Check(&Envelope{ID: "s3", To: "muse-a", From: "muse-b", Kind: "status",
		InReplyTo: "t1", Status: "flying", Payload: "B reports something with padding xxxxxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected bad status value rejection")
	}
	// Missing in_reply_to.
	if _, err := g.Check(&Envelope{ID: "s4", To: "muse-a", From: "muse-b", Kind: "status",
		Status: "started", Payload: "B reports start with padding xxxxxxxxxxxxxxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected bad_permission_ref")
	}
}

func TestHandshakeStatusCancelBothSides(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	seedTask(t, g, st, "t1", now)
	for _, from := range []string{"muse-a", "muse-b"} {
		to := "muse-b"
		if from == "muse-b" {
			to = "muse-a"
		}
		if _, err := g.Check(&Envelope{ID: "c-" + from, To: to, From: from, Kind: "status",
			InReplyTo: "t1", Status: "cancelled", Payload: "cancel by " + from + " with padding xxxxxxxxxxxxxxxxxxxx"}, now); err != nil {
			t.Fatalf("cancel by %s: %v", from, err)
		}
	}
	// Third party cannot cancel.
	if _, err := g.Check(&Envelope{ID: "c-x", To: "muse-a", From: "mallory", Kind: "status",
		InReplyTo: "t1", Status: "cancelled", Payload: "mallory cancels with padding xxxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected third-party cancel rejection")
	}
}

func TestHandshakeProgressThrottle(t *testing.T) {
	st := openTestStore(t)
	lim := testLimits()
	lim.ProgressThrottleSecs = 10
	g := New(st, lim)
	now := time.Now().Unix()
	root := seedTask(t, g, st, "t1", now)
	mk := func(id string) *Envelope {
		return &Envelope{ID: id, To: "muse-a", From: "muse-b", Kind: "status",
			InReplyTo: "t1", Status: "progress", Payload: "progress report " + id + " milestone reached padding xxxxx"}
	}
	if _, err := g.Check(mk("p1"), now); err != nil {
		t.Fatalf("first progress: %v", err)
	}
	// Persist it so the throttle sees history (guard.Check is stateless).
	if _, err := st.InsertMessage(&store.Message{ID: "p1", Sender: "muse-b", Recipient: "muse-a",
		Kind: "status", InReplyTo: "t1", RootID: root, Payload: "progress report p1 milestone reached padding xxxxx",
		ApprovalState: "n/a", CreatedAt: now, Status: "progress"}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Check(mk("p2"), now+1); err == nil {
		t.Fatal("expected progress_throttled")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "progress_throttled" {
		t.Fatalf("wrong code: %v", err)
	}
	if _, err := g.Check(mk("p3"), now+11); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestHandshakePermissionLifecycle(t *testing.T) {
	st := openTestStore(t)
	lim := testLimits()
	lim.MaxOpenPermissions = 2
	g := New(st, lim)
	now := time.Now().Unix()
	root := seedTask(t, g, st, "t1", now)
	mkReq := func(id string) *Envelope {
		return &Envelope{ID: id, To: "muse-a", From: "muse-b", Kind: "permission_request",
			InReplyTo: "t1", Op: "shell.exec", Target: "/tmp/x", Detail: "need listing " + id,
			Payload: "B needs approval to run the command for step " + id + " padding xxxxxxxxx"}
	}
	// Reversed direction refused.
	if _, err := g.Check(&Envelope{ID: "rx", To: "muse-b", From: "muse-a", Kind: "permission_request",
		InReplyTo: "t1", Payload: "A fakes a request with padding xxxxxxxxxxxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected direction rejection")
	}
	if _, err := g.Check(mkReq("r1"), now); err != nil {
		t.Fatalf("request: %v", err)
	}
	// Persist the request message so taskOf can resolve decisions to it.
	if _, err := st.InsertMessage(&store.Message{ID: "r1", Sender: "muse-b", Recipient: "muse-a",
		Kind: "permission_request", InReplyTo: "t1", RootID: root, Payload: "B needs approval to run the command for step r1 padding xxxxxxxxx",
		ApprovalState: "n/a", CreatedAt: now, Op: "shell.exec", Target: "/tmp/x", Detail: "need listing r1",
		ExpiresAt: now + 600}); err != nil {
		t.Fatal(err)
	}
	seedPermissionRow(t, st, root, "r1", now)
	seedPermissionRow(t, st, root, "r2", now)
	// Third open request trips the per-thread cap.
	if _, err := g.Check(mkReq("r3"), now); err == nil {
		t.Fatal("expected permission_rate_limited")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "permission_rate_limited" {
		t.Fatalf("wrong code: %v", err)
	}
	// Decision by the approver passes; by anyone else fails.
	if _, err := g.Check(&Envelope{ID: "d1", To: "muse-b", From: "muse-a", Kind: "permission_decision",
		InReplyTo: "r1", Decision: "allow", Payload: "A approves the listing operation once padding xxxx"}, now); err != nil {
		t.Fatalf("decision: %v", err)
	}
	if _, err := g.Check(&Envelope{ID: "d2", To: "muse-b", From: "mallory", Kind: "permission_decision",
		InReplyTo: "r1", Decision: "allow", Payload: "mallory approves with padding xxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected approver-only rejection")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "permission_not_authorized" {
		t.Fatalf("wrong code: %v", err)
	}
	// Bad decision value.
	if _, err := g.Check(&Envelope{ID: "d3", To: "muse-b", From: "muse-a", Kind: "permission_decision",
		InReplyTo: "r1", Decision: "maybe", Payload: "A decides vaguely with padding xxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected bad decision value rejection")
	}
	// Unknown request id.
	if _, err := g.Check(&Envelope{ID: "d4", To: "muse-b", From: "muse-a", Kind: "permission_decision",
		InReplyTo: "nope", Decision: "allow", Payload: "A decides unknown with padding xxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected bad_permission_ref")
	}
	// Expired request is fail-closed.
	if err := st.CreatePermissionRequest(&store.PermissionRequest{
		RequestID: "rold", Thread: root, Requester: "muse-b", Approver: "muse-a",
		Status: "pending", CreatedAt: now - 900, ExpiresAt: now - 300,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertMessage(&store.Message{ID: "rold", Sender: "muse-b", Recipient: "muse-a",
		Kind: "permission_request", InReplyTo: "t1", RootID: root, Payload: "old request that expired long ago padding xxxxx",
		ApprovalState: "n/a", CreatedAt: now - 900, ExpiresAt: now - 300}); err != nil {
		t.Fatal(err)
	}
	if _, err := g.Check(&Envelope{ID: "d5", To: "muse-b", From: "muse-a", Kind: "permission_decision",
		InReplyTo: "rold", Decision: "allow", Payload: "A decides late with padding xxxxxxxxxxxxxxxxx"}, now); err == nil {
		t.Fatal("expected permission_expired")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "permission_expired" {
		t.Fatalf("wrong code: %v", err)
	}
}

func TestHandshakeRequiresApprovalRefused(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	seedTask(t, g, st, "t1", now)
	if _, err := g.Check(&Envelope{ID: "s1", To: "muse-a", From: "muse-b", Kind: "status",
		InReplyTo: "t1", Status: "started", Payload: "held start with padding xxxxxxxxxxxxxxxxxxxxx",
		RequiresApproval: true}, now); err == nil {
		t.Fatal("expected requires_approval refusal on handshake kinds")
	}
}

func TestHandshakeSkipsLoopCheck(t *testing.T) {
	st := openTestStore(t)
	g := New(st, testLimits())
	now := time.Now().Unix()
	seedTask(t, g, st, "t1", now)
	// "allow"-class short texts must not trip the courtesy-loop rule.
	if _, err := g.Check(&Envelope{ID: "s1", To: "muse-a", From: "muse-b", Kind: "status",
		InReplyTo: "t1", Status: "started", Payload: "ok"}, now); err != nil {
		t.Fatalf("short status hit loop guard: %v", err)
	}
}

// ---- Conversation Guard Tests (§v12) ----

func setupConversation(t *testing.T, st store.Store, convID string, members ...string) {
	t.Helper()
	for _, m := range members {
		if err := st.CreatePeer(&store.Peer{ID: m, Status: "active", CreatedAt: 1000}); err != nil {
			// Ignore duplicate errors
		}
	}
	conv := &store.Conversation{
		ID: convID, Type: "group", Title: "Test", CreatedBy: members[0], CreatedAt: 1000,
	}
	if err := st.CreateConversation(conv); err != nil {
		t.Fatalf("create conversation: %v", err)
	}
	for i, m := range members {
		role := "member"
		if i == 0 {
			role = "creator"
		}
		if err := st.AddConversationMember(convID, m, role, 0); err != nil {
			t.Fatalf("add member %s: %v", m, err)
		}
	}
}

func TestConversationMemberValidation(t *testing.T) {
	st := openTestStore(t)
	setupConversation(t, st, "conv-1", "alice", "bob")

	lim := testLimits()
	lim.ConvAgentTurnBudget = 6
	g := New(st, lim)
	now := time.Now().Unix()

	// Alice (member) can send
	env := &Envelope{
		ID: "m1", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "hello", ConvID: "conv-1",
	}
	if _, err := g.Check(env, now); err != nil {
		t.Fatalf("alice should be allowed: %v", err)
	}

	// Charlie (non-member) cannot send
	st.CreatePeer(&store.Peer{ID: "charlie", Status: "active", CreatedAt: 1000})
	env = &Envelope{
		ID: "m2", To: "conv:conv-1", From: "charlie", Kind: "chat",
		Payload: "hello", ConvID: "conv-1",
	}
	if _, err := g.Check(env, now); err == nil {
		t.Fatal("charlie should be rejected")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "not_member" {
		t.Fatalf("expected not_member, got: %v", err)
	}

	// User (admin) can always send
	env = &Envelope{
		ID: "m3", To: "conv:conv-1", From: "user", Kind: "chat",
		Payload: "admin message", ConvID: "conv-1",
	}
	if _, err := g.Check(env, now); err != nil {
		t.Fatalf("user should bypass member check: %v", err)
	}
}

func TestConversationFreshnessCheck(t *testing.T) {
	st := openTestStore(t)
	setupConversation(t, st, "conv-1", "alice", "bob")

	lim := testLimits()
	lim.ConvAgentTurnBudget = 6
	g := New(st, lim)
	now := time.Now().Unix()

	// Insert first message
	env := &Envelope{
		ID: "m1", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "hello", ConvID: "conv-1",
	}
	v, _ := g.Check(env, now)
	st.InsertMessage(&store.Message{
		ID: "m1", Sender: "alice", Recipient: "conv:conv-1", Kind: "chat",
		RootID: v.RootID, Payload: "hello", CreatedAt: now, ConvID: "conv-1",
		ApprovalState: "n/a",
	})

	// Bob sends unsolicited reply without seen_seq
	env = &Envelope{
		ID: "m2", To: "conv:conv-1", From: "bob", Kind: "chat",
		Payload: "hi", ConvID: "conv-1", SeenSeq: 0,
	}
	if _, err := g.Check(env, now+1); err == nil {
		t.Fatal("should require seen_seq for unsolicited reply")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "stale" {
		t.Fatalf("expected stale, got: %v", err)
	} else if r.LatestSeq != 1 {
		t.Fatalf("expected latest_seq=1, got %d", r.LatestSeq)
	}

	// Bob sends with correct seen_seq
	env.SeenSeq = 1
	env.ID = "m3"
	if _, err := g.Check(env, now+2); err != nil {
		t.Fatalf("should accept with correct seen_seq: %v", err)
	}

	// Insert second message
	st.InsertMessage(&store.Message{
		ID: "m3", Sender: "bob", Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "hi", CreatedAt: now + 2, ConvID: "conv-1",
		ApprovalState: "n/a",
	})

	// Bob sends with stale seen_seq
	env = &Envelope{
		ID: "m4", To: "conv:conv-1", From: "bob", Kind: "chat",
		Payload: "another", ConvID: "conv-1", SeenSeq: 1, // stale
	}
	if _, err := g.Check(env, now+3); err == nil {
		t.Fatal("should reject stale seen_seq")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "stale" {
		t.Fatalf("expected stale, got: %v", err)
	} else if r.LatestSeq != 2 {
		t.Fatalf("expected latest_seq=2, got %d", r.LatestSeq)
	}
}

func TestConversationMentionBypassesFreshnessCheck(t *testing.T) {
	st := openTestStore(t)
	setupConversation(t, st, "conv-1", "alice", "bob")

	lim := testLimits()
	lim.ConvAgentTurnBudget = 6
	g := New(st, lim)
	now := time.Now().Unix()

	// Insert message from alice
	env := &Envelope{
		ID: "m1", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "hello @bob", ConvID: "conv-1", Mentions: []string{"bob"},
	}
	v, _ := g.Check(env, now)
	st.InsertMessage(&store.Message{
		ID: "m1", Sender: "alice", Recipient: "conv:conv-1", Kind: "chat",
		RootID: v.RootID, Payload: "hello @bob", CreatedAt: now, ConvID: "conv-1",
		Mentions: `["bob"]`, ApprovalState: "n/a",
	})

	// Bob replies without seen_seq but is @mentioned - should be allowed
	env = &Envelope{
		ID: "m2", To: "conv:conv-1", From: "bob", Kind: "chat",
		Payload: "yes?", ConvID: "conv-1", SeenSeq: 0,
	}
	if _, err := g.Check(env, now+1); err != nil {
		t.Fatalf("@mention should bypass freshness check: %v", err)
	}
}

func TestConversationTurnBudget(t *testing.T) {
	st := openTestStore(t)
	setupConversation(t, st, "conv-1", "user", "alice", "bob")

	lim := testLimits()
	lim.ConvAgentTurnBudget = 3 // Allow max 3 consecutive agent replies
	g := New(st, lim)
	now := time.Now().Unix()

	// User sends initial message
	env := &Envelope{
		ID: "m1", To: "conv:conv-1", From: "user", Kind: "chat",
		Payload: "hello agents", ConvID: "conv-1",
	}
	v, _ := g.Check(env, now)
	st.InsertMessage(&store.Message{
		ID: "m1", Sender: "user", Recipient: "conv:conv-1", Kind: "chat",
		RootID: v.RootID, Payload: "hello agents", CreatedAt: now, ConvID: "conv-1",
		ApprovalState: "n/a",
	})
	g.UpdateConversationStreak("conv-1", "user") // Reset to 0

	// Alice sends reply #1
	env = &Envelope{
		ID: "m2", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "hi", ConvID: "conv-1", SeenSeq: 1,
	}
	if _, err := g.Check(env, now+1); err != nil {
		t.Fatalf("first agent reply should be allowed: %v", err)
	}
	st.InsertMessage(&store.Message{
		ID: "m2", Sender: "alice", Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "hi", CreatedAt: now + 1, ConvID: "conv-1",
		ApprovalState: "n/a",
	})
	g.UpdateConversationStreak("conv-1", "alice") // Increment to 1

	// Bob sends reply #2
	env = &Envelope{
		ID: "m3", To: "conv:conv-1", From: "bob", Kind: "chat",
		Payload: "hello", ConvID: "conv-1", SeenSeq: 2,
	}
	if _, err := g.Check(env, now+2); err != nil {
		t.Fatalf("second agent reply should be allowed: %v", err)
	}
	st.InsertMessage(&store.Message{
		ID: "m3", Sender: "bob", Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "hello", CreatedAt: now + 2, ConvID: "conv-1",
		ApprovalState: "n/a",
	})
	g.UpdateConversationStreak("conv-1", "bob") // Increment to 2

	// Alice sends reply #3
	env = &Envelope{
		ID: "m4", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "again", ConvID: "conv-1", SeenSeq: 3,
	}
	if _, err := g.Check(env, now+3); err != nil {
		t.Fatalf("third agent reply should be allowed: %v", err)
	}
	st.InsertMessage(&store.Message{
		ID: "m4", Sender: "alice", Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "again", CreatedAt: now + 3, ConvID: "conv-1",
		ApprovalState: "n/a",
	})
	g.UpdateConversationStreak("conv-1", "alice") // Increment to 3

	// Bob tries reply #4 - should be rejected (budget exceeded)
	env = &Envelope{
		ID: "m5", To: "conv:conv-1", From: "bob", Kind: "chat",
		Payload: "too many", ConvID: "conv-1", SeenSeq: 4,
	}
	if _, err := g.Check(env, now+4); err == nil {
		t.Fatal("fourth agent reply should be rejected")
	} else if r, ok := err.(*Rejection); !ok || r.Code != "agent_turn_budget" {
		t.Fatalf("expected agent_turn_budget, got: %v", err)
	}

	// User sends message - resets streak
	env = &Envelope{
		ID: "m6", To: "conv:conv-1", From: "user", Kind: "chat",
		Payload: "reset", ConvID: "conv-1",
	}
	if _, err := g.Check(env, now+5); err != nil {
		t.Fatalf("user message should always be allowed: %v", err)
	}
	st.InsertMessage(&store.Message{
		ID: "m6", Sender: "user", Recipient: "conv:conv-1", Kind: "chat",
		RootID: "conv-1", Payload: "reset", CreatedAt: now + 5, ConvID: "conv-1",
		ApprovalState: "n/a",
	})
	g.UpdateConversationStreak("conv-1", "user") // Reset to 0

	// Alice can send again after reset
	env = &Envelope{
		ID: "m7", To: "conv:conv-1", From: "alice", Kind: "chat",
		Payload: "after reset", ConvID: "conv-1", SeenSeq: 6,
	}
	if _, err := g.Check(env, now+6); err != nil {
		t.Fatalf("agent reply after reset should be allowed: %v", err)
	}
}

func TestUpdateConversationStreak(t *testing.T) {
	st := openTestStore(t)
	conv := &store.Conversation{
		ID: "conv-1", Type: "group", Title: "Test", CreatedBy: "alice",
		CreatedAt: 1000, AgentStreak: 0,
	}
	st.CreateConversation(conv)

	lim := testLimits()
	g := New(st, lim)

	// User message resets to 0
	if err := g.UpdateConversationStreak("conv-1", "user"); err != nil {
		t.Fatalf("UpdateConversationStreak: %v", err)
	}
	conv, _ = st.GetConversation("conv-1")
	if conv.AgentStreak != 0 {
		t.Fatalf("user should reset streak to 0, got %d", conv.AgentStreak)
	}

	// Agent message increments
	if err := g.UpdateConversationStreak("conv-1", "alice"); err != nil {
		t.Fatalf("UpdateConversationStreak: %v", err)
	}
	conv, _ = st.GetConversation("conv-1")
	if conv.AgentStreak != 1 {
		t.Fatalf("agent should increment streak to 1, got %d", conv.AgentStreak)
	}

	// Another agent message increments again
	if err := g.UpdateConversationStreak("conv-1", "bob"); err != nil {
		t.Fatalf("UpdateConversationStreak: %v", err)
	}
	conv, _ = st.GetConversation("conv-1")
	if conv.AgentStreak != 2 {
		t.Fatalf("agent should increment streak to 2, got %d", conv.AgentStreak)
	}

	// User message resets again
	if err := g.UpdateConversationStreak("conv-1", "user"); err != nil {
		t.Fatalf("UpdateConversationStreak: %v", err)
	}
	conv, _ = st.GetConversation("conv-1")
	if conv.AgentStreak != 0 {
		t.Fatalf("user should reset streak to 0, got %d", conv.AgentStreak)
	}
}
