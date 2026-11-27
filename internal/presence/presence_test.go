package presence

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/AlixWang/agent-relay/internal/effects"
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
	if err := s.Beat("a", 1, `{"shell":true}`, 0, "", "", "", "", now); err != nil {
		t.Fatalf("beat: %v", err)
	}
	views, err := s.List(now, nil)
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
	views, _ = s.List(now+301, nil)
	if views[0].Online || views[0].OfflineSecs != 301 {
		t.Fatalf("should be offline 301s: %+v", views[0])
	}
	// Never-seen peer: offline without age.
	if err := st.CreatePeer(&store.Peer{ID: "ghost", Status: "pending", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	views, _ = s.List(now, nil)
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
	var lastKind, lastKey string
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		lastBody, _ = io.ReadAll(r.Body)
		lastKind = r.Header.Get("X-Agent-Relay-Effect-Kind")
		lastKey = r.Header.Get("X-Agent-Relay-Effect-Key")
		w.WriteHeader(200)
	}))
	defer hook.Close()

	s := New(st, 300, hook.URL)
	var offCalls atomic.Int32
	var offPeer string
	var offSecs int64
	s.OnOffline = func(peerID string, offlineSecs int64) {
		offCalls.Add(1)
		offPeer, offSecs = peerID, offlineSecs
	}
	// Baseline sweep: tracks without firing.
	s.Sweep(2100)
	if calls.Load() != 0 || offCalls.Load() != 0 {
		t.Fatal("baseline sweep must not fire")
	}
	// Cross the timeout: exactly one event + queued webhook + callback.
	s.Sweep(2301)
	fx := effects.New(st)
	fx.RegisterWebhook()
	if err := fx.RunOnce(context.Background()); err != nil {
		t.Fatalf("effects: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("webhook calls: %d", calls.Load())
	}
	if offCalls.Load() != 1 || offPeer != "a" || offSecs != 301 {
		t.Fatalf("offline callback: calls=%d peer=%q secs=%d", offCalls.Load(), offPeer, offSecs)
	}
	var body map[string]any
	if err := json.Unmarshal(lastBody, &body); err != nil {
		t.Fatalf("webhook body: %v", err)
	}
	if body["peer_id"] != "a" || body["offline_secs"] != float64(301) {
		t.Fatalf("webhook payload: %s", lastBody)
	}
	if body["kind"] != "webhook.offline" || body["key"] == "" ||
		lastKind != "webhook.offline" || lastKey == "" {
		t.Fatalf("webhook idempotency fields: body=%s headers=%q/%q", lastBody, lastKind, lastKey)
	}
	entries, _ := st.ListAudit("system", "peer.offline", 0, 10)
	if len(entries) != 1 {
		t.Fatalf("audit: %+v", entries)
	}
	// Repeat sweep: no duplicate event while still offline.
	s.Sweep(2400)
	if calls.Load() != 1 || offCalls.Load() != 1 {
		t.Fatal("duplicate offline event")
	}
	entries, _ = st.ListAudit("system", "peer.offline", 0, 10)
	if len(entries) != 1 {
		t.Fatal("duplicate audit")
	}
	// Heartbeat then sweep: online again, no event; next drop fires again.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", 2500); err != nil {
		t.Fatal(err)
	}
	s.Sweep(2550)
	if calls.Load() != 1 {
		t.Fatal("spurious event on re-online")
	}
	s.Sweep(2900)
	if err := fx.RunOnce(context.Background()); err != nil {
		t.Fatalf("effects second: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("second transition should fire: %d", calls.Load())
	}
	if offCalls.Load() != 2 {
		t.Fatalf("offline callback second transition: %d", offCalls.Load())
	}
	// The second flap is inside the 30-minute audit window: the webhook and
	// callback still fire, but the audit trail gets no duplicate row.
	entries, _ = st.ListAudit("system", "peer.offline", 0, 10)
	if len(entries) != 1 {
		t.Fatalf("throttled transition wrote audit: %+v", entries)
	}
}

