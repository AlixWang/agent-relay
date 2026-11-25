package mcpgateway

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Options wires the gateway core. Services are registered separately via
// Server.Registry().Register(...) so this package never imports service
// packages (avoids init/registration cycles).
type Options struct {
	Config       Config
	Store        store.Store
	Auth         *auth.Service
	MinProtocol  int
	MaxBodyBytes int
	Credentials  *Credentials
	ClientIP     func(*http.Request) string
}

// Server owns routing, auth, policy, approvals, limits, and audit for the MCP
// endpoint family.
type Server struct {
	cfg          Config
	st           store.Store
	registry     *Registry
	authn        *Authenticator
	approval     *ApprovalManager
	auditor      *Auditor
	limiter      *Limiter
	upstream     *Upstream
	creds        *Credentials
	maxBodyBytes int64
	clientIP     func(*http.Request) string
	now          func() time.Time
	mcpHandler   http.Handler
	adminHandler http.Handler
}

func New(opts Options) (*Server, error) {
	cfg := fillConfigDefaults(opts.Config)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if opts.Store == nil {
		return nil, errors.New("mcpgateway: store is required")
	}
	reg := NewRegistry()
	minProto := opts.MinProtocol
	if minProto < 1 {
		minProto = 1
	}
	maxBody := int64(opts.MaxBodyBytes)
	if maxBody <= 0 {
		maxBody = 1 << 20
	}
	s := &Server{
		cfg: cfg, st: opts.Store, registry: reg,
		authn:        NewAuthenticator(opts.Store, opts.Auth, minProto),
		approval:     NewApprovalManager(opts.Store, cfg),
		auditor:      NewAuditor(opts.Store, cfg),
		limiter:      NewLimiter(cfg),
		upstream:     NewUpstream(time.Duration(cfg.UpstreamTimeoutSecs) * time.Second),
		creds:        opts.Credentials,
		maxBodyBytes: maxBody,
		clientIP:     opts.ClientIP, now: time.Now,
	}
	if s.clientIP == nil {
		s.clientIP = func(r *http.Request) string {
			host, _, err := net.SplitHostPort(r.RemoteAddr)
			if err != nil {
				return r.RemoteAddr
			}
			return host
		}
	}
	s.mcpHandler = s.buildMCPHandler()
	s.adminHandler = s.buildAdminHandler()
	return s, nil
}

// InvalidateSecret drops any cached secret value when the Resource Hub rotates
// or deletes a server secret.
func (s *Server) InvalidateSecret(name string) {
	if s.creds != nil {
		s.creds.Invalidate("secret:" + name)
	}
}

// fillConfigDefaults makes New usable with a zero Config in tests while still
// keeping the safe documented defaults. Config.Load already validates a real
// file before we get here.
func fillConfigDefaults(c Config) Config {
	d := DefaultConfig()
	if c.UpstreamTimeoutSecs == 0 {
		c.UpstreamTimeoutSecs = d.UpstreamTimeoutSecs
	}
	if c.GatewayTimeoutSecs == 0 {
		c.GatewayTimeoutSecs = d.GatewayTimeoutSecs
	}
	if c.RatePerMinutePerPeer == 0 {
		c.RatePerMinutePerPeer = d.RatePerMinutePerPeer
	}
	if c.RatePerHourPerPeer == 0 {
		c.RatePerHourPerPeer = d.RatePerHourPerPeer
	}
	if c.MaxRowsPerCall == 0 {
		c.MaxRowsPerCall = d.MaxRowsPerCall
	}
	if c.Write.BatchCap == 0 {
		c.Write.BatchCap = d.Write.BatchCap
	}
	if c.Write.ApprovalTTLSeconds == 0 {
		c.Write.ApprovalTTLSeconds = d.Write.ApprovalTTLSeconds
	}
	if c.Write.ApprovalPendingTTLSeconds == 0 {
		c.Write.ApprovalPendingTTLSeconds = d.Write.ApprovalPendingTTLSeconds
	}
	if c.Write.LowRiskWritePerHour == 0 {
		c.Write.LowRiskWritePerHour = d.Write.LowRiskWritePerHour
	}
	if c.Write.HighRiskWritePerHour == 0 {
		c.Write.HighRiskWritePerHour = d.Write.HighRiskWritePerHour
	}
	if c.Keys.ReadKeyTTLDays == 0 {
		c.Keys.ReadKeyTTLDays = d.Keys.ReadKeyTTLDays
	}
	if c.Keys.WriteKeyTTLDays == 0 {
		c.Keys.WriteKeyTTLDays = d.Keys.WriteKeyTTLDays
	}
	if c.Keys.PerToolKeyTTLDays == 0 {
		c.Keys.PerToolKeyTTLDays = d.Keys.PerToolKeyTTLDays
	}
	return c
}

