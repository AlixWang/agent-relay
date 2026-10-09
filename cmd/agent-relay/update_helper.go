package main

import _ "embed"

// updateHelperScript is the canonical web-update worker (DESIGN §10.4). It is
// embedded so that deploy/install.sh extracts it from the binary
// (`agent-relay --print-update-helper`) instead of carrying its own copy, and
// so the worker can refresh itself from the binary after an update: one source
// of truth, shipped with every release.
//
//go:embed update-helper.sh
var updateHelperScript string
