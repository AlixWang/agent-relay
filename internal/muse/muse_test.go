package muse

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse/musetest"
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

	// Test-only overrides for rotation/error fixtures.
	sessionStatus  string
	sessionSet     []string
	errorSetCookie string
	retryAfter     string
	wakeSet        []string
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
		if state == "" {
			state = f.verifyState
		}
		status := f.sessionStatus
		if status == "" {
			status = "assigned"
		}
		customSet := append([]string(nil), f.sessionSet...)
		errSet := f.errorSetCookie
		retryAfter := f.retryAfter
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		if redirect {
			http.Redirect(w, r, "/should-not-hit", http.StatusFound)
			return
		}
		if code != 0 {
			if errSet != "" {
				w.Header().Add("Set-Cookie", errSet)
			}
			if retryAfter != "" {
				w.Header().Set("Retry-After", retryAfter)
			}
			w.WriteHeader(code)
			return
		}
		if f.setCookies {
			http.SetCookie(w, &http.Cookie{Name: "hatch_vml", Value: "renewed-vml", Path: "/", MaxAge: 172800})
			http.SetCookie(w, &http.Cookie{Name: "hatch_sess", Value: "renewed-sess", Path: "/", MaxAge: 2592000})
		}
		for _, c := range customSet {
			w.Header().Add("Set-Cookie", c)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": status, "vm_id": "vm-1", "vm_state": state,
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
		wakeSet := append([]string(nil), f.wakeSet...)
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		for _, c := range wakeSet {
			w.Header().Add("Set-Cookie", c)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	mux.HandleFunc("/api/auth/check", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": "test-access"})
	})
	mux.HandleFunc("/api/hatch/token", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "test-hatch"})
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
	// Most tests exercise the renew state machine, not the Noise transport.
	// The mock gateway has its own tests; here a successful verifier keeps the
	// old wake expectations meaningful without opening a socket.
	env.svc.verifyWake = func(context.Context, *renewResult, *sessionResponse) error { return nil }
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
	if _, err := env.svc.Authorize(context.Background(), "alice", "", "hatch_vml=x", true, true); !errors.Is(err, ErrMissingCookies) {
		t.Fatalf("missing: %v", err)
	} else if msg := err.Error(); !strings.Contains(msg, "hatch_sess") || strings.Contains(msg, "hatch_vml") {
		t.Fatalf("missing-cookie error should name only hatch_sess: %q", msg)
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

func TestAuthorizeAcceptsSessionOnlyCookie(t *testing.T) {
	env := newTestEnv(t, true)
	a, err := env.svc.Authorize(context.Background(), "alice", "", "hatch_sess=only-sess", true, true)
	if err != nil {
		t.Fatalf("session-only cookie rejected: %v", err)
	}
	if !a.EssentialOK || a.AuthState != "ok" || a.VMState != "RUNNING" {
		t.Fatalf("account: %+v", a)
	}
	if !contains(a.CookieNames, "hatch_vml") {
		t.Fatalf("session renewal should have stored hatch_vml: %+v", a.CookieNames)
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
	if !strings.Contains(res.Error, "访问被拒绝") {
		t.Fatalf("403 hint missing: %q", res.Error)
	}
	if res.VMState != "RUNNING" || res.VMStateAt != 1_900_000_000 {
		t.Fatalf("transient result lost confirmed VM state: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "ok" || !row.LastAttemptOK || row.LastError != "" ||
		row.LastKeepaliveAt != 1_900_000_000 || row.LastAttemptAt != 1_900_000_100 ||
		row.VMState != "RUNNING" || row.VMStateAt != 1_900_000_000 {
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
	if !res.OK || !res.WakeRequested || !res.WakeOK || res.VMState != "RUNNING" ||
		res.Error != "" || res.WakeError != "" || res.WakeState != "noise_confirmed" {
		t.Fatalf("wake result: %+v", res)
	}
	if env.fake.lastWakeVMID() != "vm-1" {
		t.Fatalf("wake used stale vm_id: %q", env.fake.lastWakeVMID())
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt == 0 || !row.LastWakeOK || row.LastWakeError != "" || row.LastWakeState != "noise_confirmed" {
		t.Fatalf("wake not recorded: %+v", row)
	}
}

func TestWakeNotConfirmedIsNotSuccess(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.svc.verifyWake = func(context.Context, *renewResult, *sessionResponse) error {
		return errors.New("noise rpc: unread 500 (attempt 3/3, 3.0s)")
	}
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !res.OK || !res.WakeRequested || res.WakeOK {
		t.Fatalf("unconfirmed wake must be OK but WakeOK=false: %+v", res)
	}
	if res.Error != "" {
		t.Fatalf("wake failure leaked into session error: %+v", res)
	}
	if !strings.Contains(res.WakeError, "noise rpc") || res.WakeState != "unconfirmed" {
		t.Fatalf("missing wake diagnostic: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.LastError != "" {
		t.Fatalf("wake failure marked session broken: %+v", row)
	}
	if row.LastWakeAt == 0 || row.LastWakeOK || row.LastWakeState != "unconfirmed" ||
		!strings.Contains(row.LastWakeError, "noise rpc") {
		t.Fatalf("wake diagnosis not persisted separately: %+v", row)
	}
}

func TestRenewUsesRealNoiseVerifier(t *testing.T) {
	env := newTestEnv(t, true)
	ws := musetest.New(t)
	env.svc.gatewayURL = ws.URL
	env.svc.verifyWake = env.svc.verifyNoise
	a := env.authorize(t, "alice") // authorize performs one real verification
	if a.LastWakeState != "noise_confirmed" || !a.LastWakeOK {
		t.Fatalf("authorize did not record a Noise-confirmed wake: %+v", a)
	}
	env.clock += 10
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !res.OK || !res.WakeOK || res.WakeState != "noise_confirmed" || res.WakeError != "" {
		t.Fatalf("wake result: %+v", res)
	}
	if got := len(ws.Requests()); got < 2 {
		t.Fatalf("real verifier did not issue RPCs: %d", got)
	}
}

func TestRenewNoiseIgnoresRUNNINGVMState(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	calls := 0
	env.svc.verifyWake = func(context.Context, *renewResult, *sessionResponse) error {
		calls++
		return nil
	}
	env.fake.mu.Lock()
	env.fake.verifyState = "RUNNING"
	env.fake.mu.Unlock()
	env.clock += 10
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if calls != 1 {
		t.Fatalf("vm_state=RUNNING took a shortcut around Noise: calls=%d", calls)
	}
	if !res.WakeOK || res.WakeState != "noise_confirmed" {
		t.Fatalf("result: %+v", res)
	}
}

func TestRenewNoiseFailureKeepsSessionHealthyAndSealsCookies(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.svc.verifyWake = func(_ context.Context, res *renewResult, _ *sessionResponse) error {
		res.Cookies["hatch_gw"] = "rotated-by-token"
		res.Exp["hatch_gw"] = env.clock + 3600
		return &noiseError{
			Stage: "token", Class: "429", Cause: &MuseHTTPError{
				Op: "noise token", StatusCode: http.StatusTooManyRequests, RetryAfter: 120 * time.Second,
			},
		}
	}
	env.clock += 10
	res, err := env.svc.Wake(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("wake: %v", err)
	}
	if !res.OK || res.AuthState != "ok" || res.Error != "" {
		t.Fatalf("Noise failure polluted session health: %+v", res)
	}
	if res.WakeOK || res.WakeState != "unconfirmed" || !strings.Contains(res.WakeError, "noise token: 429") {
		t.Fatalf("wake result: %+v", res)
	}
	if at := env.svc.retryAt(a.ID); at < env.clock+119 {
		t.Fatalf("Retry-After from wrapped token error lost: at=%d result=%+v", at, res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.LastError != "" || !row.LastAttemptOK {
		t.Fatalf("Noise failure leaked into session error: %+v", row)
	}
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_gw"] != "rotated-by-token" {
		t.Fatalf("cookie rotated during verification was not sealed: %+v", cookies)
	}
}

func TestRenewNoiseErrorIsTruncatedTo500(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.svc.verifyWake = func(context.Context, *renewResult, *sessionResponse) error {
		return errors.New("noise rpc: " + strings.Repeat("x", 800))
	}
	env.clock += 10
	if _, err := env.svc.Wake(context.Background(), a.ID); err != nil {
		t.Fatalf("wake: %v", err)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if len(row.LastWakeError) != 500 {
		t.Fatalf("last_wake_error length = %d, want 500", len(row.LastWakeError))
	}
	if !strings.HasPrefix(row.LastWakeError, "noise rpc: ") {
		t.Fatalf("wake error prefix: %q", row.LastWakeError[:20])
	}
}

func TestApplyRenewNoiseConfirmedClearsWakeError(t *testing.T) {
	a := &store.MuseAccount{
		LastWakeOK: false, LastWakeState: "unconfirmed", LastWakeError: "old failure",
	}
	applyRenew(a, &renewResult{
		OK: true, AuthState: "ok", At: 30, WakeOK: true, WakeState: "noise_confirmed",
	}, 30, true)
	if a.LastError != "" || !a.LastWakeOK || a.LastWakeError != "" ||
		a.LastWakeState != "noise_confirmed" || a.LastWakeAt != 30 {
		t.Fatalf("noise-confirmed wake not applied cleanly: %+v", a)
	}
}

func TestApplyRenewKeepsErrorScopesSeparate(t *testing.T) {
	a := &store.MuseAccount{}
	applyRenew(a, &renewResult{
		OK: true, AuthState: "ok", At: 10,
		WakeOK: false, WakeState: "unconfirmed",
		WakeError: "wake not confirmed (vm_state=UNKNOWN)",
	}, 10, true)
	if a.LastError != "" || a.LastAttemptOK != true ||
		a.LastWakeState != "unconfirmed" || a.LastWakeError == "" || a.LastWakeOK {
		t.Fatalf("wake-only failure applied wrong: %+v", a)
	}
	if a.LastWakeAt != 10 {
		t.Fatalf("wake timestamp not recorded: %+v", a)
	}

	// A session failure does not erase the last wake diagnosis, and it must
	// not pretend a wake ran when the attempt stopped before that step.
	applyRenew(a, &renewResult{OK: false, Error: "muse session HTTP 500", At: 20}, 20, false)
	if a.LastError != "muse session HTTP 500" || a.LastWakeState != "unconfirmed" ||
		a.LastWakeError == "" || a.LastWakeAt != 10 || a.LastWakeOK {
		t.Fatalf("session failure clobbered wake scope: %+v", a)
	}

	applyRenew(a, &renewResult{
		OK: true, AuthState: "ok", At: 30, WakeOK: true, WakeState: "running",
	}, 30, true)
	if a.LastError != "" || !a.LastWakeOK || a.LastWakeError != "" ||
		a.LastWakeState != "running" || a.LastWakeAt != 30 {
		t.Fatalf("successful wake did not clear wake error: %+v", a)
	}
}

func TestListPromotesLegacyWakeError(t *testing.T) {
	env := newTestEnv(t, true)
	env.authorize(t, "alice")
	row, err := env.st.GetMuseAccountByPeer("alice")
	if err != nil || row == nil {
		t.Fatalf("get account: %v %+v", err, row)
	}
	const legacy = "wake not confirmed (vm_state=UNKNOWN)"
	row.AuthState = "ok"
	row.LastAttemptOK = true
	row.LastError = legacy
	row.LastWakeError = ""
	row.LastWakeState = ""
	row.LastWakeOK = true // stale success from before the failed wake
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatalf("save legacy account: %v", err)
	}
	list, err := env.svc.List()
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %+v", err, list)
	}
	got := list[0]
	if got.LastError != "" {
		t.Fatalf("legacy wake text still marks session broken: %+v", got)
	}
	if got.LastWakeError != legacy || got.LastWakeState != "unconfirmed" || got.LastWakeOK {
		t.Fatalf("legacy wake text was not preserved in wake scope: %+v", got)
	}

	// Genuine session errors must not be reclassified.
	genuine := &store.MuseAccount{LastError: "muse session HTTP 500"}
	promoteLegacyWakeError(genuine)
	if genuine.LastError != "muse session HTTP 500" || genuine.LastWakeError != "" {
		t.Fatalf("session error was reclassified: %+v", genuine)
	}
}

func TestManualWakeSessionFailureClearsStaleSuccess(t *testing.T) {
	a := &store.MuseAccount{
		LastWakeAt: 10, LastWakeOK: true, LastWakeState: "running",
	}
	res := &renewResult{Error: "muse session HTTP 500", At: 20}
	applyRenew(a, res, 20, true)
	if a.LastError != "muse session HTTP 500" || a.LastAttemptOK {
		t.Fatalf("session error not recorded: %+v", a)
	}
	if a.LastWakeAt != 20 || a.LastWakeOK || a.LastWakeState != "unconfirmed" ||
		a.LastWakeError == "" {
		t.Fatalf("failed manual wake left stale success: %+v", a)
	}
	if res.WakeState != "unconfirmed" || res.WakeError == "" {
		t.Fatalf("result did not expose skipped wake: %+v", res)
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
	// Fresh authorizations are inside the 6h/36h gate; age the account so
	// this test exercises the disabled/needs_reauth skips it is about.
	ageKeepalive(t, env, alice.ID, 7*time.Hour)
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

func TestKeepaliveGateMatrix(t *testing.T) {
	cases := []struct {
		name          string
		lastKeepalive time.Duration // 0 means never renewed
		expiresIn     time.Duration
		authState     string
		force         bool
		wantRun       bool
	}{
		{"never renewed", 0, 48 * time.Hour, "ok", false, true},
		{"fresh under 6h, beyond 36h, ok", 1 * time.Hour, 48 * time.Hour, "ok", false, false},
		{"stale beyond 6h, beyond 36h, ok", 7 * time.Hour, 48 * time.Hour, "ok", false, true},
		{"fresh under 6h but cookie inside 36h", 1 * time.Hour, 10 * time.Hour, "ok", false, true},
		{"stale beyond 6h and cookie inside 36h", 7 * time.Hour, 10 * time.Hour, "ok", false, true},
		{"fresh cookie but account not ok", 1 * time.Hour, 48 * time.Hour, "unverified", false, true},
		{"manual force bypasses the gate", 1 * time.Hour, 48 * time.Hour, "ok", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, true)
			a := env.authorize(t, "alice")
			setEssentialExpiry(t, env, a.ID, env.clock+int64(tc.expiresIn/time.Second))
			row, _ := env.st.GetMuseAccount(a.ID)
			if tc.lastKeepalive == 0 {
				row.LastKeepaliveAt = 0
			} else {
				row.LastKeepaliveAt = env.clock - int64(tc.lastKeepalive/time.Second)
			}
			row.AuthState = tc.authState
			if err := env.st.SaveMuseAccount(row); err != nil {
				t.Fatal(err)
			}
			sessionsBefore, wakesBefore, _ := env.fake.counts()
			sum, err := env.svc.KeepaliveAll(context.Background(), tc.force)
			if err != nil {
				t.Fatalf("all: %v", err)
			}
			sessionsAfter, wakesAfter, _ := env.fake.counts()
			if sum.Total != 1 {
				t.Fatalf("summary: %+v", sum)
			}
			if tc.wantRun {
				if sum.OK != 1 || sum.Skipped != 0 {
					t.Fatalf("gate should have run: %+v", sum)
				}
				if sessionsAfter <= sessionsBefore || wakesAfter != wakesBefore+1 {
					t.Fatalf("gate did not renew: sessions %d -> %d, wakes %d -> %d",
						sessionsBefore, sessionsAfter, wakesBefore, wakesAfter)
				}
			} else {
				if sum.OK != 0 || sum.Skipped != 1 || len(sum.Results) != 0 {
					t.Fatalf("gate should have skipped: %+v", sum)
				}
				if len(sum.SkippedAccounts) != 1 || sum.SkippedAccounts[0] != "alice" {
					t.Fatalf("skipped accounts: %+v", sum)
				}
				if sessionsAfter != sessionsBefore || wakesAfter != wakesBefore {
					t.Fatalf("skipped account was renewed: sessions %d -> %d, wakes %d -> %d",
						sessionsBefore, sessionsAfter, wakesBefore, wakesAfter)
				}
			}
		})
	}
}

func TestManualKeepaliveBypassesGate(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	// The account is healthy, just-renewed and its cookie is >36h away, so
	// only a manual action can make it renew now.
	sessionsBefore, wakesBefore, _ := env.fake.counts()
	sum, err := env.svc.KeepaliveAll(context.Background(), false)
	if err != nil {
		t.Fatalf("background all: %v", err)
	}
	if sum.Skipped != 1 || sum.OK != 0 {
		t.Fatalf("background should skip fresh account: %+v", sum)
	}
	if sessionsAfter, wakesAfter, _ := env.fake.counts(); sessionsAfter != sessionsBefore || wakesAfter != wakesBefore {
		t.Fatalf("background renewed a gated account: sessions %d -> %d, wakes %d -> %d",
			sessionsBefore, sessionsAfter, wakesBefore, wakesAfter)
	}

	// Single manual "立即保活" is not gated.
	if _, err := env.svc.Keepalive(context.Background(), a.ID); err != nil {
		t.Fatalf("manual keepalive: %v", err)
	}
	sessionsAfterManual, wakesAfterManual, _ := env.fake.counts()
	if sessionsAfterManual <= sessionsBefore || wakesAfterManual != wakesBefore+1 {
		t.Fatalf("manual keepalive was gated: sessions %d -> %d, wakes %d -> %d",
			sessionsBefore, sessionsAfterManual, wakesBefore, wakesAfterManual)
	}

	// Manual "全部保活" (force=true) bypasses the gate as well.
	sum, err = env.svc.KeepaliveAll(context.Background(), true)
	if err != nil {
		t.Fatalf("manual all: %v", err)
	}
	if sum.OK != 1 || sum.Skipped != 0 {
		t.Fatalf("manual all did not bypass the gate: %+v", sum)
	}
	if _, wakesAfterForce, _ := env.fake.counts(); wakesAfterForce != wakesAfterManual+1 {
		t.Fatalf("manual all did not renew: wakes %d -> %d", wakesAfterManual, wakesAfterForce)
	}
}

func TestMaybeWakeOfflineGuards(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	// An explicit/manual wake starts the offline cooldown.
	if _, err := env.svc.Wake(context.Background(), a.ID); err != nil {
		t.Fatal(err)
	}
	before := env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	time.Sleep(50 * time.Millisecond)
	if env.fake.wakeCount() != before {
		t.Fatal("cooldown was ignored")
	}

	// Once the explicit-wake cooldown expires, the hook fires again.
	env.clock += 61
	env.svc.MaybeWakeOffline("alice", 600)
	waitFor(t, func() bool { return env.fake.wakeCount() > before })

	// Suspended peers are not woken.
	if err := env.st.UpdatePeerStatus("alice", "suspended"); err != nil {
		t.Fatal(err)
	}
	env.clock += 61
	before = env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	time.Sleep(50 * time.Millisecond)
	if env.fake.wakeCount() != before {
		t.Fatal("suspended peer was woken")
	}
}

func TestAutoWakeAuditsOnlyOnFailureOrStateChange(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	audits := func() []*store.AuditEntry {
		t.Helper()
		entries, err := env.st.ListAudit("system", "muse.auto_wake", 0, 100)
		if err != nil {
			t.Fatalf("list audit: %v", err)
		}
		return entries
	}

	// Authorize left a confirmed wake. Repeating the same successful wake is
	// routine and must not add an audit row.
	env.svc.autoWake(a.ID, "alice")
	if got := audits(); len(got) != 0 {
		t.Fatalf("routine success wrote audit: %+v", got)
	}

	// A wake_state transition is a state change worth recording.
	row, err := env.st.GetMuseAccount(a.ID)
	if err != nil || row == nil {
		t.Fatalf("account: %+v (%v)", row, err)
	}
	row.LastWakeState = "unconfirmed"
	row.LastWakeOK = true
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	env.svc.autoWake(a.ID, "alice")
	got := audits()
	if len(got) != 1 || !strings.Contains(got[0].Detail, "wake_state=noise_confirmed") {
		t.Fatalf("state change audit: %+v", got)
	}

	// A failed wake always gets an audit row, even when the state is unchanged.
	env.fake.setSessionCode(http.StatusInternalServerError)
	env.svc.autoWake(a.ID, "alice")
	got = audits()
	if len(got) != 2 || !strings.Contains(got[1].Detail, "ok=false") {
		t.Fatalf("failure audit: %+v", got)
	}
	if !strings.Contains(got[1].Detail, "error=") {
		t.Fatalf("failure detail missing error: %q", got[1].Detail)
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

func TestKeepaliveRecordedWakeDoesNotBlockOfflineWake(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if !res.WakeRequested || !res.WakeOK {
		t.Fatalf("keepalive did not wake: %+v", res)
	}
	row, _ := env.st.GetMuseAccount(a.ID)
	if row.LastWakeAt != env.clock || !row.LastWakeOK {
		t.Fatalf("keepalive did not record the wake: %+v", row)
	}

	// The recorded keepalive wake is recent enough to trip the old
	// LastWakeAt-based cooldown; the offline hook must still fire because
	// only explicit/offline wakes own that clock.
	before := env.fake.wakeCount()
	env.svc.MaybeWakeOffline("alice", 600)
	waitFor(t, func() bool { return env.fake.wakeCount() > before })
}

func TestKeepaliveRecordsWakeAttempt(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt == 0 {
		t.Fatalf("authorize wake not recorded: %+v", row)
	}
	// Reset the authorization's wake marker so the next keepalive is the only
	// possible source of a fresh wake record.
	row.LastWakeAt = 0
	row.LastWakeOK = false
	row.LastWakeState = ""
	row.LastWakeError = ""
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if !res.WakeRequested || !res.WakeOK {
		t.Fatalf("keepalive did not wake/verify: %+v", res)
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt != env.clock || !row.LastWakeOK ||
		row.LastWakeState != "noise_confirmed" || row.LastWakeError != "" {
		t.Fatalf("keepalive wake record: %+v", row)
	}

	// A failed verification is recorded as unconfirmed (and stays out of the
	// session-scoped last_error).
	env.svc.verifyWake = func(context.Context, *renewResult, *sessionResponse) error {
		return errors.New("noise handshake: timeout (attempt 3/3, 24.0s)")
	}
	env.clock += 10
	res, err = env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if !res.WakeRequested || res.WakeOK {
		t.Fatalf("keepalive should record an unconfirmed wake: %+v", res)
	}
	row, _ = env.st.GetMuseAccountByPeer("alice")
	if row.LastWakeAt != env.clock || row.LastWakeOK ||
		row.LastWakeState != "unconfirmed" || !strings.Contains(row.LastWakeError, "noise handshake") {
		t.Fatalf("unconfirmed keepalive wake record: %+v", row)
	}
	if row.LastError != "" || !row.LastAttemptOK {
		t.Fatalf("wake failure leaked into session error: %+v", row)
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

func ageKeepalive(t *testing.T, env *testEnv, id string, age time.Duration) {
	t.Helper()
	row, err := env.st.GetMuseAccount(id)
	if err != nil || row == nil {
		t.Fatalf("account %s: %+v (%v)", id, row, err)
	}
	row.LastKeepaliveAt = env.clock - int64(age/time.Second)
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
}

func setEssentialExpiry(t *testing.T, env *testEnv, id string, expiresAt int64) {
	t.Helper()
	row, err := env.st.GetMuseAccount(id)
	if err != nil || row == nil {
		t.Fatalf("account %s: %+v (%v)", id, row, err)
	}
	raw, err := json.Marshal(map[string]int64{
		"hatch_sess": expiresAt,
		"hatch_vml":  expiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	row.CookiesExp = string(raw)
	if err := env.st.SaveMuseAccount(row); err != nil {
		t.Fatal(err)
	}
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

func TestUnassignedSessionReturnsRotatedCookies(t *testing.T) {
	env := newTestEnv(t, false)
	env.fake.mu.Lock()
	env.fake.setCookies = false
	env.fake.sessionStatus = "unassigned"
	env.fake.sessionSet = []string{"hatch_sess=new-sess; Path=/; Max-Age=2592000"}
	env.fake.mu.Unlock()

	res, err := env.svc.renew(context.Background(),
		map[string]string{"hatch_sess": "old-sess"}, map[string]int64{"hatch_sess": env.clock + 10}, false)
	if err == nil {
		t.Fatal("unassigned session was treated as success")
	}
	var unavailable *MuseSessionUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error type: %T (%v)", err, err)
	}
	if unavailable.Status != "unassigned" || !unavailable.Rotated {
		t.Fatalf("typed error: %+v", unavailable)
	}
	if unavailable.Cookies["hatch_sess"] != "new-sess" {
		t.Fatalf("rotated cookies missing from typed error: %+v", unavailable.Cookies)
	}
	if unavailable.CookiesExp["hatch_sess"] != env.clock+2592000 {
		t.Fatalf("cookie expiry missing from typed error: %+v", unavailable.CookiesExp)
	}
	if res.CookiesEnc == "" || !res.Rotated || res.Transient {
		t.Fatalf("result must seal the rotated jar and stay a confirmed failure: %+v", res)
	}
}

func TestUnassignedSessionPersistsRotatedCookies(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.setCookies = false
	env.fake.sessionStatus = "unassigned"
	env.fake.sessionSet = []string{"hatch_sess=rotated-on-unassigned; Path=/; Max-Age=2592000"}
	env.fake.mu.Unlock()
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "not assigned") {
		t.Fatalf("result: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "ok" || row.LastAttemptOK || row.LastError == "" {
		t.Fatalf("confirmed session failure not recorded: %+v", row)
	}
	// A non-assigned response says nothing about the VM; keep the last
	// confirmed value and its timestamp.
	if row.VMState != "RUNNING" || row.VMStateAt != 1_900_000_000 {
		t.Fatalf("vm state overwritten by unassigned response: %+v", row)
	}
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_sess"] != "rotated-on-unassigned" {
		t.Fatalf("rotated cookie not persisted on failure: %+v", cookies)
	}
}

func TestWakeCookieIsUsedByVerification(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.setCookies = false
	env.fake.wakeSet = []string{"hatch_sess=wake-rotated; Path=/; Max-Age=2592000"}
	env.fake.mu.Unlock()
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if !res.WakeRequested || !res.WakeOK {
		t.Fatalf("wake did not verify: %+v", res)
	}
	if !strings.Contains(env.fake.lastCookie, "hatch_sess=wake-rotated") {
		t.Fatalf("verification used stale cookies: %q", env.fake.lastCookie)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	cookies, err := env.svc.openCookies(row)
	if err != nil {
		t.Fatal(err)
	}
	if cookies["hatch_sess"] != "wake-rotated" {
		t.Fatalf("wake-rotated cookie not persisted: %+v", cookies)
	}
}

func TestRejectedSessionDoesNotMergeSetCookies(t *testing.T) {
	for _, code := range []int{http.StatusUnauthorized, http.StatusInternalServerError} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			env := newTestEnv(t, true)
			a := env.authorize(t, "alice")
			env.fake.mu.Lock()
			env.fake.setCookies = false
			env.fake.sessionCode = code
			env.fake.errorSetCookie = "hatch_sess=evil; Path=/; Max-Age=2592000"
			env.fake.mu.Unlock()
			env.clock += 10
			if _, err := env.svc.Keepalive(context.Background(), a.ID); err != nil {
				t.Fatalf("keepalive: %v", err)
			}
			row, _ := env.st.GetMuseAccountByPeer("alice")
			cookies, err := env.svc.openCookies(row)
			if err != nil {
				t.Fatal(err)
			}
			if cookies["hatch_sess"] != "renewed-sess" {
				t.Fatalf("HTTP %d merged Set-Cookie: %+v", code, cookies)
			}
		})
	}
}

func TestSessionVMStateRecorded(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"running", "RUNNING", "RUNNING"},
		{"suspended", "SUSPENDED", "SUSPENDED"},
		{"explicit unknown", "UNKNOWN", "UNKNOWN"},
		{"missing", "", "UNKNOWN"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t, true)
			env.fake.mu.Lock()
			env.fake.vmState = tc.raw
			env.fake.verifyState = tc.raw
			env.fake.mu.Unlock()
			a := env.authorize(t, "alice")
			if a.VMState != tc.want || a.VMStateAt != env.clock {
				t.Fatalf("account vm state: got %q at %d, want %q at %d", a.VMState, a.VMStateAt, tc.want, env.clock)
			}
			row, _ := env.st.GetMuseAccountByPeer("alice")
			if row.VMState != tc.want || row.VMStateAt != env.clock {
				t.Fatalf("stored vm state: %+v", row)
			}
		})
	}
}

func TestAssignedMissingVMStateRefreshesTimestamp(t *testing.T) {
	env := newTestEnv(t, true)
	env.fake.mu.Lock()
	env.fake.vmState = ""
	env.fake.verifyState = ""
	env.fake.mu.Unlock()
	a := env.authorize(t, "alice")
	if a.VMState != "UNKNOWN" || a.VMStateAt != env.clock {
		t.Fatalf("initial unknown state: %+v", a)
	}
	env.clock += 100
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if !res.OK || res.VMState != "UNKNOWN" || res.VMStateAt != env.clock {
		t.Fatalf("keepalive result: %+v", res)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.VMState != "UNKNOWN" || row.VMStateAt != env.clock {
		t.Fatalf("assigned-but-missing did not refresh UNKNOWN: %+v", row)
	}
}

func TestRateLimitBacksOffNextKeepalive(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusTooManyRequests
	env.fake.retryAfter = "120"
	env.fake.mu.Unlock()
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "429") || !strings.Contains(res.Error, "请求被限流") {
		t.Fatalf("429 result: %+v", res)
	}
	if got := env.svc.retryAt(a.ID); got != env.clock+120 {
		t.Fatalf("backoff: got %d want %d", got, env.clock+120)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "ok" || !row.LastAttemptOK || row.LastError != "" ||
		row.LastKeepaliveAt != 1_900_000_000 || row.VMState != "RUNNING" || row.VMStateAt != 1_900_000_000 {
		t.Fatalf("429 clobbered confirmed state: %+v", row)
	}
	before, _, _ := env.fake.counts()
	sum, err := env.svc.KeepaliveAll(context.Background(), false)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if sum.Skipped != 1 || len(sum.SkippedAccounts) != 1 || sum.SkippedAccounts[0] != "alice" {
		t.Fatalf("background did not honor Retry-After: %+v", sum)
	}
	if after, _, _ := env.fake.counts(); after != before {
		t.Fatalf("rate-limited account was probed again: %d -> %d", before, after)
	}
	sum, err = env.svc.KeepaliveAll(context.Background(), true)
	if err != nil {
		t.Fatalf("force all: %v", err)
	}
	if sum.Failed != 0 || sum.Skipped != 1 {
		t.Fatalf("force all must still honor the upstream 429 window: %+v", sum)
	}
	env.fake.mu.Lock()
	env.fake.sessionCode = 0
	env.fake.retryAfter = ""
	env.fake.mu.Unlock()
	env.clock += 121
	// The 6h/36h cadence gate is unrelated to the expired 429 window; age the
	// last renewal so this step still exercises backoff recovery.
	ageKeepalive(t, env, a.ID, 7*time.Hour)
	sum, err = env.svc.KeepaliveAll(context.Background(), false)
	if err != nil {
		t.Fatalf("all after expiry: %v", err)
	}
	if sum.OK != 1 || sum.Skipped != 0 {
		t.Fatalf("expired backoff was not cleared: %+v", sum)
	}
	if got := env.svc.retryAt(a.ID); got != 0 {
		t.Fatalf("backoff not cleared after success: %d", got)
	}
}

func TestServiceUnavailablePreservesConfirmedState(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusServiceUnavailable
	env.fake.mu.Unlock()
	env.clock += 10
	res, err := env.svc.Keepalive(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("keepalive: %v", err)
	}
	if res.OK || !strings.Contains(res.Error, "503") {
		t.Fatalf("503 result: %+v", res)
	}
	if got := env.svc.retryAt(a.ID); got != 0 {
		t.Fatalf("503 must not create a Retry-After backoff: %d", got)
	}
	row, _ := env.st.GetMuseAccountByPeer("alice")
	if row.AuthState != "ok" || !row.LastAttemptOK || row.LastError != "" ||
		row.LastKeepaliveAt != 1_900_000_000 || row.VMState != "RUNNING" || row.VMStateAt != 1_900_000_000 {
		t.Fatalf("503 clobbered confirmed state: %+v", row)
	}
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Unix(1_900_000_000, 0).UTC()
	if got := parseRetryAfter("120", now); got != 120*time.Second {
		t.Fatalf("seconds: %v", got)
	}
	if got := parseRetryAfter(now.Add(90*time.Second).Format(http.TimeFormat), now); got != 90*time.Second {
		t.Fatalf("http-date: %v", got)
	}
	if got := parseRetryAfter("not-a-date", now); got != 0 {
		t.Fatalf("invalid: %v", got)
	}
}

func TestReauthorizeKeepsAuthorizedAtOnTransient(t *testing.T) {
	env := newTestEnv(t, true)
	a := env.authorize(t, "alice")
	old := a.AuthorizedAt
	env.clock += 10
	env.fake.mu.Lock()
	env.fake.sessionCode = http.StatusServiceUnavailable
	env.fake.mu.Unlock()
	res, err := env.svc.Reauthorize(context.Background(), a.ID, "hatch_sess=new")
	if err != nil {
		t.Fatalf("reauthorize: %v", err)
	}
	if !res.LastAttemptOK || res.LastError != "" || res.AuthState != "ok" {
		t.Fatalf("transient reauthorize reported failure: %+v", res)
	}
	row, _ := env.st.GetMuseAccount(a.ID)
	if row.AuthorizedAt != old {
		t.Fatalf("transient reauthorize refreshed AuthorizedAt: got %d want %d", row.AuthorizedAt, old)
	}
	if row.AuthState != "ok" || !row.LastAttemptOK || row.LastError != "" {
		t.Fatalf("transient reauthorize clobbered state: %+v", row)
	}
	env.fake.mu.Lock()
	env.fake.sessionCode = 0
	env.fake.mu.Unlock()
	env.clock += 10
	if _, err := env.svc.Reauthorize(context.Background(), a.ID, "hatch_sess=new"); err != nil {
		t.Fatalf("reauthorize success: %v", err)
	}
	row, _ = env.st.GetMuseAccount(a.ID)
	if row.AuthorizedAt != env.clock {
		t.Fatalf("successful reauthorize did not refresh AuthorizedAt: %+v", row)
	}
}

func TestKeepaliveAllLogsOneVerdictPerAccount(t *testing.T) {
	env := newTestEnv(t, true)
	// The acceptance case is a full 11-account round where every successful
	// session reports an empty vm_state. Every row must still produce one
	// verdict with the exact same UNKNOWN/wake-error diagnosis.
	const accounts = 11
	peers := []string{"alice", "bob"}
	for i, want := 0, accounts-len(peers); i < want; i++ {
		id := "peer-" + strconv.Itoa(i)
		if err := env.st.CreatePeer(&store.Peer{ID: id, AgentType: "muse", Status: "active", CreatedAt: 1}); err != nil {
			t.Fatalf("peer %s: %v", id, err)
		}
		peers = append(peers, id)
	}
	env.fake.mu.Lock()
	env.fake.vmState = ""
	env.fake.verifyState = ""
	env.fake.mu.Unlock()
	ids := make([]string, 0, accounts)
	for _, peer := range peers {
		ids = append(ids, env.authorize(t, peer).ID)
	}
	// Fresh authorizations are inside the 6h/36h gate; this test is about the
	// verdict line for an actual sweep.
	for _, id := range ids {
		ageKeepalive(t, env, id, 7*time.Hour)
	}

	var buf strings.Builder
	oldWriter, oldFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(oldWriter)
		log.SetFlags(oldFlags)
	})

	env.clock += 10
	sum, err := env.svc.KeepaliveAll(context.Background(), false)
	if err != nil {
		t.Fatalf("all: %v", err)
	}
	if sum.Total != accounts || sum.OK != accounts || sum.Failed != 0 || sum.WakeOK != accounts {
		t.Fatalf("summary: %+v", sum)
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != accounts {
		t.Fatalf("want one verdict per account, got %d: %s", len(lines), buf.String())
	}
	for _, id := range ids {
		var line string
		for _, candidate := range lines {
			if strings.Contains(candidate, "account="+id+" ") {
				line = candidate
			}
		}
		if line == "" {
			t.Fatalf("missing verdict for %s: %s", id, buf.String())
		}
		for _, want := range []string{
			"ok=true", "auth=ok", "vm_state=UNKNOWN", "vm_state_at=" + strconv.FormatInt(env.clock, 10),
			"wake_requested=true", "wake_ok=true", "wake_state=noise_confirmed", "wake_error=",
		} {
			if !strings.Contains(line, want) {
				t.Fatalf("verdict missing %q: %s", want, line)
			}
		}
		if strings.Contains(line, "wake_error=noise") || strings.Contains(line, "wake_ok=false") {
			t.Fatalf("Noise-confirmed wake still reported as failed: %s", line)
		}
		if strings.Contains(line, "hatch_sess") || strings.Contains(line, "hatch_vml") {
			t.Fatalf("verdict leaked cookies: %s", line)
		}
	}
}
