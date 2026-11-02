package muse

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

type fakeMuse struct {
	mu           sync.Mutex
	sessions     int
	wakes        int
	other        int
	sessionCode  int
	wakeCode     int
	vmState      string
	verifyState  string
	setCookies   bool
	redirect     bool
	lastWakeVM   string
	lastWakeBody map[string]any
	firstCookie  string
	lastCookie   string
}

func (f *fakeMuse) counts() (sessions, wakes, other int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.sessions, f.wakes, f.other
}

func (f *fakeMuse) wakeCount() int {
	_, wakes, _ := f.counts()
	return wakes
}

func (f *fakeMuse) firstCookieHeader() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.firstCookie
}

func (f *fakeMuse) lastWakeVMID() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastWakeVM
}

func (f *fakeMuse) wakeBody() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]any, len(f.lastWakeBody))
	for k, v := range f.lastWakeBody {
		out[k] = v
	}
	return out
}

func (f *fakeMuse) setSessionCode(code int) {
	f.mu.Lock()
	f.sessionCode = code
	f.mu.Unlock()
}

func (f *fakeMuse) setVerifyState(state string) {
	f.mu.Lock()
	f.verifyState = state
	f.mu.Unlock()
}

func (f *fakeMuse) setRedirect(v bool) {
	f.mu.Lock()
	f.redirect = v
	f.mu.Unlock()
}

