package verify

import (
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

// completeSmoke simulates the assistant side: result + ack for the smoke.
func completeSmoke(t *testing.T, st store.Store, peerID, smokeID string, smokeSeq int64, now int64) {
	t.Helper()
	if _, err := st.InsertMessage(&store.Message{
		ID: "r1", Sender: peerID, Recipient: "system", Kind: "result",
		InReplyTo: smokeID, RootID: "verify/" + peerID,
		Payload: "收到", CreatedAt: now,
	}); err != nil {
		t.Fatalf("result: %v", err)
	}
	if err := st.Ack(smokeSeq, peerID, now); err != nil {
		t.Fatalf("ack: %v", err)
	}
}

func TestFullLoopActivates(t *testing.T) {
	st := openTest(t)
	v := New(st, 600)
	now := int64(5000)
	if err := st.CreatePeer(&store.Peer{ID: "n", Status: "pending", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	smokeID := "smoke-n-1"
	if err := st.SetSmoke("n", smokeID, now); err != nil {
		t.Fatal(err)
	}
	smokeSeq, err := st.InsertMessage(&store.Message{
		ID: smokeID, Sender: "system", Recipient: "n", Kind: "task",
		RootID: "verify/n", Payload: "smoke", CreatedAt: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := v.MarkVerifying("n", now); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPeer("n"); p.Status != "verifying" {
		t.Fatalf("mark: %s", p.Status)
	}
	// Ack alone is not enough.
	if err := st.Ack(smokeSeq, "n", now); err != nil {
		t.Fatal(err)
	}
	if done, _ := v.CheckCompletion("n", now); done {
		t.Fatal("ack without result must not activate")
	}
	// Result completes the loop.
	if _, err := st.InsertMessage(&store.Message{
		ID: "r1", Sender: "n", Recipient: "system", Kind: "result",
		InReplyTo: smokeID, RootID: "verify/n", Payload: "收到", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if done, _ := v.CheckCompletion("n", now); !done {
		t.Fatal("should activate")
	}
	if p, _ := st.GetPeer("n"); p.Status != "active" {
		t.Fatalf("status: %s", p.Status)
	}
	if id, _, _ := st.GetSmoke("n"); id != "" {
		t.Fatal("smoke not cleared")
	}
	entries, _ := st.ListAudit("system", "peer.verified", 0, 10)
	if len(entries) != 1 {
		t.Fatal("missing peer.verified audit")
	}
}

func TestSuspendedNotOverridden(t *testing.T) {
	st := openTest(t)
	v := New(st, 600)
	now := int64(5000)
	if err := st.CreatePeer(&store.Peer{ID: "s", Status: "suspended", CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	smokeID := "smoke-s-1"
	_ = st.SetSmoke("s", smokeID, now)
	smokeSeq, _ := st.InsertMessage(&store.Message{
		ID: smokeID, Sender: "system", Recipient: "s", Kind: "task",
		RootID: "verify/s", Payload: "smoke", CreatedAt: now,
	})
	completeSmoke(t, st, "s", smokeID, smokeSeq, now)
	if done, _ := v.CheckCompletion("s", now); done {
		t.Fatal("suspended peer must not be activated by verification")
	}
	if p, _ := st.GetPeer("s"); p.Status != "suspended" {
		t.Fatalf("status changed: %s", p.Status)
	}
}

func TestSweepTimeoutReasons(t *testing.T) {
	st := openTest(t)
	v := New(st, 100)
	now := int64(10000)
	// never_polled: smoke issued, peer never heartbeat'd after.
	if err := st.CreatePeer(&store.Peer{ID: "p1", Status: "verifying", CreatedAt: now - 1000, LastSeen: now - 500}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetSmoke("p1", "smoke-p1", now-500) // lastSeen == smokeTs → never polled
	// polled: lastSeen after smoke.
	if err := st.CreatePeer(&store.Peer{ID: "p2", Status: "verifying", CreatedAt: now - 1000, LastSeen: now - 50}); err != nil {
		t.Fatal(err)
	}
	_ = st.SetSmoke("p2", "smoke-p2", now-500)
	// pending without smoke, very old → failed registered_never_verified.
	if err := st.CreatePeer(&store.Peer{ID: "p3", Status: "pending", CreatedAt: now - 1000}); err != nil {
		t.Fatal(err)
	}
	// pending without smoke, recent → untouched.
	if err := st.CreatePeer(&store.Peer{ID: "p4", Status: "pending", CreatedAt: now - 10}); err != nil {
		t.Fatal(err)
	}

	v.Sweep(now)

	if p, _ := st.GetPeer("p1"); p.Status != "failed" {
		t.Fatalf("p1: %s", p.Status)
	}
	if p, _ := st.GetPeer("p2"); p.Status != "failed" {
		t.Fatalf("p2: %s", p.Status)
	}
	if p, _ := st.GetPeer("p3"); p.Status != "failed" {
		t.Fatalf("p3: %s", p.Status)
	}
	if p, _ := st.GetPeer("p4"); p.Status != "pending" {
		t.Fatalf("p4 touched: %s", p.Status)
	}
	entries, _ := st.ListAudit("system", "peer.verify_failed", 0, 20)
	reasons := map[string]bool{}
	for _, e := range entries {
		reasons[e.Detail] = true
	}
	for _, want := range []string{"registered_never_polled", "polled_never_acked", "registered_never_verified"} {
		found := false
		for d := range reasons {
			if len(d) >= len(want) && contains(d, want) {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing reason %s in %v", want, reasons)
		}
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
