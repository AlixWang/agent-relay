package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
	"github.com/AlixWang/agent-relay/internal/stream"
)

// streamItem mirrors handlePull's per-message shape so SSE and poll clients
// share one parser. Keep both in sync.
func streamItemOf(m *store.Message) map[string]any {
	return map[string]any{
		"seq": m.Seq, "id": m.ID, "from": m.Sender, "to": m.Recipient,
		"kind": m.Kind, "in_reply_to": m.InReplyTo, "thread": m.RootID,
		"payload": m.Payload, "created_at": m.CreatedAt,
		"approval_state": m.ApprovalState,
		"status":         m.Status, "op": m.Op, "target": m.Target,
		"detail": m.Detail, "decision": m.Decision, "expires_at": m.ExpiresAt,
	}
}

// handleStream serves GET /messages/stream (DESIGN §4.4b): the same
// per-identity delivery view as GET /messages, held open as SSE.
// Protocol: backlog replay first (VisibleTo since), then incremental emits
// on every publish wake-up. Resume with ?since= or Last-Event-ID; the DB is
// the source of truth so reconnects never lose messages.
func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	peer, tok := s.authed(w, r)
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
	// Resume cursor: explicit ?since= wins, else Last-Event-ID, else 0.
	// Invalid values fall back to 0 (full replay, still bounded by limit).
	since, _ := strconv.ParseInt(r.URL.Query().Get("since"), 10, 64)
	if r.URL.Query().Get("since") == "" {
		if lid := r.Header.Get("Last-Event-ID"); lid != "" {
			since, _ = strconv.ParseInt(lid, 10, 64)
		}
	}
	if s.stream == nil {
		writeErr(w, 503, "stream unavailable")
		return
	}
	sub, err := s.stream.Subscribe(forID)
	if err != nil {
		if errors.Is(err, stream.ErrPeerLimit) || errors.Is(err, stream.ErrTotalLimit) {
			w.Header().Set("Retry-After", "10")
			writeJSON(w, 429, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		writeErr(w, 500, "stream failed")
		return
	}
	defer s.stream.Unsubscribe(sub)

	fl, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)

	keepalive := time.Duration(s.cfg.StreamKeepaliveSecs) * time.Second
	if keepalive <= 0 {
		keepalive = 20 * time.Second
	}
	ticker := time.NewTicker(keepalive)
	defer ticker.Stop()

	last := since
	emit := func() bool {
		// Revalidate identity on every wake-up: a revoked token or a
		// suspended peer must drop the stream, not keep receiving.
		if !s.streamAlive(tok.ID, forID) {
			return false
		}
		msgs, err := s.queue.Visible(forID, last, 200)
		if err != nil {
			return true // transient DB error: stay open, retry next ping
		}
		for _, m := range msgs {
			item := streamItemOf(m)
			body, _ := json.Marshal(item)
			fmt.Fprintf(w, "id: %d\nevent: message\nretry: 3000\ndata: %s\n\n", m.Seq, body)
			if m.Seq > last {
				last = m.Seq
			}
		}
		fl.Flush()
		return true
	}

	// Backlog first, then incremental.
	if !emit() {
		return
	}
	for {
		select {
		case <-r.Context().Done():
			return
		case <-sub.C:
			if !emit() {
				return
			}
		case <-ticker.C:
			// Comment ping: keeps proxies alive, wakes nothing.
			fmt.Fprintf(w, ": ping\n\n")
			fl.Flush()
		}
	}
}

// streamAlive re-checks that the token behind a held stream is still valid
// and the peer still exists and unsuspended. Mid-stream revocation or
// suspension drops the connection; the client reconnects and gets 401.
func (s *Server) streamAlive(tokID int64, peerID string) bool {
	toks, err := s.st.ListTokensByPeer(peerID)
	if err != nil {
		return true // fail open on DB error; next loop retries
	}
	found := false
	for _, t := range toks {
		if t.ID == tokID && t.RevokedAt == 0 {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	p, err := s.st.GetPeer(peerID)
	if err != nil || p == nil || p.Status == "suspended" {
		return false
	}
	return true
}
