package bing

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
	"github.com/AlixWang/agent-relay/internal/store"
)

// TestBingRemoveBlockedURLApprovalGate proves the real tool cannot execute
// without a claimed approval: the first call only creates a pending approval,
// the approved second call executes once against the httptest mock, and a
// replay is refused.
func TestBingRemoveBlockedURLApprovalGate(t *testing.T) {
	svc, mock := newTestService(t)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	now := time.Unix(1_800_000_000, 0)
	plain, prefix, hash, err := mcpgateway.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMCPKey(&store.MCPKey{
		ID: "key-bing-write", PrincipalType: "key", Name: "bing-write",
		KeyPrefix: prefix, KeyHash: hash, Scopes: []string{"mcp:bing-webmaster:write:*"},
		Status: "active", CreatedAt: now.Unix(), ExpiresAt: now.Add(90 * 24 * time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	cfg := mcpgateway.DefaultConfig()
	cfg.Enabled = true
	cfg.Services = map[string]mcpgateway.ServiceConfig{
		ServiceID: {Enabled: true, CredentialRef: "secret:TEST_BING", AllowedTools: []string{"*"}},
	}
	cfg.Write.Enabled = true
	cfg.Write.Services = map[string]mcpgateway.WriteServiceConfig{
		ServiceID: {AllowedTools: []string{ToolRemoveBlockedURL}},
	}
	srv, err := mcpgateway.New(mcpgateway.Options{Config: cfg, Store: st, MinProtocol: 1})
	if err != nil {
		t.Fatal(err)
	}
	srv.SetClock(func() time.Time { return now })
	if err := srv.Registry().Register(svc); err != nil {
		t.Fatal(err)
	}
	if err := srv.ValidateConfig(); err != nil {
		t.Fatal(err)
	}

	call := func(id int, args map[string]any) map[string]any {
		t.Helper()
		body, _ := json.Marshal(map[string]any{
			"jsonrpc": "2.0", "id": id, "method": "tools/call",
			"params": map[string]any{"name": "bing_remove_blocked_url", "arguments": args},
		})
		req := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(string(body)))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+plain)
		rec := httptest.NewRecorder()
		srv.MCPHandler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("call %d HTTP %d: %s", id, rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatalf("call %d response: %v", id, err)
		}
		return out
	}
	dataOf := func(out map[string]any) map[string]any {
		t.Helper()
		res, ok := out["result"].(map[string]any)
		if !ok {
			t.Fatalf("no result: %+v", out)
		}
		sc, ok := res["structuredContent"].(map[string]any)
		if !ok {
			t.Fatalf("no structuredContent: %+v", out)
		}
		data, ok := sc["data"].(map[string]any)
		if !ok {
			t.Fatalf("no data: %+v", out)
		}
		return data
	}

	args := map[string]any{"site": "https://x.test/", "blockedUrl": "https://x.test/private"}
	out := call(1, args)
	pending := dataOf(out)
	if pending["status"] != "pending" || pending["approval_id"] == "" {
		t.Fatalf("pending payload: %+v", pending)
	}
	mock.mu.Lock()
	before := len(mock.writes)
	mock.mu.Unlock()
	if before != 0 {
		t.Fatalf("pending approval executed upstream: %+v", mock.writes)
	}
	id := pending["approval_id"].(string)

	approveBody := strings.NewReader(`{"note":"test"}`)
	req := httptest.NewRequest(http.MethodPost, "/admin/mcp/approvals/"+id+"/approve", approveBody)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.AdminHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("approve HTTP %d: %s", rec.Code, rec.Body.String())
	}

	executeArgs := map[string]any{"site": "https://x.test/", "blockedUrl": "https://x.test/private", "approval_id": id}
	executed := dataOf(call(2, executeArgs))
	if executed["removed"] != true {
		t.Fatalf("execute payload: %+v", executed)
	}
	mock.mu.Lock()
	after := len(mock.writes)
	last := mock.writes[after-1]
	mock.mu.Unlock()
	if after != 1 || !strings.HasSuffix(last.path, "/RemoveBlockedUrl") {
		t.Fatalf("approved execution calls=%d last=%+v", after, last)
	}

	replay := call(3, executeArgs)
	replayRes, ok := replay["result"].(map[string]any)
	if !ok {
		t.Fatalf("replay result: %+v", replay)
	}
	replaySC, ok := replayRes["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("replay structuredContent: %+v", replay)
	}
	errObj, ok := replaySC["error"].(map[string]any)
	if !ok || errObj["code"] != mcpgateway.CodeApprovalUsed {
		t.Fatalf("replay response: %+v", replay)
	}
	mock.mu.Lock()
	replayed := len(mock.writes)
	mock.mu.Unlock()
	if replayed != 1 {
		t.Fatalf("replay executed upstream: %+v", mock.writes)
	}
}
