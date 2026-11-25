package mcpgateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/store"
)

type mockService struct {
	mu    sync.Mutex
	calls map[string]int
}

type mockArgs struct {
	Site       string `json:"site"`
	URL        string `json:"url"`
	Mode       string `json:"mode"`
	DryRun     bool   `json:"dry_run"`
	ApprovalID string `json:"approval_id"`
}

func (m *mockService) ID() string               { return "mock" }
func (m *mockService) DisplayName() string      { return "Mock" }
func (m *mockService) Version() string          { return "1.0.0" }
func (m *mockService) Prefix() string           { return "mock" }
func (m *mockService) CredentialRefs() []string { return nil }
func (m *mockService) Tools() []Tool {
	readSchema := Schema(map[string]any{"site": Prop("string", "site")})
	writeSchema := Schema(map[string]any{
		"site": Prop("string", "site"), "url": Prop("string", "url"), "mode": Prop("string", "mode"),
		"dry_run": Prop("boolean", "dry run"), "approval_id": Prop("string", "approval"),
	}, "site", "url")
	validateRead := func(raw json.RawMessage) error {
		var a mockArgs
		return DecodeStrict(raw, &a)
	}
	validateWrite := func(raw json.RawMessage) error {
		var a mockArgs
		if err := DecodeStrict(raw, &a); err != nil {
			return err
		}
		if a.Site == "" || a.URL == "" {
			return InvalidParams("site and url are required")
		}
		return nil
	}
	return []Tool{
		{Name: "read_tool", Risk: RiskRead, ReadOnly: true, InputSchema: readSchema, Validate: validateRead},
		{Name: "write_low", Risk: RiskLow, InputSchema: writeSchema, Validate: validateWrite},
		{Name: "write_high", Risk: RiskHigh, Destructive: true, InputSchema: writeSchema, Validate: validateWrite},
	}
}
func (m *mockService) Handle(ctx context.Context, call Call) (Result, error) {
	var a mockArgs
	_ = json.Unmarshal(call.Params, &a)
	m.mu.Lock()
	m.calls[call.Tool]++
	n := m.calls[call.Tool]
	m.mu.Unlock()
	if a.Mode == "fail" {
		return Result{}, &Error{Code: CodeUpstreamError, Message: "mock upstream failed", HTTPStatus: 200, Details: map[string]any{"upstream_status": 503}}
	}
	return Result{Data: map[string]any{"tool": call.Tool, "mode": a.Mode, "n": n}, UpstreamStatus: 200, RowCount: 1}, nil
}
func (m *mockService) count(tool string) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.calls[tool]
}

type mcpFixture struct {
	srv       *Server
	st        store.Store
	au        *auth.Service
	mock      *mockService
	peerToken string
	readKey   string
	writeKey  string
	now       time.Time
}

