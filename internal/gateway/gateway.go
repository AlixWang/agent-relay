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
	"github.com/AlixWang/agent-relay/internal/stream"
	"github.com/AlixWang/agent-relay/internal/verify"
	"github.com/AlixWang/agent-relay/internal/web"
	"golang.org/x/crypto/bcrypt"
)

// receiverRev is the receiver source revision (DESIGN §8.9), baked at release
// time: -ldflags "-X .../internal/gateway.receiverRev=<hash of cmd/relay-tail>".
// It changes only when the receiver code changes, which is what makes the
// client_update nudge meaningful. Empty in dev builds (never nudge).
var receiverRev string

// ReceiverRev returns the baked receiver revision ("" for dev builds).
func ReceiverRev() string { return receiverRev }

// Server bundles all dependencies for HTTP handlers.
type Server struct {
	cfg      *config.Config
	st       store.Store
	auth     *auth.Service
	guard    *guard.Guard
	queue    *queue.Service
	presence *presence.Service
	verify   *verify.Service
	stream   *stream.Hub

	adminHash []byte
	mu        sync.Mutex
	sessions  map[string]int64 // session token -> expiry unix
	updMgr    *updateManager   // web self-update jobs (lazy init)

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

// SetStream wires the SSE fan-out hub (DESIGN §4.4b). Kept separate from
// New so existing construction sites don't change signature. Nil hub
// disables push: Publish calls become no-ops.
func (s *Server) SetStream(h *stream.Hub) { s.stream = h }

// notifyStream wakes held SSE subscribers after a newly visible message.
// The DB stays the source of truth: subscribers re-query VisibleTo, so a
// missed or coalesced ping can never lose a message.
func (s *Server) notifyStream() {
	if s.stream != nil {
		s.stream.NotifyAll()
	}
}

func (s *Server) SetGuard(g *guard.Guard) { s.guard = g }

func (s *Server) SetAdminHash(hash []byte) { s.adminHash = hash }

// Handler builds the mux.
func (s *Server) Handler(web http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("GET /clients/relay-poll.sh", s.handlePollScript)
	mux.HandleFunc("GET /clients/relay-tail.sh", s.handleTailScript)
	mux.HandleFunc("GET /clients/relay-watch.sh", s.handleWatchScript)
	mux.HandleFunc("GET /clients/relay-watch-hermes.sh", s.handleWatchHermesScript)
	mux.HandleFunc("GET /clients/relay-tail-supervisor.py", s.handleTailSupervisorScript)
	// Go receiver prototype (DESIGN §8.9): redirect to the Release asset.
	mux.HandleFunc("GET /clients/relay-tail", s.handleTailBinary)
	mux.HandleFunc("POST /register", s.handleRegister)
	mux.HandleFunc("POST /messages", s.handleSend)
	mux.HandleFunc("GET /messages", s.handlePull)
	// SSE push (DESIGN §4.4b): same delivery view as /messages, held open.
	// Outbound-only — works behind caddy/tailnet, no inbound to assistants.
	mux.HandleFunc("GET /messages/stream", s.handleStream)
	mux.HandleFunc("POST /ack", s.handleAck)
	mux.HandleFunc("POST /heartbeat", s.handleHeartbeat)
	mux.HandleFunc("GET /peers", s.handlePeers)
	mux.HandleFunc("POST /verify/smoke", s.handleSmoke)
	// Prompt distribution (§8.6): peers pull worker-instruction updates
	// here when heartbeat signals prompt_update. Authed, per-identity.
	mux.HandleFunc("GET /prompts/current", s.handlePromptCurrent)
	// Conversation APIs (§v12): assistant-facing conversation management.
	mux.HandleFunc("POST /conversations", s.handleCreateConversation)
	mux.HandleFunc("GET /conversations", s.handleListConversations)
	mux.HandleFunc("GET /conversations/{id}/messages", s.handleGetConversationMessages)
	mux.HandleFunc("POST /conversations/{id}/leave", s.handleLeaveConversation)

	mux.HandleFunc("POST /admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /admin/logout", s.handleAdminLogout)
	mux.HandleFunc("POST /admin/invites", s.requireAdmin(s.handleAdminCreateInvite))
	mux.HandleFunc("GET /admin/invites", s.requireAdmin(s.handleAdminListInvites))
	mux.HandleFunc("DELETE /admin/invites/{code}", s.requireAdmin(s.handleAdminDeleteInvite))
	mux.HandleFunc("GET /admin/peers", s.requireAdmin(s.handleAdminListPeers))
	mux.HandleFunc("PATCH /admin/peers/{id}", s.requireAdmin(s.handleAdminPatchPeer))
	mux.HandleFunc("DELETE /admin/peers/{id}", s.requireAdmin(s.handleAdminDeletePeer))
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
	// Conversation admin APIs (§v12): admin conversation management.
	mux.HandleFunc("GET /admin/conversations", s.requireAdmin(s.handleAdminListConversations))
	mux.HandleFunc("POST /admin/conversations", s.requireAdmin(s.handleAdminCreateConversation))
	mux.HandleFunc("PATCH /admin/conversations/{id}/members", s.requireAdmin(s.handleAdminManageMembers))
	mux.HandleFunc("POST /admin/conversations/{id}/messages", s.requireAdmin(s.handleAdminSendMessage))
	mux.HandleFunc("GET /admin/conversations/{id}/messages", s.requireAdmin(s.handleAdminGetConversationMessages))
	// Web self-update (DESIGN §10.4): releases-only, verified, systemd-only.
	mux.HandleFunc("GET /admin/update/status", s.requireAdmin(s.handleAdminUpdateStatus))
	mux.HandleFunc("POST /admin/update/check", s.requireAdmin(s.handleAdminUpdateCheck))
	mux.HandleFunc("POST /admin/update/apply", s.requireAdmin(s.handleAdminUpdateApply))

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
	out := map[string]any{
		"ok": true, "version": 2,
		"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
	}
	if rev := ReceiverRev(); rev != "" {
		out["receiver_rev"] = rev
	}
	writeJSON(w, 200, out)
}

// handlePollScript serves the versioned polling script (no auth: static
// content). Assistants download it during onboarding — never hand-write
// protocol details.
func (s *Server) handlePollScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(web.PollScript()))
}

