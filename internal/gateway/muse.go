package gateway

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/muse"
	"github.com/AlixWang/agent-relay/internal/presence"
)

// adminMuseAccount is the list row: the safe account view plus the peer's live
// presence and whether a recorded wake brought the node back.
type adminMuseAccount struct {
	*muse.Account
	PeerOnline      bool   `json:"peer_online"`
	PeerLastSeen    int64  `json:"peer_last_seen"`
	PeerStatus      string `json:"peer_status"`
	WakeState       string `json:"wake_state"`
	WakeRecovered   bool   `json:"wake_recovered"`
	WakePending     bool   `json:"wake_pending"`
	WakeUnconfirmed bool   `json:"wake_unconfirmed"`
}

// museSessionOK is the single source of truth for the row's "正常" badge and
// the "正常" facet. A disabled account has its own badge/facet, and a wake
// problem is reported separately from session health.
func museSessionOK(a *muse.Account) bool {
	return a != nil && a.Enabled && a.AuthState == "ok" &&
		a.LastAttemptOK && a.LastError == ""
}

// museWakeState derives the operator-facing wake state. A recorded failed wake
// must stay unconfirmed even if the peer heartbeat later reappears: only a
// wake that actually verified RUNNING may claim "已上线".
func museWakeState(a *muse.Account, p *presence.PeerView, now int64) string {
	if a == nil {
		return "none"
	}
	if a.LastWakeError != "" || a.LastWakeState == "unconfirmed" ||
		(a.LastWakeState == "" && a.LastWakeAt > 0 && !a.LastWakeOK) {
		return "unconfirmed"
	}
	if a.LastWakeAt <= 0 {
		if a.LastWakeState == "running" {
			return "running"
		}
		return "none"
	}
	if a.LastWakeOK && p != nil && p.LastSeen >= a.LastWakeAt {
		return "recovered"
	}
	if now-a.LastWakeAt < 120 {
		return "pending"
	}
	return "running"
}

func (s *Server) museReady(w http.ResponseWriter) bool {
	if s.muse == nil {
		writeErr(w, http.StatusServiceUnavailable, "muse integration unavailable")
		return false
	}
	return true
}

func museError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, muse.ErrUnavailable):
		writeErr(w, http.StatusServiceUnavailable, "muse integration unavailable")
	case errors.Is(err, muse.ErrUnknownPeer):
		writeErr(w, http.StatusNotFound, "unknown peer")
	case errors.Is(err, muse.ErrNotFound):
		writeErr(w, http.StatusNotFound, "unknown muse account")
	case errors.Is(err, muse.ErrAlreadyAuthorized):
		writeErr(w, http.StatusConflict, "peer already has a muse account")
	case errors.Is(err, muse.ErrMissingCookies):
		// Authorize/Reauthorize wrap the sentinel with the names that were
		// actually absent; keep that detail so the dialog does not claim both
		// cookies are missing when only one is.
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, muse.ErrBadCookies):
		writeErr(w, http.StatusBadRequest, "invalid cookie string")
	case errors.Is(err, muse.ErrUnauthorized):
		writeErr(w, http.StatusBadRequest, "muse session unauthorized; re-check the cookie")
	case errors.Is(err, muse.ErrBusy):
		writeErr(w, http.StatusConflict, "muse keepalive already running")
	default:
		log.Printf("muse admin: %v", err)
		writeErr(w, http.StatusInternalServerError, "muse operation failed")
	}
}

func museWriteResponse(a *muse.Account) map[string]any {
	body := map[string]any{"ok": true, "account": a}
	if a != nil && a.LastError != "" {
		body["warning"] = a.LastError
	}
	return body
}

func (s *Server) auditMuse(action string, a *muse.Account) {
	if a == nil {
		return
	}
	detail := fmt.Sprintf("account=%s peer=%s label=%s auth=%s vm=%s",
		a.ID, a.PeerID, a.Label, a.AuthState, a.VMState)
	_ = s.st.AppendAudit("admin", action, detail, time.Now().Unix())
}

func (s *Server) auditMuseResult(action string, r *muse.Result) {
	if r == nil {
		return
	}
	detail := fmt.Sprintf("account=%s peer=%s ok=%v auth=%s vm=%s wake_requested=%v wake_ok=%v wake_state=%s error=%s wake_error=%s",
		r.ID, r.PeerID, r.OK, r.AuthState, r.VMState, r.WakeRequested, r.WakeOK, r.WakeState, r.Error, r.WakeError)
	_ = s.st.AppendAudit("admin", action, truncateRunes(detail, 500), time.Now().Unix())
}

func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// handleAdminMuseList returns every authorization with live peer presence,
// facets for the console chips and the background loop status.
func (s *Server) handleAdminMuseList(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	accounts, err := s.muse.List()
	if err != nil {
		museError(w, err)
		return
	}
	now := time.Now().Unix()
	views, _ := s.presence.List(now, s.liveTransports())
	byPeer := make(map[string]*presence.PeerView, len(views))
	for _, p := range views {
		byPeer[p.ID] = p
	}
	// Keep the two mutually exclusive health facets present even when zero;
	// the console and API callers read them unconditionally.
	facets := map[string]int{"total": len(accounts), "ok": 0, "wake_unconfirmed": 0}
	out := make([]*adminMuseAccount, 0, len(accounts))
	for _, a := range accounts {
		row := &adminMuseAccount{Account: a}
		var peer *presence.PeerView
		if p := byPeer[a.PeerID]; p != nil {
			peer = p
			row.PeerOnline = p.Online
			row.PeerLastSeen = p.LastSeen
			row.PeerStatus = p.Status
		}
		row.WakeState = museWakeState(a, peer, now)
		row.WakeRecovered = row.WakeState == "recovered"
		row.WakePending = row.WakeState == "pending"
		row.WakeUnconfirmed = row.WakeState == "unconfirmed"
		if museSessionOK(a) {
			if row.WakeUnconfirmed {
				facets["wake_unconfirmed"]++
			} else {
				facets["ok"]++
			}
		}
		switch a.AuthState {
		case "needs_reauth":
			facets["needs_reauth"]++
		case "ok":
		default:
			facets["unverified"]++
		}
		if !row.PeerOnline {
			facets["offline"]++
		}
		if !a.Enabled {
			facets["disabled"]++
		}
		if a.Enabled && a.AutoWake {
			facets["auto_wake"]++
		}
		out = append(out, row)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "accounts": out, "facets": facets, "status": s.muse.Status(),
	})
}