func newMCPFixture(t *testing.T, tweak func(*Config)) *mcpFixture {
	t.Helper()
	now := time.Unix(1_800_000_000, 0)
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	au := auth.New(st, 3600)
	peer := &store.Peer{ID: "alice", Status: "active", ProtocolVer: 1, CreatedAt: now.Unix()}
	if err := st.CreatePeer(peer); err != nil {
		t.Fatal(err)
	}
	peerToken, err := auth.GenerateToken()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateToken("alice", auth.HashToken(peerToken), "test", now.Unix()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SaveMCPPeerGrant(&store.MCPKey{
		ID: "peer-grant", PrincipalType: "peer", PeerID: "alice", Name: "alice",
		Scopes: []string{"mcp:mock:read:*", "mcp:mock:write:*"}, Status: "active", CreatedAt: now.Unix(),
	}); err != nil {
		t.Fatal(err)
	}

	mkKey := func(id, name string, scopes []string) string {
		plain, prefix, hash, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if err := st.CreateMCPKey(&store.MCPKey{
			ID: id, PrincipalType: "key", Name: name, KeyPrefix: prefix, KeyHash: hash,
			Scopes: scopes, Status: "active", CreatedAt: now.Unix(), ExpiresAt: now.Add(90 * 24 * time.Hour).Unix(),
		}); err != nil {
			t.Fatal(err)
		}
		return plain
	}
	readKey := mkKey("key-read", "read-key", []string{"mcp:mock:read:*"})
	writeKey := mkKey("key-write", "write-key", []string{"mcp:mock:read:*", "mcp:mock:write:*"})

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.Services = map[string]ServiceConfig{
		"mock": {Enabled: true, AllowedTools: []string{"*"}},
	}
	cfg.Write.Enabled = true
	cfg.Write.Services = map[string]WriteServiceConfig{
		"mock": {AllowedTools: []string{"write_low", "write_high"}},
	}
	cfg.Write.BatchCap = 100
	cfg.Write.ApprovalTTLSeconds = 600
	cfg.Write.ApprovalPendingTTLSeconds = 86400
	cfg.RatePerMinutePerPeer = 30
	cfg.RatePerHourPerPeer = 600
	if tweak != nil {
		tweak(&cfg)
	}
	srv, err := New(Options{Config: cfg, Store: st, Auth: au, MinProtocol: 1})
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	mock := &mockService{calls: map[string]int{}}
	if err := srv.Registry().Register(mock); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := srv.ValidateConfig(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	srv.SetClock(func() time.Time { return now })
	return &mcpFixture{srv: srv, st: st, au: au, mock: mock, peerToken: peerToken, readKey: readKey, writeKey: writeKey, now: now}
}

func (f *mcpFixture) call(t *testing.T, path, token string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		var err error
		raw, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out == nil {
		out = map[string]any{"_raw": rec.Body.String()}
	}
	return rec.Code, out
}

func (f *mcpFixture) admin(t *testing.T, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, strings.NewReader(string(raw)))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.AdminHandler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func rpc(method string, id any, params any) map[string]any {
	m := map[string]any{"jsonrpc": "2.0", "method": method}
	if id != nil {
		m["id"] = id
	}
	if params != nil {
		m["params"] = params
	}
	return m
}

func structured(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", out)
	}
	sc, ok := res["structuredContent"].(map[string]any)
	if !ok {
		t.Fatalf("no structuredContent: %+v", out)
	}
	return sc
}

func resultMap(t *testing.T, out map[string]any) map[string]any {
	t.Helper()
	res, ok := out["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result: %+v", out)
	}
	return res
}