// handleTailScript serves the SSE tail daemon (no auth: static content).
// Assistants download it when choosing push over polling — never hand-write
// protocol details.
func (s *Server) handleTailScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(web.TailScript()))
}

// handleWatchScript serves the Muse thin-shell layer (no auth: static
// content). Assistants download it during Go-first onboarding instead of
// hunting the repo — never hand-write wake/dedup details.
func (s *Server) handleWatchScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(web.WatchScript()))
}

// handleWatchHermesScript serves the Hermes thin-shell layer (no auth:
// static content). Same shape as the Muse layer — watchdog, spool drain,
// event dedup — with a hermes-specific wake(): one-shot `hermes chat -q`
// worker under a single-flight lock. Served so a resident Hermes during
// onboarding never hand-writes the wake/lock details (clients/hermes/).
func (s *Server) handleWatchHermesScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(web.WatchHermesScript()))
}

// handleTailSupervisorScript serves the resident Hermes wake layer (no auth:
// static content). Hermes-only by design — the other assistant types wake
// through their own runtime hooks, and their onboarding prompts never point at
// this URL (prompts_test enforces that). Onboarding downloads it instead of
// hand-writing wake/lock/backoff details (clients/hermes/).
func (s *Server) handleTailSupervisorScript(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/x-python; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write([]byte(web.TailSupervisorScript()))
}

// handleTailBinary redirects to the Go receiver prototype for the requested
// arch (DESIGN §8.9). Binaries ride the GitHub Release (same source as the
// Web updater), not the embed — the server never proxies multi-MB blobs.
// Usage: GET /clients/relay-tail?arch=amd64 (default amd64).
func (s *Server) handleTailBinary(w http.ResponseWriter, r *http.Request) {
	arch := r.URL.Query().Get("arch")
	if arch == "" {
		arch = "amd64"
	}
	if arch != "amd64" && arch != "arm64" {
		writeErr(w, 400, "arch must be amd64 or arm64")
		return
	}
	tag := web.BinaryVersion()
	if tag == "" || tag == "dev" {
		writeErr(w, 404, "binary releases start after this build; use the shell scripts")
		return
	}
	target := "https://github.com/AlixWang/agent-relay/releases/download/" + tag +
		"/relay-tail-linux-" + arch
	http.Redirect(w, r, target, http.StatusFound)
}

type registerReq struct {
	Code            string `json:"code"`
	ID              string `json:"id"`
	AgentType       string `json:"agent_type"`
	ProtocolVersion int    `json:"protocol_version"`
	// Capabilities accepts either a JSON string ('{"shell":true}') or an
	// object ({"shell":true}). Humans hand-write curl; objects are the
	// natural shape and must not 400 (hermes heartbeat bug).
	Capabilities json.RawMessage `json:"capabilities"`
	// Profile is the assistant's self-intro (§8.7): capabilities + usual
	// tasks in one or two sentences, for routing unassigned work.
	Profile string `json:"profile"`
}