func newFakeMuse(t *testing.T) (*fakeMuse, *httptest.Server) {
	t.Helper()
	f := &fakeMuse{vmState: "SUSPENDED", verifyState: "RUNNING", setCookies: true}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessions++
		if f.sessions == 1 {
			f.firstCookie = r.Header.Get("Cookie")
		}
		code, redirect, state := f.sessionCode, f.redirect, f.vmState
		if f.wakes > 0 {
			state = f.verifyState
		}
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		if redirect {
			http.Redirect(w, r, "/should-not-hit", http.StatusFound)
			return
		}
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		if f.setCookies {
			http.SetCookie(w, &http.Cookie{Name: "hatch_vml", Value: "renewed-vml", Path: "/", MaxAge: 172800})
			http.SetCookie(w, &http.Cookie{Name: "hatch_sess", Value: "renewed-sess", Path: "/", MaxAge: 2592000})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": "assigned", "vm_id": "vm-1", "vm_state": state,
		})
	})
	mux.HandleFunc("/api/hatch/vm/wake", func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.wakes++
		f.lastWakeBody = body
		if s, ok := body["vm_id"].(string); ok {
			f.lastWakeVM = s
		}
		code := f.wakeCode
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	mux.HandleFunc("/should-not-hit", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.other++
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

type testEnv struct {
	svc   *Service
	st    store.Store
	fake  *fakeMuse
	srv   *httptest.Server
	clock int64
}

func newTestEnv(t *testing.T, autoWake bool) *testEnv {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	for _, id := range []string{"alice", "bob"} {
		if err := st.CreatePeer(&store.Peer{ID: id, AgentType: "muse", Status: "active", CreatedAt: 1}); err != nil {
			t.Fatalf("peer %s: %v", id, err)
		}
	}
	fake, srv := newFakeMuse(t)
	env := &testEnv{st: st, fake: fake, srv: srv, clock: 1_900_000_000}
	env.svc, err = New(st, Options{
		BaseURL:           srv.URL,
		HTTPTimeout:       2 * time.Second,
		KeepaliveInterval: time.Hour,
		WakeCooldown:      time.Minute,
		DataDir:           t.TempDir(),
		AutoWake:          autoWake,
		Now:               func() time.Time { return time.Unix(env.clock, 0) },
	})
	if err != nil {
		t.Fatalf("muse.New: %v", err)
	}
	return env
}

func (e *testEnv) authorize(t *testing.T, peerID string) *Account {
	t.Helper()
	a, err := e.svc.Authorize(context.Background(), peerID, peerID+"@example.com",
		"hatch_sess=old-sess; hatch_vml=old-vml; hatch_gw=gw", true, true)
	if err != nil {
		t.Fatalf("authorize %s: %v", peerID, err)
	}
	return a
}

func TestAuthorizeRenewsWakesAndEncrypts(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	if !a.OKOrValid() {
		t.Fatalf("not authorized: %+v", a)
	}
	if a.AuthState != "ok" || a.VMState != "RUNNING" || !a.EssentialOK || a.SessionStatus != "assigned" {
		t.Fatalf("account: %+v", a)
	}
	if a.ExpiresAt != env.clock+172800 {
		t.Fatalf("expiry: got %d want %d", a.ExpiresAt, env.clock+172800)
	}
	if !contains(a.CookieNames, "hatch_sess") || !contains(a.CookieNames, "hatch_vml") {
		t.Fatalf("cookie names: %+v", a.CookieNames)
	}
	sessions, wakes, _ := env.fake.counts()
	if sessions != 2 || wakes != 1 {
		t.Fatalf("calls: sessions=%d wakes=%d", sessions, wakes)
	}
	if env.fake.lastWakeVMID() != "vm-1" {
		t.Fatalf("wake vm_id: %q", env.fake.lastWakeVMID())
	}
	if s, _ := env.fake.wakeBody()["connect_attempt_id"].(string); len(s) != 36 {
		t.Fatalf("connect_attempt_id: %v", env.fake.wakeBody())
	}
	if !strings.Contains(env.fake.firstCookieHeader(), "hatch_sess=old-sess") {
		t.Fatalf("authorize did not send pasted cookie: %q", env.fake.firstCookieHeader())
	}

	row, err := env.st.GetMuseAccountByPeer("alice")
	if err != nil || row == nil {
		t.Fatalf("row: %+v (%v)", row, err)
	}
	if strings.Contains(row.CookiesEnc, "hatch_sess") || strings.Contains(row.CookiesEnc, "old-sess") {
		t.Fatalf("ciphertext leaks cookie material: %q", row.CookiesEnc)
	}
	raw, err := json.Marshal(a)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "old-sess") || strings.Contains(string(raw), "renewed-vml") {
		t.Fatalf("account view leaks cookie values: %s", raw)
	}

	// A second binding for the same peer is rejected.
	if _, err := env.svc.Authorize(context.Background(), "alice", "", "hatch_sess=x; hatch_vml=y", true, true); !errors.Is(err, ErrAlreadyAuthorized) {
		t.Fatalf("duplicate: %v", err)
	}
}

func TestAuthorizeRejectsMissingCookiesAndUnknownPeer(t *testing.T) {
	env := newTestEnv(t, true)
	if _, err := env.svc.Authorize(context.Background(), "alice", "", "hatch_sess=x", true, true); !errors.Is(err, ErrMissingCookies) {
		t.Fatalf("missing: %v", err)
	}
	if _, err := env.svc.Authorize(context.Background(), "nobody", "", "hatch_sess=x; hatch_vml=y", true, true); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("unknown peer: %v", err)
	}
	if _, err := env.svc.Authorize(context.Background(), "alice", "", "not-a-cookie", true, true); !errors.Is(err, ErrBadCookies) {
		t.Fatalf("bad format: %v", err)
	}
	if list, _ := env.svc.List(); len(list) != 0 {
		t.Fatalf("failed authorization must not persist: %+v", list)
	}
}

func TestAuthorize401DoesNotPersist(t *testing.T) {
	env := newTestEnv(t, true)
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusUnauthorized
	env.fake.mu.Unlock()
	if _, err := env.svc.Authorize(context.Background(), "alice", "", "hatch_sess=x; hatch_vml=y", true, true); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: %v", err)
	}
	if list, _ := env.svc.List(); len(list) != 0 {
		t.Fatalf("401 persisted a row: %+v", list)
	}
}

func TestReauthorize401KeepsOldCookies(t *testing.T) {
	env := newTestEnv(t, true)
	env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusUnauthorized
	env.fake.mu.Unlock()
	if _, err := env.svc.Reauthorize(context.Background(), mustAccountID(t, env, "alice"), "hatch_sess=new-sess; hatch_vml=new-vml"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("reauth 401: %v", err)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_sess"] != "renewed-sess" {
		t.Fatalf("old cookies were replaced on 401: %+v", cookies)
	}
}

