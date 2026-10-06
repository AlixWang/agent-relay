package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/config"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/stream"
	"github.com/AlixWang/agent-relay/internal/verify"
	"golang.org/x/crypto/bcrypt"
)

type fixture struct {
	srv *Server
	mux http.Handler
	tok map[string]string // peer -> plaintext token
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	cfg := config.Default()
	cfg.MinClient = 1
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	au := auth.New(st, 3600)
	g := guard.New(st, guard.Limits{FuseMaxMessages: 50, FuseMaxAgeSecs: 86400, RatePerMinute: 60})
	q := queue.New(st, g)
	p := presence.New(st, 300, "")
	v := verify.New(st, 600)
	srv := New(cfg, st, au, q, p, v, "http://127.0.0.1:18789")
	srv.SetGuard(g)
	hash, _ := bcrypt.GenerateFromPassword([]byte("secret"), bcrypt.MinCost)
	srv.SetAdminHash(hash)
	return &fixture{srv: srv, mux: srv.Handler(nil), tok: map[string]string{}}
}

func (f *fixture) registerPeer(t *testing.T, id, agentType string) {
	t.Helper()
	// Use wall-clock time: handlers validate invite expiry against time.Now().
	now := time.Now().Unix()
	code, _, err := f.srv.auth.CreateInvite(id, agentType, "admin", now)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	_, plaintext, err := f.srv.auth.Register(code, id, agentType, 1, `{"shell":true}`, now)
	if err != nil {
		t.Fatalf("register %s: %v", id, err)
	}
	f.tok[id] = plaintext
}

func (f *fixture) do(t *testing.T, method, path string, body any, token string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{"_raw": rec.Body.String()}
	}
	return rec.Code, out
}

func TestRegisterValidation(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	code, _, _ := f.srv.auth.CreateInvite("alice", "muse", "admin", now)

	// Bad invite code.
	c, out := f.do(t, "POST", "/register", map[string]any{"code": "inv_nope", "id": "x", "protocol_version": 1}, "")
	if c != 400 || !strings.Contains(out["error"].(string), "invalid_code") {
		t.Fatalf("bad code: %d %+v", c, out)
	}
	// Bound identity mismatch.
	c, out = f.do(t, "POST", "/register", map[string]any{"code": code, "id": "mallory", "protocol_version": 1}, "")
	if c != 400 || !strings.Contains(out["error"].(string), "id_mismatch") {
		t.Fatalf("mismatch: %d %+v", c, out)
	}
	// Happy path.
	c, out = f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "alice", "agent_type": "muse", "protocol_version": 1}, "")
	if c != 200 || out["token"] == nil {
		t.Fatalf("register: %d %+v", c, out)
	}
	// Single-use enforced.
	c, _ = f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "alice2", "protocol_version": 1}, "")
	if c != 400 {
		t.Fatalf("reuse should 400, got %d", c)
	}
}

func TestIdentityBinding403(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")

	// from-spoof on send.
	c, _ := f.do(t, "POST", "/messages",
		map[string]any{"id": "s1", "to": "bob", "from": "bob", "payload": "spoof"},
		f.tok["alice"])
	if c != 403 {
		t.Fatalf("from-spoof should 403, got %d", c)
	}
	// from defaults to self and works.
	c, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "ok1", "to": "bob", "payload": "hi"},
		f.tok["alice"])
	if c != 200 || out["seq"] == nil {
		t.Fatalf("send: %d %+v", c, out)
	}
	// cross pull.
	c, _ = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["alice"])
	if c != 403 {
		t.Fatalf("cross-pull should 403, got %d", c)
	}
	// cross ack.
	c, _ = f.do(t, "POST", "/ack",
		map[string]any{"message_id": "ok1", "by": "bob"}, f.tok["alice"])
	if c != 403 {
		t.Fatalf("cross-ack should 403, got %d", c)
	}
	// cross heartbeat.
	c, _ = f.do(t, "POST", "/heartbeat",
		map[string]any{"id": "bob"}, f.tok["alice"])
	if c != 403 {
		t.Fatalf("cross-heartbeat should 403, got %d", c)
	}
	// unauthenticated.
	c, _ = f.do(t, "GET", "/peers", nil, "")
	if c != 401 {
		t.Fatalf("no token should 401, got %d", c)
	}
}

func TestSendPullAckFlow(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")

	c, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "t1", "to": "bob", "from": "alice", "payload": "do work"}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("send: %d %+v", c, out)
	}
	c, out = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if c != 200 {
		t.Fatalf("pull: %d %+v", c, out)
	}
	items := out["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("items: %+v", out)
	}
	first := items[0].(map[string]any)
	if first["payload"] != "do work" || first["thread"] == nil {
		t.Fatalf("envelope: %+v", first)
	}
	next := int64(out["next_since"].(float64))
	// Ack then invisible.
	c, _ = f.do(t, "POST", "/ack",
		map[string]any{"message_id": "t1", "by": "bob"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("ack: %d", c)
	}
	c, out = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if len(out["items"].([]any)) != 0 {
		t.Fatalf("acked still visible: %+v", out)
	}
	// since cursor honored.
	if next == 0 {
		t.Fatal("next_since not advanced")
	}
	// Duplicate rejected.
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "t1", "to": "bob", "from": "alice", "payload": "again"}, f.tok["alice"])
	if c != 409 {
		t.Fatalf("dup should 409, got %d", c)
	}
}

