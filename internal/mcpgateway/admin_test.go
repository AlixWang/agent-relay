package mcpgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

func TestMCPAdminKeysAndPeers(t *testing.T) {
	f := newMCPFixture(t, nil)
	// Create a read-only key; plaintext is returned exactly once.
	code, out := f.admin(t, http.MethodPost, "/admin/mcp/keys", map[string]any{
		"name": "cursor-readonly", "scopes": []string{"mcp:mock:read:*"}, "note": "test",
	})
	if code != 200 || out["plaintext"] == nil {
		t.Fatalf("create key: %d %+v", code, out)
	}
	plain := out["plaintext"].(string)
	if !strings.HasPrefix(plain, KeyPrefix) {
		t.Fatalf("key format: %q", plain)
	}
	key := out["key"].(map[string]any)
	id := key["id"].(string)
	if key["key_prefix"].(string) == "" || key["expires_at"].(float64) <= 0 {
		t.Fatalf("key view: %+v", key)
	}
	// List never returns plaintext or a hash.
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/keys", nil)
	if code != 200 || strings.Contains(toString(out), plain) || strings.Contains(toString(out), "key_hash") {
		t.Fatalf("list leaked key material: %d %s", code, toString(out))
	}
	// #25: an omitted scope list defaults to read-only across enabled services.
	code, out = f.admin(t, http.MethodPost, "/admin/mcp/keys", map[string]any{"name": "default-read"})
	if code != 200 {
		t.Fatalf("default-read key: %d %+v", code, out)
	}
	defaultScopes := out["key"].(map[string]any)["scopes"].([]any)
	if len(defaultScopes) != 1 || defaultScopes[0] != "mcp:mock:read:*" {
		t.Fatalf("default scopes: %+v", defaultScopes)
	}
	// Issuing an unknown tool or empty scope fails.
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys", map[string]any{"name": "x", "scopes": []string{"mcp:mock:read:nope"}}); code != 400 {
		t.Fatalf("unknown scope code = %d", code)
	}
	// Rotate: new plaintext, old key revoked immediately (grace 0).
	code, out = f.admin(t, http.MethodPost, "/admin/mcp/keys/"+id+"/rotate", nil)
	if code != 200 {
		t.Fatalf("rotate: %d %+v", code, out)
	}
	if out["plaintext"] == nil || out["plaintext"] == plain {
		t.Fatalf("rotate plaintext: %+v", out)
	}
	old, _ := f.st.GetMCPKey(id)
	if old.Status != "revoked" {
		t.Fatalf("old key should be revoked: %+v", old)
	}
	newID := out["key"].(map[string]any)["id"].(string)
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/"+newID+"/revoke", nil); code != 200 {
		t.Fatalf("revoke code = %d", code)
	}
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/"+newID+"/revoke", nil); code != 409 {
		t.Fatalf("double revoke code = %d", code)
	}

	// Peer grant create/update/revoke.
	code, out = f.admin(t, http.MethodPut, "/admin/mcp/peers/alice", map[string]any{
		"scopes": []string{"mcp:mock:read:*", "mcp:mock:write:write_low"},
	})
	if code != 200 || !out["authorized"].(bool) {
		t.Fatalf("peer grant: %d %+v", code, out)
	}
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/peers", nil)
	if code != 200 || !strings.Contains(toString(out), `"peer_id":"alice"`) {
		t.Fatalf("list peers: %d %s", code, toString(out))
	}
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/peers/nobody", map[string]any{"scopes": []string{"mcp:mock:read:*"}}); code != 404 {
		t.Fatalf("unknown peer code = %d", code)
	}
	if code, _ := f.admin(t, http.MethodDelete, "/admin/mcp/peers/alice", nil); code != 200 {
		t.Fatalf("peer revoke code = %d", code)
	}
	grant, _ := f.st.GetMCPPeerGrant("alice")
	if grant == nil || grant.Status != "revoked" {
		t.Fatalf("peer grant after revoke: %+v", grant)
	}
}

