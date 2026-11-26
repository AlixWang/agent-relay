package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
	"github.com/AlixWang/agent-relay/internal/mcpgateway/services/bing"
	"github.com/AlixWang/agent-relay/internal/resources"
)

func TestMCPRoutesAndAdminAuth(t *testing.T) {
	f := newFixture(t)
	cfg := mcpgateway.DefaultConfig()
	cfg.Enabled = true
	cfg.Services = map[string]mcpgateway.ServiceConfig{}
	mcpSrv, err := mcpgateway.New(mcpgateway.Options{
		Config: cfg, Store: f.srv.st, Auth: f.srv.auth, MinProtocol: 1,
	})
	if err != nil {
		t.Fatalf("mcp new: %v", err)
	}
	f.srv.SetMCP(mcpSrv)
	f.mux = f.srv.Handler(nil)

	// Public health is reachable without auth and reports no secrets.
	code, body := doRaw(t, f.mux, http.MethodGet, "/mcp/health", nil, "")
	if code != http.StatusOK {
		t.Fatalf("GET /mcp/health = %d", code)
	}
	if body["enabled"] != true {
		t.Fatalf("health body: %+v", body)
	}
	// GET is 405, not the SPA catch-all (nil web handler here anyway).
	code, _ = doRaw(t, f.mux, http.MethodGet, "/mcp", nil, "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp = %d", code)
	}
	code, _ = doRaw(t, f.mux, http.MethodDelete, "/mcp", nil, "")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("DELETE /mcp = %d", code)
	}
	// Unauthenticated POST is 401 JSON-RPC.
	code, body = doRaw(t, f.mux, http.MethodPost, "/mcp", map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "initialize",
	}, "")
	if code != http.StatusUnauthorized || body["error"] == nil {
		t.Fatalf("unauthenticated /mcp: %d %+v", code, body)
	}
	// Admin API requires the existing session cookie.
	code, _ = doRaw(t, f.mux, http.MethodGet, "/admin/mcp/health", nil, "")
	if code != http.StatusUnauthorized {
		t.Fatalf("admin mcp without cookie = %d", code)
	}
	cookie := f.adminLogin(t)
	req := httptest.NewRequest(http.MethodGet, "/admin/mcp/health", nil)
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin mcp health = %d", rec.Code)
	}
}

func doRaw(t *testing.T, h http.Handler, method, path string, body any, token string) (int, map[string]any) {
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
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestMCPLiveSecretAndConfigIntegration(t *testing.T) {
	f := newFixture(t)
	resSvc, err := resources.New(f.srv.st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.SetResources(resSvc)

	creds := mcpgateway.NewCredentials(resSvc)
	cfg := mcpgateway.DefaultConfig()
	cfg.Enabled = false
	cfg.Write.Enabled = false
	cfg.Services = map[string]mcpgateway.ServiceConfig{
		mcpgateway.ServiceBing: cfg.Service(mcpgateway.ServiceBing),
	}
	cfg.Write.Services = map[string]mcpgateway.WriteServiceConfig{
		mcpgateway.ServiceBing: cfg.Write.Services[mcpgateway.ServiceBing],
	}
	mcpSrv, err := mcpgateway.New(mcpgateway.Options{
		Config: cfg, Store: f.srv.st, Auth: f.srv.auth, MinProtocol: 1, Credentials: creds,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := mcpSrv.Registry().Register(bing.New(bing.Options{
		CredentialRef: mcpgateway.CredentialBing,
		Credentials:   creds,
		Upstream:      mcpSrv.Upstream(),
		MaxRows:       cfg.MaxRowsPerCall,
		BatchCap:      cfg.BatchCapFor(mcpgateway.ServiceBing),
	})); err != nil {
		t.Fatal(err)
	}
	if err := mcpSrv.ValidateConfig(); err != nil {
		t.Fatal(err)
	}
	f.srv.SetMCP(mcpSrv)
	f.mux = f.srv.Handler(nil)
	cookie := f.adminLogin(t)

	// 1. Initially missing secret -> credential_configured=false, effective_enabled=false.
	code, out := f.doCookie(t, http.MethodGet, "/admin/mcp/services", nil, cookie)
	if code != 200 {
		t.Fatalf("GET /admin/mcp/services: %d %+v", code, out)
	}
	svc := out["services"].([]any)[0].(map[string]any)
	if svc["credential_configured"] != false || svc["effective_enabled"] != false {
		t.Fatalf("expected missing credential state, got %+v", svc)
	}

	// 2. Save secret through console Resource Hub route -> live activation without restart.
	code, out = f.doCookie(t, http.MethodPut, "/admin/resources/secret/BING_WM_API_KEY", map[string]any{
		"description": "bing api key",
		"value":       "super-secret-bing-key",
	}, cookie)
	if code != 200 || out["ok"] != true {
		t.Fatalf("PUT /admin/resources/secret/BING_WM_API_KEY: %d %+v", code, out)
	}
	code, out = f.doCookie(t, http.MethodGet, "/admin/mcp/services", nil, cookie)
	if code != 200 {
		t.Fatalf("GET /admin/mcp/services after put: %d %+v", code, out)
	}
	svc = out["services"].([]any)[0].(map[string]any)
	if svc["credential_configured"] != true || svc["effective_enabled"] != true {
		t.Fatalf("expected live credential activation after secret PUT, got %+v", svc)
	}

	// 3. Delete secret -> invalidates MCP cache and flips back to missing.
	code, _ = f.doCookie(t, http.MethodDelete, "/admin/resources/secret/BING_WM_API_KEY", nil, cookie)
	if code != 200 {
		t.Fatalf("DELETE /admin/resources/secret/BING_WM_API_KEY: %d", code)
	}
	code, out = f.doCookie(t, http.MethodGet, "/admin/mcp/services", nil, cookie)
	if code != 200 {
		t.Fatalf("GET /admin/mcp/services after delete: %d", code)
	}
	svc = out["services"].([]any)[0].(map[string]any)
	if svc["credential_configured"] != false || svc["effective_enabled"] != false {
		t.Fatalf("expected missing credential after delete, got %+v", svc)
	}

	// 4. Console runtime config toggles reflect immediately in /admin/stats.
	code, out = f.doCookie(t, http.MethodPut, "/admin/mcp/config", map[string]any{
		"enabled": true, "write_enabled": true,
	}, cookie)
	if code != 200 || out["enabled"] != true || out["write_enabled"] != true {
		t.Fatalf("PUT /admin/mcp/config: %d %+v", code, out)
	}
	code, stats := f.doCookie(t, http.MethodGet, "/admin/stats", nil, cookie)
	if code != 200 || stats["mcp_enabled"] != true || stats["mcp_write_enabled"] != true {
		t.Fatalf("GET /admin/stats after mcp config update: %d %+v", code, stats)
	}
}