func TestApprovalHoldAndAdminApprove(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")

	c, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "h1", "to": "bob", "from": "alice", "payload": "rm -rf /", "requires_approval": true},
		f.tok["alice"])
	if c != 202 || out["held"] != true {
		t.Fatalf("hold: %d %+v", c, out)
	}
	c, out = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if len(out["items"].([]any)) != 0 {
		t.Fatalf("held visible early: %+v", out)
	}

	admin := f.adminLogin(t)
	// Find seq through thread endpoint: held msg lives in alice/h1.
	c2, detail := f.doAuth(t, "GET", "/admin/messages?thread=alice/h1", nil, admin)
	if c2 != 200 {
		t.Fatalf("thread: %d %+v", c2, detail)
	}
	mseq := int64(detail["items"].([]any)[0].(map[string]any)["seq"].(float64))
	c3, _ := f.doAuth(t, "POST", "/admin/messages/approve",
		map[string]any{"seq": mseq, "approve": true}, admin)
	_ = c3
	if c3 != 200 {
		t.Fatalf("approve: %d", c3)
	}
	c, out = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if len(out["items"].([]any)) != 1 {
		t.Fatalf("approved not visible: %+v", out)
	}
}

func TestVersionNegotiation426(t *testing.T) {
	f := newFixture(t)
	f.srv.cfg.MinClient = 2 // everyone is now stale
	f.registerPeer(t, "old", "generic")
	c, out := f.do(t, "GET", "/peers", nil, f.tok["old"])
	if c != 426 || !strings.Contains(out["error"].(string), "upgrade") {
		t.Fatalf("426: %d %+v", c, out)
	}
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "x", "to": "old", "payload": "y"}, f.tok["old"])
	if c != 426 {
		t.Fatalf("send should 426, got %d", c)
	}
}

func TestSmokeVerifyFlow(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "newbie", "muse")

	c, out := f.do(t, "POST", "/verify/smoke", nil, f.tok["newbie"])
	if c != 200 || out["smoke_id"] == nil {
		t.Fatalf("smoke: %d %+v", c, out)
	}
	smokeID := out["smoke_id"].(string)
	// Poll sees the system message.
	c, out = f.do(t, "GET", "/messages?for=newbie&since=0", nil, f.tok["newbie"])
	found := false
	for _, it := range out["items"].([]any) {
		if it.(map[string]any)["id"] == smokeID {
			found = true
		}
	}
	if !found {
		t.Fatalf("smoke not visible: %+v", out)
	}
	// Result + ack flips to active.
	f.do(t, "POST", "/messages",
		map[string]any{"id": "r1", "to": "system", "from": "newbie", "kind": "result",
			"in_reply_to": smokeID, "payload": "收到"}, f.tok["newbie"])
	f.do(t, "POST", "/ack",
		map[string]any{"message_id": smokeID, "by": "newbie"}, f.tok["newbie"])
	p, _ := f.srv.st.GetPeer("newbie")
	if p.Status != "active" {
		t.Fatalf("status: %s", p.Status)
	}
}

func TestRegisterRejectsStaleProtocol(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	code, _, _ := f.srv.auth.CreateInvite("stale", "muse", "admin", now)
	c, out := f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "stale", "protocol_version": 0}, "")
	if c != 426 {
		t.Fatalf("stale register should 426, got %d %+v", c, out)
	}
	// Invite must NOT be consumed by the rejected attempt.
	c, out = f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "stale", "protocol_version": 1}, "")
	if c != 200 {
		t.Fatalf("retry with current protocol: %d %+v", c, out)
	}
}

func TestAdminInviteRevokeFlow(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "POST", "/admin/invites",
		map[string]any{"intended_id": "z1", "agent_type": "claw"}, admin)
	if c != 200 || out["code"] == nil {
		t.Fatalf("invite: %d %+v", c, out)
	}
	c, out = f.doAuth(t, "GET", "/admin/invites", nil, admin)
	if c != 200 || len(out["invites"].([]any)) != 1 {
		t.Fatalf("list invites: %d %+v", c, out)
	}
	// Admin API without session → 401.
	c, _ = f.do(t, "GET", "/admin/peers", nil, "")
	if c != 401 {
		t.Fatalf("admin w/o session should 401, got %d", c)
	}
	// Rotate + revoke.
	f.registerPeer(t, "rot", "muse")
	c, out = f.doAuth(t, "POST", "/admin/tokens/rotate",
		map[string]any{"peer_id": "rot", "label": "r2"}, admin)
	if c != 200 || out["token"] == nil {
		t.Fatalf("rotate: %d %+v", c, out)
	}
	c, out = f.doAuth(t, "GET", "/admin/tokens", nil, admin)
	if c != 200 || len(out["tokens"].([]any)) != 2 {
		t.Fatalf("tokens: %d %+v", c, out)
	}
	toks := out["tokens"].([]any)
	firstID := int64(toks[0].(map[string]any)["id"].(float64))
	c, _ = f.doAuth(t, "DELETE", "/admin/tokens/"+itoa(firstID), nil, admin)
	if c != 200 {
		t.Fatalf("revoke: %d", c)
	}
}

