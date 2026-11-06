package gateway

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"mime/multipart"
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

func TestResourceHubMCPAndPrompt(t *testing.T) {
	f := newResourceFixture(t)
	admin := f.adminLogin(t)
	f.doCookie(t, "PUT", "/admin/resources/secret/GH", map[string]any{"value": "ghp_secret"}, admin)
	cfg := map[string]any{
		"transport": "stdio", "command": "npx",
		"args": []string{"-y", "@modelcontextprotocol/server-github"},
		"env":  map[string]string{"GITHUB_TOKEN": "${secret:GH}"},
	}
	if c, out := f.doCookie(t, "PUT", "/admin/resources/mcp/github", map[string]any{"description": "gh", "content": cfg}, admin); c != 200 {
		t.Fatalf("put mcp: %d %+v", c, out)
	}
	// Invalid configs are 400 with a reason.
	if c, out := f.doCookie(t, "PUT", "/admin/resources/mcp/bad", map[string]any{"content": map[string]any{"transport": "stdio"}}, admin); c != 400 || !strings.Contains(out["error"].(string), "command") {
		t.Fatalf("invalid mcp: %d %+v", c, out)
	}
	// Grant MCP without its secret: allowed, but the response names what is missing.
	c, out := f.doCookie(t, "POST", "/admin/resources/mcp/github/grants", map[string]any{"peer_id": "alice"}, admin)
	if c != 200 || fmt.Sprint(out["missing_secrets"]) != "[GH]" {
		t.Fatalf("grant hint: %d %+v", c, out)
	}
	// Fetch resolved config → 409 listing the missing secret, nothing leaked.
	c, out = f.do(t, "GET", "/resources/mcp/github", nil, f.tok["alice"])
	if c != 409 || fmt.Sprint(out["missing_secrets"]) != "[GH]" || strings.Contains(fmt.Sprint(out), "ghp_secret") {
		t.Fatalf("missing dep: %d %+v", c, out)
	}
	// Manifest: template inline with refs unresolved + requires_secrets + fetch_url.
	_, out = f.do(t, "GET", "/resources/current", nil, f.tok["alice"])
	e := out["resources"].([]any)[0].(map[string]any)
	content := e["content"].(map[string]any)
	if e["kind"] != "mcp" || e["fetch_url"] != "/resources/mcp/github" || fmt.Sprint(e["requires_secrets"]) != "[GH]" ||
		content["env"].(map[string]any)["GITHUB_TOKEN"] != "${secret:GH}" {
		t.Fatalf("manifest mcp entry: %+v", e)
	}
	// Grant the secret: resolved, no-store, audited per secret.
	f.doCookie(t, "POST", "/admin/resources/secret/GH/grants", map[string]any{"peer_id": "alice"}, admin)
	req := httptest.NewRequest("GET", "/resources/mcp/github", nil)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"GITHUB_TOKEN":"ghp_secret"`) {
		t.Fatalf("resolved: %d %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Header().Get("Cache-Control"), "no-store") {
		t.Fatal("resolved mcp must be no-store")
	}
	audit, _ := f.srv.st.ListAudit("alice", "secret.fetch", 0, 10)
	if len(audit) != 1 || !strings.Contains(audit[0].Detail, "via=mcp:github") {
		t.Fatalf("mcp fetch audit: %+v", audit)
	}
	// Admin view returns the editable template (refs, never plaintext).
	_, out = f.doCookie(t, "GET", "/admin/resources/mcp/github", nil, admin)
	if strings.Contains(fmt.Sprint(out), "ghp_secret") || !strings.Contains(fmt.Sprint(out), "${secret:GH}") {
		t.Fatalf("admin mcp view: %+v", out)
	}
	// bob has nothing: 404, not 409 (no existence leak).
	if c, _ := f.do(t, "GET", "/resources/mcp/github", nil, f.tok["bob"]); c != 404 {
		t.Fatalf("ungranted mcp: %d", c)
	}

	// Prompt: inline in manifest, string-only content.
	if c, out := f.doCookie(t, "PUT", "/admin/resources/prompt/reviewer", map[string]any{"content": map[string]any{"x": 1}}, admin); c != 400 {
		t.Fatalf("non-string prompt: %d %+v", c, out)
	}
	if c, out := f.doCookie(t, "PUT", "/admin/resources/prompt/reviewer", map[string]any{"content": "你是代码审查员。"}, admin); c != 200 {
		t.Fatalf("put prompt: %d %+v", c, out)
	}
	f.doCookie(t, "POST", "/admin/resources/prompt/reviewer/grants", map[string]any{"peer_id": "*"}, admin)
	_, out = f.do(t, "GET", "/resources/current", nil, f.tok["bob"])
	list := out["resources"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["content"] != "你是代码审查员。" {
		t.Fatalf("bob prompt manifest: %+v", out)
	}
}

func TestResourceSyncStatusIsOperatorOnly(t *testing.T) {
	f := newResourceFixture(t)
	admin := f.adminLogin(t)
	f.doCookie(t, "PUT", "/admin/resources/prompt/reviewer", map[string]any{"content": "x"}, admin)
	f.doCookie(t, "POST", "/admin/resources/prompt/reviewer/grants", map[string]any{"peer_id": "alice"}, admin)
	peerRow := func(out map[string]any, id string) map[string]any {
		for _, it := range out["peers"].([]any) {
			if m := it.(map[string]any); m["id"] == id {
				return m
			}
		}
		t.Fatalf("no %s in %+v", id, out)
		return nil
	}
	_, out := f.doCookie(t, "GET", "/admin/peers", nil, admin)
	if peerRow(out, "alice")["resources_pending"] != true || peerRow(out, "bob")["resources_pending"] != nil {
		t.Fatalf("pending flags: %+v", out)
	}
	rev := f.heartbeat(t, "alice", nil)["resources_rev"].(string)
	f.heartbeat(t, "alice", map[string]any{"resources_rev": rev})
	_, out = f.doCookie(t, "GET", "/admin/peers", nil, admin)
	if a := peerRow(out, "alice"); a["resources_pending"] != nil || a["resources_rev"] != rev {
		t.Fatalf("after receipt: %+v", a)
	}
	_, out = f.do(t, "GET", "/peers", nil, f.tok["bob"])
	if a := peerRow(out, "alice"); a["resources_rev"] != nil || a["resources_pending"] != nil {
		t.Fatalf("agent /peers leaked another peer's receipt: %+v", a)
	}
}

func skillArchive(t *testing.T, skillMD string, extra map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	files := map[string]string{"code-review/SKILL.md": skillMD}
	for k, v := range extra {
		files[k] = v
	}
	for name, body := range files {
		_ = tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg})
		_, _ = tw.Write([]byte(body))
	}
	_ = tw.Close()
	_ = gz.Close()
	return buf.Bytes()
}

func (f *fixture) uploadSkill(t *testing.T, cookie string, archive []byte, desc string) (int, map[string]any) {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, _ := mw.CreateFormFile("archive", "skill.tar.gz")
	_, _ = fw.Write(archive)
	if desc != "" {
		_ = mw.WriteField("description", desc)
	}
	_ = mw.Close()
	req := httptest.NewRequest("POST", "/admin/resources/skill", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: cookie})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestResourceHubSkill(t *testing.T) {
	f := newResourceFixture(t)
	admin := f.adminLogin(t)
	md := "---\nname: code-review\ndescription: Review code changes\n---\n# body\n"
	archive := skillArchive(t, md, map[string]string{"code-review/scripts/run.sh": "echo hi\n"})

	c, out := f.uploadSkill(t, admin, archive, "")
	res, _ := out["resource"].(map[string]any)
	if c != 200 || res["name"] != "code-review" || res["description"] != "Review code changes" {
		t.Fatalf("upload: %d %+v", c, out)
	}
	// Hostile archive → 400 with a reason, nothing stored.
	evil := skillArchive(t, md, map[string]string{"../../etc/cron.d/x": "* * * * * root rm -rf /\n"})
	if c, out := f.uploadSkill(t, admin, evil, ""); c != 400 || !strings.Contains(fmt.Sprint(out["error"]), "traversal") {
		t.Fatalf("evil upload: %d %+v", c, out)
	}
	// Name comes from SKILL.md, so PUT by URL is refused.
	if c, _ := f.doCookie(t, "PUT", "/admin/resources/skill/code-review", map[string]any{"value": "x"}, admin); c != 400 {
		t.Fatalf("PUT skill should 400, got %d", c)
	}
	// Missing file field.
	req := httptest.NewRequest("POST", "/admin/resources/skill", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: "agent_relay_admin", Value: admin})
	rec := httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("non-multipart upload: %d", rec.Code)
	}

	if c, _ := f.do(t, "GET", "/resources/skills/code-review", nil, f.tok["alice"]); c != 404 {
		t.Fatalf("ungranted download: %d", c)
	}
	f.doCookie(t, "POST", "/admin/resources/skill/code-review/grants", map[string]any{"peer_id": "alice"}, admin)

	// Manifest: fetch_url + file count, no archive bytes inline.
	_, out = f.do(t, "GET", "/resources/current", nil, f.tok["alice"])
	e := out["resources"].([]any)[0].(map[string]any)
	if e["kind"] != "skill" || e["fetch_url"] != "/resources/skills/code-review" || e["content"].(map[string]any)["files"] != float64(2) {
		t.Fatalf("manifest skill entry: %+v", e)
	}
	// Download: exact bytes, gzip, attachment name.
	req = httptest.NewRequest("GET", "/resources/skills/code-review", nil)
	req.Header.Set("Authorization", "Bearer "+f.tok["alice"])
	rec = httptest.NewRecorder()
	f.mux.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Equal(rec.Body.Bytes(), archive) {
		t.Fatalf("download: %d len=%d", rec.Code, rec.Body.Len())
	}
	if rec.Header().Get("Content-Type") != "application/gzip" || !strings.Contains(rec.Header().Get("Content-Disposition"), "code-review.tar.gz") {
		t.Fatalf("download headers: %+v", rec.Header())
	}
	// Re-upload with new content → digest moves → nudge.
	rev := f.heartbeat(t, "alice", nil)["resources_rev"].(string)
	f.heartbeat(t, "alice", map[string]any{"resources_rev": rev})
	f.uploadSkill(t, admin, skillArchive(t, md+"v2\n", nil), "团队规范")
	hb := f.heartbeat(t, "alice", nil)
	if hb["resources_update"] != true || hb["resources_rev"] == rev {
		t.Fatalf("re-upload did not nudge: %+v", hb)
	}
	_, out = f.doCookie(t, "GET", "/admin/resources/skill/code-review", nil, admin)
	if r := out["resource"].(map[string]any); r["description"] != "团队规范" || len(out["revisions"].([]any)) != 2 {
		t.Fatalf("admin view after re-upload: %+v", out)
	}
}
