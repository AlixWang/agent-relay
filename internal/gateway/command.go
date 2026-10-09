package gateway

import (
	"net/http"
)

// handleCommandPage redirects /admin/command to the SPA console route /#command.
func (s *Server) handleCommandPage(w http.ResponseWriter, r *http.Request) {
	target := "/#command"
	if room := r.URL.Query().Get("room"); room != "" {
		target = "/#command?room=" + room
	}
	http.Redirect(w, r, target, http.StatusFound)
}
