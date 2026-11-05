package gateway

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/resources"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Resource hub (DESIGN §4.12, ADR 0004/0005). Agent routes are read-only and
// grant-scoped; admin routes live under /admin/resources* and never return
// secret plaintext.

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
