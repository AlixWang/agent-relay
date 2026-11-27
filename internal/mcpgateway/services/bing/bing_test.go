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
		if strings.HasSuffix(r.URL.Path, "/GetChildrenUrlInfo") {
			// GetChildrenUrlInfo is the one read method documented as POST.
			_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
				map[string]any{
					"__type": "UrlInfo:#Microsoft.Bing.Webmaster.Api", "AnchorCount": 50,
					"DiscoveryDate": "/Date(1315349995284-0700)/", "DocumentSize": 0,
					"HttpStatus": 0, "IsPage": false,
					"LastCrawledDate":    "/Date(1316213995284-0700)/",
					"TotalChildUrlCount": 100, "Url": "https://x.test/blog/",
				},
			}})
			return
		}
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
	case "/webmaster/api.svc/json/GetUrlInfo":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{
			"__type": "UrlInfo:#Microsoft.Bing.Webmaster.Api", "AnchorCount": 50,
			"DiscoveryDate": "/Date(1315349995266-0700)/", "DocumentSize": 0,
			"HttpStatus": 200, "IsPage": true,
			"LastCrawledDate":    "/Date(1316213995266-0700)/",
			"TotalChildUrlCount": 100, "Url": "https://x.test/a",
		}})
	case "/webmaster/api.svc/json/GetUrlTrafficInfo":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{
			"__type": "UrlTrafficInfo:#Microsoft.Bing.Webmaster.Api",
			"Clicks": 10, "Impressions": 100, "IsPage": true, "Url": "https://x.test/a",
		}})
	case "/webmaster/api.svc/json/GetQueryPageDetailStats":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{
				"__type": "DetailedQueryStats:#Microsoft.Bing.Webmaster.Api",
				"Clicks": 15, "Date": "/Date(1316156400000-0700)/",
				"Impressions": 100, "Position": 5,
			},
		}})
	case "/webmaster/api.svc/json/GetQueryTrafficStats":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{
				"__type": "RankAndTrafficStats:#Microsoft.Bing.Webmaster.Api",
				"Clicks": 15, "Date": "/Date(1316156400000-0700)/", "Impressions": 100,
			},
		}})
	case "/webmaster/api.svc/json/GetChildrenUrlTrafficInfo":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{
				"__type": "UrlTrafficInfo:#Microsoft.Bing.Webmaster.Api",
				"Clicks": 10, "Impressions": 100, "IsPage": false, "Url": "https://x.test/blog/",
			},
		}})
	case "/webmaster/api.svc/json/GetLinkCounts":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{
			"__type": "LinkCounts:#Microsoft.Bing.Webmaster.Api",
			"Links": []any{
				map[string]any{"__type": "LinkCount:#Microsoft.Bing.Webmaster.Api", "Count": 14, "Url": "https://x.test/page1"},
			},
			"TotalPages": 3,
		}})
	case "/webmaster/api.svc/json/GetUrlLinks":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": map[string]any{
			"__type": "LinkDetails:#Microsoft.Bing.Webmaster.Api",
			"Details": []any{
				map[string]any{"__type": "LinkDetail:#Microsoft.Bing.Webmaster.Api", "AnchorText": "link", "Url": "https://x.test/page1"},
			},
			"TotalPages": 3,
		}})
	case "/webmaster/api.svc/json/GetFeedDetails":
		_ = json.NewEncoder(w).Encode(map[string]any{"d": []any{
			map[string]any{
				"__type": "Feed:#Microsoft.Bing.Webmaster.Api", "Compressed": false,
				"FileSize": 1024, "LastCrawled": "/Date(1315781995063-0700)/",
				"Status": "Success", "Submitted": "/Date(1316213995063-0700)/",
				"Type": "Sitemap", "Url": "https://x.test/sitemap.xml", "UrlCount": 1023,
			},
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
	if len(tools) != 29 {
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
	for _, name := range []string{ToolGetURLSubmissionQuota, ToolGetBlockedURLs, ToolGetURLInfo, ToolGetURLTrafficInfo} {
		if risk[name] != mcpgateway.RiskRead || !readOnly[name] {
			t.Fatalf("%s read metadata = %q/%v", name, risk[name], readOnly[name])
		}
	}
	for _, name := range []string{
		ToolQueryPageDetailStats, ToolQueryTrafficStats, ToolChildrenURLInfo,
		ToolChildrenURLTrafficInfo, ToolLinkCounts, ToolURLLinks, ToolFeedDetails,
	} {
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

func TestBingP1URLReads(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()
	res, err := svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolGetURLInfo,
		Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a"}`),
	})
	if err != nil {
		t.Fatalf("url info: %v", err)
	}
	data := res.Data.(map[string]any)
	if data["site"] != "https://x.test/" || data["requested_url"] != "https://x.test/a" {
		t.Fatalf("url info context: %+v", data)
	}
	if data["Url"] != "https://x.test/a" || data["HttpStatus"].(float64) != 200 || data["IsPage"] != true {
		t.Fatalf("url info payload: %+v", data)
	}
	if data["AnchorCount"].(float64) != 50 || data["TotalChildUrlCount"].(float64) != 100 {
		t.Fatalf("url info counts: %+v", data)
	}
	if data["LastCrawledDate"] != "2011-09-16" || data["DiscoveryDate"] != "2011-09-06" {
		t.Fatalf("url info dates not normalized: %+v", data)
	}
	if res.RowCount != 1 || res.UpstreamStatus != http.StatusOK {
		t.Fatalf("url info result metadata: %+v", res)
	}
	mock.mu.Lock()
	path, site, target := mock.lastRead, mock.lastQuery.Get("siteUrl"), mock.lastQuery.Get("url")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetUrlInfo") || site != "https://x.test/" || target != "https://x.test/a" {
		t.Fatalf("url info passthrough path=%q site=%q url=%q", path, site, target)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolGetURLTrafficInfo,
		Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a"}`),
	})
	if err != nil {
		t.Fatalf("url traffic info: %v", err)
	}
	data = res.Data.(map[string]any)
	if data["Url"] != "https://x.test/a" || data["Clicks"].(float64) != 10 || data["Impressions"].(float64) != 100 {
		t.Fatalf("url traffic payload: %+v", data)
	}
	mock.mu.Lock()
	path, site, target = mock.lastRead, mock.lastQuery.Get("siteUrl"), mock.lastQuery.Get("url")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetUrlTrafficInfo") || site != "https://x.test/" || target != "https://x.test/a" {
		t.Fatalf("url traffic passthrough path=%q site=%q url=%q", path, site, target)
	}

	if err := svc.validateURLRead(json.RawMessage(`{"site":"https://x.test/","url":"https://other.test/a"}`)); err == nil {
		t.Fatal("foreign URL accepted")
	}
}

