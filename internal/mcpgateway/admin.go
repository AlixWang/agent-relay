package mcpgateway

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/AlixWang/agent-relay/internal/store"
)

// AdminHandler serves /admin/mcp/*. The caller (internal/gateway) wraps the
// whole handler in requireAdmin; these handlers never see an unauthenticated
// request in production.
func (s *Server) AdminHandler() http.Handler {
	return s.adminHandler
}

func (s *Server) buildAdminHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /admin/mcp/services", s.adminServices)
	mux.HandleFunc("GET /admin/mcp/tools", s.adminTools)
	mux.HandleFunc("GET /admin/mcp/keys", s.adminListKeys)
	mux.HandleFunc("POST /admin/mcp/keys", s.adminCreateKey)
	mux.HandleFunc("POST /admin/mcp/keys/{id}/rotate", s.adminRotateKey)
	mux.HandleFunc("POST /admin/mcp/keys/{id}/revoke", s.adminRevokeKey)
	mux.HandleFunc("GET /admin/mcp/peers", s.adminListPeers)
	mux.HandleFunc("PUT /admin/mcp/peers/{peer_id}", s.adminPutPeer)
	mux.HandleFunc("DELETE /admin/mcp/peers/{peer_id}", s.adminDeletePeer)
	mux.HandleFunc("GET /admin/mcp/approvals", s.adminListApprovals)
	mux.HandleFunc("GET /admin/mcp/approvals/{id}", s.adminGetApproval)
	mux.HandleFunc("POST /admin/mcp/approvals/{id}/approve", s.adminApprove)
	mux.HandleFunc("POST /admin/mcp/approvals/{id}/reject", s.adminReject)
	mux.HandleFunc("GET /admin/mcp/audit", s.adminAuditList)
	mux.HandleFunc("GET /admin/mcp/audit/export", s.adminAuditExport)
	mux.HandleFunc("POST /admin/mcp/dry-run", s.adminDryRun)
	mux.HandleFunc("GET /admin/mcp/health", s.adminHealth)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Key plaintext is returned once here; never let a browser/proxy cache it.
		w.Header().Set("Cache-Control", "no-store")
		mux.ServeHTTP(w, r)
	})
}

func (s *Server) readAdminJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, s.maxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		if errors.Is(err, io.EOF) {
			return true
		}
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "bad json body: " + err.Error()})
		return false
	}
	return true
}

func adminError(w http.ResponseWriter, e *Error) {
	status := e.HTTPStatus
	if status < 400 {
		status = http.StatusConflict
	}
	writeJSON(w, status, map[string]any{"ok": false, "error": e.Message, "code": e.Code})
}

func (s *Server) auditAdmin(action, detail string) {
	s.auditor.RecordAdmin(action, detail)
}

// ---- services / tools ----

func (s *Server) adminServices(w http.ResponseWriter, r *http.Request) {
	now := s.now().Unix()
	out := []map[string]any{}
	for _, info := range s.registry.List() {
		cfgSvc := s.cfg.Service(info.ID)
		entry := map[string]any{
			"id": info.ID, "display_name": info.DisplayName, "version": info.Version,
			"prefix": info.Prefix, "enabled": cfgSvc.Enabled, "registered": true,
			"credential_ref": cfgSvc.CredentialRef, "allowed_tools": cfgSvc.AllowedTools,
			"tool_count": info.ToolCount, "enabled_tool_count": 0,
			"write_enabled":       s.cfg.Write.Enabled,
			"write_allowed_tools": s.cfg.writeService(info.ID).AllowedTools,
		}
		for _, t := range info.Tools {
			if s.cfg.toolAllowed(info.ID, t) {
				entry["enabled_tool_count"] = entry["enabled_tool_count"].(int) + 1
			}
		}
		recent, _, err := s.st.ListMCPAudit(store.MCPAuditFilter{ServiceID: info.ID}, 50, 0)
		if err == nil {
			for _, a := range recent {
				if _, ok := entry["last_call_at"]; !ok {
					entry["last_call_at"] = a.Ts
				}
				if (a.Outcome == "error" || a.Outcome == "denied") && entry["last_error"] == nil {
					entry["last_error"] = map[string]any{
						"ts": a.Ts, "tool": a.Tool, "code": a.ErrorCode, "outcome": a.Outcome,
					}
				}
			}
		}
		entry["now"] = now
		out = append(out, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "services": out})
}