// capsOrDefault normalizes the dual-shape capabilities field for
// register/heartbeat. Unparseable input falls back to "{}" instead of
// 400: capabilities are advisory metadata, never worth rejecting a peer.
// (The strict-decode 400 on object-shaped capabilities was the hermes
// heartbeat bug: humans naturally write {"shell":true}, not a string.)
func capsOrDefault(raw json.RawMessage) string {
	s, err := capsString(raw)
	if err != nil {
		return "{}"
	}
	return s
}

// capsString normalizes the dual-shape capabilities field to a JSON
// object string for storage. Empty/missing → "{}". Invalid → error.
func capsString(raw json.RawMessage) (string, error) {
	if len(raw) == 0 {
		return "{}", nil
	}
	// Quoted string: unquote, must itself be valid JSON (object preferred
	// but any valid JSON is stored verbatim — List re-parses leniently).
	if raw[0] == '"' {
		var s string
		if err := json.Unmarshal(raw, &s); err != nil {
			return "", err
		}
		if s == "" {
			return "{}", nil
		}
		if !json.Valid([]byte(s)) {
			return "", fmt.Errorf("capabilities string is not JSON")
		}
		return s, nil
	}
	if !json.Valid(raw) {
		return "", fmt.Errorf("capabilities is not JSON")
	}
	return string(raw), nil
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
		req.ProtocolVersion, capsOrDefault(req.Capabilities), time.Now().Unix())
	if err != nil {
		if re, ok := err.(*auth.RegisterError); ok {
			writeErr(w, 400, re.Code+": "+re.Detail)
			return
		}
		log.Printf("register error: %v", err)
		writeErr(w, 500, "register failed")
		return
	}
	// Profile at register (§8.7): seed the intro immediately so routing
	// works before the first heartbeat.
	now := time.Now().Unix()
	if req.Profile != "" {
		_ = s.st.UpdatePeerProfile(peer.ID, req.Profile, now)
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
	// A newly visible message wakes held SSE subscribers (DESIGN §4.4b).
	// Held (202) messages are invisible until admin approve wakes them.
	s.notifyStream()
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
		// Direction/approver violations are identity misuse: 403 like the
		// from/for/by binding checks, not a 409 content refusal.
		if rej.Code == "permission_not_authorized" {
			writeJSON(w, 403, map[string]any{"ok": false, "error": rej.Error()})
			return
		}
		code := 400
		switch rej.Code {
		case "duplicate_id", "loop_fuse_tripped", "loop_guard",
			"permission_already_decided", "permission_expired",
			"bad_permission_ref", "permission_rate_limited", "progress_throttled":
			code = 409
		}
		writeJSON(w, code, map[string]any{"ok": false, "error": rej.Error()})
		return
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "duplicate_id"),
		strings.HasPrefix(msg, "loop_fuse_tripped"),
		strings.HasPrefix(msg, "loop_guard"),
		strings.HasPrefix(msg, "permission_already_decided"),
		strings.HasPrefix(msg, "permission_expired"),
		strings.HasPrefix(msg, "bad_permission_ref"),
		strings.HasPrefix(msg, "permission_rate_limited"),
		strings.HasPrefix(msg, "progress_throttled"):
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
			"status":         m.Status, "op": m.Op, "target": m.Target,
			"detail": m.Detail, "decision": m.Decision, "expires_at": m.ExpiresAt,
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
	// Capabilities accepts a JSON string or object (same dual shape as
	// register). Missing/empty keeps the stored value.
	Capabilities json.RawMessage `json:"capabilities"`
	// PromptVersion is the peer's confirmed worker-instruction revision
	// (§8.6). The response carries prompt_update=true when the server
	// runs a newer revision and the peer should pull /prompts/current.
	PromptVersion int `json:"prompt_version"`
	// Profile is the peer's self-reported intro (§8.7): capabilities +
	// usual tasks, distilled from its own memory. Empty keeps the stored
	// value; the server nudges a refresh when it goes stale.
	Profile string `json:"profile"`
	// ClientVersion is the receiver client build tag (§8.9), e.g. the
	// relay-tail binary release. Empty = not reporting (shell scripts
	// have no version).
	ClientVersion string `json:"client_version"`
	// ClientRev is the receiver source revision (§8.9) baked at build time
	// (-X main.receiverRev). This — not the release tag — is what the
	// update nudge compares: a release that only touches the server, docs
	// or prompt templates leaves it unchanged, so nobody gets nagged.
	ClientRev string `json:"client_rev"`
	// MemoryReconciled is the assistant's one-line summary of the relay
	// rules it purged or rewrote in its own memory while applying a
	// prompt_update (§8.6). MemoryVersion is the revision it reconciled
	// against; 0 falls back to prompt_version, then the server's current.
	MemoryReconciled string `json:"memory_reconciled"`
	MemoryVersion    int    `json:"memory_version"`
}

