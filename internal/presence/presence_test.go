package presence

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
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

func TestBeatAndList(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", AgentType: "muse", Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	now := int64(2000)
	if err := s.Beat("a", 1, `{"shell":true}`, 0, now); err != nil {
		t.Fatalf("beat: %v", err)
	}
	views, err := s.List(now)
	if err != nil || len(views) != 1 {
		t.Fatalf("list: %+v (%v)", views, err)
	}
	if !views[0].Online || views[0].OfflineSecs != 0 {
		t.Fatalf("should be online: %+v", views[0])
	}
	p, _ := st.GetPeer("a")
	if p.LastSeen != now || p.ProtocolVer != 1 {
		t.Fatalf("touch/caps: %+v", p)
	}
	// Past the window → offline with age.
	views, _ = s.List(now + 301)
	if views[0].Online || views[0].OfflineSecs != 301 {
		t.Fatalf("should be offline 301s: %+v", views[0])
	}
	// Never-seen peer: offline without age.
	if err := st.CreatePeer(&store.Peer{ID: "ghost", Status: "pending", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	views, _ = s.List(now)
	for _, v := range views {
		if v.ID == "ghost" && (v.Online || v.OfflineSecs != 0) {
			t.Fatalf("ghost: %+v", v)
		}
	}
}

func TestSweepTransitionFiresOnce(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", Status: "active", CreatedAt: 1000, LastSeen: 2000}); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	var lastBody []byte
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(200)
	}))
	defer hook.Close()

	s := New(st, 300, hook.URL)
	// Baseline sweep: tracks without firing.
	s.Sweep(2100)
	if calls.Load() != 0 {
		t.Fatal("baseline sweep must not fire")
	}
	// Cross the timeout: exactly one event + webhook.
	s.Sweep(2301)
	if calls.Load() != 1 {
		t.Fatalf("webhook calls: %d", calls.Load())
	}
	var body map[string]any
	if err := json.Unmarshal(lastBody, &body); err != nil {
		t.Fatalf("webhook body: %v", err)
	}
	if body["peer_id"] != "a" || body["offline_secs"] != float64(301) {
		t.Fatalf("webhook payload: %s", lastBody)
	}
	entries, _ := st.ListAudit("system", "peer.offline", 0, 10)
	if len(entries) != 1 {
		t.Fatalf("audit: %+v", entries)
	}
	// Repeat sweep: no duplicate event while still offline.
	s.Sweep(2400)
	if calls.Load() != 1 {
		t.Fatal("duplicate offline event")
	}
	entries, _ = st.ListAudit("system", "peer.offline", 0, 10)
	if len(entries) != 1 {
		t.Fatal("duplicate audit")
	}
	// Heartbeat then sweep: online again, no event; next drop fires again.
	if err := s.Beat("a", 0, "", 0, 2500); err != nil {
		t.Fatal(err)
	}
	s.Sweep(2550)
	if calls.Load() != 1 {
		t.Fatal("spurious event on re-online")
	}
	s.Sweep(2900)
	if calls.Load() != 2 {
		t.Fatalf("second transition should fire: %d", calls.Load())
	}
}

func TestPromptVersionForwardOnly(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", AgentType: "muse", Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	now := int64(3000)
	if err := s.Beat("a", 0, "", 2, now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPeer("a")
	if p.PromptVersion != 2 || p.PromptUpdatedAt != now {
		t.Fatalf("prompt not recorded: %+v", p)
	}
	// Stale replay must not clobber.
	if err := s.Beat("a", 0, "", 1, now+10); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPeer("a")
	if p.PromptVersion != 2 || p.PromptUpdatedAt != now {
		t.Fatalf("stale clobbered: %+v", p)
	}
	// Zero means "not reporting": untouched.
	if err := s.Beat("a", 0, "", 0, now+20); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPeer("a")
	if p.PromptVersion != 2 {
		t.Fatalf("zero clobbered: %+v", p)
	}
	// List surfaces the fields.
	views, _ := s.List(now + 20)
	if len(views) != 1 || views[0].PromptVersion != 2 {
		t.Fatalf("list: %+v", views)
	}
}
