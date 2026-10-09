package views

// Console asset plumbing for the templ pages.
//
// The console is served from the embedded ui/ tree (internal/web/web.go), so
// asset URLs must be rooted at "/" — "/style.css", "/app-minimal.js",
// "/htmx-config.js", "/vendor/htmx.min.js". Referencing "/ui/..." resolves to
// ui/ui/... inside the embedded FS and 404s (that bug shipped in v0.14.0).
//
// htmx is vendored into internal/web/ui/vendor/ on purpose: the console is
// served from a single self-contained binary, so no external CDN may be
// required for it to work.
//
// assetVersion is the release tag, set once at startup by the gateway
// (web.AssetVersion()) so browsers refetch JS/CSS after a release instead of
// mixing stale cached copies with fresh HTML.
var assetVersion = "dev"

// SetAssetVersion is called once at server startup. Empty keeps "dev".
func SetAssetVersion(v string) {
	if v != "" {
		assetVersion = v
	}
}

// AssetVersion reports the value currently used for ?v= cache busting.
func AssetVersion() string { return assetVersion }