func TestBingP2P3Reads(t *testing.T) {
	svc, mock := newTestService(t)
	ctx := context.Background()

	// GetQueryPageDetailStats: query x page cross-section.
	res, err := svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolQueryPageDetailStats,
		Params: json.RawMessage(`{"site":"https://x.test/","query":"像素画","page":"https://x.test/a"}`),
	})
	if err != nil {
		t.Fatalf("query page detail: %v", err)
	}
	data := res.Data.(map[string]any)
	rows := data["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["Clicks"].(float64) != 15 || rows[0]["Impressions"].(float64) != 100 ||
		rows[0]["Position"].(float64) != 5 || rows[0]["Date"] != "2011-09-16" {
		t.Fatalf("query page detail rows: %+v", rows)
	}
	if data["query"] != "像素画" || data["page"] != "https://x.test/a" {
		t.Fatalf("query page detail context: %+v", data)
	}
	mock.mu.Lock()
	path, site, query, page := mock.lastRead, mock.lastQuery.Get("siteUrl"), mock.lastQuery.Get("query"), mock.lastQuery.Get("page")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetQueryPageDetailStats") || site != "https://x.test/" ||
		query != "像素画" || page != "https://x.test/a" {
		t.Fatalf("query page detail passthrough path=%q site=%q query=%q page=%q", path, site, query, page)
	}

	// GetQueryTrafficStats: one query's daily series.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolQueryTrafficStats,
		Params: json.RawMessage(`{"site":"https://x.test/","query":"image resizer"}`),
	})
	if err != nil {
		t.Fatalf("query traffic: %v", err)
	}
	rows = res.Data.(map[string]any)["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["Clicks"].(float64) != 15 || rows[0]["Date"] != "2011-09-16" {
		t.Fatalf("query traffic rows: %+v", rows)
	}
	mock.mu.Lock()
	path, site, query = mock.lastRead, mock.lastQuery.Get("siteUrl"), mock.lastQuery.Get("query")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetQueryTrafficStats") || site != "https://x.test/" || query != "image resizer" {
		t.Fatalf("query traffic passthrough path=%q site=%q query=%q", path, site, query)
	}

	// GetChildrenUrlInfo: POST with the documented siteUrl/url/page/
	// filterProperties body, returning UrlInfo rows.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolChildrenURLInfo,
		Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/blog/","page":2}`),
	})
	if err != nil {
		t.Fatalf("children url info: %v", err)
	}
	rows = res.Data.(map[string]any)["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["AnchorCount"].(float64) != 50 || rows[0]["IsPage"] != false ||
		rows[0]["TotalChildUrlCount"].(float64) != 100 || rows[0]["LastCrawledDate"] != "2011-09-16" {
		t.Fatalf("children url info rows: %+v", rows)
	}
	mock.mu.Lock()
	var posted recordedRequest
	for _, req := range mock.writes {
		if strings.HasSuffix(req.path, "/GetChildrenUrlInfo") {
			posted = req
		}
	}
	mock.mu.Unlock()
	if posted.method != http.MethodPost {
		t.Fatalf("children url info method = %q, want POST", posted.method)
	}
	if posted.body["siteUrl"] != "https://x.test/" || posted.body["url"] != "https://x.test/blog/" ||
		posted.body["page"].(float64) != 2 {
		t.Fatalf("children url info body: %+v", posted.body)
	}
	filters, _ := posted.body["filterProperties"].(map[string]any)
	if filters == nil || filters["CrawlDateFilter"].(float64) != 1 || filters["HttpCodeFilters"].(float64) != 0 {
		t.Fatalf("children url info filters: %+v", posted.body)
	}

	// GetChildrenUrlTrafficInfo: GET siteUrl/url/page.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolChildrenURLTrafficInfo,
		Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/blog/","page":1}`),
	})
	if err != nil {
		t.Fatalf("children url traffic info: %v", err)
	}
	rows = res.Data.(map[string]any)["rows"].([]map[string]any)
	if len(rows) != 1 || rows[0]["Clicks"].(float64) != 10 || rows[0]["IsPage"] != false {
		t.Fatalf("children url traffic rows: %+v", rows)
	}
	mock.mu.Lock()
	path, site, page = mock.lastRead, mock.lastQuery.Get("siteUrl"), mock.lastQuery.Get("page")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetChildrenUrlTrafficInfo") || site != "https://x.test/" || page != "1" {
		t.Fatalf("children url traffic passthrough path=%q site=%q page=%q", path, site, page)
	}

	// GetLinkCounts: object with Links + TotalPages.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolLinkCounts,
		Params: json.RawMessage(`{"site":"https://x.test/","page":1}`),
	})
	if err != nil {
		t.Fatalf("link counts: %v", err)
	}
	data = res.Data.(map[string]any)
	links := data["links"].([]map[string]any)
	if data["total_pages"].(int) != 3 || data["has_more"] != true || len(links) != 1 ||
		links[0]["Count"].(float64) != 14 || links[0]["Url"] != "https://x.test/page1" {
		t.Fatalf("link counts: %+v", data)
	}
	mock.mu.Lock()
	path, page = mock.lastRead, mock.lastQuery.Get("page")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetLinkCounts") || page != "1" {
		t.Fatalf("link counts passthrough path=%q page=%q", path, page)
	}

	// GetUrlLinks: object with Details + TotalPages; the upstream parameter
	// is named `link`, not `url`.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolURLLinks,
		Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a","page":2}`),
	})
	if err != nil {
		t.Fatalf("url links: %v", err)
	}
	data = res.Data.(map[string]any)
	links = data["links"].([]map[string]any)
	if data["total_pages"].(int) != 3 || data["has_more"] != false || len(links) != 1 ||
		links[0]["AnchorText"] != "link" {
		t.Fatalf("url links: %+v", data)
	}
	mock.mu.Lock()
	path, link, page := mock.lastRead, mock.lastQuery.Get("link"), mock.lastQuery.Get("page")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetUrlLinks") || link != "https://x.test/a" || page != "2" {
		t.Fatalf("url links passthrough path=%q link=%q page=%q", path, link, page)
	}

	// GetFeedDetails: sitemap index children.
	res, err = svc.Handle(ctx, mcpgateway.Call{
		Tool:   ToolFeedDetails,
		Params: json.RawMessage(`{"site":"https://x.test/","feedUrl":"https://x.test/sitemap.xml"}`),
	})
	if err != nil {
		t.Fatalf("feed details: %v", err)
	}
	feeds := res.Data.(map[string]any)["feeds"].([]map[string]any)
	if len(feeds) != 1 || feeds[0]["Compressed"] != false || feeds[0]["FileSize"].(float64) != 1024 ||
		feeds[0]["Status"] != "Success" || feeds[0]["Type"] != "Sitemap" ||
		feeds[0]["UrlCount"].(float64) != 1023 || feeds[0]["LastCrawled"] != "2011-09-11" {
		t.Fatalf("feed details: %+v", feeds)
	}
	mock.mu.Lock()
	path, feedURL := mock.lastRead, mock.lastQuery.Get("feedUrl")
	mock.mu.Unlock()
	if !strings.HasSuffix(path, "/GetFeedDetails") || feedURL != "https://x.test/sitemap.xml" {
		t.Fatalf("feed details passthrough path=%q feedUrl=%q", path, feedURL)
	}
}

