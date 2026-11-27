package bing

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

type fakeSecrets struct {
	value string
	err   error
}

func (f fakeSecrets) Resolve(ref string) (string, error) {
	if f.err != nil {
		return "", f.err
	}
	return f.value, nil
}

type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

type mockBing struct {
	t         *testing.T
	mu        sync.Mutex
	reads     int
	lastRead  string
	lastQuery url.Values
	writes    []recordedRequest
	failNext  int
}

func (m *mockBing) handler(w http.ResponseWriter, r *http.Request) {
	if got := r.URL.Query().Get("apikey"); got != "test-key" {
		m.t.Errorf("missing/wrong apikey query")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.failNext > 0 {
		m.failNext--
		http.Error(w, "upstream secret=test-key", http.StatusInternalServerError)
		return
	}
	if r.Method == http.MethodPost {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		m.writes = append(m.writes, recordedRequest{method: r.Method, path: r.URL.Path, body: body})
		_ = json.NewEncoder(w).Encode(map[string]any{"d": nil})
		return
	}
	m.reads++
	m.lastRead = r.URL.Path
	m.lastQuery = r.URL.Query()
	switch r.URL.Path {
	case "/webmaster/api.svc/json/GetUserSites":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Url": "https://x.test/", "IsVerified": true},
			map[string]any{"Url": "https://bad.test/", "IsVerified": false},
		}})
	case "/webmaster/api.svc/json/GetUrlSubmissionQuota":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{"DailyQuota": 5, "MonthlyQuota": 100}})
	case "/webmaster/api.svc/json/GetQueryStats":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Query": "像素画", "Clicks": 2, "Impressions": 10, "Date": "/Date(1700000000000)/"},
		}})
	case "/webmaster/api.svc/json/GetPageStats":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Query": "https://x.test/a", "Clicks": 1, "Impressions": 3},
		}})
	case "/webmaster/api.svc/json/GetFeeds":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Url": "https://x.test/sitemap.xml"},
		}})
	case "/webmaster/api.svc/json/GetBlockedUrls":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Date": "/Date(1700000000000)/", "EntityType": 0, "RequestType": 1, "Url": "https://x.test/private"},
			map[string]any{"Date": "/Date(1700000000000)/", "EntityType": 1, "RequestType": 1, "Url": "https://x.test/dir/"},
		}})
	case "/webmaster/api.svc/json/GetKeyword":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Keyword": "image resizer", "Impressions": 5},
		}})
	case "/webmaster/api.svc/json/GetKeywordStats":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{"Keyword": "image resizer", "Clicks": 1},
		}})
	case "/webmaster/api.svc/json/GetRelatedKeywords":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{map[string]any{"Keyword": "resize image"}}})
	default:
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{}})
	}
}

func newTestService(t *testing.T) (*Service, *mockBing) {
	t.Helper()
	mock := &mockBing{t: t}
	srv := httptest.NewServer(http.HandlerFunc(mock.handler))
	t.Cleanup(srv.Close)
	svc := New(Options{
		CredentialRef: "secret:TEST_BING", Credentials: fakeSecrets{value: "test-key"},
		Upstream: mcpgateway.NewUpstream(5 * time.Second), BaseURL: srv.URL + "/webmaster/api.svc/json",
		MaxRows: 1000, BatchCap: 2, Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	return svc, mock
}

func TestBingToolSurface(t *testing.T) {
	svc, _ := newTestService(t)
	tools := svc.Tools()
	if len(tools) != 20 {
		t.Fatalf("tools = %d", len(tools))
	}
	risk := map[string]string{}
	readOnly := map[string]bool{}
	for _, tool := range tools {
		risk[tool.Name] = tool.Risk
		readOnly[tool.Name] = tool.ReadOnly
	}
	for _, name := range []string{ToolAddBlockedURL, ToolRemoveBlockedURL} {
		if risk[name] != mcpgateway.RiskHigh {
			t.Fatalf("%s risk = %q", name, risk[name])
		}
	}
	for _, name := range []string{ToolSubmitURL, ToolSubmitURLBatch, ToolSubmitFeed, ToolRemoveFeed} {
		if risk[name] != mcpgateway.RiskLow {
			t.Fatalf("%s risk = %q", name, risk[name])
		}
	}
	for _, name := range []string{ToolGetURLSubmissionQuota, ToolGetBlockedURLs} {
		if risk[name] != mcpgateway.RiskRead || !readOnly[name] {
			t.Fatalf("%s read metadata = %q/%v", name, risk[name], readOnly[name])
		}
	}
	if svc.Prefix() != "bing" || len(svc.CredentialRefs()) != 1 {
		t.Fatalf("prefix/refs: %q %+v", svc.Prefix(), svc.CredentialRefs())
	}
}

func TestBingReadsNormalizeAndTruncate(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolListSites})
	if err != nil {
		t.Fatalf("list sites: %v", err)
	}
	if len(res.Data.(map[string]any)["sites"].([]map[string]any)) != 2 {
		t.Fatalf("sites: %+v", res.Data)
	}
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolQueryStats, Params: json.RawMessage(`{"site":"https://x.test/"}`)})
	if err != nil {
		t.Fatalf("query stats: %v", err)
	}
	rows := res.Data.(map[string]any)["rows"].([]map[string]any)
	if rows[0]["Date"] != "2023-11-14" {
		t.Fatalf("date not normalized: %+v", rows[0])
	}
	totals := res.Data.(map[string]any)["totals"].(map[string]any)
	if totals["clicks"].(float64) != 2 || totals["impressions"].(float64) != 10 {
		t.Fatalf("totals: %+v", totals)
	}
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolPageStats, Params: json.RawMessage(`{"site":"https://x.test/"}`)})
	if err != nil {
		t.Fatalf("page stats: %v", err)
	}
	rows = res.Data.(map[string]any)["rows"].([]map[string]any)
	if rows[0]["Page"] != "https://x.test/a" {
		t.Fatalf("Query not renamed to Page: %+v", rows[0])
	}
	if _, ok := rows[0]["Query"]; ok {
		t.Fatalf("Query field leaked: %+v", rows[0])
	}
	// Unverified site is rejected by the mock's GetUserSites list.
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolQueryStats, Params: json.RawMessage(`{"site":"https://bad.test/"}`)}); err == nil {
		t.Fatal("unverified site accepted")
	}
	// Keyword research does not require a site.
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolKeyword, Params: json.RawMessage(`{"q":"image resizer"}`)})
	if err != nil {
		t.Fatalf("keyword: %v", err)
	}
	if len(res.Data.(map[string]any)["rows"].([]any)) != 1 {
		t.Fatalf("keyword rows: %+v", res.Data)
	}
}

