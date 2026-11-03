package gateway

import (
	"net/http"
	"strconv"

	"github.com/AlixWang/agent-relay/internal/store"
)

// handleAdminTickets lists projected work items. Admin-only: this is a
// read model over messages the admin can already inspect.
func (s *Server) handleAdminTickets(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	p := parsePage(r)
	q := r.URL.Query()
	f := store.TicketFilter{
		Stage:    q.Get("stage"),
		State:    q.Get("state"),
		Assignee: q.Get("assignee"),
		Q:        q.Get("q"),
	}
	items, total, err := s.work.List(f, p.Size, p.offset())
	if err != nil {
		writeErr(w, 500, "list tickets: "+err.Error())
		return
	}
	body := withMeta(map[string]any{
		"ok":    true,
		"ready": s.work.Ready(),
		"items": items,
	}, p, total)
	writeJSON(w, 200, body)
}

// handleAdminAttention returns the "what needs a person now" queue plus
// per-stage counts. The work service owns both so this handler stays a thin
// HTTP shell.
func (s *Server) handleAdminAttention(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	p := parsePage(r)
	page, err := s.work.AttentionPage(p.Size)
	if err != nil {
		writeErr(w, 500, "attention: "+err.Error())
		return
	}
	body := withMeta(map[string]any{
		"ok":     true,
		"ready":  s.work.Ready(),
		"counts": page.Counts,
		"items":  page.Items,
	}, p, page.Total)
	writeJSON(w, 200, body)
}

// handleMyTickets is the agent-facing read of its own work items. It is a
// projection over messages the peer already received or sent; no admin data
// leaks because the filter is forced to the authenticated identity.
func (s *Server) handleMyTickets(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	p := parsePage(r)
	q := r.URL.Query()
	state := q.Get("state")
	if state == "" {
		state = "open"
	}
	items, total, err := s.work.List(store.TicketFilter{
		Stage: q.Get("stage"), State: state, Assignee: peer.ID, Q: q.Get("q"),
	}, p.Size, p.offset())
	if err != nil {
		writeErr(w, 500, "list tickets: "+err.Error())
		return
	}
	body := withMeta(map[string]any{"ok": true, "items": items}, p, total)
	writeJSON(w, 200, body)
}

// handleMyTicket returns one ticket owned by the authenticated peer.
func (s *Server) handleMyTicket(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad ticket id")
		return
	}
	t, err := s.work.Get(id)
	if err != nil {
		writeErr(w, 500, "ticket: "+err.Error())
		return
	}
	if t == nil {
		writeErr(w, 404, "ticket not found")
		return
	}
	if t.Assignee != peer.ID {
		writeErr(w, 403, "ticket is assigned to another peer")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ticket": t})
}

// handleAdminReassign is the operator's explicit reassignment command.
func (s *Server) handleAdminReassign(w http.ResponseWriter, r *http.Request) {
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad ticket id")
		return
	}
	var req struct {
		Assignee  string `json:"assignee"`
		LeaseSecs int64  `json:"lease_secs"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Assignee == "" {
		writeErr(w, 400, "assignee is required")
		return
	}
	if peer, _ := s.st.GetPeer(req.Assignee); peer == nil {
		writeErr(w, 404, "unknown assignee")
		return
	}
	t, err := s.work.Reassign(id, req.Assignee, req.LeaseSecs)
	if err != nil {
		writeErr(w, 500, "reassign: "+err.Error())
		return
	}
	if t == nil {
		writeErr(w, 404, "ticket not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ticket": t})
}

// handleMyTicketSubscription lets an assignee follow a related thread for
// context (mode=context) or keep the own thread subscribed. The projector's
// privacy predicate rejects peers who are not participants.
func (s *Server) handleMyTicketSubscription(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil {
		return
	}
	if s.work == nil {
		writeErr(w, http.StatusServiceUnavailable, "work items are not available")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, 400, "bad ticket id")
		return
	}
	var req struct {
		ThreadRoot string `json:"thread_root"`
		Mode       string `json:"mode"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.ThreadRoot == "" {
		writeErr(w, 400, "thread_root is required")
		return
	}
	t, err := s.work.Subscribe(id, peer.ID, req.ThreadRoot, req.Mode)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if t == nil {
		writeErr(w, 404, "ticket not found")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "ticket": t})
}