func (s *Server) adminTools(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, info := range s.registry.List() {
		for _, t := range s.registry.ToolsFor(info.ID) {
			out = append(out, map[string]any{
				"service_id": info.ID, "name": t.Name, "aggregate_name": AggregateName(info.Prefix, t.Name),
				"description": t.Description, "risk": t.Risk, "read_only": t.ReadOnly,
				"destructive": t.Destructive, "idempotent": t.Idempotent, "open_world": t.OpenWorld,
				"enabled": s.cfg.toolAllowed(info.ID, t),
				"scope":   fmt.Sprintf("mcp:%s:%s:%s", info.ID, RiskClass(t), t.Name),
			})
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "tools": out})
}

// ---- keys ----

func keyView(k *store.MCPKey, now int64) map[string]any {
	revoked := k.Status == "revoked" || (k.RevokedAt != 0 && k.RevokedAt <= now)
	status := k.Status
	if revoked && status == "active" {
		status = "revoked"
	}
	return map[string]any{
		"id": k.ID, "principal_type": k.PrincipalType, "peer_id": k.PeerID, "name": k.Name,
		"key_prefix": k.KeyPrefix, "scopes": k.Scopes, "status": status, "note": k.Note,
		"created_by": k.CreatedBy, "created_at": k.CreatedAt, "expires_at": k.ExpiresAt,
		"revoked_at": k.RevokedAt, "rotated_from": k.RotatedFrom, "last_used_at": k.LastUsedAt,
		"last_used_ip": k.LastUsedIP, "use_count": k.UseCount,
		"expired": KeyExpired(k.ExpiresAt, now), "revoked": revoked,
	}
}

func (s *Server) adminListKeys(w http.ResponseWriter, r *http.Request) {
	rows, err := s.st.ListMCPKeys()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list keys")
		return
	}
	now := s.now().Unix()
	out := make([]map[string]any, 0, len(rows))
	for _, k := range rows {
		out = append(out, keyView(k, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "keys": out})
}

type createKeyRequest struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays int      `json:"expires_in_days"`
	Note          string   `json:"note"`
}

func validKeyName(name string) bool {
	if !utf8.ValidString(name) || len(name) == 0 || len(name) > 80 {
		return false
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

func (s *Server) adminCreateKey(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if !s.readAdminJSON(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if !validKeyName(req.Name) {
		writeErr(w, http.StatusBadRequest, "name is required (1-80 printable characters)")
		return
	}
	if len(req.Scopes) == 0 {
		// #25: a new key defaults to read-only across configured services.
		for _, info := range s.registry.List() {
			if s.cfg.serviceEnabled(info.ID) {
				req.Scopes = append(req.Scopes, fmt.Sprintf("mcp:%s:read:*", info.ID))
			}
		}
	}
	scopes, err := ValidateScopes(s.registry, req.Scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if req.ExpiresInDays < 0 || req.ExpiresInDays > 3650 {
		writeErr(w, http.StatusBadRequest, "expires_in_days must be 0-3650")
		return
	}
	days := req.ExpiresInDays
	if days == 0 {
		days = keyTTLDays(scopes, s.cfg.Keys)
	}
	if len(req.Note) > 500 {
		writeErr(w, http.StatusBadRequest, "note is too long")
		return
	}
	plaintext, prefix, hash, err := GenerateKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate key")
		return
	}
	id, err := NewID("mcpkey_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate key id")
		return
	}
	now := s.now().Unix()
	row := &store.MCPKey{
		ID: id, PrincipalType: "key", Name: req.Name, KeyPrefix: prefix, KeyHash: hash,
		Scopes: scopes, Status: "active", Note: strings.TrimSpace(req.Note), CreatedBy: "admin",
		CreatedAt: now, ExpiresAt: now + int64(days)*86400,
	}
	if err := s.st.CreateMCPKey(row); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not store key")
		return
	}
	s.auditAdmin("mcp.key_create", fmt.Sprintf("id=%s name=%s prefix=%s scopes=%s", id, req.Name, prefix, strings.Join(scopes, ",")))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "key": keyView(row, now), "plaintext": plaintext,
		"notice": "this is the only time the key plaintext is returned",
	})
}