// Enabled reports the gateway master switch (console badge/stats).
func (s *Server) Enabled() bool { return s.cfg.Enabled }

// WriteEnabled reports the write master switch.
func (s *Server) WriteEnabled() bool { return s.cfg.Write.Enabled }

// RevokePeerGrant materializes peer deactivation (design v5 §4.5): when the
// operator suspends/retires a peer, its mcp_keys peer row becomes revoked.
// Re-activating does not auto-grant; the operator must re-authorize.
func (s *Server) RevokePeerGrant(peerID string) (bool, error) {
	grant, err := s.st.GetMCPPeerGrant(peerID)
	if err != nil || grant == nil {
		return false, err
	}
	ok, err := s.st.RevokeMCPKey(grant.ID, s.now().Unix())
	if err == nil && ok {
		s.auditor.RecordAdmin("mcp.peer_revoke", "peer="+peerID+" reason=peer_status")
	}
	return ok, err
}

// PendingApprovals is the console badge count (#35: no system messages).
func (s *Server) PendingApprovals() int {
	n, err := s.st.CountPendingMCPApprovals(s.now().Unix())
	if err != nil {
		return 0
	}
	return n
}

// Registry exposes the explicit registry for wiring.
func (s *Server) Registry() *Registry { return s.registry }

// Config returns a copy of the effective config.
func (s *Server) Config() Config { return s.cfg }

// Upstream exposes the shared HTTP client to service constructors.
func (s *Server) Upstream() *Upstream { return s.upstream }

// ValidateConfig performs registry-dependent startup checks.
func (s *Server) ValidateConfig() error { return s.cfg.ValidateAgainst(s.registry) }

// SetClientIP overrides the IP extractor; internal/gateway passes its
// trusted-proxy-aware version so audit rows do not record the reverse proxy.
func (s *Server) SetClientIP(fn func(*http.Request) string) {
	if fn != nil {
		s.clientIP = fn
	}
}

// SetClock is used by tests; production uses wall clock.
func (s *Server) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.now = now
	s.authn.SetClock(now)
	s.approval.SetClock(now)
	s.auditor.SetClock(now)
	s.limiter.SetClock(now)
}

// RunMaintenance expires approvals and applies audit retention. Main calls
// this from its existing 6-hour retention loop.
func (s *Server) RunMaintenance(now time.Time) (MaintenanceResult, error) {
	return s.auditor.RunMaintenance(s.approval, now)
}

// MCPHandler serves /mcp and /mcp/<service>. It is intentionally a single
// handler rather than per-method ServeMux patterns so GET/DELETE can answer
// the mandated 405 instead of falling through to the console catch-all.
func (s *Server) MCPHandler() http.Handler {
	return s.mcpHandler
}

func (s *Server) buildMCPHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		path := strings.TrimSuffix(r.URL.Path, "/")
		if path == "" {
			path = "/"
		}
		if path == "/mcp/health" {
			if r.Method != http.MethodGet {
				methodNotAllowed(w, http.MethodGet)
				return
			}
			s.handlePublicHealth(w)
			return
		}
		serviceID := ""
		switch {
		case path == "/mcp":
			// aggregate endpoint
		case strings.HasPrefix(path, "/mcp/"):
			serviceID = strings.TrimPrefix(path, "/mcp/")
			if serviceID == "" || strings.Contains(serviceID, "/") {
				writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown MCP endpoint"})
				return
			}
			if _, ok := s.registry.Get(serviceID); !ok {
				writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown MCP service"})
				return
			}
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "unknown MCP endpoint"})
			return
		}
		if r.Method != http.MethodPost {
			methodNotAllowed(w, http.MethodPost)
			return
		}
		s.handleRPC(w, r, serviceID)
	})
}

func (s *Server) handlePublicHealth(w http.ResponseWriter) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "enabled": s.cfg.Enabled, "write_enabled": s.cfg.Write.Enabled,
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]any{"ok": false, "error": "method not allowed"})
}

// ---- JSON-RPC plumbing ----

var supportedVersions = []string{"2025-06-18", "2025-03-26"}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    map[string]any `json:"data,omitempty"`
}

