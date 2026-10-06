// Package web serves the embedded operations console (DESIGN §4.8, §9).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/*
var raw embed.FS

//go:embed clients/relay-poll.sh
var pollScript string

// Handler serves the static console. API auth lives in gateway; the pages
// themselves are public (they call back into authed admin APIs).
func Handler() http.Handler {
	sub, err := fs.Sub(raw, "ui")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}

// PollScript returns the versioned polling script served at
// GET /clients/relay-poll.sh. Static content, no auth needed — assistants
// fetch it during onboarding instead of inventing protocol details.
func PollScript() string { return pollScript }
