package web

import (
	"net/http/httptest"
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
	// index.html must reference the JS/CSS the handler serves.
	req := httptest.NewRequest("GET", "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "/app.js") || !strings.Contains(body, "/style.css") {
		t.Fatal("index.html missing asset references")
	}
	// Unknown path → 404, not a panic.
	req = httptest.NewRequest("GET", "/nope.js", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 {
		t.Fatalf("unknown path: %d", rec.Code)
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
	// static $('...') refs (dynamic 'tab-'+t handled below)
	for _, ref := range []string{
		"pw", "loginbtn", "logoutbtn", "dbinfo", "ver",
		"refreshPeers", "peerSummary", "peerTable",
		"pType", "pPeer", "genPrompt", "promptOut", "inviteCode",
		"copyPrompt", "promptText", "rPeer", "genReconf", "reconfText",
		"refreshThreads", "threadTable", "threadTitle", "threadMsgs",
		"qSearch", "qBtn", "searchOut",
		"aActor", "aAction", "refreshAudit", "exportAudit", "auditTable",
		"refreshConfig", "configOut",
		"refreshTokens", "tokenTable", "rotPeer", "rotLabel", "rotBtn", "rotOut",
	} {
		if !strings.Contains(js, "$('"+ref+"')") {
			t.Fatalf("app.js no longer references #%s (test stale?)", ref)
		}
		if !ids[ref] {
			t.Fatalf("index.html missing id=%q used by app.js", ref)
		}
	}
	for _, tab := range []string{"members", "prompts", "threads", "audit"} {
		if !ids["tab-"+tab] {
			t.Fatalf("missing tab-%s", tab)
		}
	}
}