func TestOfflineAuditThrottledPerPeer(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", Status: "active", CreatedAt: 1000, LastSeen: 2000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	var offCalls atomic.Int32
	s.OnOffline = func(string, int64) { offCalls.Add(1) }
	audits := func() []*store.AuditEntry {
		t.Helper()
		entries, err := st.ListAudit("system", "peer.offline", 0, 10)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		return entries
	}

	s.Sweep(2100) // baseline
	s.Sweep(2301) // first transition -> 1 audit row
	if got := audits(); len(got) != 1 {
		t.Fatalf("first transition audits: %+v", got)
	}
	// Online again, then drop 400s after the first audit.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", 2500); err != nil {
		t.Fatal(err)
	}
	s.Sweep(2550)
	s.Sweep(2900)
	if got := audits(); len(got) != 1 {
		t.Fatalf("flap inside window wrote audit: %+v", got)
	}
	if offCalls.Load() != 2 {
		t.Fatalf("flap notification count: %d", offCalls.Load())
	}
	// 6+ hours after the last audit, a new transition is recorded again.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", 2301+6*3600+1); err != nil {
		t.Fatal(err)
	}
	s.Sweep(2301 + 6*3600 + 50)
	s.Sweep(2301 + 6*3600 + 350)
	if got := audits(); len(got) != 2 {
		t.Fatalf("transition after window missing audit: %+v", got)
	}
	if offCalls.Load() != 3 {
		t.Fatalf("notifications: %d", offCalls.Load())
	}
}

func TestOfflineAuditSuppressedByCallback(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", Status: "active", CreatedAt: 1000, LastSeen: 2000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	var offCalls atomic.Int32
	s.OnOffline = func(string, int64) { offCalls.Add(1) }
	s.ShouldAuditOffline = func(string) bool { return false }

	s.Sweep(2100)
	s.Sweep(2301)
	if offCalls.Load() != 1 {
		t.Fatalf("OnOffline calls = %d, want 1", offCalls.Load())
	}
	entries, err := st.ListAudit("system", "peer.offline", 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("suppressed peer wrote peer.offline audit: %+v", entries)
	}
}

