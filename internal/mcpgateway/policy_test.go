package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

type credMockService struct {
	credRef string
	creds   *Credentials
	calls   int
}

func (m *credMockService) ID() string          { return "credsvc" }
func (m *credMockService) DisplayName() string { return "CredSvc" }
func (m *credMockService) Version() string     { return "1.0.0" }
func (m *credMockService) Prefix() string      { return "cred" }
func (m *credMockService) CredentialRefs() []string {
	if m.credRef == "" {
		return nil
	}
	return []string{m.credRef}
}
func (m *credMockService) Tools() []Tool {
	schema := Schema(map[string]any{})
	return []Tool{
		{Name: "ping", Description: "ping read tool", Risk: RiskRead, ReadOnly: true, InputSchema: schema},
		{Name: "do_write", Description: "write tool", Risk: RiskLow, InputSchema: schema},
	}
}
func (m *credMockService) Handle(ctx context.Context, call Call) (Result, error) {
	if _, err := m.creds.Resolve(m.credRef); err != nil {
		return Result{}, &Error{Code: CodeCredentialMissing, Message: "credential not available", HTTPStatus: http.StatusOK}
	}
	m.calls++
	return Result{Data: map[string]any{"ok": true}, UpstreamStatus: 200}, nil
}

type liveSecretResolver struct {
	mu      sync.Mutex
	secrets map[string]string
}

func (r *liveSecretResolver) RevealServerSecret(name string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v, ok := r.secrets[name]
	if !ok {
		return "", ErrCredentialMissing
	}
	return v, nil
}

func (r *liveSecretResolver) Set(name, val string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if val == "" {
		delete(r.secrets, name)
		return
	}
	r.secrets[name] = val
}

func TestMCPRuntimePolicyConfigAndServiceControls(t *testing.T) {
	f := newMCPFixture(t, func(c *Config) {
		c.Enabled = false
		c.Write.Enabled = false
		c.Write.Services["mock"] = WriteServiceConfig{AllowedTools: []string{"write_low"}}
	})

	// When disabled in config.toml, POST /mcp answers 503.
	if code, _ := f.call(t, "/mcp", f.readKey, rpc("tools/list", 1, nil)); code != http.StatusServiceUnavailable {
		t.Fatalf("disabled gateway POST /mcp = %d, want 503", code)
	}

	// Enable gateway + writes live via admin API.
	code, out := f.admin(t, http.MethodPut, "/admin/mcp/config", map[string]any{
		"enabled": true, "write_enabled": true,
	})
	if code != 200 || out["enabled"] != true || out["write_enabled"] != true || out["has_runtime_overrides"] != true {
		t.Fatalf("PUT /admin/mcp/config: %d %+v", code, out)
	}
	if !f.srv.Enabled() || !f.srv.WriteEnabled() {
		t.Fatal("server switches did not update live")
	}

	// Empty PUT /admin/mcp/config rejects with 400.
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/config", map[string]any{}); code != 400 {
		t.Fatalf("empty PUT /admin/mcp/config = %d, want 400", code)
	}

	// Gateway is now live and write_low works, while write_high is still not allowlisted.
	if code, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 2, map[string]any{
		"name": "mock_write_low", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/a"},
	})); code != 200 || structured(t, out)["ok"] != true {
		t.Fatalf("write_low after enabling: %d %+v", code, out)
	}
	if code, _ = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 3, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/b"},
	})); code != http.StatusForbidden {
		t.Fatalf("unallowlisted write_high = %d, want 403", code)
	}

	// Service policy validation errors return 400 / 404.
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/unknown", map[string]any{"enabled": false}); code != 404 {
		t.Fatalf("unknown service = %d, want 404", code)
	}
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{"write_allowed_tools": []string{"*"}}); code != 400 {
		t.Fatalf("wildcard write_allowed_tools = %d, want 400", code)
	}
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{"write_allowed_tools": []string{"read_tool"}}); code != 400 {
		t.Fatalf("read tool in write_allowed_tools = %d, want 400", code)
	}
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{"allowed_tools": []string{"write_low"}}); code != 400 {
		t.Fatalf("write tool in allowed_tools = %d, want 400", code)
	}
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{"allowed_tools": []string{}}); code != 400 {
		t.Fatalf("empty allowed_tools = %d, want 400", code)
	}

	// Allow write_high via PUT /admin/mcp/services/mock -> immediate pending approval.
	code, out = f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{
		"write_allowed_tools": []string{"write_low", "write_high"},
	})
	if code != 200 {
		t.Fatalf("PUT service: %d %+v", code, out)
	}
	svc := out["service"].(map[string]any)
	if len(svc["write_tools"].([]any)) != 2 || len(svc["read_tools"].([]any)) != 1 {
		t.Fatalf("unexpected read/write_tools metadata: %+v", svc)
	}
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 4, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/b"},
	}))
	if structured(t, out)["data"].(map[string]any)["status"] != "pending" {
		t.Fatalf("write_high after allowlist update = %+v", out)
	}

	// Clearing write_allowed_tools (`[]`) disables all writes for mock.
	if code, _ := f.admin(t, http.MethodPut, "/admin/mcp/services/mock", map[string]any{
		"write_allowed_tools": []string{},
	}); code != 200 {
		t.Fatalf("clear write_allowed_tools = %d", code)
	}
	if code, _ := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 5, map[string]any{
		"name": "mock_write_low", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/c"},
	})); code != http.StatusForbidden {
		t.Fatalf("write_low after clearing write_allowed_tools = %d, want 403", code)
	}

	// Reloading Server on the same store preserves persisted runtime policy.
	baseCfg := DefaultConfig()
	baseCfg.Enabled = false
	baseCfg.Write.Enabled = false
	baseCfg.Services = map[string]ServiceConfig{"mock": {Enabled: true, AllowedTools: []string{"*"}}}
	baseCfg.Write.Services = map[string]WriteServiceConfig{"mock": {AllowedTools: []string{"write_low"}}}
	reloaded, err := New(Options{Config: baseCfg, Store: f.st, Auth: f.au, MinProtocol: 1})
	if err != nil {
		t.Fatalf("reload server: %v", err)
	}
	if err := reloaded.Registry().Register(&mockService{calls: map[string]int{}}); err != nil {
		t.Fatalf("re-register: %v", err)
	}
	if err := reloaded.ValidateConfig(); err != nil {
		t.Fatalf("re-validate: %v", err)
	}
	if !reloaded.Enabled() || !reloaded.WriteEnabled() || len(reloaded.Config().Write.Services["mock"].AllowedTools) != 0 {
		t.Fatalf("persisted runtime policy not restored: %+v", reloaded.Config())
	}

	// Resetting runtime policy restores config.toml defaults.
	code, out = f.admin(t, http.MethodDelete, "/admin/mcp/config", nil)
	if code != 200 || out["enabled"] != false || out["write_enabled"] != false || out["has_runtime_overrides"] != false {
		t.Fatalf("DELETE /admin/mcp/config: %d %+v", code, out)
	}
	if f.srv.Enabled() || f.srv.WriteEnabled() {
		t.Fatal("reset did not restore defaults")
	}

	// Audit log contains config/service actions.
	if rows, err := f.st.ListAudit("", "", 0, 20); err == nil {
		raw, _ := json.Marshal(rows)
		s := string(raw)
		for _, act := range []string{"mcp.config_update", "mcp.service_update", "mcp.config_reset"} {
			if !strings.Contains(s, act) {
				t.Fatalf("missing audit action %s in %s", act, s)
			}
		}
	}
}

