package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/presence"
	"github.com/AlixWang/agent-relay/internal/queue"
	"github.com/AlixWang/agent-relay/internal/resources"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Resource hub (DESIGN §4.12, ADR 0004/0005/0006). Agent read routes are
// grant-scoped; agent submission routes only stage writes for operator review.
// Admin routes live under /admin/resources* and never return secret plaintext.

// SetResources wires the resource hub. Nil keeps every hub route at 503 and
// heartbeat silent about resources.
func (s *Server) SetResources(r *resources.Service) { s.resources = r }

func (s *Server) resourcesReady(w http.ResponseWriter) bool {
	if s.resources == nil {
		writeErr(w, http.StatusServiceUnavailable, "resource hub unavailable")
		return false
	}
	return true
}

func resourceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, resources.ErrNotFound):
		writeErr(w, http.StatusNotFound, "resource not found")
	case errors.Is(err, resources.ErrUnknownPeer):
		writeErr(w, http.StatusNotFound, "unknown peer")
	case errors.Is(err, resources.ErrDisabled):
		writeErr(w, http.StatusForbidden, "resource self-registration is disabled")
	case errors.Is(err, resources.ErrNameTaken):
		writeErr(w, http.StatusConflict, "resource name already exists")
	case errors.Is(err, resources.ErrQuota):
		writeErr(w, http.StatusConflict, "resource quota exceeded")
	case errors.Is(err, resources.ErrRateLimited):
		w.Header().Set("Retry-After", "3600")
		writeErr(w, http.StatusTooManyRequests, "resource submission rate limited")
	case errors.Is(err, resources.ErrSubmissionNotPending):
		writeErr(w, http.StatusConflict, "submission is not pending")
	case errors.Is(err, resources.ErrTakedownExists):
		writeErr(w, http.StatusConflict, "a takedown request is already pending")
	case errors.Is(err, resources.ErrMissingSecrets):
		var m *resources.MissingSecretsError
		missing := []string{}
		if errors.As(err, &m) {
			missing = m.Names
		}
		writeJSON(w, http.StatusConflict, map[string]any{
			"ok": false, "error": "mcp config references secrets you are not granted",
			"missing_secrets": missing,
		})
	case errors.Is(err, resources.ErrUnsupportedKind):
		writeErr(w, http.StatusBadRequest, err.Error())
	case errors.Is(err, resources.ErrInvalid):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		log.Printf("resources: %v", err)
		writeErr(w, http.StatusInternalServerError, "resource operation failed")
	}
}

// noStore marks a response that carries secret plaintext.
func noStore(w http.ResponseWriter) {
	w.Header().Set("Cache-Control", "no-store, max-age=0")
	w.Header().Set("Pragma", "no-cache")
}

// resourceHeartbeat fills the hub part of a heartbeat response (ADR 0004).
// Called after the receipt in the request has been stored.
func (s *Server) resourceHeartbeat(p *store.Peer, resp map[string]any) {
	if s.resources == nil || p == nil {
		return
	}
	m, err := s.resources.Manifest(p.ID)
	if err != nil {
		log.Printf("resources: manifest for %s: %v", p.ID, err)
		return
	}
	if resources.NeedsUpdate(p.ResourcesRev, m.ManifestRev) {
		resp["resources_update"] = true
		rev := m.ManifestRev
		if rev == "" {
			rev = resources.NoneMarker
		}
		resp["resources_rev"] = rev
	}
}

// attachResourceStatus marks peers whose receipt lags their manifest
// (operator-only, GET /admin/peers).
func (s *Server) attachResourceStatus(views []*presence.PeerView) {
	if s.resources == nil {
		return
	}
	for _, v := range views {
		if m, err := s.resources.Manifest(v.ID); err == nil {
			v.ResourcesPending = resources.NeedsUpdate(v.ResourcesRev, m.ManifestRev)
		}
	}
}

// ---- agent routes ----

func (s *Server) handleResourcesCurrent(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	m, err := s.resources.Manifest(peer.ID)
	if err != nil {
		resourceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "manifest_rev": m.ManifestRev, "resources": m.Resources})
}

