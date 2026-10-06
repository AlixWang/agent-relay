// Package gateway is the HTTP entry point (DESIGN §4.1).
// Routes protocol + admin APIs, enforces body limits, uniform errors,
// per-identity auth binding, and version negotiation. No business logic.
package gateway

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/config"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/prompts"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/verify"
	"golang.org/x/crypto/bcrypt"
)

// Server bundles all dependencies for HTTP handlers.
type Server struct {
	cfg      *config.Config
	st       store.Store
	auth     *auth.Service
	guard    *guard.Guard
	queue    *queue.Service
	presence *presence.Service
	verify   *verify.Service

	adminHash []byte
	mu        sync.Mutex
	sessions  map[string]int64 // session token -> expiry unix

	serverAddr string // advertised in prompts, e.g. http://100.x.y.z:18789
}

func New(cfg *config.Config, st store.Store, au *auth.Service, q *queue.Service,
	p *presence.Service, v *verify.Service, serverAddr string) *Server {
	return &Server{
		cfg: cfg, st: st, auth: au, queue: q,
		presence: p, verify: v,
		sessions:   map[string]int64{},
		serverAddr: serverAddr,
	}
}

// SetGuard wires the guard for admin fuse-reset (kept separate from New
// so existing construction sites don't change signature).
func (s *Server) SetGuard(g *guard.Guard) { s.guard = g }

func (s *Server) SetAdminHash(hash []byte) { s.adminHash = hash }

// Handler builds the mux.
func (s *Server) Handler(web http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /messages", s.handleSend)
	mux.HandleFunc("GET /messages", s.handlePull)
	mux.HandleFunc("POST /ack", s.handleAck)
	mux.HandleFunc("POST /heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /peers", s.handlePeers)
	mux.HandleFunc("POST /verify/smoke", s.handleSmoke)

	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("POST /admin/invites", s.requireAdmin(s.handleAdminCreateInvite))
	mux.HandleFunc("GET /admin/invites", s.requireAdmin(s.handleAdminListInvites))
	mux.HandleFunc("DELETE /admin/invites/{code}", s.requireAdmin(s.handleAdminDeleteInvite))
	mux.HandleFunc("GET /admin/peers", s.requireAdmin(s.handleAdminListPeers))
	mux.HandleFunc("PATCH /admin/peers/{id}", s.requireAdmin(s.handleAdminPatchPeer))
	mux.HandleFunc("GET /admin/tokens", s.requireAdmin(s.handleAdminListTokens))
	mux.HandleFunc("DELETE /admin/tokens/{id}", s.requireAdmin(s.handleAdminRevokeToken))
	mux.HandleFunc("POST /admin/tokens/rotate", s.requireAdmin(s.handleAdminRotateToken))
	mux.HandleFunc("GET /admin/messages", s.requireAdmin(s.handleAdminMessages))
	mux.HandleFunc("GET /admin/messages/export", s.requireAdmin(s.handleAdminExport))
	mux.HandleFunc("POST /admin/messages/approve", s.requireAdmin(s.handleAdminApprove))
	mux.HandleFunc("POST /admin/fuse/reset", s.requireAdmin(s.handleAdminFuseReset))
	mux.HandleFunc("GET /admin/audit", s.requireAdmin(s.handleAdminAudit))
	mux.HandleFunc("POST /admin/prompts", s.requireAdmin(s.handleAdminPrompts))
	mux.HandleFunc("GET /admin/stats", s.requireAdmin(s.handleAdminStats))
	mux.HandleFunc("GET /admin/config", s.requireAdmin(s.handleAdminConfig))

	if web != nil {
		// Catch-all for the embedded console (longest-match wins over "/";
		// API routes above take precedence).
		mux.Handle("/", web)
	}
	return mux
}

// ---- helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func (s *Server) readJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxBodyBytes))
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		writeErr(w, 400, "bad json body")
		return false
	}
	return true
}

func bearer(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(h, "Bearer "))
}