func TestAdminRotateUnknownPeer404(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	c, _ := f.doAuth(t, "POST", "/admin/tokens/rotate",
		map[string]any{"peer_id": "ghost", "label": "x"}, admin)
	if c != 404 {
		t.Fatalf("rotate unknown peer should 404, got %d", c)
	}
}

func TestAdminExportThread(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")
	f.do(t, "POST", "/messages",
		map[string]any{"id": "e1", "to": "bob", "from": "alice", "payload": "hello"}, f.tok["alice"])
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/messages/export?thread=alice/e1&format=jsonl", nil, admin)
	_ = c
	_ = out
	// Export streams raw body, not JSON: exercise via mux directly.
	req := httptest.NewRequest("GET", "/admin/messages/export?thread=alice/e1&format=jsonl", nil)
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "jsonl") {
		t.Fatalf("jsonl: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if !strings.Contains(rec.Body.String(), `"payload":"hello"`) {
		t.Fatalf("jsonl body: %s", rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/admin/messages/export?thread=alice/e1&format=markdown", nil)
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "# thread alice/e1") {
		t.Fatalf("markdown: %d %.100s", rec.Code, rec.Body.String())
	}
	req = httptest.NewRequest("GET", "/admin/messages/export?thread=alice/e1&format=xml", nil)
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad format should 400, got %d", rec.Code)
	}
}

func TestPollScriptDownload(t *testing.T) {
	f := newFixture(t)
	// No auth required: static content.
	req := httptest.NewRequest("GET", "/clients/relay-poll.sh", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "shellscript") {
		t.Fatalf("content-type: %s", ct)
	}
	if !strings.Contains(rec.Body.String(), "/messages?for=") {
		t.Fatal("body is not the poll script")
	}
}

func TestClientIPTrustBoundary(t *testing.T) {
	f := newFixture(t)
	// Direct connection with forged XFF: ignored (peer not in trusted CIDR).
	req := httptest.NewRequest("GET", "/peers", nil)
	req.RemoteAddr = "203.0.113.9:1234"
	req.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := f.srv.clientIP(req); got != "203.0.113.9" {
		t.Fatalf("untrusted XFF honored: %s", got)
	}
	// Via loopback proxy: first XFF entry wins.
	req2 := httptest.NewRequest("GET", "/peers", nil)
	req2.RemoteAddr = "127.0.0.1:5678"
	req2.Header.Set("X-Forwarded-For", "203.0.113.9, 10.0.0.1")
	if got := f.srv.clientIP(req2); got != "203.0.113.9" {
		t.Fatalf("trusted XFF ignored: %s", got)
	}
	// No header: RemoteAddr host.
	req3 := httptest.NewRequest("GET", "/peers", nil)
	req3.RemoteAddr = "127.0.0.1:9999"
	if got := f.srv.clientIP(req3); got != "127.0.0.1" {
		t.Fatalf("direct ip: %s", got)
	}
}

func TestAdminDeletePeer(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "gone", "muse")
	admin := f.adminLogin(t)
	// Delete removes peer + revokes tokens, keeps message history.
	_, _ = f.doAuth(t, "POST", "/messages",
		map[string]any{"id": "m1", "to": "gone", "from": "gone", "payload": "x"}, f.tok["gone"])
	c, out := f.doAuth(t, "DELETE", "/admin/peers/gone", nil, admin)
	if c != 200 {
		t.Fatalf("delete: %d %+v", c, out)
	}
	if p, _ := f.srv.st.GetPeer("gone"); p != nil {
		t.Fatal("peer row kept")
	}
	toks, _ := f.srv.st.ListTokensByPeer("gone")
	for _, tk := range toks {
		if tk.RevokedAt == 0 {
			t.Fatal("token not revoked")
		}
	}
	// Auth with old token now fails.
	c, _ = f.do(t, "GET", "/peers", nil, f.tok["gone"])
	if c != 401 {
		t.Fatalf("deleted peer token should 401, got %d", c)
	}
	// Unknown peer → 404.
	c, _ = f.doAuth(t, "DELETE", "/admin/peers/nobody", nil, admin)
	if c != 404 {
		t.Fatalf("unknown peer should 404, got %d", c)
	}
}

