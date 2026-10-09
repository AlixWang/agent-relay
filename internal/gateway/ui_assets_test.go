package gateway

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/web"
)

// The console must actually work in a browser, and CI must notice when it does
// not. v0.14.0 shipped a console whose CSS/JS all 404'd (assets referenced as
// /ui/... while the embedded FS is already rooted at ui/), whose htmx came from
// an external CDN, and whose sidebar linked to four routes that did not exist —
// every check stayed green, because nothing loaded a page. This test walks the
// rendered pages, resolves every reference and asserts the fragments answer.

var refRe = regexp.MustCompile(`(?:src|href)="([^"]+)"`)

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

func postForm(t *testing.T, f *fixture, path string, form map[string][]string, cookie string) (int, string) {
	t.Helper()
	body := ""
	for k, vs := range form {
		for _, v := range vs {
			if body != "" {
				body += "&"
			}
			body += k + "=" + v
		}
	}
	req := httptest.NewRequest("POST", path, bytes.NewReader([]byte(body)))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	}
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

func TestConsolePagesRenderWithResolvableAssets(t *testing.T) {
	f := newFixture(t)
	// The default fixture mux has no static handler, so /style.css would 404
	// for the wrong reason; wire the real embedded console the way main does.
	f.mux = f.srv.Handler(web.Handler())
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	cookie := f.adminLogin(t)

	// A room to open, so the pane (not just the empty page) is exercised.
	if code, out := f.doCookie(t, "POST", "/admin/rooms", map[string]any{
		"id": "ops", "name": "运维群", "members": []string{"alice", "bob"},
	}, cookie); code != 200 {
		t.Fatalf("seed room: %d %+v", code, out)
	}

	pages := []string{"/admin/command", "/admin/command?room=grp_ops"}
	var pageHTML []string
	for _, page := range pages {
		code, body := getRaw(t, f, page, cookie)
		if code != 200 || !strings.Contains(body, "<html") {
			t.Fatalf("%s: %d %q", page, code, truncate(body))
		}
		pageHTML = append(pageHTML, body)
		if !strings.Contains(body, "指挥台") {
			t.Fatalf("%s does not look like the command center", page)
		}
		// No external asset may be required for the console to work: the relay
		// is often reached from networks where a CDN is slow or blocked.
		for _, ref := range refRe.FindAllStringSubmatch(body, -1) {
			raw := ref[1]
			if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
				t.Fatalf("%s references an external asset: %s", page, raw)
			}
			// The embedded FS is rooted at ui/, so /ui/... resolves to
			// ui/ui/... and 404s — the bug that shipped in v0.14.0.
			if strings.HasPrefix(raw, "/ui/") {
				t.Fatalf("%s references %s, which resolves inside the embedded ui/ root", page, raw)
			}
			// Resolve same-origin references (strip ?v= and #hash).
			path := raw
			if i := strings.IndexAny(path, "?#"); i >= 0 {
				path = path[:i]
			}
			if path == "" {
				continue
			}
			if code, _ := getRaw(t, f, path, cookie); code != 200 {
				t.Fatalf("%s: %s → %d (broken link/asset)", page, raw, code)
			}
		}
	}

	// The fragments htmx swaps in must answer too, including the failure paths
	// (they render a toast, not an HTTP error page).
	fragments := []string{"/admin/command/rooms", "/admin/command/inbox", "/admin/command/room/grp_ops", "/admin/command/room/grp_missing"}
	for _, path := range fragments {
		if code, body := getRaw(t, f, path, cookie); code != 200 || !strings.Contains(body, "<") {
			t.Fatalf("fragment %s: %d %q", path, code, truncate(body))
		}
	}
	// Unauthenticated access to a fragment must be refused, not rendered.
	if code, _ := getRaw(t, f, "/admin/command/rooms", ""); code != 401 {
		t.Fatalf("fragment without a session: %d", code)
	}

	// Form posts answer with HTML (toasts) and never 500.
	forms := []struct {
		path string
		form map[string][]string
	}{
		{"/admin/command/send", map[string][]string{"to": {"alice"}, "kind": {"task"}, "payload": {"控制台表单检查：请回报"}}},
		{"/admin/command/send", map[string][]string{"to": {"grp_ops"}, "kind": {"chat"}, "payload": {"群表单检查：知会一条"}}},
		{"/admin/command/send", map[string][]string{"to": {"grp_ops"}, "kind": {"chat"}, "payload": {"   "}}}, // empty → toast
		{"/admin/command/send", map[string][]string{"to": {"grp_nope"}, "kind": {"chat"}, "payload": {"typo room"}}},
		{"/admin/command/rooms", map[string][]string{"id": {"team"}, "name": {"小队"}, "members": {"alice"}}},
		{"/admin/command/rooms", map[string][]string{"id": {"Bad Slug!"}, "members": {"alice"}}},
		{"/admin/command/rooms/grp_ops/members", map[string][]string{"action": {"add"}, "peer_id": {"carol"}}},
		{"/admin/command/rooms/grp_ops/members", map[string][]string{"action": {"remove"}, "peer_id": {"alice"}}},
		{"/admin/command/inbox/read", nil},
	}
	for _, tc := range forms {
		code, body := postForm(t, f, tc.path, tc.form, cookie)
		if code != 200 {
			t.Fatalf("form %s → %d %q", tc.path, code, truncate(body))
		}
		if !strings.Contains(body, "<") {
			t.Fatalf("form %s returned no HTML: %q", tc.path, truncate(body))
		}
	}

	// And the form path must have the same effect as the API path: the room
	// message went through operatorSend, so it is visible to members.
	_, pull := f.do(t, "GET", "/messages?for=bob&since=0", nil, f.tok["bob"])
	if !strings.Contains(bodyString(pull), "群表单检查") {
		t.Fatalf("console form send did not reach the room member: %+v", pull)
	}
}

