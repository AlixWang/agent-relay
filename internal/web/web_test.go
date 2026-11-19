package web

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestEmbeddedConsoleServes(t *testing.T) {
	h := Handler()
	for path, wantCT := range map[string]string{
		"/":          "text/html",
		"/app.js":    "javascript",
		"/style.css": "text/css",
	} {
		req := httptest.NewRequest("GET", path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", path, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, wantCT) {
			t.Fatalf("%s content-type: %s", path, ct)
		}
	}
	// index.html must reference the JS/CSS the handler serves, with a cache
	// busting version; / itself must be no-cache.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "/app.js?v=") || !strings.Contains(body, "/style.css?v=") {
		t.Fatal("index.html missing versioned asset references")
	}
	if strings.Contains(body, "{{ASSET_V}}") {
		t.Fatal("unrendered asset placeholder")
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("index cache-control: %q", cc)
	}
	// Unknown path → 404, not a panic.
	req = httptest.NewRequest("GET", "/nope.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown path: %d", rec.Code)
	}
}

func TestPollScriptEmbedded(t *testing.T) {
	s := PollScript()
	if !strings.Contains(s, "relay-poll") || !strings.Contains(s, "/messages?for=") {
		t.Fatal("embedded poll script looks wrong")
	}
	// Must stay in sync with the repo copy.
	raw, err := os.ReadFile("../../clients/relay-poll.sh")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	if s != string(raw) {
		t.Fatal("internal/web/clients/relay-poll.sh diverged from clients/relay-poll.sh")
	}
}

func TestConsoleIDsMatchJS(t *testing.T) {
	// Every $('id') in app.js must exist as id="..." in index.html,
	// otherwise a button silently does nothing.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	Handler().ServeHTTP(rec, req)
	html := rec.Body.String()
	req2 := httptest.NewRequest("GET", "/app.js", nil)
	rec2 := httptest.NewRecorder()
	Handler().ServeHTTP(rec2, req2)
	js := rec2.Body.String()

	ids := map[string]bool{}
	for _, line := range strings.Split(html, "\n") {
		for {
			i := strings.Index(line, `id="`)
			if i < 0 {
				break
			}
			line = line[i+4:]
			j := strings.Index(line, `"`)
			if j < 0 {
				break
			}
			ids[line[:j]] = true
			line = line[j+1:]
		}
	}

	// Key elements must exist in index.html
	for _, key := range []string{
		"loginPage", "loginForm", "loginbtn", "username", "pw", "loginErr", "app",
		"sidebar", "menuBtn", "themeBtn", "logoutbtn", "pageTitle", "pageDesc",
		"page-work", "page-members", "page-threads", "page-command", "page-prompts", "page-audit", "page-logs", "page-tokens", "page-system", "page-muse", "page-muse-chat",
		"threadDrawer", "rotateDlg", "confirmDlg", "toasts",
	} {
		if !ids[key] {
			t.Fatalf("index.html missing key element id=%q", key)
		}
	}

	// Dynamically find every $('id') in console JS and assert it is present in index.html
	var allJS strings.Builder
	for _, p := range []string{
		"/app.js",
		"/js/api.js",
		"/js/utils.js",
		"/js/router.js",
		"/js/auth.js",
		"/js/work.js",
		"/js/members.js",
		"/js/threads.js",
		"/js/prompts.js",
		"/js/audit.js",
		"/js/logs.js",
		"/js/tokens.js",
		"/js/system.js",
		"/js/command.js",
		"/js/muse.js",
	} {
		req := httptest.NewRequest("GET", p, nil)
		rec := httptest.NewRecorder()
		Handler().ServeHTTP(rec, req)
		if rec.Code == 200 {
			allJS.WriteString(rec.Body.String() + "\n")
		}
	}
	js = allJS.String()
	lines := strings.Split(js, "\n")
	matched := 0
	for _, line := range lines {
		for {
			idx := strings.Index(line, "$('")
			if idx < 0 {
				break
			}
			line = line[idx+3:]
			end := strings.Index(line, "')")
			if end < 0 {
				break
			}
			ref := line[:end]
			line = line[end+2:]
			if !ids[ref] {
				t.Fatalf("index.html missing id=%q referenced by app.js", ref)
			}
			matched++
		}
	}
	if matched < 30 {
		t.Fatalf("expected at least 30 $('id') checks in app.js, got %d", matched)
	}
}

func TestTailScriptEmbedded(t *testing.T) {
	s := TailScript()
	if !strings.Contains(s, "relay-tail") || !strings.Contains(s, "/messages/stream?for=") {
		t.Fatal("embedded tail script looks wrong")
	}
	// Must stay in sync with the repo copy.
	raw, err := os.ReadFile("../../clients/relay-tail.sh")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	if s != string(raw) {
		t.Fatal("internal/web/clients/relay-tail.sh diverged from clients/relay-tail.sh")
	}
}

func TestWatchScriptEmbedded(t *testing.T) {
	s := WatchScript()
	if !strings.Contains(s, "relay-watch") || !strings.Contains(s, "spool") {
		t.Fatal("embedded watch script looks wrong")
	}
	for _, must := range []string{"AGENT_LOG_FILE", "flush_agent_logs", `"$RELAY_URL/logs"`, "wake.requested", "watchdog.binary_missing", "worker.timeout"} {
		if !strings.Contains(s, must) {
			t.Fatalf("embedded watch script missing log reporting %q", must)
		}
	}
	// Must stay in sync with the repo copy.
	raw, err := os.ReadFile("../../clients/muse/relay-watch.sh")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	if s != string(raw) {
		t.Fatal("internal/web/clients/relay-watch.sh diverged from clients/muse/relay-watch.sh")
	}
}