func TestAdminConfigSnapshot(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/config", nil, admin)
	if c != 200 {
		t.Fatalf("config: %d", c)
	}
	for _, key := range []string{"retention", "guard", "presence"} {
		if _, ok := out[key].(map[string]any); !ok {
			t.Fatalf("config missing %s: %+v", key, out)
		}
	}
	c, _ = f.do(t, "GET", "/admin/config", nil, "")
	if c != 401 {
		t.Fatalf("config w/o session should 401, got %d", c)
	}
}

func TestAdminPromptsReconfigure(t *testing.T) {
	f := newFixture(t)
	admin := f.adminLogin(t)
	f.registerPeer(t, "vet", "muse")

	// Reconfigure: no invite minted, token reused, registration skipped.
	before, _ := f.srv.st.ListInvites()
	c, out := f.doAuth(t, "POST", "/admin/prompts",
		map[string]any{"agent_type": "muse", "peer_id": "vet", "reconfigure": true}, admin)
	if c != 200 {
		t.Fatalf("reconfigure: %d %+v", c, out)
	}
	if _, hasCode := out["code"]; hasCode {
		t.Fatal("reconfigure must not mint an invite")
	}
	after, _ := f.srv.st.ListInvites()
	if len(after) != len(before) {
		t.Fatal("reconfigure minted an invite row")
	}
	prompt := out["prompt"].(string)
	if !strings.Contains(prompt, "vet") || strings.Contains(prompt, "$RELAY/register") {
		t.Fatal("reconfigure prompt wrong")
	}
	// Reconfigure requires a peer_id.
	c, _ = f.doAuth(t, "POST", "/admin/prompts",
		map[string]any{"agent_type": "muse", "reconfigure": true}, admin)
	if c != 400 {
		t.Fatalf("reconfigure w/o peer should 400, got %d", c)
	}
	// Unknown agent_type rejected.
	c, _ = f.doAuth(t, "POST", "/admin/prompts",
		map[string]any{"agent_type": "nope", "peer_id": "vet", "create_invite": true}, admin)
	if c != 400 {
		t.Fatalf("unknown type should 400, got %d", c)
	}
}

func (f *fixture) adminLogin(t *testing.T) string {
	t.Helper()
	req := httptest.NewRequest("POST", "/admin/login",
		bytes.NewReader([]byte(`{"password":"secret"}`)))
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("admin login: %d %s", rec.Code, rec.Body.String())
	}
	for _, c := range rec.Result().Cookies() {
		if c.Name == "agent_relay_admin" {
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func (f *fixture) doAuth(t *testing.T, method, path string, body any, cookie string) (int, map[string]any) {
	return f.doCookie(t, method, path, body, cookie)
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	s := ""
	for n > 0 {
		s = string(rune('0'+n%10)) + s
		n /= 10
	}
	return s
}

func (f *fixture) doCookie(t *testing.T, method, path string, body any, cookie string) (int, map[string]any) {
	t.Helper()
	var rdr *bytes.Reader
	if body == nil {
		rdr = bytes.NewReader(nil)
	} else {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{}
	}
	return rec.Code, out
}

func TestFuseTripAndReset(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")
	// Fuse limit is 50 in the fixture; fill the alice/seed thread with
	// distinct long payloads so the loop heuristic can't fire first.
	c, _ := f.do(t, "POST", "/messages",
		map[string]any{"id": "seed", "to": "bob", "from": "alice",
			"payload": "seed task opening the thread with enough length to be unique payload number zero padding xxxxxxxxxxxxxxxxx"},
		f.tok["alice"])
	if c != 200 {
		t.Fatalf("seed: %d", c)
	}
	for i := 0; i < 49; i++ {
		body := map[string]any{
			"id":          "fill" + itoa(int64(i)),
			"to":          "bob",
			"from":        "alice",
			"in_reply_to": "seed",
			"payload":     "filler task with distinct long content index to avoid dup detection entirely " + itoa(int64(i)) + strings.Repeat("x", 40),
		}
		c, _ := f.do(t, "POST", "/messages", body, f.tok["alice"])
		if c != 200 {
			t.Fatalf("fill %d: %d", i, c)
		}
	}
	// 51st trips the fuse.
	c, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "over", "to": "bob", "from": "alice",
			"in_reply_to": "seed", "payload": "one past the limit with completely fresh wording and padding " + strings.Repeat("y", 40)},
		f.tok["alice"])
	if c != 409 || !strings.Contains(out["error"].(string), "loop_fuse_tripped") {
		t.Fatalf("fuse: %d %+v", c, out)
	}
	// Admin reset reopens the thread without deleting history.
	admin := f.adminLogin(t)
	c, _ = f.doAuth(t, "POST", "/admin/fuse/reset", map[string]any{"root_id": "alice/seed"}, admin)
	if c != 200 {
		t.Fatalf("reset: %d", c)
	}
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "after", "to": "bob", "from": "alice",
			"in_reply_to": "seed", "payload": "post-reset message with fresh distinct long wording " + strings.Repeat("z", 40)},
		f.tok["alice"])
	if c != 200 {
		t.Fatalf("after reset: %d", c)
	}
}

