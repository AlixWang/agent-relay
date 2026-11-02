package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse"
)

type fakeMuseAPI struct {
	mu          sync.Mutex
	sessions    int
	wakes       int
	sessionCode int
	vmState     string
	verifyState string
	lastWakeVM  string
	firstCookie string
	lastCookie  string
}

func newFakeMuseAPI(t *testing.T) (*fakeMuseAPI, *httptest.Server) {
	t.Helper()
	f := &fakeMuseAPI{vmState: "SUSPENDED", verifyState: "RUNNING"}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/session", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.sessions++
		if f.sessions == 1 {
			f.firstCookie = r.Header.Get("Cookie")
		}
		code := f.sessionCode
		state := f.vmState
		if f.wakes > 0 {
			state = f.verifyState
		}
		f.lastCookie = r.Header.Get("Cookie")
		f.mu.Unlock()
		if code != 0 {
			w.WriteHeader(code)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "hatch_vml", Value: "rotated-vml", Path: "/", MaxAge: 172800})
		http.SetCookie(w, &http.Cookie{Name: "hatch_sess", Value: "rotated-sess", Path: "/", MaxAge: 2592000})
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
		f.lastWakeVM, _ = body["vm_id"].(string)
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return f, srv
}

func (f *fixture) enableMuse(t *testing.T, baseURL string) *muse.Service {
	t.Helper()
	svc, err := muse.New(f.srv.st, muse.Options{
		BaseURL:           baseURL,
		HTTPTimeout:       2 * time.Second,
		KeepaliveInterval: time.Hour,
		WakeCooldown:      time.Minute,
		DataDir:           t.TempDir(),
		AutoWake:          true,
	})
	if err != nil {
		t.Fatalf("muse.New: %v", err)
	}
	f.srv.SetMuse(svc)
	return svc
}

func TestMuseAdminRoutesRequireAdmin(t *testing.T) {
	f := newFixture(t)
	fake, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	_ = fake
	for _, tc := range []struct{ method, path string }{
		{"GET", "/admin/muse/accounts"},
		{"POST", "/admin/muse/accounts"},
		{"PATCH", "/admin/muse/accounts/x"},
		{"DELETE", "/admin/muse/accounts/x"},
		{"POST", "/admin/muse/accounts/x/keepalive"},
		{"POST", "/admin/muse/accounts/x/wake"},
		{"POST", "/admin/muse/keepalive"},
		{"GET", "/admin/muse/status"},
	} {
		if code, _ := f.do(t, tc.method, tc.path, nil, ""); code != 401 {
			t.Fatalf("%s %s without admin: %d", tc.method, tc.path, code)
		}
	}
}

func TestMuseAuthorizeListPeersAndStats(t *testing.T) {
	f := newFixture(t)
	fake, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")
	admin := f.adminLogin(t)

	code, out := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "label": "a@example.com",
		"cookies": "hatch_sess=fixture-sess; hatch_vml=fixture-vml",
	}, admin)
	if code != 200 || out["ok"] != true {
		t.Fatalf("authorize: %d %+v", code, out)
	}
	acct := out["account"].(map[string]any)
	if acct["auth_state"] != "ok" || acct["vm_state"] != "RUNNING" || acct["essential_ok"] != true {
		t.Fatalf("account: %+v", acct)
	}
	if _, has := out["warning"]; has {
		t.Fatalf("unexpected warning: %+v", out)
	}
	if fake.sessions != 2 || fake.wakes != 1 {
		t.Fatalf("calls: sessions=%d wakes=%d", fake.sessions, fake.wakes)
	}
	if fake.lastWakeVM != "vm-1" {
		t.Fatalf("wake vm: %q", fake.lastWakeVM)
	}
	if !strings.Contains(fake.firstCookie, "hatch_sess=fixture-sess") {
		t.Fatalf("authorization never sent pasted cookie: %q", fake.firstCookie)
	}

	code, out = f.doAuth(t, "GET", "/admin/muse/accounts", nil, admin)
	if code != 200 {
		t.Fatalf("list: %d %+v", code, out)
	}
	body, _ := json.Marshal(out)
	if strings.Contains(string(body), "fixture-sess") || strings.Contains(string(body), "fixture-vml") {
		t.Fatalf("list leaked cookie values: %s", body)
	}
	if strings.Contains(string(body), "cookies_enc") {
		t.Fatalf("list leaked ciphertext field: %s", body)
	}
	rows := out["accounts"].([]any)
	if len(rows) != 1 {
		t.Fatalf("accounts: %+v", out)
	}
	row := rows[0].(map[string]any)
	if row["auth_state"] != "ok" || row["vm_state"] != "RUNNING" || row["peer_online"] != false {
		t.Fatalf("embedded account fields: %+v", row)
	}
	facets := out["facets"].(map[string]any)
	if facets["total"] != float64(1) || facets["ok"] != float64(1) || facets["auto_wake"] != float64(1) {
		t.Fatalf("facets: %+v", facets)
	}
	status := out["status"].(map[string]any)
	if status["loop_enabled"] != true {
		t.Fatalf("status: %+v", status)
	}

	// Admin peer list carries the operator-only summary; agent /peers does not.
	_, peersOut := f.doAuth(t, "GET", "/admin/peers", nil, admin)
	for _, x := range peersOut["peers"].([]any) {
		p := x.(map[string]any)
		if p["id"] == "alice" {
			m, ok := p["muse"].(map[string]any)
			if !ok || m["account_id"] == nil || m["auth_state"] != "ok" {
				t.Fatalf("admin peer muse summary: %+v", p)
			}
		}
		if p["id"] == "bob" {
			if _, has := p["muse"]; has {
				t.Fatalf("unbound peer has muse summary: %+v", p)
			}
		}
	}
	_, agentPeers := f.do(t, "GET", "/peers", nil, f.tok["alice"])
	for _, x := range agentPeers["peers"].([]any) {
		if _, has := x.(map[string]any)["muse"]; has {
			t.Fatalf("agent /peers leaked muse state: %+v", x)
		}
	}

	_, stats := f.doAuth(t, "GET", "/admin/stats", nil, admin)
	if stats["muse_authorized"] != float64(1) || stats["muse_ok"] != float64(1) || stats["muse_needs_reauth"] != float64(0) {
		t.Fatalf("stats: %+v", stats)
	}
	_, cfg := f.doAuth(t, "GET", "/admin/config", nil, admin)
	m := cfg["muse"].(map[string]any)
	if m["enabled"] != true || m["keepalive_secs"] != float64(900) || m["auto_wake"] != true {
		t.Fatalf("config: %+v", m)
	}
}