// TestScriptVersions is the Phase 0 invariant: the Muse watch script carries
// exactly one parseable version constant, the server reads that constant from
// the same embedded bytes it serves, and the two repo copies are identical.
func TestScriptVersions(t *testing.T) {
	embedded := WatchScript()
	repo, err := os.ReadFile("../../clients/muse/relay-watch.sh")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	served, err := os.ReadFile("clients/relay-watch.sh")
	if err != nil {
		t.Fatalf("embedded copy: %v", err)
	}
	if !bytes.Equal(repo, served) || string(repo) != embedded {
		t.Fatal("relay-watch.sh copies diverged (clients/muse, internal/web/clients, embed)")
	}
	if !strings.Contains(string(repo), "\nSCRIPT_KIND=\"muse-watch\"\n") {
		t.Fatal("relay-watch.sh missing SCRIPT_KIND=muse-watch")
	}
	ver := WatchScriptVersion()
	if ver == "" {
		t.Fatal("WATCH_VERSION missing or malformed in embedded script")
	}
	if !strings.Contains(string(repo), "\nWATCH_VERSION=\""+ver+"\"\n") {
		t.Fatalf("WatchScriptVersion()=%q does not match the script constant", ver)
	}
	if got, want := WatchScriptVersion(), watchVersionFrom(string(repo)); got != want {
		t.Fatalf("version parsed from embed %q != repo copy %q", got, want)
	}
	if MuseWatchKind != "muse-watch" || MuseWatchPath != "/clients/relay-watch.sh" {
		t.Fatalf("unexpected script identity constants: %q %q", MuseWatchKind, MuseWatchPath)
	}
	sum := sha256.Sum256([]byte(embedded))
	if got, want := WatchScriptSHA256(), hex.EncodeToString(sum[:]); got != want {
		t.Fatalf("sha256 %q != %q", got, want)
	}
	if len(WatchScriptSHA256()) != 64 {
		t.Fatalf("sha256 must be 64 hex chars, got %d", len(WatchScriptSHA256()))
	}
}

func TestWatchHermesScriptEmbedded(t *testing.T) {
	s := WatchHermesScript()
	// The Hermes edition must keep the shared thin-layer shape and carry its
	// own wake implementation (one-shot worker + single-flight lock).
	for _, must := range []string{"spool", "wake_running", "hermes", "wake.lock", "AGENT_LOG_FILE", "flush_agent_logs", `"$RELAY_URL/logs"`} {
		if !strings.Contains(s, must) {
			t.Fatalf("embedded hermes watch script missing %q", must)
		}
	}
	// Must stay in sync with the repo copy.
	raw, err := os.ReadFile("../../clients/hermes/relay-watch.sh")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	if s != string(raw) {
		t.Fatal("internal/web/clients/relay-watch-hermes.sh diverged from clients/hermes/relay-watch.sh")
	}
}

func TestTailSupervisorScriptEmbedded(t *testing.T) {
	s := TailSupervisorScript()
	// Served artifact for resident Hermes: it must stay a runnable stdlib-only
	// supervisor (no third-party imports) with a zombie-aware single-flight lock.
	if !strings.HasPrefix(s, "#!/usr/bin/env python3") {
		t.Fatal("embedded supervisor lost its shebang")
	}
	for _, must := range []string{"import json", "import subprocess", "wake_running", "other_tail_running", "WAKE_CMD"} {
		if !strings.Contains(s, must) {
			t.Fatalf("embedded supervisor missing %q", must)
		}
	}
	// Must stay in sync with the repo copy.
	raw, err := os.ReadFile("../../clients/hermes/relay-tail-supervisor.py")
	if err != nil {
		t.Fatalf("repo copy: %v", err)
	}
	if s != string(raw) {
		t.Fatal("internal/web/clients/relay-tail-supervisor.py diverged from clients/hermes/relay-tail-supervisor.py")
	}
}

// The wake layers inject their text into every Hermes wake, so a rule copy
// there re-teaches stale rules on each task. They must defer to the served
// prompt-current.md instead of enumerating redlines themselves.
func TestWakeLayersDeferRulesToServedPrompt(t *testing.T) {
	for name, s := range map[string]string{
		"relay-watch-hermes.sh":    WatchHermesScript(),
		"relay-tail-supervisor.py": TailSupervisorScript(),
		"relay-watch.sh":           WatchScript(),
	} {
		for _, bad := range []string{"四类红线", "删不可恢复数据", "对外发布或花钱", "三步走"} {
			if strings.Contains(s, bad) {
				t.Fatalf("%s carries stale rule text %q", name, bad)
			}
		}
	}
	for name, s := range map[string]string{
		"relay-watch-hermes.sh":    WatchHermesScript(),
		"relay-tail-supervisor.py": TailSupervisorScript(),
	} {
		if !strings.Contains(s, "prompt-current.md") {
			t.Fatalf("%s must point the worker at prompt-current.md", name)
		}
	}
}