func TestPermissionHandshakeEndToEnd(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")

	// A→B task.
	c, _ := f.do(t, "POST", "/messages",
		map[string]any{"id": "t1", "to": "bob", "from": "alice",
			"payload": "list /tmp/x for me with enough distinct wording to avoid dup entirely xxxxx"}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("task: %d", c)
	}
	// B started.
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "s1", "to": "alice", "from": "bob", "kind": "status",
			"in_reply_to": "t1", "status": "started",
			"payload": "B started executing the delegated listing task now in detail xxxxx"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("started: %d", c)
	}
	// B blocked + permission request.
	c, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "pr1", "to": "alice", "from": "bob", "kind": "permission_request",
			"in_reply_to": "t1", "op": "shell.exec", "target": "/tmp/x", "detail": "list it",
			"payload": "B needs approval to run the listing command for this step in detail xxxxx"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("request: %d %+v", c, out)
	}
	// A pulls: status + request visible with structured fields.
	c, out = f.do(t, "GET", "/messages?for=alice&since=0", nil, f.tok["alice"])
	if c != 200 {
		t.Fatalf("pull: %d", c)
	}
	items := out["items"].([]any)
	byKind := map[string]map[string]any{}
	for _, it := range items {
		m := it.(map[string]any)
		byKind[m["kind"].(string)] = m
	}
	st, ok := byKind["status"]
	if !ok || st["status"] != "started" || st["in_reply_to"] != "t1" {
		t.Fatalf("status not surfaced: %+v", items)
	}
	pr, ok := byKind["permission_request"]
	if !ok || pr["op"] != "shell.exec" || pr["target"] != "/tmp/x" || pr["detail"] != "list it" {
		t.Fatalf("request fields not surfaced: %+v", items)
	}
	if pr["expires_at"] == nil || pr["expires_at"].(float64) <= 0 {
		t.Fatalf("expires_at missing: %+v", pr)
	}
	// A allows.
	c, out = f.do(t, "POST", "/messages",
		map[string]any{"id": "pd1", "to": "bob", "from": "alice", "kind": "permission_decision",
			"in_reply_to": "pr1", "decision": "allow", "payload": "A approves this one listing operation only xxxx"}, f.tok["alice"])
	if c != 200 || out["seq"] == nil {
		t.Fatalf("allow: %d %+v", c, out)
	}
	allowSeq := out["seq"]
	// Same-value replay (fresh id): idempotent, same seq.
	c, out = f.do(t, "POST", "/messages",
		map[string]any{"id": "pd1-retry", "to": "bob", "from": "alice", "kind": "permission_decision",
			"in_reply_to": "pr1", "decision": "allow", "payload": "A re-sends the same approval after retry xxxxx"}, f.tok["alice"])
	if c != 200 || out["seq"] != allowSeq {
		t.Fatalf("replay: %d %+v (want seq %v)", c, out, allowSeq)
	}
	// Conflicting flip: 409.
	c, out = f.do(t, "POST", "/messages",
		map[string]any{"id": "pd2", "to": "bob", "from": "alice", "kind": "permission_decision",
			"in_reply_to": "pr1", "decision": "deny", "payload": "A changes mind and denies the operation now xxxxxx"}, f.tok["alice"])
	if c != 409 || !strings.Contains(out["error"].(string), "permission_already_decided") {
		t.Fatalf("flip: %d %+v", c, out)
	}
	// Third party deciding: 403-class.
	f.registerPeer(t, "mallory", "muse")
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "pd3", "to": "bob", "from": "mallory", "kind": "permission_decision",
			"in_reply_to": "pr1", "decision": "deny", "payload": "mallory meddles with padding xxxxxxxxxxxxxxxxx"}, f.tok["mallory"])
	if c != 409 && c != 403 {
		t.Fatalf("third-party decide should 409/403, got %d", c)
	}
	// B sees the decision, resumes, finishes with result + ack.
	c, out = f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	found := false
	for _, it := range out["items"].([]any) {
		m := it.(map[string]any)
		if m["kind"] == "permission_decision" && m["decision"] == "allow" {
			found = true
		}
	}
	if !found {
		t.Fatalf("B cannot see decision: %+v", out)
	}
	for _, body := range []map[string]any{
		{"id": "s2", "to": "alice", "from": "bob", "kind": "status",
			"in_reply_to": "t1", "status": "resumed", "payload": "B resumed after approval and continues working xxxxx"},
		{"id": "r1", "to": "alice", "from": "bob", "kind": "result",
			"in_reply_to": "t1", "payload": "listing done: a.txt b.txt with full output here xxxxxxxxx"},
	} {
		c, _ = f.do(t, "POST", "/messages", body, f.tok["bob"])
		if c != 200 {
			t.Fatalf("B follow-up %+v: %d", body["id"], c)
		}
	}
	c, _ = f.do(t, "POST", "/ack", map[string]any{"message_id": "t1", "by": "bob"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("ack task: %d", c)
	}
	// Thread view shows the full chain with structured fields.
	admin := f.adminLogin(t)
	c, detail := f.doAuth(t, "GET", "/admin/messages?thread=alice/t1", nil, admin)
	if c != 200 {
		t.Fatalf("thread: %d", c)
	}
	kinds := []string{}
	for _, it := range detail["items"].([]any) {
		kinds = append(kinds, it.(map[string]any)["kind"].(string))
	}
	for _, want := range []string{"task", "status", "permission_request", "permission_decision", "result"} {
		found := false
		for _, k := range kinds {
			if k == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("thread missing %s: %v", want, kinds)
		}
	}
}