func (s *Server) adminRotateKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	old, err := s.st.GetMCPKey(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load key")
		return
	}
	if old == nil || old.PrincipalType != "key" {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	now := s.now().Unix()
	if old.Status != "active" || old.RevokedAt != 0 || KeyExpired(old.ExpiresAt, now) {
		writeErr(w, http.StatusConflict, "key is not active")
		return
	}
	plaintext, prefix, hash, err := GenerateKey()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate key")
		return
	}
	newID, err := NewID("mcpkey_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate key id")
		return
	}
	days := keyTTLDays(old.Scopes, s.cfg.Keys)
	row := &store.MCPKey{
		ID: newID, PrincipalType: "key", Name: old.Name, KeyPrefix: prefix, KeyHash: hash,
		Scopes: old.Scopes, Status: "active", Note: old.Note, CreatedBy: "admin",
		CreatedAt: now, ExpiresAt: now + int64(days)*86400, RotatedFrom: old.ID,
	}
	grace := s.cfg.Keys.normalized().RotateGraceSecs
	var graceRevokeAt int64
	if grace > 0 {
		graceRevokeAt = now + int64(grace)
	}
	ok, err := s.st.RotateMCPKey(old.ID, row, now, graceRevokeAt)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not rotate key")
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, "key is not active")
		return
	}
	s.auditAdmin("mcp.key_rotate", fmt.Sprintf("old=%s new=%s name=%s grace_secs=%d", old.ID, newID, old.Name, grace))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "key": keyView(row, now), "plaintext": plaintext, "rotated_from": old.ID,
		"notice": "this is the only time the key plaintext is returned",
	})
}

func (s *Server) adminRevokeKey(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	row, err := s.st.GetMCPKey(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load key")
		return
	}
	if row == nil || row.PrincipalType != "key" {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	ok, err := s.st.RevokeMCPKey(id, s.now().Unix())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not revoke key")
		return
	}
	if !ok {
		writeErr(w, http.StatusConflict, "key is already revoked")
		return
	}
	s.auditAdmin("mcp.key_revoke", "id="+id)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- peers ----

func (s *Server) adminListPeers(w http.ResponseWriter, r *http.Request) {
	peers, err := s.st.ListPeers()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list peers")
		return
	}
	grants, gerr := s.st.ListMCPPeerGrants()
	byPeer := map[string]*store.MCPKey{}
	if gerr == nil {
		for _, g := range grants {
			byPeer[g.PeerID] = g
		}
	}
	now := s.now().Unix()
	out := make([]map[string]any, 0, len(peers))
	for _, p := range peers {
		view := map[string]any{
			"peer_id": p.ID, "display_name": p.DisplayName, "agent_type": p.AgentType,
			"status": p.Status, "protocol_version": p.ProtocolVer, "last_seen": p.LastSeen,
			"authorized": false, "scopes": []string{}, "mcp_status": "none",
		}
		if grant := byPeer[p.ID]; grant != nil {
			view["authorized"] = p.Status == "active" && grant.Status == "active" && len(grant.Scopes) > 0 && !KeyExpired(grant.ExpiresAt, now)
			view["scopes"] = grant.Scopes
			view["mcp_status"] = grant.Status
			view["note"] = grant.Note
			view["updated_at"] = grant.CreatedAt
		}
		out = append(out, view)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peers": out})
}

type putPeerRequest struct {
	Scopes  []string `json:"scopes"`
	Enabled *bool    `json:"enabled"`
	Note    string   `json:"note"`
}

func (s *Server) adminPutPeer(w http.ResponseWriter, r *http.Request) {
	peerID := r.PathValue("peer_id")
	peer, err := s.st.GetPeer(peerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load peer")
		return
	}
	if peer == nil {
		writeErr(w, http.StatusNotFound, "peer not found")
		return
	}
	var req putPeerRequest
	if !s.readAdminJSON(w, r, &req) {
		return
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	if len(req.Note) > 500 {
		writeErr(w, http.StatusBadRequest, "note is too long")
		return
	}
	now := s.now().Unix()
	if !enabled {
		if grant, _ := s.st.GetMCPPeerGrant(peerID); grant != nil {
			_, _ = s.st.RevokeMCPKey(grant.ID, now)
		}
		s.auditAdmin("mcp.peer_revoke", "peer="+peerID)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "peer_id": peerID, "enabled": false})
		return
	}
	scopes, err := ValidateScopes(s.registry, req.Scopes)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	id, err := NewID("mcppeer_")
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not generate grant id")
		return
	}
	row := &store.MCPKey{
		ID: id, PrincipalType: "peer", PeerID: peerID, Name: peerID,
		Scopes: scopes, Status: "active", Note: strings.TrimSpace(req.Note), CreatedBy: "admin",
		CreatedAt: now, ExpiresAt: 0,
	}
	grant, err := s.st.SaveMCPPeerGrant(row)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not save peer grant")
		return
	}
	s.auditAdmin("mcp.peer_grant", fmt.Sprintf("peer=%s scopes=%s", peerID, strings.Join(scopes, ",")))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "peer_id": peerID, "authorized": peer.Status == "active",
		"scopes": grant.Scopes, "mcp_status": grant.Status,
	})
}