const maxMemoryNoteRunes = 500

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
	caps := capsOrDefault(req.Capabilities)
	if caps == "{}" {
		caps = peer.Capabilities
	}
	now := time.Now().Unix()
	if err := s.presence.Beat(peer.ID, pv, caps, req.PromptVersion, req.Profile, req.ClientVersion, now); err != nil {
		writeErr(w, 500, "heartbeat failed")
		return
	}
	_ = s.presence.BeatClientRev(peer.ID, req.ClientRev, now)
	if note := strings.TrimSpace(req.MemoryReconciled); note != "" {
		if r := []rune(note); len(r) > maxMemoryNoteRunes {
			note = string(r[:maxMemoryNoteRunes])
		}
		mv := req.MemoryVersion
		if mv <= 0 {
			mv = req.PromptVersion
		}
		if mv <= 0 {
			mv = prompts.PromptVersion
		}
		if err := s.presence.BeatMemory(peer.ID, note, mv, now); err == nil {
			_ = s.st.AppendAudit(peer.ID, "peer.memory_reconciled",
				fmt.Sprintf("version=%d note=%s", mv, note), now)
		}
	}
	p2, _ := s.st.GetPeer(peer.ID)
	if p2 != nil && !s.checkVersion(w, p2) {
		return
	}
	resp := map[string]any{"ok": true}
	if p2 != nil && p2.PromptVersion < prompts.PromptVersion {
		resp["prompt_update"] = true
		resp["prompt_version"] = prompts.PromptVersion
	}
	// Receiver client nudge (§8.9): same channel as prompt_update.
	//
	// The predicate is the receiver *source revision*, not the release tag:
	// a release that leaves cmd/relay-tail untouched produces a byte-different
	// binary (the tag is baked in) but identical behaviour, so tag comparison
	// nagged the whole fleet for nothing. Now:
	//   rev reported -> nudge only when it differs from ours
	//   no rev yet   -> legacy tag comparison (one-time transition: upgrading
	//                   to a rev-reporting binary ends the nagging for good)
	//   nothing      -> never nudge (shell scripts, dev builds)
	// Advisory only: never gates protocol access, the client just stages a
	// wake event and may re-check locally.
	if rev := ReceiverRev(); p2 != nil && rev != "" {
		resp["receiver_rev"] = rev
		switch {
		case p2.ClientRev != "":
			if p2.ClientRev != rev {
				resp["client_update"] = true
				resp["client_version"] = web.BinaryVersion()
			}
		case p2.ClientVersion != "":
			if latest := web.BinaryVersion(); latest != "" && latest != "dev" && p2.ClientVersion != latest {
				resp["client_update"] = true
				resp["client_version"] = latest
			}
		}
	}
	// Profile refresh nudge (§8.7): never-set or older than
	// profile_refresh_days. Same channel as prompt_update; the assistant
	// rewrites its intro from memory and confirms via the next heartbeat.
	if refr := int64(s.cfg.ProfileRefreshDays) * 86400; refr > 0 && p2 != nil &&
		(p2.Profile == "" || now-p2.ProfileUpdatedAt > refr) {
		resp["profile_refresh"] = true
	}
	writeJSON(w, 200, resp)
}

func (s *Server) handlePeers(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	views, err := s.presence.List(time.Now().Unix(), s.liveTransports())
	if err != nil {
		writeErr(w, 500, "peers failed")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "peers": views})
}