func TestMCPAdminApprovalAuditDryRunAndHealth(t *testing.T) {
	f := newMCPFixture(t, nil)
	// Generate one read audit row.
	_, _ = f.call(t, "/mcp", f.peerToken, rpc("tools/call", 1, map[string]any{
		"name": "mock_read_tool", "arguments": map[string]any{"site": "https://x.test"},
	}))
	// High-risk request creates a pending approval.
	_, out := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 2, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/blocked"},
	}))
	id := structured(t, out)["data"].(map[string]any)["approval_id"].(string)
	code, out := f.admin(t, http.MethodGet, "/admin/mcp/approvals?status=pending", nil)
	if code != 200 || int(out["total"].(float64)) != 1 {
		t.Fatalf("list approvals: %d %+v", code, out)
	}
	appr := out["approvals"].([]any)[0].(map[string]any)
	if appr["params"].(map[string]any)["url"] != "https://x.test/blocked" {
		t.Fatalf("frozen params: %+v", appr)
	}
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/approvals/"+id, nil)
	if code != 200 || !out["ok"].(bool) {
		t.Fatalf("get approval: %d %+v", code, out)
	}
	// Dry-run validates and hashes without creating an approval.
	before, _ := f.st.CountMCPApprovals("pending")
	code, out = f.admin(t, http.MethodPost, "/admin/mcp/dry-run", map[string]any{
		"service": "mock", "tool": "write_high",
		"params": map[string]any{"site": "https://x.test/", "url": "https://x.test/z"},
	})
	if code != 200 || !out["valid"].(bool) || out["would_require_approval"] != true {
		t.Fatalf("dry-run: %d %+v", code, out)
	}
	after, _ := f.st.CountMCPApprovals("pending")
	if before != after {
		t.Fatal("dry-run created an approval")
	}
	// Audit list/export includes both gateway and transition rows.
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/audit?page=1&page_size=50", nil)
	if code != 200 || int(out["total"].(float64)) < 2 {
		t.Fatalf("audit list: %d %+v", code, out)
	}
	req := newTestRequest(http.MethodGet, "/admin/mcp/audit/export", nil)
	rec := newTestRecorder()
	f.srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") ||
		!strings.Contains(rec.Body.String(), "request_id") {
		t.Fatalf("audit export: %d %q", rec.Code, rec.Header().Get("Content-Type"))
	}
	// Health and services reflect the registry.
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/health", nil)
	if code != 200 || !out["enabled"].(bool) || int(out["pending_approvals"].(float64)) != after {
		t.Fatalf("health: %d %+v", code, out)
	}
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/services", nil)
	if code != 200 || len(out["services"].([]any)) != 1 {
		t.Fatalf("services: %d %+v", code, out)
	}
}

func TestMCPAdminKeyRotationGraceExpiryAndCSVExport(t *testing.T) {
	f := newMCPFixture(t, func(c *Config) {
		c.Keys.RotateGraceSecs = 60
	})
	// Create key with 1 day TTL.
	code, out := f.admin(t, http.MethodPost, "/admin/mcp/keys", map[string]any{
		"name": "grace-key", "scopes": []string{"mcp:mock:read:*"}, "expires_in_days": 1,
	})
	if code != 200 {
		t.Fatalf("create: %d", code)
	}
	id := out["key"].(map[string]any)["id"].(string)
	// First rotate succeeds and sets grace revoked_at.
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/"+id+"/rotate", nil); code != 200 {
		t.Fatalf("first rotate = %d", code)
	}
	// Second rotate during grace must be rejected with 409.
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/"+id+"/rotate", nil); code != 409 {
		t.Fatalf("double rotate during grace = %d", code)
	}
	// After grace elapses, keyView must report revoked=true even though status column stayed active.
	f.srv.SetClock(func() time.Time { return f.now.Add(120 * time.Second) })
	code, out = f.admin(t, http.MethodGet, "/admin/mcp/keys", nil)
	if code != 200 {
		t.Fatalf("list keys = %d", code)
	}
	var sawRevoked bool
	for _, raw := range out["keys"].([]any) {
		k := raw.(map[string]any)
		if k["id"] == id {
			if k["revoked"] != true || k["status"] != "revoked" {
				t.Fatalf("past-grace key view = %+v", k)
			}
			sawRevoked = true
		}
	}
	if !sawRevoked {
		t.Fatal("missing rotated key in list")
	}
	// Expired key cannot be rotated.
	f.srv.SetClock(func() time.Time { return f.now.Add(100 * 24 * time.Hour) })
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/key-read/rotate", nil); code != 409 {
		t.Fatalf("expired key rotate = %d", code)
	}
	// Revoking a peer grant via /keys/{id}/revoke must 404.
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/keys/peer-grant/revoke", nil); code != 404 {
		t.Fatalf("revoke peer via key endpoint = %d", code)
	}
	// CSV export neutralizes formula injection.
	_ = f.st.AppendMCPAudit(&store.MCPAuditEntry{
		Ts: f.now.Unix(), Actor: "peer:alice", ServiceID: "mock", Tool: "=CMD|calc", ParamsSummary: "+SUM(1,1)", Outcome: "denied",
	})
	req := newTestRequest(http.MethodGet, "/admin/mcp/audit/export", nil)
	rec := newTestRecorder()
	f.srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "'=CMD|calc") || !strings.Contains(rec.Body.String(), "'+SUM(1,1)") {
		t.Fatalf("csv injection not escaped: %s", rec.Body.String())
	}
}

func toString(v any) string { b, _ := json.Marshal(v); return string(b) }

func newTestRequest(method, path string, body []byte) *http.Request {
	return httptest.NewRequest(method, path, strings.NewReader(string(body)))
}
func newTestRecorder() *httptest.ResponseRecorder { return httptest.NewRecorder() }