func TestBingP0Reads(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetURLSubmissionQuota, Params: json.RawMessage(`{"site":"https://x.test/"}`)})
	if err != nil {
		t.Fatalf("quota: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["daily_quota"].(int) != 5 || data["monthly_quota"].(int) != 100 {
		t.Fatalf("quota payload: %+v", data)
	}
	mock.mu.Lock()
	path, site := mock.lastRead, mock.lastQuery.Get("siteUrl")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetUrlSubmissionQuota") || site != "https://x.test/" {
		t.Fatalf("quota passthrough path=%q site=%q", path, site)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetBlockedURLs, Params: json.RawMessage(`{"site":"https://x.test/"}`)})
	if err != nil {
		t.Fatalf("blocked urls: %v", err)
	}
	data = res.Data.(map[string]any)
	rows := data["rows"].([]map[string]any)
	blocked := data["blocked_urls"].([]map[string]any)
	if len(rows) != 2 || len(blocked) != 2 {
		t.Fatalf("blocked rows: %+v", data)
	}
	if rows[0]["Date"] != "2023-11-14" || rows[0]["Url"] != "https://x.test/private" {
		t.Fatalf("blocked row not normalized: %+v", rows[0])
	}
	mock.mu.Lock()
	path, site = mock.lastRead, mock.lastQuery.Get("siteUrl")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetBlockedUrls") || site != "https://x.test/" {
		t.Fatalf("blocked passthrough path=%q site=%q", path, site)
	}
}

func TestBingRemoveBlockedURL(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRemoveBlockedURL, Params: json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/private"}`)})
	if err != nil {
		t.Fatalf("remove blocked: %v", err)
	}
	if res.Data.(map[string]any)["removed"] != true {
		t.Fatalf("remove result: %+v", res.Data)
	}
	mock.mu.Lock()
	last := mock.writes[len(mock.writes)-1]
	mock.mu.Unlock()
	if !strings.HasSuffix(last.path, "/RemoveBlockedUrl") {
		t.Fatalf("upstream method = %q", last.path)
	}
	blocked, _ := last.body["blockedUrl"].(map[string]any)
	if blocked["Url"] != "https://x.test/private" || int(blocked["RequestType"].(float64)) != 0 || int(blocked["EntityType"].(float64)) != 0 {
		t.Fatalf("remove body: %+v", last.body)
	}

	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRemoveBlockedURL, Params: json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/dir/","entity_type":"directory"}`)}); err != nil {
		t.Fatalf("remove directory: %v", err)
	}
	mock.mu.Lock()
	last = mock.writes[len(mock.writes)-1]
	mock.mu.Unlock()
	blocked, _ = last.body["blockedUrl"].(map[string]any)
	if int(blocked["EntityType"].(float64)) != 1 {
		t.Fatalf("directory EntityType = %v, want 1", blocked["EntityType"])
	}
	if err := svc.validateRemoveBlockedURL(json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/"}`)); err == nil {
		t.Fatal("removing the site root accepted")
	}
	if err := svc.validateRemoveBlockedURL(json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://other.test/x"}`)); err == nil {
		t.Fatal("removing a foreign URL accepted")
	}
}