func (s *Server) handleRPC(w http.ResponseWriter, r *http.Request, serviceID string) {
	start := time.Now()
	if !s.cfg.Enabled {
		e := NewError(CodeServiceDisabled, "MCP gateway is disabled", http.StatusServiceUnavailable)
		writeRPCError(w, http.StatusServiceUnavailable, nil, -32003, e.Code, e.Message, nil)
		return
	}
	if !allowedOrigin(r) {
		e := NewError(CodeUnauthorized, "origin not allowed", http.StatusForbidden)
		writeRPCError(w, http.StatusForbidden, nil, -32002, e.Code, e.Message, nil)
		return
	}
	if !jsonContentType(r.Header.Get("Content-Type")) {
		writeRPCError(w, http.StatusUnsupportedMediaType, nil, -32600, CodeInvalidParams, "Content-Type must be application/json", nil)
		return
	}
	if onlySSEAccept(r.Header.Get("Accept")) {
		writeRPCError(w, http.StatusNotAcceptable, nil, -32600, CodeInvalidParams, "this server is stateless JSON-only; Accept text/event-stream is not supported", nil)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBodyBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeRPCError(w, http.StatusRequestEntityTooLarge, nil, -32600, CodeRequestTooLarge, fmt.Sprintf("request body exceeds %d bytes", s.maxBodyBytes), nil)
			return
		}
		writeRPCError(w, http.StatusBadRequest, nil, -32600, CodeInvalidParams, "could not read request body", nil)
		return
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		writeRPCError(w, http.StatusBadRequest, nil, -32600, CodeInvalidParams, "empty request body", nil)
		return
	}
	if trimmed[0] == '[' {
		writeRPCError(w, http.StatusBadRequest, nil, -32600, CodeInvalidParams, "batch requests are not supported", nil)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(trimmed, &req); err != nil {
		writeRPCError(w, http.StatusBadRequest, nil, -32700, CodeInvalidParams, "invalid JSON-RPC request", nil)
		return
	}
	if req.ID != nil && !validRPCID(req.ID) {
		writeRPCError(w, http.StatusBadRequest, nil, -32600, CodeInvalidParams, "jsonrpc id must be a string or number", nil)
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != "2.0" {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32600, CodeInvalidParams, "jsonrpc must be 2.0", nil)
		return
	}
	if req.Method == "" {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32600, CodeInvalidParams, "method is required", nil)
		return
	}

	requestID := s.newRequestID()
	// Authenticate every MCP request, including notifications: a stray
	// notification must not be a way to bypass identity logs.
	ip := s.clientIP(r)
	actor, aerr := s.authn.Authenticate(bearerToken(r), ip)
	if aerr != nil {
		if actor.Kind != "" || s.limiter.AllowUnauthAudit(ip) {
			s.recordDenied(r, actor, endpointLabel(serviceID), "", "", "", "", "", requestID, aerr)
		}
		if aerr.HTTPStatus == http.StatusUnauthorized {
			w.Header().Set("WWW-Authenticate", `Bearer realm="relay-mcp"`)
		}
		writeRPCError(w, aerr.HTTPStatus, req.ID, rpcCodeFor(aerr), aerr.Code, aerr.Message, nil)
		return
	}
	if hdr := r.Header.Get("MCP-Protocol-Version"); hdr != "" && !versionSupported(hdr) {
		writeRPCError(w, http.StatusBadRequest, req.ID, -32600, CodeUnsupportedVersion,
			"unsupported MCP protocol version", map[string]any{"supported": supportedVersions})
		return
	}
	if req.ID == nil {
		// Notifications are fire-and-forget.
		w.WriteHeader(http.StatusAccepted)
		return
	}
	if req.Method == "initialize" {
		s.handleInitialize(w, req)
		return
	}
	if strings.HasPrefix(req.Method, "notifications/") {
		writeRPCResult(w, req.ID, map[string]any{})
		return
	}
	switch req.Method {
	case "ping":
		writeRPCResult(w, req.ID, map[string]any{})
	case "tools/list":
		s.handleToolsList(w, req, serviceID, actor, requestID, start, r)
	case "tools/call":
		s.handleToolsCall(w, req, serviceID, actor, requestID, start, r)
	default:
		writeRPCError(w, http.StatusOK, req.ID, -32601, CodeInvalidParams, "method not found", nil)
	}
}

