// Package web serves the embedded operations console (DESIGN §4.8, §9).
package web

import (
	"embed"
	"io/fs"
	"net/http"
)

//go:embed ui/*
var raw embed.FS

// Handler serves the static console. API auth lives in gateway; the pages
// themselves are public (they call back into authed admin APIs).
func Handler() http.Handler {
	sub, err := fs.Sub(raw, "ui")
	if err != nil {
		panic(err)
	}
	return http.FileServer(http.FS(sub))
}