// clientIP resolves the peer IP. X-Forwarded-For is trusted only when the
// TCP peer is inside the configured trusted-proxy CIDR (default loopback);
// otherwise any client could spoof audit/last_ip via a forged header.
func (s *Server) clientIP(r *http.Request) string {
	remote := r.RemoteAddr
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	if h := r.Header.Get("X-Forwarded-For"); h != "" {
		if ip := net.ParseIP(host); ip != nil && s.cfg.TrustedProxyNet().Contains(ip) {
			parts := strings.Split(h, ",")
			if first := strings.TrimSpace(parts[0]); first != "" {
				return first
			}
		}
	}
	return host
}

// authed resolves the bearer token. Writes 401 on failure.
func (s *Server) authed(w http.ResponseWriter, r *http.Request) (*store.Peer, *store.TokenRow) {
	tok := bearer(r)
	p, t, err := s.auth.Authenticate(tok)
	if err != nil {
		log.Printf("auth error: %v", err)
		writeErr(w, 500, "auth failure")
		return nil, nil
	}
	if p == nil {
		writeErr(w, 401, "unauthorized")
		return nil, nil
	}
	s.auth.Touch(t.ID, s.clientIP(r))
	return p, t
}

// checkVersion enforces §8.5: below min_client → 426.
func (s *Server) checkVersion(w http.ResponseWriter, p *store.Peer) bool {
	if p.ProtocolVer < s.cfg.MinClient {
		writeJSON(w, 426, map[string]any{
			"ok": false, "error": "upgrade required: re-run the current onboarding prompt",
		})
		return false
	}
	return true
}

// ---- protocol handlers (Appendix A) ----

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true, "version": 2,
		"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
	})
}

type registerReq struct {
	Code            string `json:"code"`
	ID              string `json:"id"`
	AgentType       string `json:"agent_type"`
	ProtocolVersion int    `json:"protocol_version"`
	Capabilities    string `json:"capabilities"`
}