func (s *Server) handleInitialize(w http.ResponseWriter, req rpcRequest) {
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	_ = json.Unmarshal(req.Params, &p)
	version := "2025-06-18"
	if versionSupported(p.ProtocolVersion) {
		version = p.ProtocolVersion
	}
	writeRPCResult(w, req.ID, map[string]any{
		"protocolVersion": version,
		"serverInfo":      map[string]any{"name": "relay-mcp", "version": Version},
		"capabilities":    map[string]any{"tools": map[string]any{}},
	})
}

func (s *Server) handleToolsList(w http.ResponseWriter, req rpcRequest, serviceID string, actor Actor, requestID string, start time.Time, r *http.Request) {
	tools := s.visibleTools(serviceID, actor)
	if serviceID != "" && !s.cfg.serviceEnabled(serviceID) {
		e := NewError(CodeServiceDisabled, "service is disabled", http.StatusForbidden)
		s.recordDenied(r, actor, "/mcp/"+serviceID, serviceID, "", "", "", "", requestID, e)
		writeRPCError(w, http.StatusForbidden, req.ID, -32003, e.Code, e.Message, nil)
		return
	}
	out := make([]map[string]any, 0, len(tools))
	for _, t := range tools {
		out = append(out, toolDescriptor(t))
	}
	writeRPCResult(w, req.ID, map[string]any{"tools": out})
}

