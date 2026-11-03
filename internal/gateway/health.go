package gateway

import (
	"net/http"
	"time"
)

// handleAdminHealth is the operator-facing health view for the new
// projection/effects components. Existing /health stays a tiny public probe.
func (s *Server) handleAdminHealth(w http.ResponseWriter, r *http.Request) {
	body := map[string]any{"ok": true}
	if s.work != nil {
		body["work"] = map[string]any{"ready": s.work.Ready(), "last_error": s.work.LastError()}
	}
	if s.effects != nil {
		health, err := s.st.EffectStats(time.Now().Unix())
		if err != nil {
			writeErr(w, 500, "effect stats: "+err.Error())
			return
		}
		body["effects"] = map[string]any{
			"pending": health.Pending, "due": health.Due, "failed": health.Failed,
			"kinds": health.Kinds, "last_error": s.effects.LastError(),
		}
	}
	if s.guard != nil {
		body["policy_version"] = s.guard.PolicyVersion()
		body["policy_rules"] = s.guard.PolicyRules()
		if p, err := s.st.GetPolicyVersion(s.guard.PolicyVersion()); err == nil && p != nil {
			body["policy_snapshot"] = p
		}
	}
	writeJSON(w, 200, body)
}
