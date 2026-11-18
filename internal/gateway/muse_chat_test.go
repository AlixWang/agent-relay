package gateway

import (
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse"
	"github.com/AlixWang/agent-relay/internal/muse/musetest"
)

func (f *fixture) enableMuseChat(t *testing.T, baseURL string) *muse.Service {
	t.Helper()
	noise := musetest.New(t)
	svc, err := muse.New(f.srv.st, muse.Options{
		BaseURL:             baseURL,
		GatewayURL:          noise.URL,
		HTTPTimeout:         2 * time.Second,
		KeepaliveInterval:   time.Hour,
		WakeCooldown:        time.Minute,
		DataDir:             t.TempDir(),
		AutoWake:            true,
		DirectChatEnabled:   true,
		ChatMaxConcurrent:   4,
		ChatDefaultBudget:   30 * time.Second,
		ChatMaxBudget:       300 * time.Second,
		ChatPollInterval:    time.Second,
		ChatPerPeerPerMin:   6,
		ChatPerPeerPerHour:  30,
		ChatGlobalPerMinute: 30,
	})
	if err != nil {
		t.Fatalf("muse.New: %v", err)
	}
	f.srv.SetMuse(svc)
	return svc
}

func (f *fixture) authorizeMuse(t *testing.T, admin, peer, label string) map[string]any {
	t.Helper()
	code, out := f.doAuth(t, "POST", "/admin/muse/accounts", map[string]any{
		"peer_id": peer, "label": label,
		"cookies": "hatch_sess=fixture-sess; hatch_vml=fixture-vml",
	}, admin)
	if code != 200 {
		t.Fatalf("authorize: %d %+v", code, out)
	}
	return out["account"].(map[string]any)
}

func TestMuseChatAdminRoutesRequireAdmin(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuseChat(t, srv.URL)
	for _, tc := range []struct{ method, path string }{
		{"POST", "/admin/muse/chat"},
		{"GET", "/admin/muse/chat/status"},
		{"POST", "/admin/muse/chat/settings"},
		{"GET", "/admin/muse/chat/mc_x"},
		{"POST", "/admin/muse/chat/mc_x/cancel"},
		{"POST", "/admin/muse/accounts/ma_x/chat-consent"},
	} {
		if code, _ := f.do(t, tc.method, tc.path, nil, ""); code != 401 {
			t.Fatalf("%s %s without admin: %d", tc.method, tc.path, code)
		}
	}
}

func TestMuseChatStructuredAndUnknownFieldsRejected(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuseChat(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)

	for _, tc := range []struct {
		field string
		value any
		code  string
	}{
		{"payload", map[string]any{"kind": "x"}, "structured_field_forbidden"},
		{"op", "approve", "structured_field_forbidden"},
		{"requires_approval", true, "structured_field_forbidden"},
		{"bogus", 1, "unknown_field"},
	} {
		body := map[string]any{"peer_id": "alice", "text": "hi"}
		body[tc.field] = tc.value
		code, out := f.doAuth(t, "POST", "/admin/muse/chat", body, admin)
		if code != 400 || out["code"] != tc.code {
			t.Fatalf("%s: %d %+v", tc.field, code, out)
		}
	}
}