func (s *Server) adminDeletePeer(w http.ResponseWriter, r *http.Request) {
	peerID := r.PathValue("peer_id")
	grant, err := s.st.GetMCPPeerGrant(peerID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load peer grant")
		return
	}
	if grant == nil {
		writeErr(w, http.StatusNotFound, "peer authorization not found")
		return
	}
	if _, err := s.st.RevokeMCPKey(grant.ID, s.now().Unix()); err != nil {
		writeErr(w, http.StatusInternalServerError, "could not revoke peer grant")
		return
	}
	s.auditAdmin("mcp.peer_revoke", "peer="+peerID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ---- approvals ----

func approvalView(a *store.MCPApproval, now int64) map[string]any {
	remaining := int64(0)
	switch a.Status {
	case "pending":
		remaining = a.ExpiresAt - now
	case "approved":
		remaining = a.ExecuteBefore - now
	}
	if remaining < 0 {
		remaining = 0
	}
	view := map[string]any{
		"id": a.ID, "key_id": a.KeyID, "key_name": a.KeyName, "principal": a.Principal,
		"auth_kind": a.AuthKind, "endpoint": a.Endpoint, "service_id": a.ServiceID,
		"tool": a.Tool, "risk": a.Risk, "params_json": a.ParamsJSON, "params_hash": a.ParamsHash,
		"status": a.Status, "requested_at": a.RequestedAt, "expires_at": a.ExpiresAt,
		"decided_at": a.DecidedAt, "decided_by": a.DecidedBy, "decision_note": a.DecisionNote,
		"execute_before": a.ExecuteBefore, "executed_at": a.ExecutedAt, "outcome": a.Outcome,
		"error_code": a.ErrorCode, "upstream_status": a.UpstreamStatus,
		"request_id": a.RequestID, "remaining_seconds": remaining,
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(a.ParamsJSON), &params); err == nil {
		view["params"] = params
	}
	return view
}

func (s *Server) adminListApprovals(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	status := q.Get("status")
	if status != "" {
		switch status {
		case "pending", "approved", "rejected", "expired", "executed":
		default:
			writeErr(w, http.StatusBadRequest, "invalid status filter")
			return
		}
	}
	page, size := parsePage(q)
	f := store.MCPApprovalFilter{
		Status: status, Principal: q.Get("principal"),
		ServiceID: q.Get("service_id"), Tool: q.Get("tool"),
	}
	rows, total, err := s.st.ListMCPApprovals(f, size, (page-1)*size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list approvals")
		return
	}
	now := s.now().Unix()
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, approvalView(a, now))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "approvals": out, "total": total, "page": page, "page_size": size,
	})
}

func (s *Server) adminGetApproval(w http.ResponseWriter, r *http.Request) {
	row, err := s.st.GetMCPApproval(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not load approval")
		return
	}
	if row == nil {
		writeErr(w, http.StatusNotFound, "approval not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approval": approvalView(row, s.now().Unix())})
}

func (s *Server) auditApproval(a *store.MCPApproval, outcome, note string) {
	s.auditor.Record(&store.MCPAuditEntry{
		Ts: s.now().Unix(), RequestID: a.RequestID, Actor: "admin", AuthKind: AuthAdmin,
		KeyID: a.KeyID, KeyName: a.KeyName, Endpoint: a.Endpoint, ServiceID: a.ServiceID,
		Tool: a.Tool, Risk: a.Risk, ParamsHash: a.ParamsHash, ApprovalID: a.ID,
		Outcome: outcome, HTTPStatus: http.StatusOK, Detail: note,
	})
}