func TestPermissionDenyEndsTask(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "muse")
	f.do(t, "POST", "/messages",
		map[string]any{"id": "t1", "to": "bob", "from": "alice",
			"payload": "delete the temp dir with distinct wording to avoid dup entirely xxxxx"}, f.tok["alice"])
	f.do(t, "POST", "/messages",
		map[string]any{"id": "pr1", "to": "alice", "from": "bob", "kind": "permission_request",
			"in_reply_to": "t1", "op": "shell.exec", "target": "/tmp/scratch", "detail": "rm -rf",
			"payload": "B needs approval for the destructive removal step in detail xxxxxxxxx"}, f.tok["bob"])
	// A denies with scope reason; B ends the task with an explanatory result.
	c, _ := f.do(t, "POST", "/messages",
		map[string]any{"id": "pd1", "to": "bob", "from": "alice", "kind": "permission_decision",
			"in_reply_to": "pr1", "decision": "deny", "payload": "out of the scope I was given, not approving xxxxx"}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("deny: %d", c)
	}
	c, _ = f.do(t, "POST", "/messages",
		map[string]any{"id": "r1", "to": "alice", "from": "bob", "kind": "result",
			"in_reply_to": "t1", "payload": "declined: approval denied by A (out of scope), nothing executed x"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("terminal result: %d", c)
	}
	c, _ = f.do(t, "POST", "/ack", map[string]any{"message_id": "t1", "by": "bob"}, f.tok["bob"])
	if c != 200 {
		t.Fatalf("ack: %d", c)
	}
	// Audit captured the handshake.
	admin := f.adminLogin(t)
	c, out := f.doAuth(t, "GET", "/admin/audit?action=permission.decided&limit=5", nil, admin)
	if c != 200 || len(out["entries"].([]any)) != 1 {
		t.Fatalf("audit decided: %d %+v", c, out)
	}
	c, out = f.doAuth(t, "GET", "/admin/audit?action=permission.requested&limit=5", nil, admin)
	if c != 200 || len(out["entries"].([]any)) != 1 {
		t.Fatalf("audit requested: %d %+v", c, out)
	}
}

func TestPromptDistributionFlow(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")

	// Fresh peer reports nothing: heartbeat nudges (stored 0 < current 2).
	c, out := f.do(t, "POST", "/heartbeat", map[string]any{"id": "alice"}, f.tok["alice"])
	if c != 200 || out["prompt_update"] != true || out["prompt_version"] == nil {
		t.Fatalf("nudge: %d %+v", c, out)
	}
	// Pull the current instructions.
	c, out = f.do(t, "GET", "/prompts/current", nil, f.tok["alice"])
	if c != 200 || out["prompt_version"] == nil {
		t.Fatalf("current: %d %+v", c, out)
	}
	prompt, _ := out["prompt"].(string)
	if prompt == "" || !strings.Contains(prompt, "alice") {
		t.Fatalf("prompt not personalized: %.120s", prompt)
	}
	if out["agent_type"] != "muse" {
		t.Fatalf("agent_type: %+v", out)
	}
	// Confirm via heartbeat: nudge stops.
	ver := int64(out["prompt_version"].(float64))
	c, out = f.do(t, "POST", "/heartbeat",
		map[string]any{"id": "alice", "prompt_version": ver}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("confirm heartbeat: %d", c)
	}
	if _, nudged := out["prompt_update"]; nudged {
		t.Fatalf("still nudged after confirm: %+v", out)
	}
	p, _ := f.srv.st.GetPeer("alice")
	if p.PromptVersion != int(ver) || p.PromptUpdatedAt == 0 {
		t.Fatalf("peer row: %+v", p)
	}
	// After confirm the stored version is current: no more nudges.
	c, out = f.do(t, "POST", "/heartbeat", map[string]any{"id": "alice"}, f.tok["alice"])
	if c != 200 {
		t.Fatalf("post-confirm heartbeat: %d", c)
	}
	if _, nudged := out["prompt_update"]; nudged {
		t.Fatalf("nudged after confirm: %+v", out)
	}
	c, _ = f.do(t, "GET", "/prompts/current", nil, "")
	if c != 401 {
		t.Fatalf("unauth prompt pull should 401, got %d", c)
	}
	// Admin peers view surfaces prompt_version.
	admin := f.adminLogin(t)
	c, out = f.doAuth(t, "GET", "/admin/peers", nil, admin)
	if c != 200 {
		t.Fatalf("admin peers: %d", c)
	}
	found := false
	for _, it := range out["peers"].([]any) {
		m := it.(map[string]any)
		if m["id"] == "alice" && m["prompt_version"] == float64(ver) {
			found = true
		}
	}
	if !found {
		t.Fatalf("admin peers missing prompt_version: %+v", out)
	}
}