func TestMCPLiveCredentialActivationWithoutRestart(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	resolver := &liveSecretResolver{secrets: map[string]string{}}
	creds := NewCredentials(resolver)

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Services = map[string]ServiceConfig{
		"credsvc": {Enabled: true, CredentialRef: "secret:LIVE_TEST_KEY", AllowedTools: []string{"*"}},
	}
	cfg.Write.Services = map[string]WriteServiceConfig{
		"credsvc": {AllowedTools: []string{"do_write"}},
	}

	srv, err := New(Options{Config: cfg, Store: st, MinProtocol: 1, Credentials: creds})
	if err != nil {
		t.Fatal(err)
	}
	svc := &credMockService{credRef: "secret:LIVE_TEST_KEY", creds: creds}
	if err := srv.Registry().Register(svc); err != nil {
		t.Fatal(err)
	}
	if err := srv.ValidateConfig(); err != nil {
		t.Fatal(err)
	}
	srv.SetClock(func() time.Time { return now })

	plain, prefix, hash, err := GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateMCPKey(&store.MCPKey{
		ID: "k1", PrincipalType: "key", Name: "live-key", KeyPrefix: prefix, KeyHash: hash,
		Scopes: []string{"mcp:credsvc:read:*"}, Status: "active", CreatedAt: now.Unix(), ExpiresAt: now.Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	// Initially missing secret: admin shows credential_configured=false, effective_enabled=false,
	// aggregate tools/list hides it, and single-service /mcp/credsvc tools/list answers 403.
	rec := newTestRecorder()
	srv.AdminHandler().ServeHTTP(rec, newTestRequest(http.MethodGet, "/admin/mcp/services", nil))
	var adminOut map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &adminOut)
	first := adminOut["services"].([]any)[0].(map[string]any)
	if first["credential_configured"] != false || first["effective_enabled"] != false || first["credential_name"] != "LIVE_TEST_KEY" {
		t.Fatalf("unexpected initial service state: %+v", first)
	}

	req := newTestRequest(http.MethodPost, "/mcp/credsvc", []byte(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = newTestRecorder()
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("missing credential /mcp/credsvc tools/list = %d, want 403", rec.Code)
	}

	// Configure secret live & invalidate cache without restart.
	resolver.Set("LIVE_TEST_KEY", "secret-val")
	srv.InvalidateSecret("LIVE_TEST_KEY")

	rec = newTestRecorder()
	srv.AdminHandler().ServeHTTP(rec, newTestRequest(http.MethodGet, "/admin/mcp/services", nil))
	_ = json.Unmarshal(rec.Body.Bytes(), &adminOut)
	first = adminOut["services"].([]any)[0].(map[string]any)
	if first["credential_configured"] != true || first["effective_enabled"] != true {
		t.Fatalf("expected live credential activation, got: %+v", first)
	}

	req = newTestRequest(http.MethodPost, "/mcp", []byte(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"cred_ping","arguments":{}}}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = newTestRecorder()
	srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("live call after configuring secret = %d %s", rec.Code, rec.Body.String())
	}
}

func TestMCPRuntimePolicyRaceSafety(t *testing.T) {
	f := newMCPFixture(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			enabled := i%2 == 0
			_, _, _ = f.srv.applyRuntimeConfigUpdate(&enabled, &enabled)
			_, _ = f.callConfigSnapshotCheck()
		}(i)
	}
	wg.Wait()
}

func (f *mcpFixture) callConfigSnapshotCheck() (bool, error) {
	cfg := f.srv.snapshotConfig()
	return cfg.Enabled, nil
}