func (s *Server) adminApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if !s.readAdminJSON(w, r, &req) {
		return
	}
	if len(req.Note) > 500 {
		writeErr(w, http.StatusBadRequest, "note is too long")
		return
	}
	row, aerr := s.approval.Approve(r.PathValue("id"), "admin", strings.TrimSpace(req.Note))
	if aerr != nil {
		if aerr.Code == CodeApprovalExpired {
			s.recordApprovalExpired(r.PathValue("id"), Actor{})
		}
		adminError(w, aerr)
		return
	}
	s.auditApproval(row, "approved", "approved by admin")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approval": approvalView(row, s.now().Unix())})
}

func (s *Server) adminReject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Note string `json:"note"`
	}
	if !s.readAdminJSON(w, r, &req) {
		return
	}
	if len(req.Note) > 500 {
		writeErr(w, http.StatusBadRequest, "note is too long")
		return
	}
	row, aerr := s.approval.Reject(r.PathValue("id"), "admin", strings.TrimSpace(req.Note))
	if aerr != nil {
		if aerr.Code == CodeApprovalExpired {
			s.recordApprovalExpired(r.PathValue("id"), Actor{})
		}
		adminError(w, aerr)
		return
	}
	s.auditApproval(row, "rejected", "rejected by admin: "+strings.TrimSpace(req.Note))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "approval": approvalView(row, s.now().Unix())})
}

// ---- audit ----

func (s *Server) auditFilter(r *http.Request) store.MCPAuditFilter {
	q := r.URL.Query()
	f := store.MCPAuditFilter{
		Actor: q.Get("actor"), ServiceID: q.Get("service_id"), Tool: q.Get("tool"),
		Outcome: q.Get("outcome"), ApprovalID: q.Get("approval_id"),
	}
	if v, err := strconv.ParseInt(q.Get("since"), 10, 64); err == nil {
		f.Since = v
	}
	if v, err := strconv.ParseInt(q.Get("until"), 10, 64); err == nil {
		f.Until = v
	}
	return f
}

func auditView(a *store.MCPAuditEntry) map[string]any {
	return map[string]any{
		"seq": a.Seq, "ts": a.Ts, "request_id": a.RequestID, "actor": a.Actor,
		"auth_kind": a.AuthKind, "key_id": a.KeyID, "key_name": a.KeyName,
		"endpoint": a.Endpoint, "service_id": a.ServiceID, "tool": a.Tool, "risk": a.Risk,
		"params_hash": a.ParamsHash, "params_summary": a.ParamsSummary, "approval_id": a.ApprovalID,
		"outcome": a.Outcome, "error_code": a.ErrorCode, "http_status": a.HTTPStatus,
		"upstream_status": a.UpstreamStatus, "latency_ms": a.LatencyMS,
		"client_ip": a.ClientIP, "detail": a.Detail,
	}
}

func (s *Server) adminAuditList(w http.ResponseWriter, r *http.Request) {
	page, size := parsePage(r.URL.Query())
	rows, total, err := s.st.ListMCPAudit(s.auditFilter(r), size, (page-1)*size)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not list audit rows")
		return
	}
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		out = append(out, auditView(a))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "entries": out, "total": total, "page": page, "page_size": size,
	})
}

func (s *Server) adminAuditExport(w http.ResponseWriter, r *http.Request) {
	rows, _, err := s.st.ListMCPAudit(s.auditFilter(r), 10000, 0)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "could not export audit rows")
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", fmt.Sprintf("mcp-audit-%d.csv", s.now().Unix())))
	w.Header().Set("Cache-Control", "no-store")
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{
		"seq", "ts", "time", "request_id", "actor", "auth_kind", "key_name", "endpoint",
		"service", "tool", "risk", "params_hash", "params_summary", "approval_id",
		"outcome", "error_code", "http_status", "upstream_status", "latency_ms", "client_ip", "detail",
	})
	for _, a := range rows {
		_ = cw.Write([]string{
			strconv.FormatInt(a.Seq, 10), strconv.FormatInt(a.Ts, 10),
			time.Unix(a.Ts, 0).UTC().Format(time.RFC3339),
			csvSafeCell(a.RequestID), csvSafeCell(a.Actor), csvSafeCell(a.AuthKind),
			csvSafeCell(a.KeyName), csvSafeCell(a.Endpoint), csvSafeCell(a.ServiceID),
			csvSafeCell(a.Tool), csvSafeCell(a.Risk), csvSafeCell(a.ParamsHash),
			csvSafeCell(a.ParamsSummary), csvSafeCell(a.ApprovalID), csvSafeCell(a.Outcome),
			csvSafeCell(a.ErrorCode), strconv.Itoa(a.HTTPStatus),
			strconv.Itoa(a.UpstreamStatus), strconv.Itoa(a.LatencyMS),
			csvSafeCell(a.ClientIP), csvSafeCell(a.Detail),
		})
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		log.Printf("mcp audit export: %v", err)
	}
}

