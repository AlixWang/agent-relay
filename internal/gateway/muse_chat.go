package gateway

// muse_chat.go is the admin-only direct-chat surface (D1-D26). Every route is
// behind requireAdmin; there is no peer-token or agent-facing entry point.
// The POST body is text-only and rejects structured/approval fields before the
// orchestrator sees them.

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/AlixWang/agent-relay/internal/muse"
)

var chatStartFields = map[string]bool{
	"peer_id": true, "text": true, "session_id": true,
	"budget_secs": true, "block_secs": true, "mode": true, "client_request_id": true,
}

var chatConsentFields = map[string]bool{
	"enabled": true, "consent_version": true, "consent_evidence": true, "reason": true,
}

var chatStructuredFields = map[string]bool{
	"payload": true, "op": true, "target": true, "decision": true,
	"in_reply_to": true, "requires_approval": true, "approval": true,
	"permission": true, "permission_decision": true,
}

func (s *Server) readMuseChatJSON(w http.ResponseWriter, r *http.Request, allowed map[string]bool, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			writeChatError(w, 413, "body_too_large", 0, "")
			return false
		}
		writeChatError(w, 400, "bad_json_body", 0, "")
		return false
	}
	for k := range raw {
		if chatStructuredFields[k] {
			writeChatError(w, 400, "structured_field_forbidden", 0, "")
			return false
		}
		if !allowed[k] {
			writeChatError(w, 400, "unknown_field", 0, "")
			return false
		}
	}
	b, err := json.Marshal(raw)
	if err != nil || json.Unmarshal(b, dst) != nil {
		writeChatError(w, 400, "bad_json_body", 0, "")
		return false
	}
	return true
}

func writeChatError(w http.ResponseWriter, status int, code string, retry int, jobID string) {
	if retry > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(retry))
	}
	body := map[string]any{"ok": false, "error": code, "code": code}
	if retry > 0 {
		body["retry_after_secs"] = retry
	}
	if jobID != "" {
		body["chat_id"] = jobID
	}
	writeJSON(w, status, body)
}

func museChatError(w http.ResponseWriter, err error) {
	var ce *muse.ChatAdmissionError
	if errors.As(err, &ce) {
		writeChatError(w, ce.Status, ce.Code, ce.RetryAfterSecs, ce.JobID)
		return
	}
	switch {
	case errors.Is(err, muse.ErrChatNotFound):
		writeChatError(w, 404, "unknown_chat", 0, "")
	case errors.Is(err, muse.ErrChatNotActive):
		writeChatError(w, 409, "chat_not_active", 0, "")
	case errors.Is(err, muse.ErrNotFound):
		writeChatError(w, 404, "unknown_muse_account", 0, "")
	default:
		writeChatError(w, 500, "muse_chat_failed", 0, "")
	}
}

func (s *Server) handleAdminMuseChatStart(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	var req muse.ChatStartRequest
	if !s.readMuseChatJSON(w, r, chatStartFields, &req) {
		return
	}
	req.Actor = "admin"
	view, err := s.muse.StartChat(r.Context(), req)
	if err != nil {
		museChatError(w, err)
		return
	}
	writeJSON(w, http.StatusAccepted, struct {
		OK bool `json:"ok"`
		*muse.ChatJobView
	}{true, view})
}

func (s *Server) handleAdminMuseChatGet(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	view, err := s.muse.GetChat(r.PathValue("id"))
	if err != nil {
		museChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		OK bool `json:"ok"`
		*muse.ChatJobView
	}{true, view})
}

func (s *Server) handleAdminMuseChatCancel(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	view, err := s.muse.CancelChat(r.Context(), r.PathValue("id"), "admin")
	if err != nil {
		museChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "status": view.Status, "cancel": view.Cancel, "chat": view,
	})
}

func (s *Server) handleAdminMuseChatSettings(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if !s.readMuseChatJSON(w, r, map[string]bool{"enabled": true}, &req) {
		return
	}
	if req.Enabled == nil {
		writeChatError(w, 400, "enabled_required", 0, "")
		return
	}
	status, err := s.muse.SetChatRuntime(*req.Enabled, "admin")
	if err != nil {
		museChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "enabled": status.RuntimeEnabled, "status": status})
}

func (s *Server) handleAdminMuseChatStatus(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.muse.ChatStatus()})
}

func (s *Server) handleAdminMuseChatConsent(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	var req struct {
		Enabled         *bool  `json:"enabled"`
		ConsentVersion  int    `json:"consent_version"`
		ConsentEvidence string `json:"consent_evidence"`
		Reason          string `json:"reason"`
	}
	if !s.readMuseChatJSON(w, r, chatConsentFields, &req) {
		return
	}
	if req.Enabled == nil {
		writeChatError(w, 400, "enabled_required", 0, "")
		return
	}
	acct, err := s.muse.SetChatConsent(r.PathValue("id"), *req.Enabled,
		req.ConsentVersion, req.ConsentEvidence, req.Reason, "admin")
	if err != nil {
		museChatError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": acct})
}