func TestBingP2P3Validation(t *testing.T) {
	svc, _ := newTestService(t)
	cases := []struct {
		tool   string
		params string
	}{
		{ToolQueryPageDetailStats, `{"site":"https://x.test/","query":"","page":"https://x.test/a"}`},
		{ToolQueryPageDetailStats, `{"site":"https://x.test/","query":"q","page":"https://other.test/a"}`},
		{ToolQueryTrafficStats, `{"site":"https://x.test/","query":"  "}`},
		{ToolChildrenURLInfo, `{"site":"https://x.test/","url":"https://other.test/blog/"}`},
		{ToolChildrenURLInfo, `{"site":"https://x.test/","url":"https://x.test/blog/","page":70000}`},
		{ToolChildrenURLTrafficInfo, `{"site":"https://x.test/","url":"https://x.test/blog/","page":-1}`},
		{ToolLinkCounts, `{"site":"https://x.test/","page":-1}`},
		{ToolURLLinks, `{"site":"https://x.test/","url":"https://other.test/a"}`},
		{ToolFeedDetails, `{"site":"https://x.test/","feedUrl":"https://other.test/sitemap.xml"}`},
	}
	for _, tc := range cases {
		if _, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: tc.tool, Params: json.RawMessage(tc.params)}); err == nil {
			t.Fatalf("%s accepted invalid params %s", tc.tool, tc.params)
		}
	}
}

