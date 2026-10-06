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
