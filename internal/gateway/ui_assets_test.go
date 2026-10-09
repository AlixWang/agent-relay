package gateway

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/web"
)

// The embedded console assets must never 404 under the gateway (DESIGN §4.8).
// A missing stylesheet or JS bundle is a total outage of the operations console
// and can't be caught by API unit tests alone.

func getRaw(t *testing.T, f *fixture, path, cookie string) (int, string) {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestConsolePagesRenderWithResolvableAssets(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	cookie := f.adminLogin(t)

	// Seed room
	if code, out := f.doCookie(t, "POST", "/admin/rooms", map[string]any{
		"id": "ops", "name": "运维群", "members": []string{"alice", "bob"},
	}, cookie); code != 200 {
		t.Fatalf("seed room: %d %+v", code, out)
	}

	// 1. The SPA console page renders with 200 and references valid assets
	code, body := getRaw(t, f, "/", cookie)
	if code != 200 || !strings.Contains(body, "<html") {
		t.Fatalf("/: %d %q", code, truncate(body))
	}
	if !strings.Contains(body, "指挥台") {
		t.Fatalf("/ does not contain command center entry")
	}

	refRe := regexp.MustCompile(`(?:href|src)="([^"]*)"`)
	for _, ref := range refRe.FindAllStringSubmatch(body, -1) {
		raw := ref[1]
		if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
			t.Fatalf("/ references an external asset: %s", raw)
		}
		if strings.HasPrefix(raw, "/ui/") {
			t.Fatalf("/ references %s, which resolves inside the embedded ui/ root", raw)
		}
		path := raw
		if i := strings.IndexAny(path, "?#"); i >= 0 {
			path = path[:i]
		}
		if path == "" {
			continue
		}
		if code, _ := getRaw(t, f, path, cookie); code != 200 {
			t.Fatalf("/ broken link/asset: %s → %d", raw, code)
		}
	}

	// 2. /admin/command redirects to the SPA route /#command
	for _, page := range []string{"/admin/command", "/admin/command?room=grp_ops"} {
		req := httptest.NewRequest("GET", page, nil)
		req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != 302 {
			t.Fatalf("%s expected 302 redirect, got %d", page, rec.Code)
		}
		loc := rec.Header().Get("Location")
		if !strings.HasPrefix(loc, "/#command") {
			t.Fatalf("%s redirected to %q, expected /#command", page, loc)
		}
	}
}

// Every admin route the console's assets call must exist.
func TestConsoleAssetsDoNotCallMissingAdminRoutes(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)

	substRe := regexp.MustCompile(`\$\{[^}]*\}`)
	candRe := regexp.MustCompile(`/admin/[A-Za-z0-9/_.\-]*`)
	for _, asset := range []string{
		"/app.js",
		"/js/api.js",
		"/js/utils.js",
		"/js/router.js",
		"/js/auth.js",
		"/js/members.js",
		"/js/threads.js",
		"/js/prompts.js",
		"/js/audit.js",
		"/js/tokens.js",
		"/js/system.js",
		"/js/command.js",
		"/",
	} {
		code, body := getRaw(t, f, asset, cookie)
		if code != 200 {
			t.Fatalf("asset %s: %d", asset, code)
		}
		normalized := substRe.ReplaceAllString(body, "x")
		seen := map[string]bool{}
		for _, cand := range candRe.FindAllString(normalized, -1) {
			cand = strings.TrimRight(cand, "/.-")
			if i := strings.IndexAny(cand, "?#"); i >= 0 {
				cand = cand[:i]
			}
			if cand == "/admin" || seen[cand] {
				continue
			}
			seen[cand] = true
			ok := false
			for _, method := range []string{"GET", "POST", "PATCH", "DELETE"} {
				status := probeRoute(t, f, method, cand, cookie)
				if status != 404 {
					ok = true
					break
				}
			}
			if !ok {
				t.Fatalf("%s calls %s, which no route serves (404)", asset, cand)
			}
		}
	}
}