func TestMuseAuthorizationValidation(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)

	if code, _ := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=x",
	}, admin); code != 400 {
		t.Fatalf("missing cookie: %d", code)
	}
	if code, _ := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "not-a-cookie",
	}, admin); code != 400 {
		t.Fatalf("bad cookie format: %d", code)
	}
	if code, _ := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "nobody", "cookies": "hatch_sess=x; hatch_vml=y",
	}, admin); code != 404 {
		t.Fatalf("unknown peer: %d", code)
	}
	if code, _ := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=x; hatch_vml=y",
	}, admin); code != 200 {
		t.Fatalf("first authorize: %d", code)
	}
	if code, _ := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=x; hatch_vml=y",
	}, admin); code != 409 {
		t.Fatalf("duplicate: %d", code)
	}
	if code, _ := f.doAuth(t, "PATCH", "/admin/muse/accounts/x", map[string]any{"nothing": true}, admin); code != 400 {
		// DisallowUnknownFields rejects the body before the "nothing to update" check.
		t.Fatalf("unknown patch field: %d", code)
	}
	if code, _ := f.doAuth(t, "PATCH", "/admin/muse/accounts/x", map[string]any{}, admin); code != 400 {
		t.Fatalf("empty patch: %d", code)
	}
	if code, _ := f.doAuth(t, "DELETE", "/admin/muse/accounts/x", nil, admin); code != 404 {
		t.Fatalf("delete unknown: %d", code)
	}
}

func TestMuseKeepaliveWakeSettingsAndRevoke(t *testing.T) {
	f := newFixture(t)
	fake, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)

	_, out := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=s; hatch_vml=v",
	}, admin)
	id := out["account"].(map[string]any)["id"].(string)

	code, out := f.doAuth(t, "POST", "/admin/muse/accounts/"+id+"/keepalive", nil, admin)
	if code != 200 {
		t.Fatalf("keepalive: %d %+v", code, out)
	}
	res := out["result"].(map[string]any)
	if res["ok"] != true || res["wake_ok"] != true {
		t.Fatalf("keepalive result: %+v", res)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/accounts/"+id+"/wake", nil, admin)
	if code != 200 {
		t.Fatalf("wake: %d %+v", code, out)
	}
	if out["result"].(map[string]any)["wake_ok"] != true {
		t.Fatalf("wake result: %+v", out)
	}

	code, out = f.doAuth(t, "PATCH", "/admin/muse/accounts/"+id, map[string]any{
		"label": "renamed", "auto_wake": false, "enabled": false,
	}, admin)
	if code != 200 {
		t.Fatalf("patch settings: %d %+v", code, out)
	}
	acct := out["account"].(map[string]any)
	if acct["label"] != "renamed" || acct["auto_wake"] != false || acct["enabled"] != false {
		t.Fatalf("settings: %+v", acct)
	}

	code, out = f.doAuth(t, "PATCH", "/admin/muse/accounts/"+id, map[string]any{
		"cookies": "hatch_sess=new-s; hatch_vml=new-v",
	}, admin)
	if code != 200 || out["account"].(map[string]any)["auth_state"] != "ok" {
		t.Fatalf("reauth: %d %+v", code, out)
	}
	if fake.sessions == 0 || fake.wakes == 0 {
		t.Fatal("reauth did not call Muse")
	}

	// The all-run skips a disabled account; re-enable first.
	if _, out := f.doAuth(t, "PATCH", "/admin/muse/accounts/"+id, map[string]any{"enabled": true}, admin); out["ok"] != true {
		t.Fatalf("re-enable: %+v", out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/keepalive", nil, admin)
	if code != 200 {
		t.Fatalf("keepalive all: %d %+v", code, out)
	}
	sum := out["summary"].(map[string]any)
	if sum["total"] != float64(1) || sum["ok"] != float64(1) {
		t.Fatalf("summary: %+v", sum)
	}

	code, out = f.doAuth(t, "DELETE", "/admin/muse/accounts/"+id, nil, admin)
	if code != 200 {
		t.Fatalf("delete: %d %+v", code, out)
	}
	_, out = f.doAuth(t, "GET", "/admin/muse/accounts", nil, admin)
	if len(out["accounts"].([]any)) != 0 {
		t.Fatalf("account survived revoke: %+v", out)
	}
}

func TestMusePatchRejectsCookieAndPeerTogether(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)
	_, out := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=s; hatch_vml=v",
	}, admin)
	id := out["account"].(map[string]any)["id"].(string)
	if code, _ := f.doAuth(t, "PATCH", "/admin/muse/accounts/"+id, map[string]any{
		"peer_id": "alice", "cookies": "hatch_sess=x; hatch_vml=y",
	}, admin); code != 400 {
		t.Fatalf("cookie+peer patch: %d", code)
	}
}