func TestMCPProtocolSurface(t *testing.T) {
	f := newMCPFixture(t, nil)

	// Health is public and leaks no credentials.
	req := httptest.NewRequest(http.MethodGet, "/mcp/health", nil)
	rec := httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"enabled":true`) || strings.Contains(rec.Body.String(), "secret") {
		t.Fatalf("health: %d %s", rec.Code, rec.Body.String())
	}
	// GET /mcp is 405, not the console.
	req = httptest.NewRequest(http.MethodGet, "/mcp", nil)
	rec = httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("GET /mcp = %d", rec.Code)
	}
	// Unauthenticated POST.
	code, out := f.call(t, "/mcp", "", rpc("initialize", 1, map[string]any{"protocolVersion": "2025-06-18"}))
	if code != 401 || out["error"] == nil {
		t.Fatalf("no auth: %d %+v", code, out)
	}
	// initialize negotiates; unsupported falls back to latest.
	code, out = f.call(t, "/mcp", f.peerToken, rpc("initialize", 1, map[string]any{"protocolVersion": "2025-03-26"}))
	if code != 200 || resultMap(t, out)["protocolVersion"] != "2025-03-26" {
		t.Fatalf("initialize: %d %+v", code, out)
	}
	code, out = f.call(t, "/mcp", f.peerToken, rpc("initialize", 2, map[string]any{"protocolVersion": "1999-01-01"}))
	if code != 200 || resultMap(t, out)["protocolVersion"] != "2025-06-18" {
		t.Fatalf("initialize fallback: %d %+v", code, out)
	}
	// Notifications: 202 without id, result with id.
	code, _ = f.call(t, "/mcp", f.peerToken, rpc("notifications/initialized", nil, nil))
	if code != 202 {
		t.Fatalf("notification = %d", code)
	}
	code, out = f.call(t, "/mcp", f.peerToken, rpc("notifications/initialized", 3, nil))
	if code != 200 || out["result"] == nil {
		t.Fatalf("notification with id: %d %+v", code, out)
	}
	// Unknown method.
	_, out = f.call(t, "/mcp", f.peerToken, rpc("no/such", 4, nil))
	if out["error"] == nil || int(out["error"].(map[string]any)["code"].(float64)) != -32601 {
		t.Fatalf("unknown method: %+v", out)
	}
	// Batch is rejected.
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`[{"jsonrpc":"2.0","id":1,"method":"ping"}]`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.peerToken)
	rec = httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), "-32600") {
		t.Fatalf("batch: %d %s", rec.Code, rec.Body.String())
	}
	// Oversized body is 413.
	big := `{"jsonrpc":"2.0","id":1,"method":"ping","params":"` + strings.Repeat("x", 1<<20) + `"}`
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(big))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+f.peerToken)
	rec = httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 413 {
		t.Fatalf("oversized body = %d", rec.Code)
	}
	// SSE-only Accept is 406; wrong content type is 415.
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"ping"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Authorization", "Bearer "+f.peerToken)
	rec = httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 406 {
		t.Fatalf("sse accept = %d", rec.Code)
	}
	req = httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "text/plain")
	req.Header.Set("Authorization", "Bearer "+f.peerToken)
	rec = httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 415 {
		t.Fatalf("bad content type = %d", rec.Code)
	}
}

func TestMCPToolsListAndScopes(t *testing.T) {
	f := newMCPFixture(t, nil)
	_, out := f.call(t, "/mcp", f.peerToken, rpc("tools/list", 1, nil))
	tools := resultMap(t, out)["tools"].([]any)
	if len(tools) != 3 {
		t.Fatalf("aggregate tools = %d: %+v", len(tools), tools)
	}
	names := map[string]bool{}
	for _, raw := range tools {
		names[raw.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"mock_read_tool", "mock_write_low", "mock_write_high"} {
		if !names[want] {
			t.Fatalf("missing aggregate tool %q in %+v", want, names)
		}
	}
	// Service endpoint exposes short names.
	_, out = f.call(t, "/mcp/mock", f.peerToken, rpc("tools/list", 2, nil))
	tools = resultMap(t, out)["tools"].([]any)
	var short string
	for _, raw := range tools {
		if raw.(map[string]any)["name"].(string) == "read_tool" {
			short = "read_tool"
		}
	}
	if short == "" {
		t.Fatal("service endpoint did not expose read_tool")
	}
	// Read-only key sees only the read tool and is forbidden on write.
	_, out = f.call(t, "/mcp", f.readKey, rpc("tools/list", 3, nil))
	if n := len(resultMap(t, out)["tools"].([]any)); n != 1 {
		t.Fatalf("read key tools = %d", n)
	}
	code, out := f.call(t, "/mcp", f.readKey, rpc("tools/call", 4, map[string]any{
		"name": "mock_write_low", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/a"},
	}))
	if code != 403 {
		t.Fatalf("read key write = %d %+v", code, out)
	}
	rpcErr := out["error"].(map[string]any)
	if rpcErr["data"].(map[string]any)["error_code"] != CodeInsufficientScope {
		t.Fatalf("wrong error: %+v", rpcErr)
	}
	// Short name on aggregate is a tool error, not an accidental resolution.
	code, out = f.call(t, "/mcp", f.peerToken, rpc("tools/call", 5, map[string]any{"name": "read_tool", "arguments": map[string]any{}}))
	if code != 200 {
		t.Fatalf("unknown aggregate tool code = %d", code)
	}
	sc := structured(t, out)
	if sc["ok"].(bool) || sc["error"].(map[string]any)["code"] != CodeUnknownTool {
		t.Fatalf("short name on aggregate: %+v", sc)
	}
	// Unknown service is 404 before auth.
	req := httptest.NewRequest(http.MethodPost, "/mcp/nope", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	f.srv.MCPHandler().ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown service = %d", rec.Code)
	}
}

func TestMCPReadAndLowRiskWrite(t *testing.T) {
	f := newMCPFixture(t, nil)
	code, out := f.call(t, "/mcp", f.peerToken, rpc("tools/call", 1, map[string]any{
		"name": "mock_read_tool", "arguments": map[string]any{"site": "https://x.test"},
	}))
	if code != 200 {
		t.Fatalf("read code: %d %+v", code, out)
	}
	sc := structured(t, out)
	if !sc["ok"].(bool) || sc["data"].(map[string]any)["tool"] != "read_tool" {
		t.Fatalf("read result: %+v", sc)
	}
	code, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 2, map[string]any{
		"name": "mock_write_low", "arguments": map[string]any{"site": "https://x.test", "url": "https://x.test/a"},
	}))
	if code != 200 {
		t.Fatalf("write code: %d %+v", code, out)
	}
	sc = structured(t, out)
	if !sc["ok"].(bool) || f.mock.count("write_low") != 1 {
		t.Fatalf("write result: %+v calls=%d", sc, f.mock.count("write_low"))
	}
	// Dry run never calls upstream.
	code, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 3, map[string]any{
		"name": "mock_write_low", "arguments": map[string]any{"site": "https://x.test", "url": "https://x.test/b", "dry_run": true},
	}))
	sc = structured(t, out)
	if !sc["ok"].(bool) || f.mock.count("write_low") != 1 || !sc["data"].(map[string]any)["dry_run"].(bool) {
		t.Fatalf("dry run: %+v calls=%d", sc, f.mock.count("write_low"))
	}
	if code != 200 {
		t.Fatalf("dry run code = %d", code)
	}
	// Audit rows must never carry the key plaintext or its hash.
	rows, _, err := f.st.ListMCPAudit(store.MCPAuditFilter{}, 100, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if strings.Contains(row.ParamsSummary, f.writeKey) || strings.Contains(row.Detail, f.writeKey) {
			t.Fatalf("audit leaked key material: %+v", row)
		}
	}
}

func TestMCPHighRiskApprovalFlow(t *testing.T) {
	f := newMCPFixture(t, nil)
	args := map[string]any{"site": "https://x.test/", "url": "https://x.test/blocked"}
	code, out := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 1, map[string]any{
		"name": "mock_write_high", "arguments": args,
	}))
	if code != 200 {
		t.Fatalf("pending code: %d %+v", code, out)
	}
	sc := structured(t, out)
	if !sc["ok"].(bool) {
		t.Fatalf("pending not ok: %+v", sc)
	}
	pending := sc["data"].(map[string]any)
	if pending["status"] != "pending" || pending["approval_id"] == "" {
		t.Fatalf("pending payload: %+v", pending)
	}
	id := pending["approval_id"].(string)
	if f.mock.count("write_high") != 0 {
		t.Fatal("pending approval must not call upstream")
	}
	// Repeating the same request reuses the open approval id.
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 2, map[string]any{
		"name": "mock_write_high", "arguments": args,
	}))
	if structured(t, out)["data"].(map[string]any)["approval_id"] != id {
		t.Fatalf("duplicate approval ids differ: %+v", structured(t, out))
	}
	// Changed params are rejected and do not execute.
	changed := map[string]any{"site": "https://x.test/", "url": "https://x.test/other", "approval_id": id}
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 3, map[string]any{
		"name": "mock_write_high", "arguments": changed,
	}))
	if structured(t, out)["error"].(map[string]any)["code"] != CodeParamsMismatch {
		t.Fatalf("changed params: %+v", structured(t, out))
	}
	if f.mock.count("write_high") != 0 {
		t.Fatal("mismatched params must not execute")
	}
	// Operator approves.
	code, aout := f.admin(t, http.MethodPost, "/admin/mcp/approvals/"+id+"/approve", map[string]any{"note": "ok"})
	if code != 200 || !aout["ok"].(bool) {
		t.Fatalf("approve: %d %+v", code, aout)
	}
	// Execute exactly once.
	execArgs := map[string]any{"site": "https://x.test/", "url": "https://x.test/blocked", "approval_id": id}
	code, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 4, map[string]any{
		"name": "mock_write_high", "arguments": execArgs,
	}))
	if code != 200 || !structured(t, out)["ok"].(bool) || f.mock.count("write_high") != 1 {
		t.Fatalf("execute: %d %+v calls=%d", code, structured(t, out), f.mock.count("write_high"))
	}
	// Replay is refused.
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 5, map[string]any{
		"name": "mock_write_high", "arguments": execArgs,
	}))
	if structured(t, out)["error"].(map[string]any)["code"] != CodeApprovalUsed || f.mock.count("write_high") != 1 {
		t.Fatalf("replay: %+v calls=%d", structured(t, out), f.mock.count("write_high"))
	}
	row, _ := f.st.GetMCPApproval(id)
	if row == nil || row.Status != "executed" || row.Outcome != "ok" {
		t.Fatalf("approval row: %+v", row)
	}
	// Dry-run high creates no approval.
	before, _ := f.st.CountMCPApprovals("pending")
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 6, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/z", "dry_run": true},
	}))
	after, _ := f.st.CountMCPApprovals("pending")
	if before != after || f.mock.count("write_high") != 1 {
		t.Fatalf("high dry run changed state: before=%d after=%d calls=%d", before, after, f.mock.count("write_high"))
	}
}

func TestMCPApprovalRejectExpireAndUpstreamFailure(t *testing.T) {
	f := newMCPFixture(t, nil)
	callHigh := func(id int, url, approvalID, mode string) map[string]any {
		args := map[string]any{"site": "https://x.test/", "url": url}
		if approvalID != "" {
			args["approval_id"] = approvalID
		}
		if mode != "" {
			args["mode"] = mode
		}
		_, out := f.call(t, "/mcp", f.writeKey, rpc("tools/call", id, map[string]any{
			"name": "mock_write_high", "arguments": args,
		}))
		return structured(t, out)
	}
	// Reject: the rejected approval is only observable when the caller retries
	// with its approval_id (a fresh request intentionally creates a new one).
	sc := callHigh(1, "https://x.test/reject", "", "")
	id := sc["data"].(map[string]any)["approval_id"].(string)
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/approvals/"+id+"/reject", map[string]any{"note": "no"}); code != 200 {
		t.Fatalf("reject code = %d", code)
	}
	sc = callHigh(2, "https://x.test/reject", id, "")
	if sc["error"].(map[string]any)["code"] != CodeApprovalRejected {
		t.Fatalf("rejected execute: %+v", sc)
	}
	// Pending expires after 24h.
	sc = callHigh(3, "https://x.test/expire", "", "")
	id = sc["data"].(map[string]any)["approval_id"].(string)
	f.srv.SetClock(func() time.Time { return f.now.Add(25 * time.Hour) })
	sc = callHigh(4, "https://x.test/expire", id, "")
	if sc["error"].(map[string]any)["code"] != CodeApprovalExpired {
		t.Fatalf("expired execute: %+v", sc)
	}
	// Approved execution window expires after 10 minutes.
	f.srv.SetClock(func() time.Time { return f.now })
	sc = callHigh(5, "https://x.test/window", "", "")
	id = sc["data"].(map[string]any)["approval_id"].(string)
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/approvals/"+id+"/approve", nil); code != 200 {
		t.Fatalf("approve code = %d", code)
	}
	f.srv.SetClock(func() time.Time { return f.now.Add(11 * time.Minute) })
	sc = callHigh(6, "https://x.test/window", id, "")
	if sc["error"].(map[string]any)["code"] != CodeApprovalExpired {
		t.Fatalf("window expiry: %+v", sc)
	}
	// Upstream failure still transitions to executed and does not retry.
	f.srv.SetClock(func() time.Time { return f.now })
	_, out := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 7, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/fail", "mode": "fail"},
	}))
	id = structured(t, out)["data"].(map[string]any)["approval_id"].(string)
	if code, _ := f.admin(t, http.MethodPost, "/admin/mcp/approvals/"+id+"/approve", nil); code != 200 {
		t.Fatalf("approve fail code = %d", code)
	}
	_, out = f.call(t, "/mcp", f.writeKey, rpc("tools/call", 8, map[string]any{
		"name": "mock_write_high", "arguments": map[string]any{"site": "https://x.test/", "url": "https://x.test/fail", "mode": "fail", "approval_id": id},
	}))
	if structured(t, out)["error"].(map[string]any)["code"] != CodeUpstreamError {
		t.Fatalf("upstream failure: %+v", structured(t, out))
	}
	row, _ := f.st.GetMCPApproval(id)
	if row == nil || row.Status != "executed" || row.Outcome != "upstream_error" || row.UpstreamStatus != 503 {
		t.Fatalf("upstream failure row: %+v", row)
	}
}

func TestMCPRateLimitAndKeyRevocation(t *testing.T) {
	f := newMCPFixture(t, func(c *Config) {
		c.Write.LowRiskWritePerHour = 1
	})
	args := map[string]any{"site": "https://x.test", "url": "https://x.test/a"}
	if code, _ := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 1, map[string]any{"name": "mock_write_low", "arguments": args})); code != 200 {
		t.Fatalf("first write code = %d", code)
	}
	code, out := f.call(t, "/mcp", f.writeKey, rpc("tools/call", 2, map[string]any{"name": "mock_write_low", "arguments": args}))
	if code != 429 || out["error"].(map[string]any)["data"].(map[string]any)["error_code"] != CodeRateLimited {
		t.Fatalf("rate limit: %d %+v", code, out)
	}
	// Revoking the external key is immediate.
	key, _ := f.st.GetMCPKey("key-write")
	if ok, _ := f.st.RevokeMCPKey(key.ID, f.now.Unix()); !ok {
		t.Fatal("revoke key")
	}
	code, _ = f.call(t, "/mcp", f.writeKey, rpc("tools/list", 3, nil))
	if code != 401 {
		t.Fatalf("revoked key = %d", code)
	}
	// Revoking the peer grant leaves other identities alone.
	grant, _ := f.st.GetMCPPeerGrant("alice")
	if ok, _ := f.st.RevokeMCPKey(grant.ID, f.now.Unix()); !ok {
		t.Fatal("revoke peer grant")
	}
	code, _ = f.call(t, "/mcp", f.peerToken, rpc("tools/list", 4, nil))
	if code != 403 {
		t.Fatalf("revoked peer grant = %d", code)
	}
}