func TestBingP0CredentialMissing(t *testing.T) {
	missing := New(Options{
		CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing},
		MaxRows: 10,
	})
	calls := []mcpgateway.Call{
		{Tool: ToolGetURLSubmissionQuota, Params: json.RawMessage(`{"site":"https://x.test/"}`)},
		{Tool: ToolGetBlockedURLs, Params: json.RawMessage(`{"site":"https://x.test/"}`)},
		{Tool: ToolRemoveBlockedURL, Params: json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/private"}`)},
	}
	for _, call := range calls {
		_, err := missing.Handle(context.Background(), call)
		if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
			t.Fatalf("%s missing credential: %v", call.Tool, err)
		}
	}
}

func TestBingWrites(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()
	// submit_url
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolSubmitURL, Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a"}`)})
	if err != nil {
		t.Fatalf("submit url: %v", err)
	}
	if res.Data.(map[string]any)["submitted"].(int) != 1 {
		t.Fatalf("submit result: %+v", res.Data)
	}
	// submit_url_batch cap 2.
	bad := json.RawMessage(`{"site":"https://x.test/","urls":["https://x.test/a","https://x.test/b","https://x.test/c"]}`)
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolSubmitURLBatch, Params: bad}); err == nil {
		t.Fatal("batch over cap accepted")
	}
	good := json.RawMessage(`{"site":"https://x.test/","urls":["https://x.test/a","https://x.test/a","https://x.test/b"]}`)
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolSubmitURLBatch, Params: good}); err != nil {
		t.Fatalf("batch: %v", err)
	}
	// submit_feed and remove_feed.
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolSubmitFeed, Params: json.RawMessage(`{"site":"https://x.test/","feedUrl":"https://x.test/sitemap.xml"}`)}); err != nil {
		t.Fatalf("submit feed: %v", err)
	}
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRemoveFeed, Params: json.RawMessage(`{"site":"https://x.test/","feedUrl":"https://x.test/sitemap.xml"}`)}); err != nil {
		t.Fatalf("remove feed: %v", err)
	}
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRemoveFeed, Params: json.RawMessage(`{"site":"https://x.test/","feedUrl":"https://x.test/not-there.xml"}`)}); err == nil {
		t.Fatal("remove of unknown feed accepted")
	}
	// add_blocked_url high risk (approval is enforced by the core, not here).
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolAddBlockedURL, Params: json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/private"}`)}); err != nil {
		t.Fatalf("add blocked: %v", err)
	}
	mock.mu.Lock()
	defer mock.mu.Unlock()
	var sawBlocked bool
	for _, req := range mock.writes {
		if strings.HasSuffix(req.path, "/AddBlockedUrl") {
			blocked, _ := req.body["blockedUrl"].(map[string]any)
			if blocked["Url"] != "https://x.test/private" {
				t.Fatalf("blocked body: %+v", req.body)
			}
			sawBlocked = true
		}
	}
	if !sawBlocked {
		t.Fatalf("AddBlockedUrl not called: %+v", mock.writes)
	}
}

func TestBingCredentialAndUpstreamErrorsDoNotLeakKey(t *testing.T) {
	svc, mock := newTestService(t)
	mock.mu.Lock()
	mock.failNext = 2 // DoRead retries once; both attempts fail
	mock.mu.Unlock()
	_, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: ToolListSites})
	if err == nil {
		t.Fatal("expected upstream error")
	}
	if strings.Contains(err.Error(), "test-key") {
		t.Fatalf("upstream error leaked key: %v", err)
	}
	missing := New(Options{CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing}, MaxRows: 10})
	_, err = missing.Handle(context.Background(), mcpgateway.Call{Tool: ToolListSites})
	if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
		t.Fatalf("missing credential: %v", err)
	}
}

func TestBingHardeningSiteRootDirectoryAndTotals(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()
	// Site root cannot be blocked.
	if err := svc.validateAddBlockedURL(json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/"}`)); err == nil {
		t.Fatal("blocking site root accepted")
	}
	// entity_type directory maps to EntityType=1.
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolAddBlockedURL, Params: json.RawMessage(`{"site":"https://x.test/","blockedUrl":"https://x.test/dir/","entity_type":"directory"}`)}); err != nil {
		t.Fatalf("directory block: %v", err)
	}
	mock.mu.Lock()
	last := mock.writes[len(mock.writes)-1].body["blockedUrl"].(map[string]any)
	mock.mu.Unlock()
	if int(last["EntityType"].(float64)) != 1 {
		t.Fatalf("directory EntityType = %v, want 1", last["EntityType"])
	}
}
