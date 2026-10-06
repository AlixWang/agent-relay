package queue

import (
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/guard"
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

func testSvc(st store.Store) *Service {
	g := guard.New(st, guard.Limits{FuseMaxMessages: 50, FuseMaxAgeSecs: 86400, RatePerMinute: 60})
	return New(st, g)
}

func mustPeer(t *testing.T, st store.Store, id string) {
	t.Helper()
	now := time.Now().Unix()
	if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: now}); err != nil {
		t.Fatalf("peer: %v", err)
	}
}

func TestDirectAndAck(t *testing.T) {
	st := openTestStore(t)
	mustPeer(t, st, "muse-a")
	mustPeer(t, st, "muse-b")
	q := testSvc(st)
	now := time.Now().Unix()
	seq, held, _, err := q.Send(&SendRequest{ID: "t1", To: "muse-b", From: "muse-a", Payload: "hi"}, now)
	if err != nil || held || seq == 0 {
		t.Fatalf("send: %v held=%v seq=%d", err, held, seq)
	}
	// Sender does not see their own direct message; recipient does.
	if vis, _ := q.Visible("muse-a", 0, 50); len(vis) != 0 {
		t.Fatalf("sender sees own message: %d", len(vis))
	}
	vis, _ := q.Visible("muse-b", 0, 50)
	if len(vis) != 1 || vis[0].Seq != seq {
		t.Fatalf("recipient view: %+v", vis)
	}
	// Ack hides it; since-cursor advances.
	if _, err := q.AckByID("t1", "muse-b", now); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if vis, _ := q.Visible("muse-b", 0, 50); len(vis) != 0 {
		t.Fatalf("acked message still visible")
	}
	if vis, _ := q.Visible("muse-b", seq, 50); len(vis) != 0 {
		t.Fatalf("since cursor broken")
	}
}

func TestBroadcastFanout(t *testing.T) {
	st := openTestStore(t)
	for _, id := range []string{"muse-a", "muse-b", "muse-c"} {
		mustPeer(t, st, id)
	}
	q := testSvc(st)
	now := time.Now().Unix()
	if _, _, _, err := q.Send(&SendRequest{ID: "b1", To: "*", From: "muse-a", Payload: "broadcast hello everyone"}, now); err != nil {
		t.Fatalf("send: %v", err)
	}
	// Sender excluded; both others see it.
	if vis, _ := q.Visible("muse-a", 0, 50); len(vis) != 0 {
		t.Fatalf("sender sees own broadcast")
	}
	for _, id := range []string{"muse-b", "muse-c"} {
		if vis, _ := q.Visible(id, 0, 50); len(vis) != 1 {
			t.Fatalf("%s should see broadcast", id)
		}
	}
	// One recipient acks; the other still sees it (ack isolation).
	if _, err := q.AckByID("b1", "muse-b", now); err != nil {
		t.Fatalf("ack: %v", err)
	}
	if vis, _ := q.Visible("muse-b", 0, 50); len(vis) != 0 {
		t.Fatalf("muse-b still sees acked broadcast")
	}
	if vis, _ := q.Visible("muse-c", 0, 50); len(vis) != 1 {
		t.Fatalf("muse-c lost broadcast after muse-b ack")
	}
}

func TestHeldInvisibleUntilApproved(t *testing.T) {
	st := openTestStore(t)
	mustPeer(t, st, "muse-a")
	mustPeer(t, st, "muse-b")
	q := testSvc(st)
	now := time.Now().Unix()
	seq, held, _, err := q.Send(&SendRequest{ID: "h1", To: "muse-b", From: "muse-a",
		Payload: "sensitive action here", RequiresApproval: true}, now)
	if err != nil || !held {
		t.Fatalf("send: %v held=%v", err, held)
	}
	if vis, _ := q.Visible("muse-b", 0, 50); len(vis) != 0 {
		t.Fatalf("held message visible before approval")
	}
	if err := st.ApproveMessage(seq, true); err != nil {
		t.Fatalf("approve: %v", err)
	}
	if vis, _ := q.Visible("muse-b", 0, 50); len(vis) != 1 {
		t.Fatalf("approved message not visible")
	}
}

func TestSmokeRetryIdempotent(t *testing.T) {
	st := openTestStore(t)
	mustPeer(t, st, "muse-a")
	q := testSvc(st)
	seq1, id1, err := q.Smoke("muse-a")
	if err != nil || seq1 == 0 || id1 == "" {
		t.Fatalf("smoke: %v %d %s", err, seq1, id1)
	}
	// Immediate retry returns the same smoke, no duplicate row.
	seq2, id2, err := q.Smoke("muse-a")
	if err != nil || seq2 != seq1 || id2 != id1 {
		t.Fatalf("retry: %v %d/%d %s/%s", err, seq1, seq2, id1, id2)
	}
	msgs, _ := st.GetByIDAnySender(id1)
	n := 0
	for _, m := range msgs {
		if m.Sender == "system" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("duplicate smoke rows: %d", n)
	}
}