func (s *Server) handleAdminMuseCreate(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	var req struct {
		PeerID   string `json:"peer_id"`
		Label    string `json:"label"`
		Cookies  string `json:"cookies"`
		AutoWake *bool  `json:"auto_wake"`
		Enabled  *bool  `json:"enabled"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	autoWake, enabled := true, true
	if req.AutoWake != nil {
		autoWake = *req.AutoWake
	}
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	acct, err := s.muse.Authorize(r.Context(), strings.TrimSpace(req.PeerID), req.Label, req.Cookies, autoWake, enabled)
	if err != nil {
		museError(w, err)
		return
	}
	s.auditMuse("muse.authorized", acct)
	writeJSON(w, http.StatusOK, museWriteResponse(acct))
}

func (s *Server) handleAdminMusePatch(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	var req struct {
		Label    *string `json:"label"`
		PeerID   *string `json:"peer_id"`
		Cookies  string  `json:"cookies"`
		AutoWake *bool   `json:"auto_wake"`
		Enabled  *bool   `json:"enabled"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	cookies := strings.TrimSpace(req.Cookies)
	if cookies != "" && req.PeerID != nil {
		writeErr(w, http.StatusBadRequest, "cookies and peer_id cannot change together")
		return
	}
	if cookies == "" && req.Label == nil && req.PeerID == nil && req.AutoWake == nil && req.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "nothing to update")
		return
	}
	id := r.PathValue("id")
	var acct *muse.Account
	var err error
	if cookies != "" {
		acct, err = s.muse.Reauthorize(r.Context(), id, cookies)
		if err != nil {
			museError(w, err)
			return
		}
		s.auditMuse("muse.reauthorized", acct)
	}
	if req.Label != nil || req.PeerID != nil || req.AutoWake != nil || req.Enabled != nil {
		acct, err = s.muse.Update(id, req.Label, req.PeerID, req.AutoWake, req.Enabled)
		if err != nil {
			museError(w, err)
			return
		}
		s.auditMuse("muse.settings", acct)
	}
	writeJSON(w, http.StatusOK, museWriteResponse(acct))
}

func (s *Server) handleAdminMuseDelete(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	id := r.PathValue("id")
	acct, err := s.muse.Get(id)
	if err != nil {
		museError(w, err)
		return
	}
	if err := s.muse.Revoke(id); err != nil {
		museError(w, err)
		return
	}
	s.auditMuse("muse.revoked", acct)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleAdminMuseKeepalive(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	res, err := s.muse.Keepalive(r.Context(), r.PathValue("id"))
	if err != nil {
		museError(w, err)
		return
	}
	s.auditMuseResult("muse.keepalive", res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) handleAdminMuseWake(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	res, err := s.muse.Wake(r.Context(), r.PathValue("id"))
	if err != nil {
		museError(w, err)
		return
	}
	s.auditMuseResult("muse.wake", res)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "result": res})
}

func (s *Server) handleAdminMuseKeepaliveAll(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	sum, err := s.muse.KeepaliveAll(r.Context(), true)
	if err != nil {
		museError(w, err)
		return
	}
	_ = s.st.AppendAudit("admin", "muse.keepalive_all",
		fmt.Sprintf("total=%d ok=%d failed=%d skipped=%d needs_reauth=%d",
			sum.Total, sum.OK, sum.Failed, sum.Skipped, sum.NeedsReauth),
		time.Now().Unix())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "summary": sum})
}

func (s *Server) handleAdminMuseStatus(w http.ResponseWriter, r *http.Request) {
	if !s.museReady(w) {
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "status": s.muse.Status()})
}

// attachMuse decorates the admin member list with the Muse authorization
// summary. The agent-facing GET /peers never calls this, so assistants cannot
// see operator-only account state.
func (s *Server) attachMuse(views []*presence.PeerView) {
	if s.muse == nil {
		return
	}
	accounts, err := s.muse.List()
	if err != nil {
		log.Printf("muse: list for peers: %v", err)
		return
	}
	byPeer := make(map[string]*muse.Account, len(accounts))
	for _, a := range accounts {
		byPeer[a.PeerID] = a
	}
	for _, v := range views {
		a := byPeer[v.ID]
		if a == nil {
			continue
		}
		v.Muse = &presence.PeerMuseView{
			AccountID: a.ID, AuthState: a.AuthState, VMState: a.VMState, VMStateAt: a.VMStateAt,
			ExpiresAt: a.ExpiresAt, LastKeepaliveAt: a.LastKeepaliveAt,
			LastWakeAt: a.LastWakeAt, LastAttemptOK: a.LastAttemptOK,
			LastError: a.LastError, Enabled: a.Enabled, AutoWake: a.AutoWake,
		}
	}
}