func (s *Server) handleToolsCall(w http.ResponseWriter, req rpcRequest, serviceID string, actor Actor, requestID string, start time.Time, r *http.Request) {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(req.Params, &p); err != nil || p.Name == "" {
		writeToolError(w, req.ID, InvalidParams("tools/call requires name and arguments"), nil)
		return
	}
	if allowed, retry, reason := s.limiter.AllowGeneral(actor.Principal()); !allowed {
		e := &Error{Code: CodeRateLimited, Message: "rate limit exceeded", HTTPStatus: http.StatusTooManyRequests,
			Details: map[string]any{"retry_after_seconds": retry, "limit": reason}}
		w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
		s.recordCall(r, actor, endpointLabel(serviceID), serviceID, truncateUTF8(p.Name, 128), "", "", "", "", requestID, start, "denied", e, http.StatusTooManyRequests, nil)
		writeRPCError(w, http.StatusTooManyRequests, req.ID, -32007, e.Code, e.Message, e.Details)
		return
	}
	aggregate := serviceID == ""
	rt, ok := s.registry.ResolveTool(serviceID, p.Name, aggregate)
	if !ok {
		hint := "unknown tool; /mcp expects prefixed names and /mcp/<service> expects short names"
		e := NewError(CodeUnknownTool, hint, http.StatusOK)
		s.recordCall(r, actor, endpointLabel(serviceID), serviceID, truncateUTF8(p.Name, 128), "", "", "", "", requestID, start, "denied", e, http.StatusOK, nil)
		writeToolError(w, req.ID, e, nil)
		return
	}
	raw := p.Arguments
	if len(bytes.TrimSpace(raw)) == 0 {
		raw = json.RawMessage(`{}`)
	}
	if !s.cfg.serviceEnabled(rt.ServiceID) {
		e := NewError(CodeServiceDisabled, "service is disabled", http.StatusForbidden)
		s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusForbidden, nil)
		writeRPCError(w, http.StatusForbidden, req.ID, -32003, e.Code, e.Message, nil)
		return
	}
	if !s.cfg.toolAllowed(rt.ServiceID, rt.Tool) {
		e := NewError(CodeToolDisabled, "tool is disabled", http.StatusForbidden)
		s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusForbidden, nil)
		writeRPCError(w, http.StatusForbidden, req.ID, -32003, e.Code, e.Message, nil)
		return
	}
	class := RiskClass(rt.Tool)
	if !ScopeMatch(actor.Scopes, rt.ServiceID, class, rt.Name) {
		e := NewError(CodeInsufficientScope, "insufficient scope for this tool", http.StatusForbidden)
		s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusForbidden, nil)
		writeRPCError(w, http.StatusForbidden, req.ID, -32002, e.Code, e.Message, nil)
		return
	}
	if rt.Tool.Validate != nil {
		if err := rt.Tool.Validate(raw); err != nil {
			e := AsError(err)
			if e.HTTPStatus == 0 {
				e.HTTPStatus = 400
			}
			if e.Code == "" {
				e = InvalidParams("%s", err.Error())
			}
			s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusOK, nil)
			writeToolError(w, req.ID, e, nil)
			return
		}
	}
	control, cerr := parseControl(raw)
	if cerr != nil {
		e := InvalidParams("%s", cerr.Error())
		s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusOK, nil)
		writeToolError(w, req.ID, e, nil)
		return
	}
	canonical, hash, ferr := FreezeParams(rt.ServiceID, rt.Name, raw)
	if ferr != nil {
		e := InvalidParams("%s", ferr.Error())
		s.recordCall(r, actor, endpointLabel(serviceID), rt.ServiceID, rt.Name, rt.Tool.Risk, "", ParamsSummary(raw), "", requestID, start, "denied", e, http.StatusOK, nil)
		writeToolError(w, req.ID, e, nil)
		return
	}
	call := Call{
		Actor: actor, ServiceID: rt.ServiceID, Tool: rt.Name, Params: raw,
		ParamsHash: hash, ApprovalID: control.ApprovalID, DryRun: control.DryRun,
		RequestID: requestID, Endpoint: endpointLabel(serviceID),
	}
	if WriteTool(rt.Tool) {
		if control.DryRun {
			res := Result{Data: map[string]any{
				"dry_run": true, "valid": true, "service": rt.ServiceID, "tool": rt.Name,
				"params_hash": hash, "would_require_approval": rt.Tool.Risk == RiskHigh,
			}}
			s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), "", requestID, start, "ok", nil, http.StatusOK, &res)
			writeToolResult(w, req.ID, s.envelope(res, rt, time.Since(start)))
			return
		}
		if rt.Tool.Risk == RiskHigh {
			if control.ApprovalID == "" {
				ap, aerr := s.approval.Request(actor, call.Endpoint, rt.ServiceID, rt.Name, raw, requestID)
				if aerr != nil {
					s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), "", requestID, start, "error", aerr, http.StatusOK, nil)
					writeToolError(w, req.ID, aerr, nil)
					return
				}
				res := Result{Data: map[string]any{
					"status": ap.Status, "approval_id": ap.ID, "expires_at": ap.ExpiresAt,
					"execute_before": ap.ExecuteBefore, "params_hash": ap.ParamsHash,
				}}
				s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, ap.ParamsHash, ParamsSummary(canonical), ap.ID, requestID, start, "pending", nil, http.StatusOK, &res)
				writeToolResult(w, req.ID, s.envelope(res, rt, time.Since(start)))
				return
			}
			if allowed, retry, reason := s.limiter.AllowWrite(actor.Principal(), rt.ServiceID, rt.Tool.Risk); !allowed {
				e := &Error{Code: CodeRateLimited, Message: "rate limit exceeded", HTTPStatus: http.StatusTooManyRequests,
					Details: map[string]any{"retry_after_seconds": retry, "limit": reason}}
				w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
				s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), control.ApprovalID, requestID, start, "denied", e, http.StatusTooManyRequests, nil)
				writeRPCError(w, http.StatusTooManyRequests, req.ID, -32007, e.Code, e.Message, e.Details)
				return
			}
			claimed, aerr := s.approval.ClaimForExecution(control.ApprovalID, actor, call.Endpoint, rt.ServiceID, rt.Name, raw)
			if aerr != nil {
				if aerr.Code == CodeApprovalExpired {
					s.recordApprovalExpired(control.ApprovalID, actor)
				}
				s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), control.ApprovalID, requestID, start, "denied", aerr, http.StatusOK, nil)
				writeToolError(w, req.ID, aerr, nil)
				return
			}
			// Execute strictly against the frozen JSON from approval creation,
			// never the raw request bytes.
			call.Params = json.RawMessage(claimed.ParamsJSON)
			res, herr := s.execute(r.Context(), rt, call)
			if herr != nil {
				s.approval.Complete(claimed.ID, approvalOutcome(herr), herr.Code, upstreamStatus(herr))
				s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, claimed.ParamsHash, ParamsSummary(canonical), claimed.ID, requestID, start, approvalOutcome(herr), herr, http.StatusOK, &res)
				writeToolError(w, req.ID, herr, failedMeta(rt, time.Since(start), res, herr))
				return
			}
			s.approval.Complete(claimed.ID, "ok", "", res.UpstreamStatus)
			s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, claimed.ParamsHash, ParamsSummary(canonical), claimed.ID, requestID, start, "executed", nil, http.StatusOK, &res)
			writeToolResult(w, req.ID, s.envelope(res, rt, time.Since(start)))
			return
		}
		if allowed, retry, reason := s.limiter.AllowWrite(actor.Principal(), rt.ServiceID, rt.Tool.Risk); !allowed {
			e := &Error{Code: CodeRateLimited, Message: "rate limit exceeded", HTTPStatus: http.StatusTooManyRequests,
				Details: map[string]any{"retry_after_seconds": retry, "limit": reason}}
			w.Header().Set("Retry-After", fmt.Sprintf("%d", retry))
			s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), "", requestID, start, "denied", e, http.StatusTooManyRequests, nil)
			writeRPCError(w, http.StatusTooManyRequests, req.ID, -32007, e.Code, e.Message, e.Details)
			return
		}
	}
	res, herr := s.execute(r.Context(), rt, call)
	if herr != nil {
		s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), "", requestID, start, "error", herr, http.StatusOK, &res)
		writeToolError(w, req.ID, herr, failedMeta(rt, time.Since(start), res, herr))
		return
	}
	s.recordCall(r, actor, call.Endpoint, rt.ServiceID, rt.Name, rt.Tool.Risk, hash, ParamsSummary(canonical), "", requestID, start, "ok", nil, http.StatusOK, &res)
	writeToolResult(w, req.ID, s.envelope(res, rt, time.Since(start)))
}