// handlePromptCurrent serves the worker-instruction delta (§8.6).
// The peer pulls this when heartbeat signals prompt_update, applies the
// instructions, and confirms by reporting the new prompt_version in its
// next heartbeat. The payload is the full current worker section for the
// peer's type (small, self-contained) — not a diff — so application is
// "replace your §6.x with this text", idempotent on re-pull.
func (s *Server) handlePromptCurrent(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if !s.checkVersion(w, peer) {
		return
	}
	prompt, err := prompts.Render(peer.AgentType, prompts.Data{
		ServerAddr: s.serverAddr, PeerID: peer.ID,
		ProtocolVersion: s.cfg.Protocol, IsReconfigure: true,
		NetworkNote: prompts.NetworkNoteFor(s.cfg.BehindProxy, s.cfg.PublicAddr, s.cfg.Public),
	})
	if err != nil {
		writeErr(w, 500, "render failed")
		return
	}
	// Upgrade guide (§8.6): only the entries newer than the peer's
	// reported version, so assistants upgrade incrementally. Unknown (0)
	// gets the full log — same cost as reading the full text once.
	writeJSON(w, 200, map[string]any{
		"ok": true, "prompt_version": prompts.PromptVersion,
		"agent_type": peer.AgentType, "prompt": prompt,
		"changes": prompts.ChangesSince(peer.PromptVersion),
	})
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
		// Username is accepted but not verified: single-admin deployment.
		// It exists so browsers offer to save the credential (the login
		// form sends username=admin with autocomplete=username).
		Username string `json:"username"`
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

func (s *Server) requirePeer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		peer, _ := s.authed(w, r)
		if peer == nil {
			return // authed already wrote error response
		}
		next(w, r)
	}
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
	views, err := s.presence.List(time.Now().Unix(), s.liveTransports())
	if err != nil {
		writeErr(w, 500, "peers failed")
		return
	}
	// Facets are computed over the unfiltered set so filter chips can show
	// "全部 N / 在线 M" regardless of the current selection.
	facets := map[string]int{"all": len(views)}
	types := map[string]int{}
	for _, v := range views {
		if v.Online {
			facets["online"]++
		}
		facets["status:"+v.Status]++
		types[v.AgentType]++
	}
	p := parsePage(r)
	filtered := filterPeers(views, r)
	writeJSON(w, 200, withMeta(map[string]any{
		"ok": true, "peers": slicePage(filtered, p), "facets": facets, "types": types,
	}, p, len(filtered)))
}

// liveTransports snapshots the SSE hub's live peers for the members
// console (§4.4b). Nil hub → nil → presence reports everyone as "poll".
func (s *Server) liveTransports() map[string]bool {
	if s.stream == nil {
		return nil
	}
	return s.stream.LivePeers()
}

func (s *Server) handleAdminDeletePeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	now := time.Now().Unix()
	n, err := s.st.DeletePeer(id, now)
	if err != nil {
		if strings.Contains(err.Error(), "unknown peer") {
			writeErr(w, 404, "unknown peer")
			return
		}
		writeErr(w, 500, "delete failed")
		return
	}
	_ = s.st.AppendAudit("admin", "peer.deleted",
		fmt.Sprintf("peer=%s tokens_revoked=%d", id, n), now)
	writeJSON(w, 200, map[string]any{"ok": true, "tokens_revoked": n})
}