func TestBingP2P3CredentialMissing(t *testing.T) {
	missing := New(Options{
		CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing},
		MaxRows: 10,
	})
	calls := []mcpgateway.Call{
		{Tool: ToolQueryPageDetailStats, Params: json.RawMessage(`{"site":"https://x.test/","query":"q","page":"https://x.test/a"}`)},
		{Tool: ToolQueryTrafficStats, Params: json.RawMessage(`{"site":"https://x.test/","query":"q"}`)},
		{Tool: ToolChildrenURLInfo, Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/blog/"}`)},
		{Tool: ToolChildrenURLTrafficInfo, Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/blog/"}`)},
		{Tool: ToolLinkCounts, Params: json.RawMessage(`{"site":"https://x.test/"}`)},
		{Tool: ToolURLLinks, Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a"}`)},
		{Tool: ToolFeedDetails, Params: json.RawMessage(`{"site":"https://x.test/","feedUrl":"https://x.test/sitemap.xml"}`)},
	}
	for _, call := range calls {
		_, err := missing.Handle(context.Background(), call)
		if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
			t.Fatalf("%s missing credential: %v", call.Tool, err)
		}
	}
}

func TestBingP1URLCredentialMissing(t *testing.T) {
	missing := New(Options{
		CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing},
		MaxRows: 10,
	})
	for _, tool := range []string{ToolGetURLInfo, ToolGetURLTrafficInfo} {
		_, err := missing.Handle(context.Background(), mcpgateway.Call{
			Tool:   tool,
			Params: json.RawMessage(`{"site":"https://x.test/","url":"https://x.test/a"}`),
		})
		if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
			t.Fatalf("%s missing credential: %v", tool, err)
		}
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