func (s *Server) execute(parent context.Context, rt *ResolvedTool, call Call) (Result, *Error) {
	timeout := time.Duration(s.cfg.GatewayTimeoutSecs) * time.Second
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	start := time.Now()
	res, err := rt.Service.Handle(ctx, call)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return res, &Error{Code: CodeUpstreamTimeout, Message: "gateway timeout", HTTPStatus: 200}
		}
		return res, AsError(err)
	}
	res.Latency = time.Since(start)
	return res, nil
}

func approvalOutcome(e *Error) string {
	if e == nil {
		return "ok"
	}
	if e.Code == CodeUpstreamTimeout {
		return "timeout"
	}
	return "upstream_error"
}

func upstreamStatus(e *Error) int {
	if e == nil || e.Details == nil {
		return 0
	}
	if v, ok := e.Details["upstream_status"]; ok {
		switch n := v.(type) {
		case int:
			return n
		case int64:
			return int(n)
		case float64:
			return int(n)
		}
	}
	return 0
}

func failedMeta(rt *ResolvedTool, latency time.Duration, res Result, herr *Error) map[string]any {
	st := res.UpstreamStatus
	if st == 0 {
		st = upstreamStatus(herr)
	}
	return map[string]any{
		"service": rt.ServiceID, "tool": rt.Name, "duration_ms": latency.Milliseconds(),
		"truncated": false, "upstream_status": st,
	}
}

// ---- envelopes ----

func (s *Server) envelope(res Result, rt *ResolvedTool, latency time.Duration) map[string]any {
	if res.Latency == 0 {
		res.Latency = latency
	}
	data := res.Data
	if data == nil {
		data = map[string]any{}
	}
	return map[string]any{
		"ok":   true,
		"data": data,
		"meta": map[string]any{
			"service": rt.ServiceID, "tool": rt.Name,
			"duration_ms": res.Latency.Milliseconds(), "truncated": res.Truncated,
			"row_count": res.RowCount, "upstream_status": res.UpstreamStatus,
		},
	}
}

func errorEnvelope(e *Error) map[string]any {
	if e == nil {
		e = &Error{Code: CodeInternal, Message: "internal error"}
	}
	details := e.Details
	if details == nil {
		details = map[string]any{}
	}
	return map[string]any{"ok": false, "error": map[string]any{
		"code": e.Code, "message": e.Message, "details": details,
	}}
}

func writeToolResult(w http.ResponseWriter, id json.RawMessage, env map[string]any) {
	text, _ := json.Marshal(env)
	result := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": env,
		"isError":           false,
	}
	writeRPCResult(w, id, result)
}

func writeToolError(w http.ResponseWriter, id json.RawMessage, e *Error, meta map[string]any) {
	env := errorEnvelope(e)
	if meta != nil {
		env["meta"] = meta
	}
	text, _ := json.Marshal(env)
	result := map[string]any{
		"content":           []any{map[string]any{"type": "text", "text": string(text)}},
		"structuredContent": env,
		"isError":           true,
	}
	writeRPCResult(w, id, result)
}

func writeRPCResult(w http.ResponseWriter, id json.RawMessage, v any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	writeJSON(w, http.StatusOK, rpcResponse{JSONRPC: "2.0", ID: id, Result: v})
}