func TestStreamBacklogAndAuth(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")

	// No hub wired in fixture: 503.
	req := httptest.NewRequest("GET", "/messages/stream?for=alice&since=0", nil)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 503 {
		t.Fatalf("nil hub should 503, got %d", rec.Code)
	}

	// Wire hub, seed a message, stream it.
	f.srv.SetStream(stream.New(3, 100))
	_, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "s1", "to": "alice", "from": "bob", "payload": "hi"}, f.tok["bob"])
	if out["ok"] != true {
		t.Fatalf("send: %+v", out)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req = httptest.NewRequest("GET", "/messages/stream?for=alice&since=0", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec = httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.mux.ServeHTTP(rec, req); close(done) }()
	// Wait for the backlog frame, then cancel.
	deadline := time.Now().Add(4 * time.Second)
	for !strings.Contains(rec.Body.String(), `"id":"s1"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no backlog frame: %q", rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done
	body := rec.Body.String()
	if !strings.Contains(body, "event: message") || !strings.Contains(body, "retry: 3000") {
		t.Fatalf("frame shape: %q", body)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type: %q", ct)
	}

	// Cross-identity for: 403.
	req2 := httptest.NewRequest("GET", "/messages/stream?for=bob&since=0", nil)
	req2.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec2 := httptest.NewRecorder()
	f.mux.ServeHTTP(rec2, req2)
	if rec2.Code != 403 {
		t.Fatalf("for-spoof should 403, got %d", rec2.Code)
	}
	// No token: 401.
	req3 := httptest.NewRequest("GET", "/messages/stream?for=alice&since=0", nil)
	rec3 := httptest.NewRecorder()
	f.mux.ServeHTTP(rec3, req3)
	if rec3.Code != 401 {
		t.Fatalf("unauth should 401, got %d", rec3.Code)
	}
}

func TestStreamLivePushAndResume(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	f.srv.SetStream(stream.New(3, 100))

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/messages/stream?for=bob&since=0", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tok["bob"])
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.mux.ServeHTTP(rec, req); close(done) }()
	// Give the handler a moment to subscribe, then send.
	time.Sleep(200 * time.Millisecond)
	_, out := f.do(t, "POST", "/messages",
		map[string]any{"id": "live1", "to": "bob", "from": "alice", "payload": "live"}, f.tok["alice"])
	if out["ok"] != true {
		t.Fatalf("send: %+v", out)
	}
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(rec.Body.String(), `"id":"live1"`) {
		if time.Now().After(deadline) {
			t.Fatalf("no live frame: %q", rec.Body.String())
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	// Resume with since=past-live: no replay of the old item, stream stays open.
	msgs, _ := f.srv.queue.Visible("bob", 0, 200)
	var maxSeq int64
	for _, m := range msgs {
		if m.Seq > maxSeq {
			maxSeq = m.Seq
		}
	}
	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	req2 := httptest.NewRequest("GET", "/messages/stream?for=bob&since="+itoa(maxSeq), nil)
	req2 = req2.WithContext(ctx2)
	req2.Header.Set("Authorization", "Bearer "+f.tok["bob"])
	rec2 := httptest.NewRecorder()
	done2 := make(chan struct{})
	go func() { f.mux.ServeHTTP(rec2, req2); close(done2) }()
	<-done2 // ctx timeout ends it
	if strings.Contains(rec2.Body.String(), `"id":"live1"`) {
		t.Fatalf("resume replayed old item: %q", rec2.Body.String())
	}
}

func TestStreamRevokedMidStream(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	f.srv.SetStream(stream.New(3, 100))
	// Revoke alice's only token: stream must drop on next wake-up.
	toks, _ := f.srv.st.ListTokensByPeer("alice")
	for _, tk := range toks {
		_ = f.srv.st.RevokeToken(tk.ID, time.Now().Unix())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req := httptest.NewRequest("GET", "/messages/stream?for=alice&since=0", nil)
	req = req.WithContext(ctx)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { f.mux.ServeHTTP(rec, req); close(done) }()
	select {
	case <-done:
	case <-time.After(4 * time.Second):
		t.Fatal("revoked stream did not drop")
	}
}

func TestTailScriptServed(t *testing.T) {
	f := newFixture(t)
	req := httptest.NewRequest("GET", "/clients/relay-tail.sh", nil)
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("tail script: %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "shellscript") {
		t.Fatalf("content-type: %q", ct)
	}
	if !strings.Contains(rec.Body.String(), "messages/stream") {
		t.Fatal("tail script body wrong")
	}
}

func TestCapabilitiesDualShape(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	// Register with object-shaped capabilities (the hermes case).
	code, _, _ := f.srv.auth.CreateInvite("cap1", "hermes", "admin", now)
	c, out := f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "cap1", "agent_type": "hermes",
			"protocol_version": 1, "capabilities": map[string]any{"shell": true}}, "")
	if c != 200 || out["token"] == nil {
		t.Fatalf("object caps register: %d %+v", c, out)
	}
	tok := out["token"].(string)
	p, _ := f.srv.st.GetPeer("cap1")
	if p.Capabilities != `{"shell":true}` {
		t.Fatalf("stored caps: %q", p.Capabilities)
	}
	// Heartbeat with object-shaped capabilities: must 200, not 400.
	c, out = f.do(t, "POST", "/heartbeat",
		map[string]any{"id": "cap1", "capabilities": map[string]any{"polling": true}}, tok)
	if c != 200 {
		t.Fatalf("object caps heartbeat: %d %+v", c, out)
	}
	p, _ = f.srv.st.GetPeer("cap1")
	if p.Capabilities != `{"polling":true}` {
		t.Fatalf("updated caps: %q", p.Capabilities)
	}
	// String shape still works (backward compat with templates).
	code2, _, _ := f.srv.auth.CreateInvite("cap2", "muse", "admin", now)
	c, out = f.do(t, "POST", "/register",
		map[string]any{"code": code2, "id": "cap2", "protocol_version": 1,
			"capabilities": `{"shell":true}`}, "")
	if c != 200 {
		t.Fatalf("string caps register: %d %+v", c, out)
	}
	// Garbage capabilities degrade to {} instead of 400.
	c, _ = f.do(t, "POST", "/heartbeat",
		map[string]any{"id": "cap1", "capabilities": 42}, tok)
	if c != 200 {
		t.Fatalf("garbage caps should degrade, got %d", c)
	}
}

func TestProfileLifecycle(t *testing.T) {
	f := newFixture(t)
	now := time.Now().Unix()
	// Register with a profile: stored immediately.
	code, _, _ := f.srv.auth.CreateInvite("pro1", "muse", "admin", now)
	c, out := f.do(t, "POST", "/register",
		map[string]any{"code": code, "id": "pro1", "protocol_version": 1,
			"profile": "shell+git, 常做仓库巡检"}, "")
	if c != 200 || out["token"] == nil {
		t.Fatalf("register: %d %+v", c, out)
	}
	tok := out["token"].(string)
	p, _ := f.srv.st.GetPeer("pro1")
	if p.Profile == "" || p.ProfileUpdatedAt == 0 {
		t.Fatalf("profile not seeded: %+v", p)
	}
	// Fresh profile: no nudge.
	c, out = f.do(t, "POST", "/heartbeat", map[string]any{"id": "pro1"}, tok)
	if c != 200 {
		t.Fatalf("heartbeat: %d", c)
	}
	if _, nudged := out["profile_refresh"]; nudged {
		t.Fatalf("fresh profile should not nudge: %+v", out)
	}
	// Stale profile (backdate 8 days > default 7): nudge fires.
	_ = f.srv.st.UpdatePeerProfile("pro1", p.Profile, now-8*86400)
	c, out = f.do(t, "POST", "/heartbeat", map[string]any{"id": "pro1"}, tok)
	if c != 200 || out["profile_refresh"] != true {
		t.Fatalf("stale nudge: %d %+v", c, out)
	}
	// Refresh via heartbeat: stored + nudge stops.
	c, out = f.do(t, "POST", "/heartbeat",
		map[string]any{"id": "pro1", "profile": "shell+git+docker, 常做巡检与发布"}, tok)
	if c != 200 {
		t.Fatalf("refresh: %d", c)
	}
	if _, nudged := out["profile_refresh"]; nudged {
		t.Fatalf("still nudged after refresh: %+v", out)
	}
	p, _ = f.srv.st.GetPeer("pro1")
	if p.Profile != "shell+git+docker, 常做巡检与发布" {
		t.Fatalf("profile: %q", p.Profile)
	}
	// Never-set profile: nudge fires.
	f.registerPeer(t, "pro2", "claw")
	tok2 := f.tok["pro2"]
	c, out = f.do(t, "POST", "/heartbeat", map[string]any{"id": "pro2"}, tok2)
	if c != 200 || out["profile_refresh"] != true {
		t.Fatalf("empty nudge: %d %+v", c, out)
	}
	// /peers surfaces the profile for routing.
	c, out = f.do(t, "GET", "/peers", nil, tok2)
	if c != 200 {
		t.Fatalf("peers: %d", c)
	}
	found := false
	for _, it := range out["peers"].([]any) {
		m := it.(map[string]any)
		if m["id"] == "pro1" && m["profile"] == "shell+git+docker, 常做巡检与发布" {
			found = true
		}
	}
	if !found {
		t.Fatalf("peers missing profile: %+v", out)
	}
}