// csvSafeCell neutralizes spreadsheet formula injection (OWASP CSV Injection).
func csvSafeCell(s string) string {
	trimmed := strings.TrimLeft(s, " \t\r\n")
	if trimmed == "" {
		return s
	}
	switch trimmed[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + s
	}
	return s
}

// ---- dry run / health ----

type dryRunRequest struct {
	Service string          `json:"service"`
	Tool    string          `json:"tool"`
	Params  json.RawMessage `json:"params"`
}

func (s *Server) adminDryRun(w http.ResponseWriter, r *http.Request) {
	var req dryRunRequest
	if !s.readAdminJSON(w, r, &req) {
		return
	}
	rt, ok := s.registry.ResolveTool(req.Service, req.Tool, false)
	if !ok {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "valid": false, "error": "unknown service/tool"})
		return
	}
	if len(req.Params) == 0 {
		req.Params = json.RawMessage(`{}`)
	}
	enabled := s.cfg.toolAllowed(rt.ServiceID, rt.Tool)
	if rt.Tool.Validate != nil {
		if err := rt.Tool.Validate(req.Params); err != nil {
			e := AsError(err)
			writeJSON(w, http.StatusOK, map[string]any{"ok": true, "valid": false, "error": e.Message, "code": e.Code, "tool_enabled": enabled})
			return
		}
	}
	_, hash, err := FreezeParams(rt.ServiceID, rt.Name, req.Params)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "valid": false, "error": err.Error(), "code": CodeInvalidParams})
		return
	}
	s.auditAdmin("mcp.dry_run", fmt.Sprintf("service=%s tool=%s valid=true", rt.ServiceID, rt.Name))
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "valid": true, "service": rt.ServiceID, "tool": rt.Name,
		"risk": rt.Tool.Risk, "tool_enabled": enabled, "write_enabled": s.cfg.Write.Enabled,
		"params_hash": hash, "would_require_approval": rt.Tool.Risk == RiskHigh && s.cfg.Write.Enabled,
	})
}

func (s *Server) adminHealth(w http.ResponseWriter, r *http.Request) {
	now := s.now().Unix()
	pending := s.PendingApprovals()
	keys, _ := s.st.ListMCPKeys()
	activeKeys := 0
	for _, k := range keys {
		if k.Status == "active" && (k.RevokedAt == 0 || k.RevokedAt > now) && !KeyExpired(k.ExpiresAt, now) {
			activeKeys++
		}
	}
	services := []map[string]any{}
	for _, info := range s.registry.List() {
		services = append(services, map[string]any{
			"id": info.ID, "enabled": s.cfg.serviceEnabled(info.ID),
			"tool_count": info.ToolCount,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "version": Version, "enabled": s.cfg.Enabled, "write_enabled": s.cfg.Write.Enabled,
		"pending_approvals": pending, "keys": activeKeys, "services": services,
		"protocol_versions": supportedVersions, "batch_cap": s.cfg.Write.BatchCap,
	})
}

// parsePage is the gateway-independent page parser (same limits as console).
func parsePage(q map[string][]string) (int, int) {
	page, size := 1, 20
	if v, err := strconv.Atoi(first(q["page"])); err == nil && v > 0 {
		if v > 100000 {
			v = 100000
		}
		page = v
	}
	if v, err := strconv.Atoi(first(q["page_size"])); err == nil && v > 0 {
		size = v
	}
	if size > 200 {
		size = 200
	}
	return page, size
}

func first(v []string) string {
	if len(v) == 0 {
		return ""
	}
	return v[0]
}