// All sidebar entries must be reachable in-page SPA hash routes with matching metadata.
func TestConsoleSidebarEntriesAreReachable(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	_, html := getRaw(t, f, "/", cookie)
	_, js := getRaw(t, f, "/js/router.js", cookie)

	anchorRe := regexp.MustCompile(`<a\s[^>]*class="[^"]*sb-link[^"]*"[^>]*>`)
	attr := func(name string) *regexp.Regexp { return regexp.MustCompile(name + `="([^"]*)"`) }
	hashRoutes := 0
	for _, tag := range anchorRe.FindAllString(html, -1) {
		href := attr("href").FindStringSubmatch(tag)
		if href == nil {
			t.Fatalf("sidebar entry without href: %s", tag)
		}
		route := attr("data-route").FindStringSubmatch(tag)
		if !strings.HasPrefix(href[1], "#") {
			t.Fatalf("sidebar entry %s should be in-page SPA hash route", href[1])
		}
		hashRoutes++
		want := strings.TrimPrefix(href[1], "#")
		if route == nil || route[1] != want {
			t.Fatalf("%s must carry data-route=%q", tag, want)
		}
		routeLine := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(want) + `:\s*\{`)
		if !routeLine.MatchString(js) {
			t.Fatalf("sidebar route %q is not in the SPA routeMeta table", want)
		}
	}
	if hashRoutes < 7 {
		t.Fatalf("expected at least 7 in-page sidebar entries, saw %d", hashRoutes)
	}
}

func probeRoute(t *testing.T, f *fixture, method, path, cookie string) int {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Code
}

// The console must use class names defined in style.css and contain shell containers.
func TestConsoleMarkupMatchesStylesheet(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	_, css := getRaw(t, f, "/style.css", cookie)
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([A-Za-z][A-Za-z0-9_-]*)`).FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	classRe := regexp.MustCompile(`(?:^|[^:])\bclass="([^"]*)"`)

	code, body := getRaw(t, f, "/", cookie)
	if code != 200 {
		t.Fatalf("/: %d", code)
	}
	for _, m := range classRe.FindAllStringSubmatch(body, -1) {
		for _, tok := range strings.Fields(m[1]) {
			if strings.HasPrefix(tok, "{") || strings.HasPrefix(tok, "'") || strings.Contains(tok, ":") || strings.Contains(tok, "}") {
				continue
			}
			if !defined[tok] {
				t.Fatalf("index.html uses class %q, which style.css does not define", tok)
			}
		}
	}

	for _, needle := range []string{
		`<div id="app" class="shell"`,
		`class="main-col"`,
		`class="topbar"`,
		`class="tb-title"`,
		`class="tb-right"`,
		`class="toasts"`,
		`id="page-command"`,
	} {
		if !strings.Contains(body, needle) {
			t.Fatalf("console shell is missing %s", needle)
		}
	}
}

func truncate(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
}

// The update panel is the one screen an operator watches while the service
// restarts underneath them. Reported from production: it showed a bare
// "running" with no log and then sat there forever, and the only way to find
// out whether the update worked was a manual refresh. Pin the three things
// that were missing: a live job panel, the helper log in it, and a page that
// reloads itself into the new version.
func TestConsoleUpdatePanelWiring(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)

	code, page := getRaw(t, f, "/", cookie)
	if code != 200 {
		t.Fatalf("console page: %d", code)
	}
	for _, need := range []string{
		`id="updJob"`, `id="updSteps"`, `id="updLog"`, `id="updJobHint"`, `id="updJobState"`,
	} {
		if !strings.Contains(page, need) {
			t.Fatalf("update panel markup lost %s", need)
		}
	}
	code, js := getRaw(t, f, "/js/system.js", cookie)
	if code != 200 {
		code, js = getRaw(t, f, "/app.js", cookie)
		if code != 200 {
			t.Fatalf("system js asset: %d", code)
		}
	}
	// Console test files (underscore-prefixed, so //go:embed ui/* skips them)
	// must never be reachable from the internet.
	for _, notServed := range []string{"/_update-panel.test.mjs", "/app.test.mjs"} {
		if c, _ := getRaw(t, f, notServed, cookie); c != 404 {
			t.Fatalf("%s must not be served, got %d", notServed, c)
		}
	}
	for _, need := range []string{
		"location.reload()",    // success lands the operator in the new version
		"/admin/update/status", // polls the job
		"j.phases",             // renders the phase checklist
		"j.log",                // renders the helper log live
		"updSeenInFlight",      // only self-reload for a job this view watched
		"服务重启中",                // tolerates the restart window
	} {
		if !strings.Contains(js, need) {
			t.Fatalf("update panel behaviour lost %q", need)
		}
	}
}