func (s *Server) handleResourceSecret(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	name := r.PathValue("name")
	v, err := s.resources.RevealSecret(peer.ID, name)
	if err != nil {
		resourceError(w, err)
		return
	}
	_ = s.st.AppendAudit(peer.ID, "secret.fetch",
		fmt.Sprintf("secret=%s ip=%s", name, s.clientIP(r)), time.Now().Unix())
	noStore(w)
	writeJSON(w, 200, map[string]any{"ok": true, "name": name, "value": v})
}

// handleResourceSkill serves a granted skill's tar.gz (ADR 0005). No secret
// material is involved, so no audit row; still no-store, since the archive
// is per-peer and changes over time.
func (s *Server) handleResourceSkill(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	name := r.PathValue("name")
	res, err := s.resources.SkillArchive(peer.ID, name)
	if err != nil {
		resourceError(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar.gz"))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(res.Content)
}

// handleAdminResourceSkillUpload takes a multipart skill archive: the file
// field is "archive", the optional "description" field overrides the
// frontmatter description. The resource name comes from SKILL.md.
func (s *Server) handleAdminResourceSkillUpload(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	limit := int64(s.cfg.MaxBodyBytes)
	if limit < 2<<20 {
		limit = 2 << 20
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(limit); err != nil {
		writeErr(w, 400, "expected multipart form with an 'archive' file")
		return
	}
	f, _, err := r.FormFile("archive")
	if err != nil {
		writeErr(w, 400, "missing 'archive' file field")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, 4<<20))
	if err != nil {
		writeErr(w, 400, "could not read archive")
		return
	}
	v, err := s.resources.PutSkill(raw, r.FormValue("description"))
	if err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.put",
		fmt.Sprintf("kind=skill name=%s revision=%s bytes=%d", v.Name, v.RevisionID, len(raw)))
	writeJSON(w, 200, map[string]any{"ok": true, "resource": v})
}

// handleResourceMCP returns the MCP config with ${secret:} resolved. It
// reveals secrets, so it follows the secret-fetch rules (ADR 0005).
func (s *Server) handleResourceMCP(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	name := r.PathValue("name")
	cfg, revealed, err := s.resources.ResolveMCP(peer.ID, name)
	if err != nil {
		resourceError(w, err)
		return
	}
	now, ip := time.Now().Unix(), s.clientIP(r)
	for _, sec := range revealed {
		_ = s.st.AppendAudit(peer.ID, "secret.fetch",
			fmt.Sprintf("secret=%s via=mcp:%s ip=%s", sec, name, ip), now)
	}
	noStore(w)
	writeJSON(w, 200, map[string]any{"ok": true, "name": name, "config": cfg})
}

// ---- admin routes ----

func (s *Server) auditResource(action, detail string) {
	_ = s.st.AppendAudit("admin", action, detail, time.Now().Unix())
}

func (s *Server) handleAdminResourcesList(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	views, err := s.resources.List(r.URL.Query().Get("kind"))
	if err != nil {
		resourceError(w, err)
		return
	}
	if owner := strings.TrimSpace(r.URL.Query().Get("owner")); owner != "" {
		filtered := views[:0]
		for _, v := range views {
			if v.OwnerPeer == owner || (owner == "operator" && v.OwnerPeer == "") {
				filtered = append(filtered, v)
			}
		}
		views = filtered
	}
	writeJSON(w, 200, map[string]any{"ok": true, "resources": views})
}

func (s *Server) handleAdminResourceGet(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	v, err := s.resources.Get(kind, name)
	if err != nil {
		resourceError(w, err)
		return
	}
	revs, err := s.resources.Revisions(kind, name)
	if err != nil {
		resourceError(w, err)
		return
	}
	type revView struct {
		ID          string `json:"id"`
		Description string `json:"description"`
		CreatedAt   int64  `json:"created_at"`
		CreatedBy   string `json:"created_by"`
		Current     bool   `json:"current"`
	}
	hist := make([]revView, 0, len(revs))
	for _, rv := range revs {
		hist = append(hist, revView{rv.ID, rv.Description, rv.CreatedAt, rv.CreatedBy, rv.ID == v.RevisionID})
	}
	writeJSON(w, 200, map[string]any{"ok": true, "resource": v, "revisions": hist})
}

// handleAdminResourcePut creates or overwrites. For secrets, an omitted or
// empty value keeps the stored value (description-only edit).
func (s *Server) handleAdminResourcePut(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	// value: secret plaintext (write-only; "" keeps the stored value).
	// content: mcp → neutral config object; prompt → text.
	var req struct {
		Description string          `json:"description"`
		Value       string          `json:"value"`
		Content     json.RawMessage `json:"content"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	var (
		v   *resources.View
		err error
	)
	switch kind {
	case resources.KindSecret:
		v, err = s.resources.PutSecret(name, req.Description, req.Value)
	case resources.KindMCP:
		v, err = s.resources.PutMCP(name, req.Description, req.Content)
	case resources.KindPrompt:
		var text string
		if json.Unmarshal(req.Content, &text) != nil {
			err = fmt.Errorf("%w: prompt content must be a string", resources.ErrInvalid)
		} else {
			v, err = s.resources.PutPrompt(name, req.Description, text)
		}
	case resources.KindSkill:
		// The skill's name comes from its SKILL.md frontmatter, not the URL.
		err = fmt.Errorf("%w: upload skills as multipart to /admin/resources/skill", resources.ErrInvalid)
	default:
		err = resources.CheckKind(kind)
		if err == nil {
			err = fmt.Errorf("%w: %s", resources.ErrUnsupportedKind, kind)
		}
	}
	if err != nil {
		resourceError(w, err)
		return
	}
	detail := fmt.Sprintf("kind=%s name=%s revision=%s", kind, name, v.RevisionID)
	if kind == resources.KindSecret && req.Value != "" {
		detail += " value=rotated"
	}
	s.auditResource("resource.put", detail)
	writeJSON(w, 200, map[string]any{"ok": true, "resource": v})
}

func (s *Server) handleAdminResourceDelete(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	if err := s.resources.Delete(kind, name); err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.deleted", fmt.Sprintf("kind=%s name=%s", kind, name))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminResourceGrant(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	kind, name := r.PathValue("kind"), r.PathValue("name")
	var req struct {
		PeerID string `json:"peer_id"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	peer := strings.TrimSpace(req.PeerID)
	if peer == "" {
		writeErr(w, 400, "peer_id required ('*' = every assistant)")
		return
	}
	if err := s.resources.Grant(kind, name, peer); err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.granted", fmt.Sprintf("kind=%s name=%s peer=%s", kind, name, peer))
	out := map[string]any{"ok": true}
	if kind == resources.KindMCP {
		// Granted anyway; the console tells the operator what else to grant.
		if missing, err := s.resources.MissingForGrant(name, peer); err == nil && len(missing) > 0 {
			out["missing_secrets"] = missing
		}
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleAdminResourceRevoke(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	kind, name, peer := r.PathValue("kind"), r.PathValue("name"), r.PathValue("peer")
	if err := s.resources.Revoke(kind, name, peer); err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.revoked", fmt.Sprintf("kind=%s name=%s peer=%s", kind, name, peer))
	out := map[string]any{"ok": true}
	if kind == resources.KindSecret {
		// Revocation cannot reach copies already on the assistant's host.
		out["warning"] = "the assistant may still hold a local copy; rotate the key upstream"
	}
	writeJSON(w, 200, out)
}

// ---- self-service registration (ADR 0006) ----

const maxResourceSkillUpload = 4 << 20

// resourceSubmitAllowed enforces the global and per-peer switches. Only
// active peers may contribute resources; suspended/pending peers keep all
// existing read access.
func (s *Server) resourceSubmitAllowed(w http.ResponseWriter, p *store.Peer) bool {
	if !s.resourcesReady(w) {
		return false
	}
	if !s.cfg.ResourceSelfRegistration {
		writeErr(w, http.StatusForbidden, "resource self-registration is disabled")
		return false
	}
	if p.Status != "active" {
		writeErr(w, http.StatusForbidden, "only active assistants may submit resources")
		return false
	}
	if !p.ResourceSubmitEnabled {
		writeErr(w, http.StatusForbidden, "resource submission is disabled for this assistant")
		return false
	}
	return true
}

// notifyResourceSubmission tells the operator about new resource work, at
// most once per peer per configured window. The exact pending count remains
// visible in the console badge.
func (s *Server) notifyResourceSubmission(peerID string) {
	if s.resources == nil {
		return
	}
	now := time.Now().Unix()
	window := int64(s.cfg.ResourceNotifyBatchSecs)
	s.resourceNotifyMu.Lock()
	last := s.resourceNotifyAt[peerID]
	if window > 0 && last > 0 && now-last < window {
		s.resourceNotifyMu.Unlock()
		return
	}
	s.resourceNotifyAt[peerID] = now
	s.resourceNotifyMu.Unlock()

	pending, _, err := s.resources.PendingCounts()
	if err != nil {
		pending = 0
	}
	text := fmt.Sprintf("助手 %s 提交了资源，当前资源中心有 %d 个待审核提交。到「资源中心 → 待审核」处理。", peerID, pending)
	if _, _, _, err := s.queue.Send(&queue.SendRequest{
		ID: auth.NewMessageID("sys"), To: auth.OperatorID, From: auth.SystemID,
		Kind: "system", Payload: text,
	}, now); err == nil {
		s.notifyStream()
	}
}

func (s *Server) notifyResourceTakedown(peerID string) {
	if s.resources == nil {
		return
	}
	now := time.Now().Unix()
	text := fmt.Sprintf("助手 %s 提交了资源下架申请，到「资源中心 → 下架申请」处理。", peerID)
	if _, _, _, err := s.queue.Send(&queue.SendRequest{
		ID: auth.NewMessageID("sys"), To: auth.OperatorID, From: auth.SystemID,
		Kind: "system", Payload: text,
	}, now); err == nil {
		s.notifyStream()
	}
}

func (s *Server) handleResourceSubmit(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourceSubmitAllowed(w, peer) {
		return
	}
	var req struct {
		Kind        string          `json:"kind"`
		Name        string          `json:"name"`
		Description string          `json:"description"`
		Value       string          `json:"value"`
		Content     json.RawMessage `json:"content"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	var (
		view      *resources.SubmissionView
		unchanged bool
		err       error
	)
	switch req.Kind {
	case resources.KindSecret:
		view, unchanged, err = s.resources.SubmitSecret(peer.ID, req.Name, req.Description, req.Value)
	case resources.KindMCP:
		view, unchanged, err = s.resources.SubmitMCP(peer.ID, req.Name, req.Description, req.Content)
	case resources.KindPrompt:
		var text string
		if json.Unmarshal(req.Content, &text) != nil {
			err = fmt.Errorf("%w: prompt content must be a string", resources.ErrInvalid)
		} else {
			view, unchanged, err = s.resources.SubmitPrompt(peer.ID, req.Name, req.Description, text)
		}
	case resources.KindSkill:
		err = fmt.Errorf("%w: upload skills as multipart to /resources/submissions/skill", resources.ErrInvalid)
	default:
		err = resources.CheckKind(req.Kind)
	}
	if err != nil {
		resourceError(w, err)
		return
	}
	if view.ID != "" {
		s.auditResource("resource.submitted",
			fmt.Sprintf("peer=%s kind=%s name=%s submission=%s hash=%s", peer.ID, view.Kind, view.Name, view.ID, shortHash(view.Hash)))
		s.notifyResourceSubmission(peer.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "submission": view, "unchanged": unchanged})
}

func shortHash(h string) string {
	if len(h) > 16 {
		return h[:16]
	}
	return h
}

func (s *Server) handleResourceSubmitSkill(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourceSubmitAllowed(w, peer) {
		return
	}
	limit := int64(maxResourceSkillUpload)
	if s.cfg.MaxBodyBytes > 0 && int64(s.cfg.MaxBodyBytes) > limit {
		limit = int64(s.cfg.MaxBodyBytes)
	}
	r.Body = http.MaxBytesReader(w, r.Body, limit)
	if err := r.ParseMultipartForm(maxResourceSkillUpload); err != nil {
		writeErr(w, http.StatusBadRequest, "expected multipart form with an 'archive' file")
		return
	}
	f, _, err := r.FormFile("archive")
	if err != nil {
		writeErr(w, http.StatusBadRequest, "missing 'archive' file field")
		return
	}
	defer f.Close()
	raw, err := io.ReadAll(io.LimitReader(f, maxResourceSkillUpload+1))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "could not read archive")
		return
	}
	if len(raw) > maxResourceSkillUpload {
		writeErr(w, http.StatusRequestEntityTooLarge, "skill archive too large")
		return
	}
	view, unchanged, err := s.resources.SubmitSkill(peer.ID, raw, r.FormValue("description"))
	if err != nil {
		resourceError(w, err)
		return
	}
	if view.ID != "" {
		s.auditResource("resource.submitted",
			fmt.Sprintf("peer=%s kind=skill name=%s submission=%s bytes=%d", peer.ID, view.Name, view.ID, len(raw)))
		s.notifyResourceSubmission(peer.ID)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "submission": view, "unchanged": unchanged})
}

func (s *Server) handleResourceSubmissionsMine(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	p := parsePage(r)
	limit, offset := 0, 0
	if p.Paged {
		limit, offset = p.Size, p.offset()
	}
	status := strings.TrimSpace(r.URL.Query().Get("status"))
	items, total, err := s.resources.ListMine(peer.ID, status, limit, offset)
	if err != nil {
		resourceError(w, err)
		return
	}
	writeJSON(w, 200, withMeta(map[string]any{"ok": true, "submissions": items}, p, total))
}

func (s *Server) handleResourceSubmissionWithdraw(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	id := r.PathValue("id")
	if err := s.resources.Withdraw(peer.ID, id); err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.withdrawn", fmt.Sprintf("peer=%s submission=%s", peer.ID, id))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleResourceTakedownRequest(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourceSubmitAllowed(w, peer) {
		return
	}
	var req struct {
		Kind   string `json:"kind"`
		Name   string `json:"name"`
		Reason string `json:"reason"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	view, err := s.resources.RequestTakedown(peer.ID, req.Kind, req.Name, req.Reason)
	if err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.takedown_requested", fmt.Sprintf("peer=%s kind=%s name=%s takedown=%s", peer.ID, req.Kind, req.Name, view.ID))
	s.notifyResourceTakedown(peer.ID)
	writeJSON(w, 200, map[string]any{"ok": true, "takedown": view})
}

func (s *Server) handleResourceTakedownsMine(w http.ResponseWriter, r *http.Request) {
	peer, _ := s.authed(w, r)
	if peer == nil || !s.checkVersion(w, peer) || !s.resourcesReady(w) {
		return
	}
	p := parsePage(r)
	limit, offset := 0, 0
	if p.Paged {
		limit, offset = p.Size, p.offset()
	}
	items, total, err := s.resources.ListMineTakedowns(peer.ID, limit, offset)
	if err != nil {
		resourceError(w, err)
		return
	}
	writeJSON(w, 200, withMeta(map[string]any{"ok": true, "takedowns": items}, p, total))
}

// ---- admin review routes ----

func (s *Server) handleAdminResourceSubmissions(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	p := parsePage(r)
	q := r.URL.Query()
	filter := store.ResourceSubmissionFilter{
		Status: strings.TrimSpace(q.Get("status")),
		Kind:   strings.TrimSpace(q.Get("kind")),
		Peer:   strings.TrimSpace(q.Get("peer")),
		Q:      strings.TrimSpace(q.Get("q")),
	}
	if p.Paged {
		filter.Limit, filter.Offset = p.Size, p.offset()
	}
	items, total, err := s.resources.AdminList(filter)
	if err != nil {
		resourceError(w, err)
		return
	}
	subsPending, tdsPending, _ := s.resources.PendingCounts()
	writeJSON(w, 200, withMeta(map[string]any{
		"ok": true, "submissions": items,
		"pending_submissions": subsPending, "pending_takedowns": tdsPending,
	}, p, total))
}

func (s *Server) handleAdminResourceSubmissionGet(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	detail, err := s.resources.GetSubmissionDetail(r.PathValue("id"))
	if err != nil {
		resourceError(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "submission": detail})
}

func (s *Server) handleAdminResourceSubmissionArchive(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	raw, err := s.resources.SubmissionSkillArchive(r.PathValue("id"))
	if err != nil {
		resourceError(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", `attachment; filename="submission.tar.gz"`)
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (s *Server) handleAdminResourceSkillArchive(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	name := r.PathValue("name")
	raw, err := s.resources.LiveSkillArchive(name)
	if err != nil {
		resourceError(w, err)
		return
	}
	noStore(w)
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name+".tar.gz"))
	w.WriteHeader(200)
	_, _ = w.Write(raw)
}

func (s *Server) handleAdminResourceSubmissionApprove(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	var req struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	v, err := s.resources.Approve(id, "admin", req.Name, req.Description)
	if err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.approved", fmt.Sprintf("submission=%s kind=%s name=%s revision=%s", id, v.Kind, v.Name, v.RevisionID))
	writeJSON(w, 200, map[string]any{"ok": true, "resource": v})
}

func (s *Server) handleAdminResourceSubmissionReject(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.resources.Reject(id, "admin", req.Note); err != nil {
		resourceError(w, err)
		return
	}
	s.auditResource("resource.rejected", fmt.Sprintf("submission=%s", id))
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAdminResourceSubmissionsBulk(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	var req struct {
		IDs    []string `json:"ids"`
		Action string   `json:"action"`
		Note   string   `json:"note"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	if req.Action != "approve" && req.Action != "reject" {
		writeErr(w, 400, "action must be approve or reject")
		return
	}
	type result struct {
		ID    string `json:"id"`
		OK    bool   `json:"ok"`
		Error string `json:"error,omitempty"`
	}
	out := make([]result, 0, len(req.IDs))
	for _, id := range req.IDs {
		res := result{ID: id}
		detail, err := s.resources.GetSubmissionDetail(id)
		if err != nil {
			res.Error = "not found"
			out = append(out, res)
			continue
		}
		if detail.Kind == resources.KindSkill {
			res.Error = "skills must be reviewed individually"
			out = append(out, res)
			continue
		}
		if req.Action == "approve" {
			v, err := s.resources.Approve(id, "admin", "", "")
			if err != nil {
				res.Error = err.Error()
				out = append(out, res)
				continue
			}
			s.auditResource("resource.approved", fmt.Sprintf("submission=%s kind=%s name=%s revision=%s", id, v.Kind, v.Name, v.RevisionID))
		} else {
			if err := s.resources.Reject(id, "admin", req.Note); err != nil {
				res.Error = err.Error()
				out = append(out, res)
				continue
			}
			s.auditResource("resource.rejected", fmt.Sprintf("submission=%s bulk=1", id))
		}
		res.OK = true
		out = append(out, res)
	}
	writeJSON(w, 200, map[string]any{"ok": true, "results": out})
}

func (s *Server) handleAdminResourceTakedowns(w http.ResponseWriter, r *http.Request) {
	if !s.resourcesReady(w) {
		return
	}
	p := parsePage(r)
	limit, offset := 0, 0
	if p.Paged {
		limit, offset = p.Size, p.offset()
	}
	items, total, err := s.resources.AdminListTakedowns(strings.TrimSpace(r.URL.Query().Get("status")), limit, offset)
	if err != nil {
		resourceError(w, err)
		return
	}
	writeJSON(w, 200, withMeta(map[string]any{"ok": true, "takedowns": items}, p, total))
}

func (s *Server) handleAdminResourceTakedownApprove(w http.ResponseWriter, r *http.Request) {
	s.resolveTakedown(w, r, true)
}

func (s *Server) handleAdminResourceTakedownReject(w http.ResponseWriter, r *http.Request) {
	s.resolveTakedown(w, r, false)
}

func (s *Server) resolveTakedown(w http.ResponseWriter, r *http.Request, approve bool) {
	if !s.resourcesReady(w) {
		return
	}
	var req struct {
		Note string `json:"note"`
	}
	if !s.readJSON(w, r, &req) {
		return
	}
	id := r.PathValue("id")
	if err := s.resources.ResolveTakedown(id, "admin", approve, req.Note); err != nil {
		resourceError(w, err)
		return
	}
	action := "resource.takedown_rejected"
	if approve {
		action = "resource.takedown_approved"
	}
	s.auditResource(action, fmt.Sprintf("takedown=%s", id))
	writeJSON(w, 200, map[string]any{"ok": true})
}
