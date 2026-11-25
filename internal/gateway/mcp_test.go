package gateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
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