// The command console is one Alpine root, and this guards it. Two production
// incidents came from getting that wrong:
//   - several x-data="commandApp" roots (page + dialogs) each built their own
//     state, so entering the route filled a dialog's state and the page showed
//     群聊频道 (0) / 助手私聊 (0) although /admin/rooms had the data;
//   - "fixing" it by handing all roots one shared object broke the console
//     completely: Alpine stamps its magics with a non-configurable
//     Object.defineProperty per root, so the second root threw
//     "Cannot redefine property: $nextTick" and Alpine.start() died.
//
// Hence: exactly one root in the markup, and the dialogs inside it (they need
// the page's peers/rooms and must inherit its scope).
// Console JS/CSS must revalidate: the ?v= query only wraps the entry points in
// index.html, while the modules (js/*.js) are imported by stable URL — a stale
// cached module after an update is a silent, confusing bug (the browser runs old
// logic against new markup).
func TestConsoleAssetsRevalidate(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	for _, asset := range []string{"/app.js", "/style.css", "/js/command.js", "/js/system.js"} {
		req := httptest.NewRequest("GET", asset, nil)
		req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", asset, rec.Code)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
			t.Fatalf("%s must revalidate (Cache-Control: no-cache), got %q", asset, cc)
		}
	}
}

func TestConsoleCommandRootIsSingle(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	code, page := getRaw(t, f, "/", cookie)
	if code != 200 {
		t.Fatalf("console page: %d", code)
	}
	if n := strings.Count(page, `x-data="commandApp"`); n != 1 {
		t.Fatalf("expected exactly one x-data=\"commandApp\" root (several roots = several states = empty lists; one shared object = Alpine crash), found %d", n)
	}
	// The section runs from its opening tag to the matching </section>.
	start := strings.Index(page, `id="page-command"`)
	if start < 0 {
		t.Fatal("command page section missing")
	}
	tagStart := strings.LastIndex(page[:start], "<section")
	if tagStart < 0 {
		t.Fatal("command page section tag not found")
	}
	depth, end := 0, -1
	for i := tagStart; i < len(page); {
		switch {
		case strings.HasPrefix(page[i:], "<section"):
			depth++
			i += len("<section")
		case strings.HasPrefix(page[i:], "</section>"):
			depth--
			if depth == 0 {
				end = i
			}
			i += len("</section>")
		default:
			i++
		}
		if end >= 0 {
			break
		}
	}
	if end < 0 {
		t.Fatal("command page section never closes")
	}
	section := page[start:end]
	for _, dlg := range []string{`id="dlgCreateRoom"`, `id="dlgRoomMembers"`} {
		at := strings.Index(section, dlg)
		if at < 0 {
			t.Fatalf("%s must live inside #page-command so it inherits the page's Alpine scope", dlg)
		}
		// ...and it must not declare its own root.
		line := section[at:min(at+160, len(section))]
		if strings.Contains(line, "x-data=") {
			t.Fatalf("%s must not declare its own x-data root (it inherits the page scope; a second root either gets its own state or crashes Alpine)", dlg)
		}
	}
}