func TestTransientFailureKeepsCookiesAndAuth(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	id := a.ID
	env.clock += 100
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusForbidden
	env.fake.mu.Unlock()
	res, err := env.svc.Keepalive(context.Background(), id)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK || res.AuthState != "ok" || !strings.Contains(res.Error, "403") {
		t.Fatalf("transient result: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "ok" || row.LastAttemptOK || row.LastKeepaliveAt != 1_900_000_000 {
		t.Fatalf("transient clobbered state: %+v", row)
	}
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_sess"] != "renewed-sess" {
		t.Fatalf("cookies lost on 403: %+v", cookies)
	}
}

func TestKeepalive401MarksNeedsReauth(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusUnauthorized
	env.fake.mu.Unlock()
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK || res.AuthState != "needs_reauth" {
		t.Fatalf("401 result: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "needs_reauth" {
		t.Fatalf("401 state: %+v", row)
	}
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_sess"] != "renewed-sess" {
		t.Fatalf("401 wiped cookies: %+v", cookies)
	}
}

func TestWakeUsesFreshVMIDAndVerifies(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	row, _ := env.st.GetMuseAccountByPeer("alice")
	row.VMID = "vm-stale"
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !res.OK || !res.WakeRequested || !res.WakeOK || res.VMState != "RUNNING" {
		t.Fatalf("wake result: %+v", res)
	}
	if env.fake.lastWakeVMID() != "vm-1" {
		t.Fatalf("wake used stale vm_id: %q", env.fake.lastWakeVMID())
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt == 0 || !row.LastWakeOK {
		t.Fatalf("wake not recorded: %+v", row)
	}
}

func TestWakeNotConfirmedIsNotSuccess(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.verifyState = "SUSPENDED"
	env.fake.mu.Unlock()
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !res.OK || !res.WakeRequested || res.WakeOK {
		t.Fatalf("unconfirmed wake must be OK but WakeOK=false: %+v", res)
	}
	if !strings.Contains(res.Error, "not confirmed") {
		t.Fatalf("missing diagnostic: %+v", res)
	}
}

func TestNoRedirectFollowed(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.redirect = true
	env.fake.mu.Unlock()
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK {
		t.Fatalf("redirect treated as success: %+v", res)
	}
	if _, _, other := env.fake.counts(); other != 0 {
		t.Fatalf("redirect target was followed: %d", other)
	}
}

func TestKeepaliveAllSkipsAndForce(t *testing.T) {
	env := newTestEnv(t, true)
	alice := env.authorize(t, "alice")
	env.authorize(t, "bob")
	if _, err := env.svc.Update(mustAccountID(t, env, "bob"), nil, nil, nil, boolPtr(false)); err != nil {
		t.Fatal(err)
	}
	sum, err := env.svc.KeepaliveAll(context.Background(), false)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if sum.Total != 2 || sum.OK != 1 || sum.Skipped != 1 {
		t.Fatalf("summary: %+v", sum)
	}
	// A needs_reauth account is skipped by the background sweep but retried by
	// a manual (force) run.
	row, _ := env.st.GetMuseAccount(alice.ID)
	row.AuthState = "needs_reauth"
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	sum, _ = env.svc.KeepaliveAll(context.Background(), false)
	if sum.Skipped != 2 {
		t.Fatalf("background should skip needs_reauth: %+v", sum)
	}
	sum, _ = env.svc.KeepaliveAll(context.Background(), true)
	if sum.OK != 1 {
		t.Fatalf("force should retry needs_reauth: %+v", sum)
	}
}

func TestMaybeWakeOfflineGuards(t *testing.T) {
	env := newTestEnv(t, true)
	env.authorize(t, "alice")
	row, _ := env.st.GetMuseAccountByPeer("alice")
	row.LastWakeAt = env.clock
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	before := env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	time.Sleep(50 * time.Millisecond)
	if env.fake.wakeCount() != before {
		t.Fatal("cooldown was ignored")
	}

	row.LastWakeAt = 0
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	env.svc.MaybeWakeOffline("alice", 600)
	waitFor(t, func() bool { return env.fake.wakeCount() > before })

	// Suspended peers are not woken.
	if err := env.st.UpdatePeerStatus("alice", "suspended"); err != nil {
		t.Fatal(err)
	}
	row.LastWakeAt = 0
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	before = env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	time.Sleep(50 * time.Millisecond)
	if env.fake.wakeCount() != before {
		t.Fatal("suspended peer was woken")
	}
}

func TestAutoWakeDisabledGlobally(t *testing.T) {
	env := newTestEnv(t, false)
	env.authorize(t, "alice")
	before := env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	time.Sleep(50 * time.Millisecond)
	if env.fake.wakeCount() != before {
		t.Fatal("auto_wake=false still woke")
	}
}

func TestKeepaliveDoesNotRecordWake(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt == 0 {
		t.Fatalf("authorize wake not recorded: %+v", row)
	}
	// Reset the authorization's wake marker: a background keepalive must not
	// look like a new wake event.
	row.LastWakeAt = 0
	row.LastWakeOK = false
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	if _, err := env.svc.Keepalive(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt != 0 {
		t.Fatalf("keepalive recorded a wake: %+v", row)
	}
	if _, err := env.svc.Wake(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt == 0 {
		t.Fatalf("wake not recorded: %+v", row)
	}
}

func TestUpdateAndRevoke(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.authorize(t, "bob")
	if _, err := env.svc.Update(a.ID, nil, strPtr("bob"), nil, nil); !errors.Is(err, ErrAlreadyAuthorized) {
		t.Fatalf("rebind to a bound peer: %v", err)
	}
	updated, err := env.svc.Update(a.ID, strPtr("new label"), nil, boolPtr(false), boolPtr(false))
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if updated.PeerID != "alice" || updated.Label != "new label" || updated.AutoWake || updated.Enabled {
		t.Fatalf("update: %+v", updated)
	}
	if err := env.svc.Revoke(a.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := env.svc.Get(a.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("get after revoke: %v", err)
	}
}

func TestStatusLoopEnabled(t *testing.T) {
	env := newTestEnv(t, true)
	st := env.svc.Status()
	if !st.LoopEnabled || st.Running {
		t.Fatalf("status: %+v", st)
	}
}

func TestNewValidation(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := New(nil, Options{DataDir: t.TempDir()}); err == nil {
		t.Fatal("nil store accepted")
	}
	if _, err := New(st, Options{}); err == nil {
		t.Fatal("missing data_dir accepted")
	}
	if _, err := New(st, Options{DataDir: t.TempDir(), BaseURL: "http://muse.ai"}); err == nil {
		t.Fatal("non-loopback http accepted")
	}
	if _, err := New(st, Options{DataDir: t.TempDir(), BaseURL: "https://muse.ai", ProxyURL: "ftp://127.0.0.1:1"}); err == nil {
		t.Fatal("bad proxy scheme accepted")
	}
	svc, err := New(st, Options{DataDir: t.TempDir(), BaseURL: "http://127.0.0.1:1"})
	if err != nil || svc == nil {
		t.Fatalf("loopback http: %+v (%v)", svc, err)
	}
}

func mustAccountID(t *testing.T, env *testEnv, peer string) string {
	t.Helper()
	a, err := env.st.GetMuseAccountByPeer(peer)
	if err != nil || a == nil {
		t.Fatalf("account %s: %+v (%v)", peer, a, err)
	}
	return a.ID
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func boolPtr(b bool) *bool    { return &b }
func strPtr(s string) *string { return &s }
func waitFor(t *testing.T, fn func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if fn() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition not met in time")
}

// OKOrValid is a tiny test helper: the account is usable when the first
// renewal confirmed the session.
func (a *Account) OKOrValid() bool { return a.AuthState == "ok" }