// Every admin route the console's assets call must exist. v0.15.0 removed the
// v0.14.0 conversation endpoints but left the single-page console's dead
// 会话 page in place, so "create group chat" in the SPA POSTed to
// /admin/conversations and got a 404 — while CI was green, because nothing
// compared UI calls against the router. This walks the shipped JS/HTML and
// requires every /admin/... literal to answer something other than 404
// (401/400/405 are fine: they mean the route exists).
func TestConsoleAssetsDoNotCallMissingAdminRoutes(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)

	substRe := regexp.MustCompile(`\$\{[^}]*\}`)
	candRe := regexp.MustCompile(`/admin/[A-Za-z0-9/_.\-]*`)
	for _, asset := range []string{"/app.js", "/"} {
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
			// The literal may be a prefix of a templ path: only fail when no
			// method answers at all.
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

// A sidebar entry must actually be reachable: either an in-page route the SPA
// knows, or a link that leaves the SPA while the nav handler keeps its hands
// off. v0.15.1 shipped the 指挥台 entry as /admin/command — the route existed
// and the link existed — but app.js called preventDefault() on every .sb-link
// and routed it to setRoute(undefined) → 成员, so the page could not be entered
// by clicking. Route existence alone does not catch that.
func TestConsoleSidebarEntriesAreReachable(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	_, html := getRaw(t, f, "/", cookie)
	_, js := getRaw(t, f, "/app.js", cookie)

	anchorRe := regexp.MustCompile(`<a\s[^>]*class="[^"]*sb-link[^"]*"[^>]*>`)
	attr := func(name string) *regexp.Regexp { return regexp.MustCompile(name + `="([^"]*)"`) }
	hashRoutes, outRoutes := 0, 0
	for _, tag := range anchorRe.FindAllString(html, -1) {
		href := attr("href").FindStringSubmatch(tag)
		if href == nil {
			t.Fatalf("sidebar entry without href: %s", tag)
		}
		route := attr("data-route").FindStringSubmatch(tag)
		if strings.HasPrefix(href[1], "#") {
			hashRoutes++
			want := strings.TrimPrefix(href[1], "#")
			if route == nil || route[1] != want {
				t.Fatalf("%s must carry data-route=%q", tag, want)
			}
			// routeMeta keys are aligned with padding, so match the key line.
			routeLine := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(want) + `:\s*\{`)
			if !routeLine.MatchString(js) {
				t.Fatalf("sidebar route %q is not in the SPA routeMeta table", want)
			}
			continue
		}
		outRoutes++
		if code := probeRoute(t, f, "GET", href[1], cookie); code == 404 {
			t.Fatalf("sidebar link %s is not served", href[1])
		}
	}
	if hashRoutes == 0 || outRoutes == 0 {
		t.Fatalf("expected in-page and cross-page sidebar entries, saw %d/%d", hashRoutes, outRoutes)
	}
	if !strings.Contains(js, "startsWith('#')") {
		t.Fatal("app.js no longer guards .sb-link clicks by href: cross-page sidebar entries would be swallowed")
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

// The templ console must use the class names the stylesheet actually defines,
// and the same shell containers as the SPA that stylesheet was written for.
// v0.15.2 shipped the 指挥台 with class="app" (the stylesheet has ".shell") and
// .topbar-title/.topbar-right (the stylesheet has .tb-title/.tb-right), so the
// sidebar and the content column stacked instead of rendering as a flex shell:
// the page looked broken and parts of it could not be clicked. Nothing compared
// markup against CSS, so it shipped green.
func TestTemplConsoleMarkupMatchesStylesheet(t *testing.T) {
	f := newFixture(t)
	f.mux = f.srv.Handler(web.Handler())
	cookie := f.adminLogin(t)
	_, css := getRaw(t, f, "/style.css", cookie)
	defined := map[string]bool{}
	for _, m := range regexp.MustCompile(`\.([A-Za-z][A-Za-z0-9_-]*)`).FindAllStringSubmatch(css, -1) {
		defined[m[1]] = true
	}
	classRe := regexp.MustCompile(`class="([^"]*)"`)

	// Pages and fragments: fragments carry the same classes and are swapped in
	// on their own, so an undefined class there is just as visible.
	pages := []string{"/admin/command", "/admin/command/rooms", "/admin/command/inbox", "/admin/command/room/grp_ops"}
	for _, page := range pages {
		code, body := getRaw(t, f, page, cookie)
		if code != 200 {
			t.Fatalf("%s: %d", page, code)
		}
		for _, m := range classRe.FindAllStringSubmatch(body, -1) {
			for _, tok := range strings.Fields(m[1]) {
				if !defined[tok] {
					t.Fatalf("%s uses class %q, which style.css does not define (it would render unstyled)", page, tok)
				}
			}
		}
	}
	// Shell parity with the single-page console, which the stylesheet targets.
	_, page := getRaw(t, f, "/admin/command", cookie)
	for _, needle := range []string{
		`<div id="app" class="shell">`,
		`class="main-col"`,
		`class="topbar"`,
		`class="tb-title"`,
		`class="tb-right"`,
		`class="toasts"`,
		`id="page-command"`,
	} {
		if !strings.Contains(page, needle) {
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