func (s *Server) handleAdminPatchPeer(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		DisplayName string `json:"display_name"`
		Status      string `json:"status"`
		Profile     string `json:"profile"`
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
	// Admin can fix a peer's self-intro (§8.7) without waiting for the
	// next heartbeat refresh.
	if req.Profile != "" {
		if err := s.st.UpdatePeerProfile(id, req.Profile, time.Now().Unix()); err != nil {
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
	p := parsePage(r)
	toks = filterTokens(toks, r)
	if p.Paged {
		// Console view: newest first (legacy unpaged callers keep id ASC).
		for i, j := 0, len(toks)-1; i < j; i, j = i+1, j-1 {
			toks[i], toks[j] = toks[j], toks[i]
		}
	}
	total := len(toks)
	toks = slicePage(toks, p)
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
	writeJSON(w, 200, withMeta(map[string]any{"ok": true, "tokens": out}, p, total))
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
	p := parsePage(r)
	if q := r.URL.Query().Get("q"); q != "" {
		if !p.Paged {
			p.Size = 50 // legacy: top 50 hits
		}
		msgs, total, err := s.st.SearchMessagesPage(q, p.Size, p.offset())
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
				"status": m.Status, "op": m.Op, "target": m.Target,
				"detail": m.Detail, "decision": m.Decision, "expires_at": m.ExpiresAt,
			})
		}
		writeJSON(w, 200, withMeta(map[string]any{"ok": true, "items": items}, p, total))
		return
	}
	thread := r.URL.Query().Get("thread")
	if thread == "" {
		if !p.Paged {
			p.Size = 100 // legacy: latest 100 threads
		}
		f := store.ThreadFilter{Q: r.URL.Query().Get("filter")}
		switch r.URL.Query().Get("state") {
		case "held":
			f.Held = true
		case "fused":
			f.FusedMin = s.cfg.FuseMaxMessages
		}
		sums, total, err := s.st.ThreadSummaries(f, p.Size, p.offset())
		if err != nil {
			writeErr(w, 500, "threads failed")
			return
		}
		out := make([]map[string]any, 0, len(sums))
		for _, t := range sums {
			out = append(out, map[string]any{
				"root_id": t.RootID, "count": t.Count,
				"participants": t.Participants, "held": t.Held,
				"fused":       s.cfg.FuseMaxMessages > 0 && t.SinceReset >= s.cfg.FuseMaxMessages,
				"since_reset": t.SinceReset, "first_at": t.FirstAt, "last_at": t.LastAt,
				"last_seq": t.LastSeq, "last_sender": t.LastSender, "last_kind": t.LastKind,
				"last_preview": t.LastPreview,
			})
		}
		writeJSON(w, 200, withMeta(map[string]any{
			"ok": true, "threads": out, "fuse_max_messages": s.cfg.FuseMaxMessages,
		}, p, total))
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
			"status": m.Status, "op": m.Op, "target": m.Target,
			"detail": m.Detail, "decision": m.Decision, "expires_at": m.ExpiresAt,
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
				"status": m.Status, "op": m.Op, "target": m.Target,
				"detail": m.Detail, "decision": m.Decision, "expires_at": m.ExpiresAt,
			})
		}
		return
	}
	fmt.Fprintf(w, "# thread %s\n\n", thread)
	for _, m := range msgs {
		fmt.Fprintf(w, "## #%d %s → %s (%s, %s)\n\n%s\n\n",
			m.Seq, m.Sender, m.Recipient, m.Kind, m.ApprovalState, m.Payload)
		if m.Kind == "permission_request" {
			fmt.Fprintf(w, "- op=%s target=%s detail=%s expires_at=%d\n\n", m.Op, m.Target, m.Detail, m.ExpiresAt)
		}
		if m.Kind == "permission_decision" {
			fmt.Fprintf(w, "- decision=%s (re %s)\n\n", m.Decision, m.InReplyTo)
		}
		if m.Kind == "status" && m.Status != "" {
			fmt.Fprintf(w, "- status=%s (re %s)\n\n", m.Status, m.InReplyTo)
		}
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
	// An approved message becomes visible: wake SSE subscribers (§4.4b).
	// Rejected messages stay invisible; no wake-up needed.
	if req.Approve {
		s.notifyStream()
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
	if p := parsePage(r); p.Paged {
		// Console mode: newest first, action is a prefix, q searches detail.
		entries, total, err := s.st.ListAuditPage(store.AuditFilter{
			Actor: actor, Action: action, Q: r.URL.Query().Get("q"),
		}, p.Size, p.offset())
		if err != nil {
			writeErr(w, 500, "audit failed")
			return
		}
		body := map[string]any{"ok": true, "entries": entries}
		if r.URL.Query().Get("facets") == "1" {
			actors, actions, err := s.st.AuditFacets()
			if err == nil {
				body["actors"], body["actions"] = actors, actions
			}
		}
		writeJSON(w, 200, withMeta(body, p, total))
		return
	}
	// Legacy cursor mode (scripts): seq ASC after `since`.
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
			NetworkNote: prompts.NetworkNoteFor(s.cfg.BehindProxy, s.cfg.PublicAddr, s.cfg.Public),
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
		NetworkNote:     prompts.NetworkNoteFor(s.cfg.BehindProxy, s.cfg.PublicAddr, s.cfg.Public),
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
	body := map[string]any{
		"ok": true, "db_bytes": size, "max_seq": maxSeq,
		"peers_total": len(peers), "peers_online": online,
		"protocol": s.cfg.Protocol, "min_client": s.cfg.MinClient,
		"prompt_version": prompts.PromptVersion,
		"client_version": web.BinaryVersion(),
		"receiver_rev":   ReceiverRev(),
	}
	if c, err := s.st.AdminCounts(); err == nil {
		body["threads_total"] = c.Threads
		body["pending_approvals"] = c.PendingApprovals
		body["tokens_active"] = c.TokensActive
		body["audit_total"] = c.AuditTotal
	}
	writeJSON(w, 200, body)
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
