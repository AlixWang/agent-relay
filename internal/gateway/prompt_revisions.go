package gateway

import (
	"errors"
	"net/http"

	"github.com/AlixWang/agent-relay/internal/prompts"
)

// handleAdminPromptVersions exposes the immutable per-template revisions.
// The revision catalog is assembled by the prompts package so the gateway
// only serializes it.
func (s *Server) handleAdminPromptVersions(w http.ResponseWriter, r *http.Request) {
	current, revs, err := prompts.RevisionCatalog(s.st)
	if err != nil {
		writeErr(w, 500, "prompt revisions: "+err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "prompt_version": prompts.PromptVersion,
		"current": current, "revisions": revs,
	})
}

// handleAdminPromptDiff compares two immutable prompt revisions. All
// resolution (current default, stored base link, same-type check) happens in
// prompts.DiffRevisions; the handler maps its sentinel errors to HTTP codes.
func (s *Server) handleAdminPromptDiff(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	diff, err := prompts.DiffRevisions(s.st, q.Get("agent_type"), q.Get("from"), q.Get("to"))
	if err != nil {
		switch {
		case errors.Is(err, prompts.ErrRevisionQuery):
			writeErr(w, 400, err.Error())
		case errors.Is(err, prompts.ErrRevisionNotFound):
			writeErr(w, 404, err.Error())
		default:
			writeErr(w, 500, "prompt diff: "+err.Error())
		}
		return
	}
	writeJSON(w, 200, map[string]any{
		"ok": true, "agent_type": diff.AgentType, "from": diff.From, "to": diff.To,
		"added": diff.Added, "removed": diff.Removed, "diff": diff,
	})
}