func (s *Server) handleRegister(w http.ResponseWriter, r *http.Request) {
	var req registerReq
	if !s.readJSON(w, r, &req) {
		return
	}
	// Stale onboarding prompts are rejected at the door (§8.5): minting a
	// token for a client that can never pass the 426 gate would only
	// produce a confusing register-ok-then-everything-426 experience.
	if req.ProtocolVersion < s.cfg.MinClient {
		writeJSON(w, 426, map[string]any{
			"ok": false, "error": "upgrade required: re-run the current onboarding prompt",
		})
		return
	}
	peer, plaintext, err := s.auth.Register(req.Code, req.ID, req.AgentType,
		req.ProtocolVersion, req.Capabilities, time.Now().Unix())
	if err != nil {
		if re, ok := err.(*auth.RegisterError); ok {
			writeErr(w, 400, re.Code+": "+re.Detail)
			return
		}
		log.Printf("register error: %v", err)
		writeErr(w, 500, "register failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "peer_id": peer.ID, "token": plaintext})
}

func (s *Server) handleSend(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	var req queue.SendRequest
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.From == "" {
		req.From = peer.ID
	}
	if req.From != peer.ID {
		writeErr(w, 403, "from must match your authenticated identity")
		return
	}
	now := time.Now().Unix()
	seq, held, rootID, err := s.queue.Send(&req, now)
	if err != nil {
		s.sendRejection(w, err)
		return
	}
	if held {
		writeJSON(w, 202, map[string]any{"ok": true, "held": true, "seq": seq, "id": req.ID, "thread": rootID})
		return
	}
	// Opportunistic verify completion: a result/ack may complete smoke.
	if done, _ := s.verify.CheckCompletion(peer.ID, now); done {
		log.Printf("peer verified: %s", peer.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "seq": seq, "id": req.ID})
}

func (s *Server) sendRejection(w http.ResponseWriter, err error) {
	if rej, ok := err.(*guard.Rejection); ok {
		if rej.Code == "rate_limited" {
			retry := rej.Retry
			if retry < 1 {
				retry = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retry))
			writeJSON(w, 429, map[string]any{"ok": false, "error": rej.Error()})
			return
		}
		code := 400
		switch rej.Code {
		case "duplicate_id", "loop_fuse_tripped", "loop_guard":
			code = 409
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": rej.Error()})
		return
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "duplicate_id"),
		strings.HasPrefix(msg, "loop_fuse_tripped"),
		strings.HasPrefix(msg, "loop_guard"):
		writeJSON(w, 409, map[string]any{"ok": false, "error": msg})
	case strings.HasPrefix(msg, "rate_limited"):
		w.Header().Set("Retry-After", "10")
		writeJSON(w, 429, map[string]any{"ok": false, "error": msg})
	default:
		if strings.HasPrefix(msg, "bad_message") {
			writeErr(w, 400, msg)
			return
		}
		log.Printf("send error: %v", err)
		writeErr(w, 500, "send failed")
	}
}

func (s *Server) handlePull(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	forID := r.URL.Query().Get("for")
	if forID == "" {
		forID = peer.ID
	}
	if forID != peer.ID {
		writeErr(w, 403, "for must match your authenticated identity")
		return
	}
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	msgs, err := s.queue.Visible(forID, since, 200)
	if err != nil {
		log.Printf("pull error: %v", err)
		writeErr(w, 500, "pull failed")
		return
	}
	next := since
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if m.Seq > next {
			next = m.Seq
		}
		items = append(items, map[string]any{
			"seq": m.Seq, "id": m.ID, "from": m.Sender, "to": m.Recipient,
			"kind": m.Kind, "in_reply_to": m.InReplyTo, "thread": m.RootID,
			"payload": m.Payload, "created_at": m.CreatedAt,
			"approval_state": m.ApprovalState,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": items, "next_since": next})
}

type ackReq struct {
	MessageID string `json:"message_id"`
	By        string `json:"by"`
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	var req ackReq
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.By == "" {
		req.By = peer.ID
	}
	if req.By != peer.ID {
		writeErr(w, 403, "by must match your authenticated identity")
		return
	}
	now := time.Now().Unix()
	_, err := s.queue.AckByID(req.MessageID, req.By, now)
	if err != nil {
		log.Printf("ack error: %v", err)
		writeErr(w, 500, "ack failed")
		return
	}
	if done, _ := s.verify.CheckCompletion(peer.ID, now); done {
		log.Printf("peer verified: %s", peer.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

type heartbeatReq struct {
	ID              string `json:"id"`
	ProtocolVersion int    `json:"protocol_version"`
	Capabilities    string `json:"capabilities"`
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	var req heartbeatReq
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.ID == "" {
		req.ID = peer.ID
	}
	if req.ID != peer.ID {
		writeErr(w, 403, "id must match your authenticated identity")
		return
	}
	pv := req.ProtocolVersion
	if pv == 0 {
		pv = peer.ProtocolVer
	}
	caps := req.Capabilities
	if caps == "" {
		caps = peer.Capabilities
	}
	if err := s.presence.Beat(peer.ID, pv, caps, time.Now().Unix()); err != nil {
		writeErr(w, 500, "heartbeat failed")
		return
	}
	p2, _ := s.st.GetPeer(peer.ID)
	if p2 != nil && !s.checkVersion(w, p2) {
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	views, err := s.presence.List(time.Now().Unix())
	if err != nil {
		writeErr(w, 500, "peers failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "peers": views})
}

func (s *Server) handleSmoke(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	seq, smokeID, err := s.queue.Smoke(peer.ID)
	if err != nil {
		log.Printf("smoke error: %v", err)
		writeErr(w, 500, "smoke failed")
		return
	}
	_ = s.verify.MarkVerifying(peer.ID, time.Now().Unix())
	writeJSON(w, 200, map[string]any{"ok": true, "seq": seq, "smoke_id": smokeID})
}

// ---- admin auth ----

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Password string `json:"password"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if len(s.adminHash) == 0 {
		writeErr(w, 500, "admin password not configured")
		return
	}
	if err := bcrypt.CompareHashAndPassword(s.adminHash, []byte(req.Password)); err != nil {
		writeErr(w, 401, "bad password")
		return
	}
	var b [32]byte
	_, _ = rand.Read(b[:])
	tok := base64.RawURLEncoding.EncodeToString(b[:])
	s.mu.Lock()
	s.sessions[tok] = time.Now().Unix() + 12*3600
	s.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: "agent_relay_admin", Value: tok, Path: "/",
		HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 12 * 3600,
		Secure: s.cfg.Public || s.cfg.BehindProxy,
	})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie("agent_relay_admin"); err == nil {
		s.mu.Lock()
		delete(s.sessions, c.Value)
		s.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: "agent_relay_admin", Value: "", Path: "/", MaxAge: -1})
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) isAdmin(r *http.Request) bool {
	c, err := r.Cookie("agent_relay_admin")
	if err != nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	exp, ok := s.sessions[c.Value]
	if !ok || time.Now().Unix() > exp {
		delete(s.sessions, c.Value)
		return false
	}
	return true
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.isAdmin(r) {
			writeErr(w, 401, "admin login required")
			return
		}
		next(w, r)
	}
}

// ---- admin APIs ----

func (s *Server) handleAdminCreateInvite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IntendedID string `json:"intended_id"`
		AgentType  string `json:"agent_type"`
	}
	if r.ContentLength != 0 {
		// Tolerate an empty body (curl with no -d, chunked encoding, etc).
		body := http.MaxBytesReader(w, r.Body, int64(s.cfg.MaxBodyBytes))
		dec := json.NewDecoder(body)
		if err := dec.Decode(&req); err != nil {
			msg := err.Error()
			if !strings.Contains(msg, "EOF") {
				writeErr(w, 400, "bad json body")
				return
			}
		}
	}
	if req.IntendedID != "" && !auth.ValidID(req.IntendedID) {
		writeErr(w, 400, "bad intended_id")
		return
	}
	code, inv, err := s.auth.CreateInvite(req.IntendedID, req.AgentType, "admin", time.Now().Unix())
	if err != nil {
		writeErr(w, 500, "invite failed")
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "code": code,
		"expires_at": inv.ExpiresAt, "intended_id": inv.IntendedID, "agent_type": inv.AgentType,
	})
}

func (s *Server) handleAdminListInvites(w http.ResponseWriter, r *http.Request) {
	invs, err := s.st.ListInvites()
	if err != nil {
		writeErr(w, 500, "list failed")
		return
	}
	out := make([]map[string]any, 0, len(invs))
	for _, inv := range invs {
		prefix := ""
		if len(inv.CodeHash) >= 12 {
			prefix = inv.CodeHash[:12]
		}
		out = append(out, map[string]any{
			"hash_prefix": prefix, "intended_id": inv.IntendedID,
			"agent_type": inv.AgentType, "created_at": inv.CreatedAt,
			"expires_at": inv.ExpiresAt, "used_by": inv.UsedBy, "used_at": inv.UsedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "invites": out})
}

func (s *Server) handleAdminDeleteInvite(w http.ResponseWriter, r *http.Request) {
	// DELETE /admin/invites/{code} takes the hash prefix; find full match.
	prefix := r.PathValue("code")
	invs, err := s.st.ListInvites()
	if err != nil {
		writeErr(w, 500, "list failed")
		return
	}
	deleted := 0
	now := time.Now().Unix()
	for _, inv := range invs {
		if strings.HasPrefix(inv.CodeHash, prefix) && inv.UsedAt == 0 && now < inv.ExpiresAt {
			_ = s.st.DeleteInvite(inv.CodeHash)
			deleted++
		}
	}
	writeJSON(w, 200, map[string]any{"ok": true, "deleted": deleted})
}

func (s *Server) handleAdminListPeers(w http.ResponseWriter, r *http.Request) {
	views, err := s.presence.List(time.Now().Unix())
	if err != nil {
		writeErr(w, 500, "peers failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "peers": views})
}

func (s *Server) handleAdminPatchPeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		DisplayName string `json:"display_name"`
		Status      string `json:"status"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.DisplayName != "" {
		if err := s.st.UpdatePeerMeta(id, req.DisplayName); err != nil {
			writeErr(w, 500, "update failed")
			return
		}
	}
	if req.Status != "" {
		switch req.Status {
		case "pending", "verifying", "active", "suspended", "failed":
		default:
			writeErr(w, 400, "bad status")
			return
		}
		if err := s.st.UpdatePeerStatus(id, req.Status); err != nil {
			writeErr(w, 500, "update failed")
			return
		}
		_ = s.st.AppendAudit("admin", "peer.status",
			fmt.Sprintf("peer=%s status=%s", id, req.Status), time.Now().Unix())
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminListTokens(w http.ResponseWriter, r *http.Request) {
	toks, err := s.st.ListTokens()
	if err != nil {
		writeErr(w, 500, "tokens failed")
		return
	}
	out := make([]map[string]any, 0, len(toks))
	for _, t := range toks {
		prefix := ""
		if len(t.TokenHash) >= 12 {
			prefix = t.TokenHash[:12]
		}
		out = append(out, map[string]any{
			"id": t.ID, "peer_id": t.PeerID, "hash_prefix": prefix,
			"label": t.Label, "created_at": t.CreatedAt,
			"last_used_at": t.LastUsedAt, "last_ip": t.LastIP, "revoked_at": t.RevokedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "tokens": out})
}

func (s *Server) handleAdminRevokeToken(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	now := time.Now().Unix()
	if err := s.st.RevokeToken(id, now); err != nil {
		writeErr(w, 500, "revoke failed")
		return
	}
	_ = s.st.AppendAudit("admin", "token.revoked", fmt.Sprintf("token=%d", id), now)
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminRotateToken(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PeerID string `json:"peer_id"`
		Label  string `json:"label"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if !auth.ValidID(req.PeerID) {
		writeErr(w, 400, "bad peer_id")
		return
	}
	peer, err := s.st.GetPeer(req.PeerID)
	if err != nil {
		writeErr(w, 500, "peer lookup failed")
		return
	}
	if peer == nil {
		writeErr(w, 404, "unknown peer")
		return
	}
	now := time.Now().Unix()
	plaintext, err := s.auth.Rotate(req.PeerID, req.Label, now)
	if err != nil {
		writeErr(w, 500, "rotate failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "token": plaintext})
}

func (s *Server) handleAdminMessages(w http.ResponseWriter, r *http.Request) {
	if q := r.URL.Query().Get("q"); q != "" {
		msgs, err := s.st.SearchMessages(q, 50)
		if err != nil {
			writeErr(w, 500, "search failed")
			return
		}
		items := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			items = append(items, map[string]any{
				"seq": m.Seq, "id": m.ID, "sender": m.Sender, "recipient": m.Recipient,
				"kind": m.Kind, "thread": m.RootID, "payload": m.Payload,
				"approval_state": m.ApprovalState, "created_at": m.CreatedAt,
			})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "items": items})
		return
	}
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		roots, err := s.st.ThreadRoots(100)
		if err != nil {
			writeErr(w, 500, "threads failed")
			return
		}
		out := make([]map[string]any, 0, len(roots))
		for _, root := range roots {
			msgs, _ := s.st.ThreadMessages(root, 1000)
			participants := map[string]bool{}
			held := 0
			for _, m := range msgs {
				participants[m.Sender] = true
				if m.Recipient != "*" {
					participants[m.Recipient] = true
				}
				if m.ApprovalState == "pending" {
					held++
				}
			}
			fused := len(msgs) >= s.cfg.FuseMaxMessages
			names := []string{}
			for p := range participants {
				names = append(names, p)
			}
			out = append(out, map[string]any{
				"root_id": root, "count": len(msgs),
				"participants": names, "held": held, "fused": fused,
			})
		}
		writeJSON(w, 200, map[string]any{"ok": true, "threads": out})
		return
	}
	msgs, err := s.st.ThreadMessages(thread, 1000)
	if err != nil {
		writeErr(w, 500, "thread failed")
		return
	}
	items := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		acked, _ := s.st.AckedCount(m.Seq)
		items = append(items, map[string]any{
			"seq": m.Seq, "id": m.ID, "sender": m.Sender, "recipient": m.Recipient,
			"kind": m.Kind, "in_reply_to": m.InReplyTo, "payload": m.Payload,
			"approval_state": m.ApprovalState, "acked_count": acked, "created_at": m.CreatedAt,
		})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "items": items})
}

// handleAdminExport dumps one thread as Markdown or JSONL (DESIGN §9.3).
// GET /admin/messages/export?thread=<root_id>&format=markdown|jsonl
func (s *Server) handleAdminExport(w http.ResponseWriter, r *http.Request) {
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		writeErr(w, 400, "thread required")
		return
	}
	format := r.URL.Query().Get("format")
	if format == "" {
		format = "markdown"
	}
	if format != "markdown" && format != "jsonl" {
		writeErr(w, 400, "format must be markdown|jsonl")
		return
	}
	msgs, err := s.st.ThreadMessages(thread, 1000)
	if err != nil {
		writeErr(w, 500, "thread failed")
		return
	}
	fname := "thread-" + sanitizeFilename(thread) + "." + map[string]string{"markdown": "md", "jsonl": "jsonl"}[format]
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/jsonl")
	} else {
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+fname+"\"")
	if format == "jsonl" {
		enc := json.NewEncoder(w)
		for _, m := range msgs {
			_ = enc.Encode(map[string]any{
				"seq": m.Seq, "id": m.ID, "sender": m.Sender, "recipient": m.Recipient,
				"kind": m.Kind, "in_reply_to": m.InReplyTo, "thread": m.RootID,
				"payload": m.Payload, "approval_state": m.ApprovalState, "created_at": m.CreatedAt,
			})
		}
		return
	}
	fmt.Fprintf(w, "# thread %s\n\n", thread)
	for _, m := range msgs {
		fmt.Fprintf(w, "## #%d %s → %s (%s, %s)\n\n%s\n\n",
			m.Seq, m.Sender, m.Recipient, m.Kind, m.ApprovalState, m.Payload)
	}
}

func sanitizeFilename(s string) string {
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "thread"
	}
	if len(out) > 80 {
		out = out[:80]
	}
	return out
}

func (s *Server) handleAdminApprove(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Seq     int64 `json:"seq"`
		Approve bool  `json:"approve"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if err := s.st.ApproveMessage(req.Seq, req.Approve); err != nil {
		writeErr(w, 500, "approve failed")
		return
	}
	action := "message.approved"
	if !req.Approve {
		action = "message.rejected"
	}
	_ = s.st.AppendAudit("admin", action, fmt.Sprintf("seq=%d", req.Seq), time.Now().Unix())
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminFuseReset(w http.ResponseWriter, r *http.Request) {
	var req struct {
		RootID string `json:"root_id"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if s.guard != nil {
		if err := s.guard.ResetFuse(req.RootID, time.Now().Unix()); err != nil {
			writeErr(w, 500, "reset failed")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true})
		return
	}
	maxSeq, err := s.st.MaxSeq()
	if err != nil {
		writeErr(w, 500, "reset failed")
		return
	}
	if err := s.st.SetFuseWatermark(req.RootID, maxSeq); err != nil {
		writeErr(w, 500, "reset failed")
		return
	}
	_ = s.st.AppendAudit("admin", "fuse.reset", "thread="+req.RootID, time.Now().Unix())
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminAudit(w http.ResponseWriter, r *http.Request) {
	actor := r.URL.Query().Get("actor")
	action := r.URL.Query().Get("action")
	since, _ := strconv.Atoi(r.URL.Query().Get("since"))
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	entries, err := s.st.ListAudit(actor, action, since, limit)
	if err != nil {
		writeErr(w, 500, "audit failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "entries": entries})
}

func (s *Server) handleAdminPrompts(w http.ResponseWriter, r *http.Request) {
	var req struct {
		AgentType string `json:"agent_type"`
		PeerID    string `json:"peer_id"`
		// Either supply an existing invite code or have one minted.
		InviteCode string `json:"invite_code"`
		// Reconfigure renders fresh instructions for an existing peer
		// (protocol upgrade): no invite is minted, token is reused.
		CreateInvite bool `json:"create_invite"`
		Reconfigure  bool `json:"reconfigure"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.AgentType == "" {
		req.AgentType = "generic"
	}
	switch req.AgentType {
	case "muse", "hermes", "claw", "generic":
	default:
		writeErr(w, 400, "unknown agent_type")
		return
	}
	if req.Reconfigure {
		if !auth.ValidID(req.PeerID) {
			writeErr(w, 400, "peer_id required for reconfigure")
			return
		}
		if _, err := s.st.GetPeer(req.PeerID); err != nil {
			writeErr(w, 500, "peer lookup failed")
			return
		}
		prompt, err := prompts.Render(req.AgentType, prompts.Data{
			ServerAddr: s.serverAddr, PeerID: req.PeerID,
			ProtocolVersion: s.cfg.Protocol, IsReconfigure: true,
		})
		if err != nil {
			writeErr(w, 500, "render failed")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "prompt": prompt})
		return
	}
	code := req.InviteCode
	if code == "" && req.CreateInvite {
		var err error
		code, _, err = s.auth.CreateInvite(req.PeerID, req.AgentType, "admin", time.Now().Unix())
		if err != nil {
			writeErr(w, 500, "invite failed")
			return
		}
	}
	if code == "" {
		writeErr(w, 400, "invite_code or create_invite required")
		return
	}
	peerID := req.PeerID
	if peerID == "" {
		peerID = "(invite-bound identity, or a name you choose matching ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$)"
	}
	prompt, err := prompts.Render(req.AgentType, prompts.Data{
		ServerAddr: s.serverAddr, InviteCode: code, PeerID: peerID,
		ProtocolVersion: s.cfg.Protocol,
	})
	if err != nil {
		writeErr(w, 500, "render failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "code": code, "prompt": prompt})
}

func (s *Server) handleAdminStats(w http.ResponseWriter, r *http.Request) {
	size, _ := s.st.DBSize()
	maxSeq, _ := s.st.MaxSeq()
	peers, _ := s.st.ListPeers()
	online := 0
	now := time.Now().Unix()
	for _, p := range peers {
		if now-p.LastSeen < int64(s.cfg.OnlineTimeoutSecs) && p.LastSeen != 0 {
			online++
		}
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "db_bytes": size, "max_seq": maxSeq,
		"peers_total": len(peers), "peers_online": online,
		"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
	})
}

// handleAdminConfig exposes the effective retention/guard policy (DESIGN §9.4).
// Read-only: changes go through config.toml + restart, so archive semantics
// stay predictable across retention passes.
func (s *Server) handleAdminConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"ok": true,
		"retention": map[string]any{
			"message_ttl_days":      s.cfg.MessageTTLDays,
			"peer_prune_after_days": s.cfg.PeerPruneAfterDays,
			"audit_retention_days":  s.cfg.AuditRetentionDays,
		},
		"guard": map[string]any{
			"fuse_max_messages": s.cfg.FuseMaxMessages,
			"fuse_max_age_secs": s.cfg.FuseMaxAgeSecs,
			"rate_per_minute":   s.cfg.RatePerMinute,
		},
		"presence": map[string]any{
			"online_timeout_secs": s.cfg.OnlineTimeoutSecs,
			"verify_timeout_secs": s.cfg.VerifyTimeoutSecs,
		},
	})
}