func TestMuseChatAdmissionDisabledAndValidation(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuse(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)
	f.authorizeMuse(t, admin, "alice", "alice@example.com")

	code, out := f.doAuth(t, "POST", "/admin/muse/chat", map[string]any{
		"peer_id": "alice", "text": "hi",
	}, admin)
	if code != 503 || out["code"] != "muse_chat_disabled" {
		t.Fatalf("deploy flag off: %d %+v", code, out)
	}

	f.enableMuseChat(t, srv.URL)

	code, out = f.doAuth(t, "POST", "/admin/muse/chat", map[string]any{
		"peer_id": "alice", "text": "   ",
	}, admin)
	if code != 400 || out["code"] != "text_empty" {
		t.Fatalf("empty text: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat", map[string]any{
		"peer_id": "nobody", "text": "hi",
	}, admin)
	if code != 404 || out["code"] != "unknown_member" {
		t.Fatalf("unknown member: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "GET", "/admin/muse/chat/mc_missing", nil, admin)
	if code != 404 || out["code"] != "unknown_chat" {
		t.Fatalf("unknown chat: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat/mc_missing/cancel", nil, admin)
	if code != 404 || out["code"] != "unknown_chat" {
		t.Fatalf("cancel unknown: %d %+v", code, out)
	}
}

func TestMuseChatConsentAndRuntimeSettings(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuseChat(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)
	acct := f.authorizeMuse(t, admin, "alice", "alice@example.com")
	id := acct["id"].(string)

	consentPath := "/admin/muse/accounts/" + id + "/chat-consent"
	code, out := f.doAuth(t, "POST", consentPath, map[string]any{
		"enabled": true, "consent_version": 1, "consent_evidence": "short",
	}, admin)
	if code != 400 || out["code"] != "consent_evidence_required" {
		t.Fatalf("short evidence: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", consentPath, map[string]any{
		"enabled": true, "consent_version": 2, "consent_evidence": "member said yes",
	}, admin)
	if code != 400 || out["code"] != "consent_version_mismatch" {
		t.Fatalf("version: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", consentPath, map[string]any{
		"enabled": true, "consent_version": 1, "consent_evidence": "member said yes",
	}, admin)
	if code != 200 || out["account"].(map[string]any)["chat_enabled"] != true {
		t.Fatalf("consent enable: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", consentPath, map[string]any{"enabled": false, "reason": "asked"}, admin)
	if code != 200 || out["account"].(map[string]any)["chat_enabled"] != false {
		t.Fatalf("consent disable: %d %+v", code, out)
	}

	code, out = f.doAuth(t, "GET", "/admin/muse/chat/status", nil, admin)
	if code != 200 {
		t.Fatalf("status: %d %+v", code, out)
	}
	status := out["status"].(map[string]any)
	if status["deploy_enabled"] != true || status["runtime_enabled"] != true ||
		status["capacity"] != float64(4) {
		t.Fatalf("status body: %+v", status)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat/settings", map[string]any{"enabled": false}, admin)
	if code != 200 || out["enabled"] != false {
		t.Fatalf("runtime disable: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat/settings", map[string]any{"enabled": true}, admin)
	if code != 200 || out["enabled"] != true {
		t.Fatalf("runtime enable: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat/settings", map[string]any{}, admin)
	if code != 400 || out["code"] != "enabled_required" {
		t.Fatalf("missing enabled: %d %+v", code, out)
	}
}

func TestMuseChatStartReturns202AndGetSnapshot(t *testing.T) {
	f := newFixture(t)
	_, srv := newFakeMuseAPI(t)
	f.enableMuseChat(t, srv.URL)
	f.registerPeer(t, "alice", "muse")
	admin := f.adminLogin(t)
	acct := f.authorizeMuse(t, admin, "alice", "alice@example.com")
	id := acct["id"].(string)
	code, out := f.doAuth(t, "POST", "/admin/muse/accounts/"+id+"/chat-consent", map[string]any{
		"enabled": true, "consent_version": 1, "consent_evidence": "member said yes",
	}, admin)
	if code != 200 {
		t.Fatalf("consent: %d %+v", code, out)
	}

	code, out = f.doAuth(t, "POST", "/admin/muse/chat", map[string]any{
		"peer_id": "alice", "text": "hello", "block_secs": 0,
		"client_request_id": "gw-req-1",
	}, admin)
	if code != 202 || out["ok"] != true {
		t.Fatalf("start: %d %+v", code, out)
	}
	chatID, _ := out["chat_id"].(string)
	if chatID == "" || out["peer_id"] != "alice" || out["visibility"] != "member_main_chat" {
		t.Fatalf("snapshot: %+v", out)
	}
	code, out = f.doAuth(t, "GET", "/admin/muse/chat/"+chatID, nil, admin)
	if code != 200 || out["chat_id"] != chatID {
		t.Fatalf("get: %d %+v", code, out)
	}
	code, out = f.doAuth(t, "POST", "/admin/muse/chat", map[string]any{
		"peer_id": "alice", "text": "hello", "client_request_id": "gw-req-1",
	}, admin)
	if code != 202 || out["duplicate"] != true || out["chat_id"] != chatID {
		t.Fatalf("duplicate: %d %+v", code, out)
	}
}