func TestPromptVersionForwardOnly(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", AgentType: "muse", Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	now := int64(3000)
	if err := s.Beat("a", 0, "", 2, "", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPeer("a")
	if p.PromptVersion != 2 || p.PromptUpdatedAt != now {
		t.Fatalf("prompt not recorded: %+v", p)
	}
	// Stale replay must not clobber.
	if err := s.Beat("a", 0, "", 1, "", "", "", "", now+10); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPeer("a")
	if p.PromptVersion != 2 || p.PromptUpdatedAt != now {
		t.Fatalf("stale clobbered: %+v", p)
	}
	// Zero means "not reporting": untouched.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", now+20); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPeer("a")
	if p.PromptVersion != 2 {
		t.Fatalf("zero clobbered: %+v", p)
	}
	// List surfaces the fields.
	views, _ := s.List(now+20, nil)
	if len(views) != 1 || views[0].PromptVersion != 2 {
		t.Fatalf("list: %+v", views)
	}
}

func TestProfileViaBeat(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", AgentType: "muse", Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	now := int64(4000)
	// Non-empty overwrites with timestamp.
	if err := s.Beat("a", 0, "", 0, "做巡检", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPeer("a")
	if p.Profile != "做巡检" || p.ProfileUpdatedAt != now {
		t.Fatalf("profile: %+v", p)
	}
	// Empty keeps the stored value.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", now+10); err != nil {
		t.Fatal(err)
	}
	p, _ = st.GetPeer("a")
	if p.Profile != "做巡检" || p.ProfileUpdatedAt != now {
		t.Fatalf("empty clobbered: %+v", p)
	}
	views, _ := s.List(now+10, nil)
	if len(views) != 1 || views[0].Profile != "做巡检" {
		t.Fatalf("list: %+v", views)
	}
}

func TestTransportMarksLive(t *testing.T) {
	st := openTest(t)
	for _, id := range []string{"a", "b"} {
		if err := st.CreatePeer(&store.Peer{ID: id, Status: "active", CreatedAt: 1000}); err != nil {
			t.Fatal(err)
		}
	}
	s := New(st, 300, "")
	now := int64(5000)
	if err := s.Beat("a", 0, "", 0, "", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	if err := s.Beat("b", 0, "", 0, "", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	views, _ := s.List(now, map[string]bool{"a": true})
	got := map[string]string{}
	for _, v := range views {
		got[v.ID] = v.Transport
	}
	if got["a"] != "sse" || got["b"] != "poll" {
		t.Fatalf("transport: %+v", got)
	}
	// Nil live map → all poll.
	views, _ = s.List(now, nil)
	for _, v := range views {
		if v.Transport != "poll" {
			t.Fatalf("nil live: %+v", v)
		}
	}
}

func TestSweepOnStillOfflineBaselineAndRepeat(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "offline", Status: "active", CreatedAt: 1000, LastSeen: 1000}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePeer(&store.Peer{ID: "online", Status: "active", CreatedAt: 1000, LastSeen: 2050}); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePeer(&store.Peer{ID: "ghost", Status: "pending", CreatedAt: 1000, LastSeen: 0}); err != nil {
		t.Fatal(err)
	}

	s := New(st, 300, "")
	offCounts := map[string]int{}
	stillCounts := map[string]int{}
	lastStillSecs := map[string]int64{}
	s.OnOffline = func(peerID string, _ int64) {
		offCounts[peerID]++
	}
	s.OnStillOffline = func(peerID string, offlineSecs int64) {
		stillCounts[peerID]++
		lastStillSecs[peerID] = offlineSecs
	}

	// Baseline sweep: already-offline peer triggers OnStillOffline without
	// emitting OnOffline or a peer.offline audit row. Never-seen ghost stays silent.
	s.Sweep(2100)
	if offCounts["offline"] != 0 || stillCounts["offline"] != 1 || lastStillSecs["offline"] != 1100 {
		t.Fatalf("baseline offline: off=%v still=%v secs=%v", offCounts, stillCounts, lastStillSecs)
	}
	if stillCounts["ghost"] != 0 || offCounts["ghost"] != 0 {
		t.Fatalf("ghost must never trigger offline callbacks: off=%v still=%v", offCounts, stillCounts)
	}
	if entries, _ := st.ListAudit("system", "peer.offline", 0, 10); len(entries) != 0 {
		t.Fatalf("baseline sweep wrote audit: %+v", entries)
	}

	// Repeat sweep while still offline: OnStillOffline fires again; OnOffline stays 0.
	s.Sweep(2200)
	if offCounts["offline"] != 0 || stillCounts["offline"] != 2 || lastStillSecs["offline"] != 1200 {
		t.Fatalf("repeat offline: off=%v still=%v secs=%v", offCounts, stillCounts, lastStillSecs)
	}

	// Online peer crosses timeout: fires OnOffline (not OnStillOffline) once.
	s.Sweep(2400)
	if offCounts["online"] != 1 || stillCounts["online"] != 0 {
		t.Fatalf("transition tick: off=%v still=%v", offCounts, stillCounts)
	}
	if stillCounts["offline"] != 3 {
		t.Fatalf("still offline peer missed tick: %v", stillCounts)
	}

	// Subsequent sweep while online peer remains offline: fires OnStillOffline, not OnOffline.
	s.Sweep(2500)
	if offCounts["online"] != 1 || stillCounts["online"] != 1 || lastStillSecs["online"] != 450 {
		t.Fatalf("post-transition offline: off=%v still=%v secs=%v", offCounts, stillCounts, lastStillSecs)
	}
}

func TestClientVersionViaBeat(t *testing.T) {
	st := openTest(t)
	if err := st.CreatePeer(&store.Peer{ID: "a", AgentType: "muse", Status: "active", CreatedAt: 1000}); err != nil {
		t.Fatal(err)
	}
	s := New(st, 300, "")
	now := int64(3000)
	// Empty = not reporting (shell scripts): untouched.
	if err := s.Beat("a", 0, "", 0, "", "", "", "", now); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPeer("a"); p.ClientVersion != "" {
		t.Fatalf("empty clobbered: %+v", p)
	}
	// Non-empty overwrites unconditionally (opaque tags, no ordering).
	if err := s.Beat("a", 0, "", 0, "", "v0.7.0", "", "", now+10); err != nil {
		t.Fatal(err)
	}
	p, _ := st.GetPeer("a")
	if p.ClientVersion != "v0.7.0" || p.ClientUpdatedAt != now+10 {
		t.Fatalf("client not recorded: %+v", p)
	}
	// Downgrade tag also overwrites (no forward-only gate for clients).
	if err := s.Beat("a", 0, "", 0, "", "v0.6.0", "", "", now+20); err != nil {
		t.Fatal(err)
	}
	if p, _ := st.GetPeer("a"); p.ClientVersion != "v0.6.0" {
		t.Fatalf("overwrite blocked: %+v", p)
	}
	// List surfaces the fields.
	views, _ := s.List(now+20, nil)
	if len(views) != 1 || views[0].ClientVersion != "v0.6.0" {
		t.Fatalf("list: %+v", views)
	}
}
