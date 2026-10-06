// Package web serves the embedded operations console (DESIGN §4.8, §9).
package web

import (
	"embed"
	"io/fs"
	"net/http"
	"strings"
)

//go:embed ui/*
var raw embed.FS

//go:embed clients/relay-poll.sh
var pollScript string

// assetVersion is baked at build time (ldflags -X) so browsers refetch
// app.js/style.css after each release instead of mixing stale cached copies.
var assetVersion = "dev"

// Handler serves the static console. API auth lives in gateway; the pages
// themselves are public (they call back into authed admin APIs).
func Handler() http.Handler {
	sub, err := fs.Sub(raw, "ui")
	if err != nil {
		panic(err)
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			// index.html is never cached; asset URLs carry ?v= so a new
			// release can never mix stale app.js/style.css with fresh HTML.
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			page, err := fs.ReadFile(sub, "index.html")
			if err != nil {
				http.Error(w, "console unavailable", http.StatusInternalServerError)
				return
			}
			out := strings.ReplaceAll(string(page), "{{ASSET_V}}", assetVersion)
			_, _ = w.Write([]byte(out))
			return
		}
		files.ServeHTTP(w, r)
	})
}

// AssetVersion reports the baked asset version (?v= query on app.js/style.css).
func AssetVersion() string { return assetVersion }

// PollScript returns the versioned polling script served at
// GET /clients/relay-poll.sh. Static content, no auth needed — assistants
// fetch it during onboarding instead of inventing protocol details.
func PollScript() string { return pollScript }
