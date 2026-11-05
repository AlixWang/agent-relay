package gateway

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/resources"
)

func newResourceFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t)
	svc, err := resources.New(f.srv.st, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f.srv.SetResources(svc)
	f.registerPeer(t, "alice", "muse")
	f.registerPeer(t, "bob", "claw")
	return f
}

func (f *fixture) heartbeat(t *testing.T, peer string, extra map[string]any) map[string]any {
	t.Helper()
	body := map[string]any{"id": peer}
	for k, v := range extra {
		body[k] = v
	}
	c, out := f.do(t, "POST", "/heartbeat", body, f.tok[peer])
	if c != 200 {
		t.Fatalf("heartbeat %s: %d %+v", peer, c, out)
	}
	return out
}

func TestResourceHubSecretFlow(t *testing.T) {
	f := newResourceFixture(t)
	admin := f.adminLogin(t)

	// No grants: no nudge, empty manifest.
	if out := f.heartbeat(t, "alice", nil); out["resources_update"] != nil {
		t.Fatalf("nudged with nothing granted: %+v", out)
	}

	c, out := f.doCookie(t, "PUT", "/admin/resources/secret/OPENAI_API_KEY",
		map[string]any{"description": "gpt", "value": "sk-live-123"}, admin)
	if c != 200 {
		t.Fatalf("put: %d %+v", c, out)
	}
	// Admin responses never carry plaintext.
	for _, path := range []string{"/admin/resources", "/admin/resources/secret/OPENAI_API_KEY"} {
		req := httptest.NewRequest("GET", path, nil)
		req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
		rec := httptest.NewRecorder()
		f.mux.ServeHTTP(rec, req)
		if rec.Code != 200 || strings.Contains(rec.Body.String(), "sk-live-123") {
			t.Fatalf("%s leaked or failed: %d %s", path, rec.Code, rec.Body.String())
		}
	}

	// Not granted yet: 404 (indistinguishable from missing).
	if c, _ := f.do(t, "GET", "/resources/secrets/OPENAI_API_KEY", nil, f.tok["alice"]); c != 404 {
		t.Fatalf("ungranted fetch: %d", c)
	}
	if c, _ := f.do(t, "GET", "/resources/secrets/NOPE", nil, f.tok["alice"]); c != 404 {
		t.Fatalf("missing fetch: %d", c)
	}

	if c, out := f.doCookie(t, "POST", "/admin/resources/secret/OPENAI_API_KEY/grants",
		map[string]any{"peer_id": "alice"}, admin); c != 200 {
		t.Fatalf("grant: %d %+v", c, out)
	}

	// Heartbeat nudges with the digest.
	out = f.heartbeat(t, "alice", nil)
	rev, _ := out["resources_rev"].(string)
	if out["resources_update"] != true || rev == "" || rev == resources.NoneMarker {
		t.Fatalf("no nudge after grant: %+v", out)
	}
	if out := f.heartbeat(t, "bob", nil); out["resources_update"] != nil {
		t.Fatalf("bob nudged for alice's grant: %+v", out)
	}

	// Manifest lists the secret by fetch_url only.
	c, out = f.do(t, "GET", "/resources/current", nil, f.tok["alice"])
	if c != 200 || out["manifest_rev"] != rev {
		t.Fatalf("manifest: %d %+v", c, out)
	}
	list := out["resources"].([]any)
	entry := list[0].(map[string]any)
	if len(list) != 1 || entry["fetch_url"] != "/resources/secrets/OPENAI_API_KEY" || entry["content"] != nil {
		t.Fatalf("manifest entry: %+v", entry)
	}

	// Fetch: plaintext, no-store, audited.
	req := httptest.NewRequest("GET", "/resources/secrets/OPENAI_API_KEY", nil)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"value":"sk-live-123"`) {
		t.Fatalf("fetch: %d %s", rec.Code, rec.Body.String())
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Fatalf("Cache-Control: %q", cc)
	}
	audit, _ := f.srv.st.ListAudit("", "secret.fetch", 0, 50)
	found := false
	for _, a := range audit {
		if a.Action == "secret.fetch" && a.Actor == "alice" && strings.Contains(a.Detail, "OPENAI_API_KEY") {
			if strings.Contains(a.Detail, "sk-live") {
				t.Fatal("audit leaked plaintext")
			}
			found = true
		}
	}
	if !found {
		t.Fatal("secret.fetch not audited")
	}

	// Receipt stops the nudge.
	if out := f.heartbeat(t, "alice", map[string]any{"resources_rev": rev}); out["resources_update"] != nil {
		t.Fatalf("still nudged after receipt: %+v", out)
	}
	// Rotation nudges again.
	f.doCookie(t, "PUT", "/admin/resources/secret/OPENAI_API_KEY", map[string]any{"value": "sk-rotated"}, admin)
	out = f.heartbeat(t, "alice", nil)
	if out["resources_update"] != true || out["resources_rev"] == rev {
		t.Fatalf("rotation not nudged: %+v", out)
	}

	// Revoke: warning, fetch 404, nudge to the "none" marker until confirmed.
	c, out = f.doCookie(t, "DELETE", "/admin/resources/secret/OPENAI_API_KEY/grants/alice", nil, admin)
	if c != 200 || out["warning"] == nil {
		t.Fatalf("revoke: %d %+v", c, out)
	}
	if c, _ := f.do(t, "GET", "/resources/secrets/OPENAI_API_KEY", nil, f.tok["alice"]); c != 404 {
		t.Fatalf("revoked fetch: %d", c)
	}
	out = f.heartbeat(t, "alice", nil)
	if out["resources_update"] != true || out["resources_rev"] != resources.NoneMarker {
		t.Fatalf("revocation not nudged: %+v", out)
	}
	if out := f.heartbeat(t, "alice", map[string]any{"resources_rev": resources.NoneMarker}); out["resources_update"] != nil {
		t.Fatalf("still nudged after empty receipt: %+v", out)
	}
}

func TestResourceHubWildcardAndAuth(t *testing.T) {
	f := newResourceFixture(t)
	admin := f.adminLogin(t)
	f.doCookie(t, "PUT", "/admin/resources/secret/GITHUB_TOKEN", map[string]any{"value": "ghp_x"}, admin)
	if c, out := f.doCookie(t, "POST", "/admin/resources/secret/GITHUB_TOKEN/grants",
		map[string]any{"peer_id": "*"}, admin); c != 200 {
		t.Fatalf("wildcard grant: %d %+v", c, out)
	}
	for _, p := range []string{"alice", "bob"} {
		if c, out := f.do(t, "GET", "/resources/secrets/GITHUB_TOKEN", nil, f.tok[p]); c != 200 || out["value"] != "ghp_x" {
			t.Fatalf("%s via *: %d %+v", p, c, out)
		}
	}
	// The console revokes the wildcard via DELETE …/grants/* (unencoded).
	if c, _ := f.doCookie(t, "DELETE", "/admin/resources/secret/GITHUB_TOKEN/grants/*", nil, admin); c != 200 {
		t.Fatalf("revoke wildcard: %d", c)
	}
	if c, _ := f.do(t, "GET", "/resources/secrets/GITHUB_TOKEN", nil, f.tok["bob"]); c != 404 {
		t.Fatalf("after wildcard revoke: %d", c)
	}
	f.doCookie(t, "POST", "/admin/resources/secret/GITHUB_TOKEN/grants", map[string]any{"peer_id": "*"}, admin)
	// Agent routes need a bearer token; admin routes need the cookie.
	if c, _ := f.do(t, "GET", "/resources/current", nil, ""); c != 401 {
		t.Fatalf("unauth manifest: %d", c)
	}
	if c, _ := f.do(t, "GET", "/admin/resources", nil, f.tok["alice"]); c != 401 {
		t.Fatalf("agent token on admin route: %d", c)
	}
	// Validation surfaces as 400/404, not 500.
	if c, _ := f.doCookie(t, "POST", "/admin/resources/secret/GITHUB_TOKEN/grants", map[string]any{"peer_id": "ghost"}, admin); c != 404 {
		t.Fatalf("grant unknown peer: %d", c)
	}
	if c, _ := f.doCookie(t, "PUT", "/admin/resources/secret/bad%20name", map[string]any{"value": "v"}, admin); c != 400 {
		t.Fatalf("bad name: %d", c)
	}
	if c, _ := f.doCookie(t, "PUT", "/admin/resources/mcp/github", map[string]any{"value": "{}"}, admin); c != 400 {
		t.Fatalf("mcp not yet supported should 400: %d", c)
	}
	if c, _ := f.doCookie(t, "PUT", "/admin/resources/bogus/x", map[string]any{"value": "v"}, admin); c != 400 {
		t.Fatalf("bogus kind: %d", c)
	}
	// Delete drops it for everybody.
	if c, _ := f.doCookie(t, "DELETE", "/admin/resources/secret/GITHUB_TOKEN", nil, admin); c != 200 {
		t.Fatalf("delete: %d", c)
	}
	if c, _ := f.do(t, "GET", "/resources/secrets/GITHUB_TOKEN", nil, f.tok["bob"]); c != 404 {
		t.Fatalf("deleted fetch: %d", c)
	}
}

func TestResourceHubDisabledIs503(t *testing.T) {
	f := newFixture(t)
	f.registerPeer(t, "alice", "muse")
	if c, _ := f.do(t, "GET", "/resources/current", nil, f.tok["alice"]); c != 503 {
		t.Fatalf("disabled hub: %d", c)
	}
	if out := f.heartbeat(t, "alice", nil); out["resources_update"] != nil {
		t.Fatalf("disabled hub nudged: %+v", out)
	}
}
