//go:build tools

// This file pins dependencies that are only referenced from generated code
// (internal/web/views/*_templ.go is gitignored), so that `go mod tidy` keeps
// them in go.mod even in a checkout where `templ generate` has not run.
//
// Keep the version in lockstep with the templ CLI pinned in .github/workflows
// and the Makefile.
package tools

import _ "github.com/a-h/templ"