func writeRPCError(w http.ResponseWriter, status int, id json.RawMessage, code int, errorCode, msg string, data map[string]any) {
	if id == nil {
		id = json.RawMessage("null")
	}
	if data == nil {
		data = map[string]any{}
	}
	data["error_code"] = errorCode
	writeJSON(w, status, rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg, Data: data}})
}

func rpcCodeFor(e *Error) int {
	switch e.Code {
	case CodeUnauthorized:
		return -32001
	case CodeInsufficientScope, CodeServiceDisabled, CodeToolDisabled:
		return -32002
	case CodeRateLimited:
		return -32007
	case CodeInvalidParams, CodeUnknownTool:
		return -32602
	default:
		return -32603
	}
}

func toolDescriptor(t Tool) map[string]any {
	schema := t.InputSchema
	if len(schema) == 0 {
		schema = json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`)
	}
	desc := map[string]any{
		"name": t.Name, "description": t.Description, "inputSchema": schema,
		"annotations": map[string]any{
			"readOnlyHint": t.ReadOnly, "destructiveHint": t.Destructive,
			"idempotentHint": t.Idempotent, "openWorldHint": t.OpenWorld,
		},
	}
	if len(t.OutputSchema) > 0 {
		desc["outputSchema"] = t.OutputSchema
	}
	return desc
}

func (s *Server) visibleTools(serviceID string, actor Actor) []Tool {
	var out []Tool
	if serviceID != "" {
		for _, t := range s.registry.ToolsFor(serviceID) {
			if s.toolVisible(serviceID, t, actor) {
				out = append(out, t)
			}
		}
		return out
	}
	for _, info := range s.registry.List() {
		if !s.cfg.serviceEnabled(info.ID) {
			continue
		}
		for _, t := range s.registry.ToolsFor(info.ID) {
			if !s.toolVisible(info.ID, t, actor) {
				continue
			}
			t.Name = AggregateName(info.Prefix, t.Name)
			out = append(out, t)
		}
	}
	sortTools(out)
	return out
}

func (s *Server) toolVisible(serviceID string, t Tool, actor Actor) bool {
	if !s.cfg.toolAllowed(serviceID, t) {
		return false
	}
	return ScopeMatch(actor.Scopes, serviceID, RiskClass(t), t.Name)
}

func sortTools(tools []Tool) {
	for i := 1; i < len(tools); i++ {
		for j := i; j > 0 && tools[j].Name < tools[j-1].Name; j-- {
			tools[j], tools[j-1] = tools[j-1], tools[j]
		}
	}
}

// ---- control/params ----

type controlFields struct {
	DryRun     bool   `json:"dry_run"`
	ApprovalID string `json:"approval_id"`
}

func parseControl(raw json.RawMessage) (controlFields, error) {
	var c controlFields
	if len(bytes.TrimSpace(raw)) == 0 {
		return c, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return c, fmt.Errorf("params must be a JSON object")
	}
	if v, ok := m["dry_run"]; ok {
		if err := json.Unmarshal(v, &c.DryRun); err != nil {
			return c, fmt.Errorf("dry_run must be a boolean")
		}
	}
	if v, ok := m["approval_id"]; ok {
		if err := json.Unmarshal(v, &c.ApprovalID); err != nil {
			return c, fmt.Errorf("approval_id must be a string")
		}
	}
	if len(c.ApprovalID) > 128 {
		return c, fmt.Errorf("approval_id is too long")
	}
	return c, nil
}

// ---- request metadata / audit ----

func (s *Server) newRequestID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "req_unknown"
	}
	return "req_" + hex.EncodeToString(b[:])
}

func bearerToken(r *http.Request) string {
	h := strings.TrimSpace(r.Header.Get("Authorization"))
	if len(h) < 7 || !strings.EqualFold(h[:7], "Bearer ") {
		return ""
	}
	return strings.TrimSpace(h[7:])
}

func validRPCID(id json.RawMessage) bool {
	trimmed := bytes.TrimSpace(id)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return false
	}
	return trimmed[0] == '"' || (trimmed[0] >= '0' && trimmed[0] <= '9') || trimmed[0] == '-'
}

func allowedOrigin(r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	if strings.EqualFold(u.Host, r.Host) {
		return true
	}
	host := strings.ToLower(u.Hostname())
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}

func endpointLabel(serviceID string) string {
	if serviceID == "" {
		return "/mcp"
	}
	return "/mcp/" + serviceID
}

func jsonContentType(h string) bool {
	if strings.TrimSpace(h) == "" {
		return true // tolerate clients that omit it; body must still parse
	}
	base := strings.ToLower(strings.TrimSpace(strings.Split(h, ";")[0]))
	return base == "application/json" || strings.HasSuffix(base, "+json")
}

func onlySSEAccept(h string) bool {
	if strings.TrimSpace(h) == "" {
		return false
	}
	parts := strings.Split(h, ",")
	sawSSE := false
	for _, p := range parts {
		p = strings.ToLower(strings.TrimSpace(strings.Split(p, ";")[0]))
		if p == "text/event-stream" {
			sawSSE = true
			continue
		}
		if p == "application/json" || p == "*/*" {
			return false
		}
	}
	return sawSSE
}

func versionSupported(v string) bool {
	for _, s := range supportedVersions {
		if v == s {
			return true
		}
	}
	return false
}

// recordApprovalExpired records the on-demand expiry transition once, even
// if the client retries the same dead approval_id.
func (s *Server) recordApprovalExpired(id string, actor Actor) {
	if id == "" {
		return
	}
	if rows, _, err := s.st.ListMCPAudit(store.MCPAuditFilter{ApprovalID: id, Outcome: "expired"}, 1, 0); err == nil && len(rows) > 0 {
		return
	}
	row, err := s.st.GetMCPApproval(id)
	if err != nil || row == nil || row.Status != "expired" {
		return
	}
	auditActor, authKind := "admin", AuthAdmin
	if actor.Kind != "" {
		auditActor, authKind = actor.AuditActor(), actor.AuthKind
	}
	s.auditor.Record(&store.MCPAuditEntry{
		Ts: s.now().Unix(), RequestID: row.RequestID, Actor: auditActor, AuthKind: authKind,
		KeyID: row.KeyID, KeyName: row.KeyName, Endpoint: row.Endpoint, ServiceID: row.ServiceID,
		Tool: row.Tool, Risk: row.Risk, ParamsHash: row.ParamsHash, ApprovalID: row.ID,
		Outcome: "expired", ErrorCode: CodeApprovalExpired, HTTPStatus: http.StatusOK,
		Detail: "approval expired",
	})
}

func (s *Server) recordDenied(r *http.Request, actor Actor, endpoint, serviceID, tool, risk, hash, summary, requestID string, e *Error) {
	entry := &store.MCPAuditEntry{
		Ts: s.now().Unix(), RequestID: requestID, Actor: "anonymous",
		Endpoint: endpoint, ServiceID: serviceID, Tool: tool, Risk: risk,
		ParamsHash: hash, ParamsSummary: summary, Outcome: "denied",
		ErrorCode: e.Code, HTTPStatus: e.HTTPStatus, Detail: e.Message,
	}
	if actor.Kind != "" {
		entry.Actor, entry.AuthKind, entry.KeyID, entry.KeyName = actor.AuditActor(), actor.AuthKind, actor.KeyID, actor.Name
	}
	if r != nil {
		entry.ClientIP = s.clientIP(r)
	}
	s.auditor.Record(entry)
}

func (s *Server) recordCall(r *http.Request, actor Actor, endpoint, serviceID, tool, risk, hash, summary, approvalID, requestID string, start time.Time, outcome string, e *Error, httpStatus int, res *Result) {
	entry := &store.MCPAuditEntry{
		Ts: s.now().Unix(), RequestID: requestID, Actor: actor.AuditActor(), AuthKind: actor.AuthKind,
		KeyID: actor.KeyID, KeyName: actor.Name, Endpoint: endpoint, ServiceID: serviceID,
		Tool: tool, Risk: risk, ParamsHash: hash, ParamsSummary: summary,
		ApprovalID: approvalID, Outcome: outcome, HTTPStatus: httpStatus,
		LatencyMS: int(time.Since(start).Milliseconds()),
	}
	if actor.Kind == ActorAdmin {
		entry.Actor, entry.AuthKind = "admin", AuthAdmin
	}
	if r != nil {
		entry.ClientIP = s.clientIP(r)
	}
	if e != nil {
		entry.ErrorCode = e.Code
		entry.Detail = e.Message
		entry.UpstreamStatus = upstreamStatus(e)
	}
	if res != nil && res.UpstreamStatus != 0 {
		entry.UpstreamStatus = res.UpstreamStatus
	}
	s.auditor.Record(entry)
}
