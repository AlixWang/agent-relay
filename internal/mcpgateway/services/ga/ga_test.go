package ga

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
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

const testADC = `{"type":"authorized_user","client_id":"test-client","client_secret":"test-secret","refresh_token":"test-refresh"}`

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

type mockGA struct {
	t *testing.T

	mu           sync.Mutex
	tokenCalls   int
	apiCalls     int
	tokenStatus  int
	lastReport   map[string]any
	lastRealtime map[string]any
	lastConv     map[string]any
	lastFunnel   map[string]any
}

func (m *mockGA) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		m.t.Errorf("token method = %s", r.Method)
	}
	_ = r.ParseForm()
	if r.FormValue("grant_type") != "refresh_token" ||
		r.FormValue("client_id") != "test-client" ||
		r.FormValue("client_secret") != "test-secret" ||
		r.FormValue("refresh_token") != "test-refresh" {
		m.t.Errorf("token form: %+v", r.Form)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.tokenCalls++
	if m.tokenStatus != 0 {
		http.Error(w, "denied", m.tokenStatus)
		return
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "test-access-token", "expires_in": 3600,
	})
}

func decodeBody(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	return body
}

func (m *mockGA) api(w http.ResponseWriter, r *http.Request) {
	if got := r.Header.Get("Authorization"); got != "Bearer test-access-token" {
		m.t.Errorf("Authorization = %q", got)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.apiCalls++
	switch {
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/accountSummaries":
		if r.URL.Query().Get("pageSize") != "200" {
			m.t.Errorf("pageSize = %q", r.URL.Query().Get("pageSize"))
		}
		switch r.URL.Query().Get("pageToken") {
		case "":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accountSummaries": []any{
					map[string]any{
						"account": "accounts/1", "displayName": "Main",
						"propertySummaries": []any{
							map[string]any{"property": "properties/470553943", "displayName": "Example"},
						},
					},
				},
				"nextPageToken": "page-2",
			})
		case "page-2":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"accountSummaries": []any{
					map[string]any{"account": "accounts/2", "displayName": "Second"},
				},
			})
		default:
			m.t.Errorf("unexpected pageToken %q", r.URL.Query().Get("pageToken"))
		}
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/properties/470553943":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"name": "properties/470553943", "displayName": "Example",
			"timeZone": "UTC", "currencyCode": "USD", "propertyType": "PROPERTY_TYPE_ORDINARY",
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/properties/470553943/googleAdsLinks":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"googleAdsLinks": []any{
				map[string]any{
					"name": "properties/470553943/googleAdsLinks/1", "customerId": "123-456-7890",
					"creatorEmailAddress": "owner@example.com", "adsPersonalizationEnabled": true,
				},
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1alpha/properties/470553943/reportingDataAnnotations":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"reportingDataAnnotations": []any{
				map[string]any{
					"name": "properties/470553943/reportingDataAnnotations/1", "title": "Launch",
					"color": "BLUE", "annotationDate": map[string]any{"year": 2026, "month": 11, "day": 25},
				},
			},
		})
	case r.Method == http.MethodGet && r.URL.Path == "/v1beta/properties/470553943/metadata":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dimensions": []any{
				map[string]any{"apiName": "customEvent:foo", "uiName": "Foo", "customDefinition": true},
				map[string]any{"apiName": "date", "customDefinition": false},
			},
			"metrics": []any{
				map[string]any{"apiName": "customMetric:bar", "uiName": "Bar", "customDefinition": true},
			},
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1beta/properties/470553943:runReport":
		m.lastReport = decodeBody(m.t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dimensionHeaders": []any{map[string]any{"name": "date"}},
			"metricHeaders":    []any{map[string]any{"name": "sessions", "type": "TYPE_INTEGER"}},
			"rows": []any{
				map[string]any{"dimensionValues": []any{map[string]any{"value": "2026-11-23"}}, "metricValues": []any{map[string]any{"value": "1"}}},
				map[string]any{"dimensionValues": []any{map[string]any{"value": "2026-11-24"}}, "metricValues": []any{map[string]any{"value": "2"}}},
				map[string]any{"dimensionValues": []any{map[string]any{"value": "2026-11-25"}}, "metricValues": []any{map[string]any{"value": "3"}}},
			},
			"rowCount": 3, "kind": "analyticsData#runReport",
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1beta/properties/470553943:runRealtimeReport":
		m.lastRealtime = decodeBody(m.t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"dimensionHeaders": []any{map[string]any{"name": "city"}},
			"metricHeaders":    []any{map[string]any{"name": "activeUsers"}},
			"rows":             []any{map[string]any{"dimensionValues": []any{map[string]any{"value": "Tokyo"}}}},
			"rowCount":         1,
		})
	case r.Method == http.MethodPost && r.URL.Path == "/v1alpha/properties/470553943:runReport":
		m.lastConv = decodeBody(m.t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{"rowCount": 0, "kind": "analyticsData#runReport"})
	case r.Method == http.MethodPost && r.URL.Path == "/v1alpha/properties/470553943:runFunnelReport":
		m.lastFunnel = decodeBody(m.t, r)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"funnelTable": map[string]any{
				"dimensionHeaders": []any{map[string]any{"name": "step"}},
				"metricHeaders":    []any{map[string]any{"name": "users"}},
				"rows": []any{
					map[string]any{"dimensionValues": []any{map[string]any{"value": "1"}}},
					map[string]any{"dimensionValues": []any{map[string]any{"value": "2"}}},
					map[string]any{"dimensionValues": []any{map[string]any{"value": "3"}}},
				},
			},
			"funnelVisualization": map[string]any{
				"rows": []any{map[string]any{"dimensionValues": []any{map[string]any{"value": "1"}}}},
			},
			"kind": "analyticsData#runFunnelReport",
		})
	default:
		m.t.Errorf("unexpected upstream request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}
}

func newTestGA(t *testing.T) (*Service, *mockGA, *testClock) {
	t.Helper()
	clock := &testClock{t: time.Unix(1_800_000_000, 0)}
	mock := &mockGA{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			mock.token(w, r)
			return
		}
		mock.api(w, r)
	}))
	t.Cleanup(srv.Close)
	svc := New(Options{
		CredentialRef: "secret:GA_ADSENSE_ADC", Credentials: fakeSecrets{value: testADC},
		Upstream:     mcpgateway.NewUpstream(5 * time.Second),
		AdminBaseURL: srv.URL + "/v1alpha", DataBaseURL: srv.URL + "/v1beta",
		DataAlphaBaseURL: srv.URL + "/v1alpha", TokenURL: srv.URL + "/token",
		MaxRows: 2, Now: clock.Now,
	})
	return svc, mock, clock
}

func TestGAToolSurfaceIsReadOnly(t *testing.T) {
	svc, _, _ := newTestGA(t)
	tools := svc.Tools()
	if len(tools) != 9 {
		t.Fatalf("tools = %d", len(tools))
	}
	want := ReadToolNames()
	got := map[string]bool{}
	for _, tool := range tools {
		got[tool.Name] = true
		if tool.Risk != mcpgateway.RiskRead || !tool.ReadOnly {
			t.Fatalf("tool %s is not read-only: %+v", tool.Name, tool)
		}
		if tool.Name == "create_property_annotation" {
			t.Fatal("write tool exposed")
		}
	}
	for _, name := range want {
		if !got[name] {
			t.Fatalf("missing tool %q", name)
		}
	}
	// The report schemas differ by endpoint: run_report has no conversion_spec,
	// conversions requires it, and realtime has neither date ranges nor currency.
	schemas := map[string]map[string]any{}
	for _, tool := range tools {
		var schema map[string]any
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatalf("%s schema: %v", tool.Name, err)
		}
		schemas[tool.Name] = schema
	}
	propsOf := func(name string) map[string]any {
		props, _ := schemas[name]["properties"].(map[string]any)
		return props
	}
	if _, ok := propsOf(ToolRunReport)["date_ranges"]; !ok {
		t.Fatal("run_report schema missing date_ranges")
	}
	if _, ok := propsOf(ToolRunReport)["conversion_spec"]; ok {
		t.Fatal("run_report must not accept conversion_spec")
	}
	if _, ok := propsOf(ToolRunConversionsReport)["conversion_spec"]; !ok {
		t.Fatal("conversions schema missing conversion_spec")
	}
	if _, ok := propsOf(ToolRunRealtimeReport)["date_ranges"]; ok {
		t.Fatal("realtime schema must not accept date_ranges")
	}
	if _, ok := propsOf(ToolRunRealtimeReport)["currency_code"]; ok {
		t.Fatal("realtime schema must not accept currency_code")
	}
	requiredOf := func(name string) map[string]bool {
		required, _ := schemas[name]["required"].([]any)
		out := map[string]bool{}
		for _, item := range required {
			out[item.(string)] = true
		}
		return out
	}
	if req := requiredOf(ToolRunConversionsReport); !req["conversion_spec"] || !req["date_ranges"] {
		t.Fatalf("conversions required fields: %+v", req)
	}
	if req := requiredOf(ToolRunFunnelReport); !req["property_id"] || !req["funnel_steps"] {
		t.Fatalf("funnel required fields: %+v", req)
	}

	if svc.ID() != "ga" || svc.Prefix() != "ga" || svc.DisplayName() != "Google Analytics" {
		t.Fatalf("service metadata: %s/%s/%s", svc.ID(), svc.Prefix(), svc.DisplayName())
	}
	refs := svc.CredentialRefs()
	if len(refs) != 1 || refs[0] != "secret:GA_ADSENSE_ADC" {
		t.Fatalf("credential refs: %+v", refs)
	}
}

func TestGAAdminAndMetadataReads(t *testing.T) {
	svc, mock, _ := newTestGA(t)
	ctx := context.Background()

	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetAccountSummaries})
	if err != nil {
		t.Fatalf("account summaries: %v", err)
	}
	accounts := res.Data.([]any)
	if len(accounts) != 2 {
		t.Fatalf("accounts = %d", len(accounts))
	}
	first := accounts[0].(map[string]any)
	if first["display_name"] != "Main" {
		t.Fatalf("account keys not snake_case: %+v", first)
	}
	summaries := first["property_summaries"].([]any)
	if len(summaries) != 1 {
		t.Fatalf("property_summaries = %+v", summaries)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetPropertyDetails, Params: json.RawMessage(`{"property_id":470553943}`)})
	if err != nil {
		t.Fatalf("property details: %v", err)
	}
	property := res.Data.(map[string]any)
	if property["display_name"] != "Example" || property["time_zone"] != "UTC" || property["currency_code"] != "USD" {
		t.Fatalf("property: %+v", property)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolListGoogleAdsLinks, Params: json.RawMessage(`{"property_id":"properties/470553943"}`)})
	if err != nil {
		t.Fatalf("ads links: %v", err)
	}
	links := res.Data.([]any)
	if len(links) != 1 || links[0].(map[string]any)["customer_id"] != "123-456-7890" {
		t.Fatalf("links: %+v", links)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolListPropertyAnnotations, Params: json.RawMessage(`{"property_id":"470553943"}`)})
	if err != nil {
		t.Fatalf("annotations: %v", err)
	}
	annotations := res.Data.([]any)
	if len(annotations) != 1 || annotations[0].(map[string]any)["annotation_date"] == nil {
		t.Fatalf("annotations: %+v", annotations)
	}

	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetCustomDimensionsMetrics, Params: json.RawMessage(`{"property_id":"470553943"}`)})
	if err != nil {
		t.Fatalf("metadata: %v", err)
	}
	meta := res.Data.(map[string]any)
	dims := meta["custom_dimensions"].([]any)
	metrics := meta["custom_metrics"].([]any)
	if len(dims) != 1 || len(metrics) != 1 {
		t.Fatalf("custom metadata: %+v", meta)
	}
	if dims[0].(map[string]any)["api_name"] != "customEvent:foo" {
		t.Fatalf("dimension keys: %+v", dims[0])
	}
	if mock.tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1 cached token", mock.tokenCalls)
	}
}

func TestGAReportAndRealtimePassthrough(t *testing.T) {
	svc, mock, _ := newTestGA(t)
	ctx := context.Background()

	reportParams := json.RawMessage(`{
		"property_id":"properties/470553943",
		"date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-25","name":"Nov"}],
		"dimensions":["date","country"],
		"metrics":["sessions","activeUsers"],
		"dimension_filter":{"filter":{"field_name":"country","string_filter":{"match_type":"EXACT","value":"US"}}},
		"metric_filter":{"filter":{"field_name":"sessions","numeric_filter":{"operation":"GREATER_THAN","value":{"int64_value":"10"}}}},
		"order_bys":[{"metric":{"metric_name":"sessions"},"desc":true}],
		"limit":25,
		"offset":5,
		"currency_code":"USD",
		"return_property_quota":true
	}`)
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRunReport, Params: reportParams})
	if err != nil {
		t.Fatalf("run report: %v", err)
	}
	if !res.Truncated || res.RowCount != 2 {
		t.Fatalf("report truncation: %+v", res)
	}
	rep := res.Data.(map[string]any)
	if rep["dimension_headers"] == nil || rep["metric_headers"] == nil || rep["row_count"].(float64) != 3 {
		t.Fatalf("report response not snake_case: %+v", rep)
	}
	if len(rep["rows"].([]any)) != 2 {
		t.Fatalf("rows not trimmed: %+v", rep["rows"])
	}

	mock.mu.Lock()
	body := mock.lastReport
	mock.mu.Unlock()
	if body == nil {
		t.Fatal("runReport body not recorded")
	}
	dateRanges := body["dateRanges"].([]any)
	if len(dateRanges) != 1 {
		t.Fatalf("dateRanges: %+v", body)
	}
	dr := dateRanges[0].(map[string]any)
	if dr["startDate"] != "2026-11-01" || dr["endDate"] != "2026-11-25" || dr["name"] != "Nov" {
		t.Fatalf("dateRange: %+v", dr)
	}
	dims := body["dimensions"].([]any)
	if len(dims) != 2 || dims[0].(map[string]any)["name"] != "date" {
		t.Fatalf("dimensions: %+v", dims)
	}
	df := body["dimensionFilter"].(map[string]any)["filter"].(map[string]any)
	if df["fieldName"] != "country" {
		t.Fatalf("dimension filter not camelized: %+v", df)
	}
	mf := body["metricFilter"].(map[string]any)["filter"].(map[string]any)["numericFilter"].(map[string]any)
	if mf["operation"] != "GREATER_THAN" {
		t.Fatalf("metric filter: %+v", mf)
	}
	ob := body["orderBys"].([]any)[0].(map[string]any)
	if ob["metric"].(map[string]any)["metricName"] != "sessions" || ob["desc"] != true {
		t.Fatalf("orderBys: %+v", ob)
	}
	if body["limit"].(float64) != 3 || body["offset"].(float64) != 5 ||
		body["currencyCode"] != "USD" || body["returnPropertyQuota"] != true {
		t.Fatalf("window/quota: %+v", body)
	}

	realtimeParams := json.RawMessage(`{
		"property_id":470553943,
		"dimensions":["city"],
		"metrics":["activeUsers"],
		"dimension_filter":{"filter":{"field_name":"city","string_filter":{"match_type":"EXACT","value":"Tokyo"}}},
		"limit":10,
		"return_property_quota":true
	}`)
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolRunRealtimeReport, Params: realtimeParams})
	if err != nil {
		t.Fatalf("realtime: %v", err)
	}
	realtime := res.Data.(map[string]any)
	if realtime["dimension_headers"] == nil || len(realtime["rows"].([]any)) != 1 {
		t.Fatalf("realtime response: %+v", realtime)
	}
	mock.mu.Lock()
	rtBody := mock.lastRealtime
	mock.mu.Unlock()
	if _, has := rtBody["dateRanges"]; has {
		t.Fatalf("realtime must not send dateRanges: %+v", rtBody)
	}
	if _, has := rtBody["currencyCode"]; has {
		t.Fatalf("realtime must not send currencyCode: %+v", rtBody)
	}
	if _, has := rtBody["offset"]; has {
		t.Fatalf("realtime must not send offset: %+v", rtBody)
	}
	if rtBody["limit"].(float64) != 3 || rtBody["returnPropertyQuota"] != true {
		t.Fatalf("realtime body: %+v", rtBody)
	}
	rtFilter := rtBody["dimensionFilter"].(map[string]any)["filter"].(map[string]any)
	if rtFilter["fieldName"] != "city" {
		t.Fatalf("realtime filter: %+v", rtFilter)
	}
	if mock.tokenCalls != 1 {
		t.Fatalf("token calls = %d, want cache reuse", mock.tokenCalls)
	}
}

func TestGAFunnelAndConversionsPassthrough(t *testing.T) {
	svc, mock, _ := newTestGA(t)
	ctx := context.Background()

	funnelParams := json.RawMessage(`{
		"property_id":"470553943",
		"funnel_steps":[
			{"name":"Landing","event":"page_view"},
			{"name":"Purchase","filter_expression":{"funnel_event_filter":{"event_name":"purchase"}}}
		],
		"date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-25"}],
		"funnel_breakdown":{"breakdown_dimension":"deviceCategory"},
		"funnel_next_action":{"next_action_dimension":"eventName","limit":5},
		"segments":[{"user_segment":{"name":"US"}}],
		"return_property_quota":true
	}`)
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRunFunnelReport, Params: funnelParams})
	if err != nil {
		t.Fatalf("funnel: %v", err)
	}
	if !res.Truncated || res.RowCount != 2 {
		t.Fatalf("funnel truncation: %+v", res)
	}
	rep := res.Data.(map[string]any)
	table := rep["funnel_table"].(map[string]any)
	if len(table["rows"].([]any)) != 2 {
		t.Fatalf("funnel table not trimmed: %+v", table)
	}
	if len(rep["funnel_visualization"].(map[string]any)["rows"].([]any)) != 1 {
		t.Fatalf("funnel visualization: %+v", rep)
	}

	mock.mu.Lock()
	funnelBody := mock.lastFunnel
	mock.mu.Unlock()
	funnel := funnelBody["funnel"].(map[string]any)
	steps := funnel["steps"].([]any)
	if len(steps) != 2 {
		t.Fatalf("steps: %+v", steps)
	}
	step0 := steps[0].(map[string]any)
	if step0["name"] != "Landing" || step0["filterExpression"].(map[string]any)["funnelEventFilter"].(map[string]any)["eventName"] != "page_view" {
		t.Fatalf("event step: %+v", step0)
	}
	step1 := steps[1].(map[string]any)
	if step1["filterExpression"].(map[string]any)["funnelEventFilter"].(map[string]any)["eventName"] != "purchase" {
		t.Fatalf("filter step: %+v", step1)
	}
	breakdown := funnelBody["funnelBreakdown"].(map[string]any)["breakdownDimension"].(map[string]any)
	if breakdown["name"] != "deviceCategory" {
		t.Fatalf("breakdown: %+v", breakdown)
	}
	next := funnelBody["funnelNextAction"].(map[string]any)
	if next["nextActionDimension"].(map[string]any)["name"] != "eventName" || next["limit"].(float64) != 5 {
		t.Fatalf("next action: %+v", next)
	}
	segment := funnelBody["segments"].([]any)[0].(map[string]any)
	if segment["userSegment"] == nil {
		t.Fatalf("segments not camelized: %+v", segment)
	}
	if funnelBody["returnPropertyQuota"] != true {
		t.Fatalf("quota not passed: %+v", funnelBody)
	}

	convParams := json.RawMessage(`{
		"property_id":"470553943",
		"date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-25"}],
		"dimensions":["campaignName"],
		"metrics":["advertiserAdCost"],
		"conversion_spec":{"conversion_actions":[],"attribution_model":"DATA_DRIVEN"},
		"limit":100
	}`)
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolRunConversionsReport, Params: convParams})
	if err != nil {
		t.Fatalf("conversions: %v", err)
	}
	if res.RowCount != 0 {
		t.Fatalf("conversions rows: %+v", res)
	}
	mock.mu.Lock()
	convBody := mock.lastConv
	mock.mu.Unlock()
	spec := convBody["conversionSpec"].(map[string]any)
	if spec["attributionModel"] != "DATA_DRIVEN" {
		t.Fatalf("conversion spec not camelized: %+v", spec)
	}
	if _, ok := spec["conversionActions"].([]any); !ok {
		t.Fatalf("conversionActions: %+v", spec)
	}
}

func TestGATokenRefreshAndCache(t *testing.T) {
	svc, mock, clock := newTestGA(t)
	ctx := context.Background()
	call := func() {
		t.Helper()
		if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolGetAccountSummaries}); err != nil {
			t.Fatalf("account summaries: %v", err)
		}
	}
	call()
	call()
	mock.mu.Lock()
	if mock.tokenCalls != 1 {
		t.Fatalf("token calls = %d, want 1", mock.tokenCalls)
	}
	mock.mu.Unlock()

	clock.Advance(3560 * time.Second)
	call()
	mock.mu.Lock()
	if mock.tokenCalls != 2 {
		t.Fatalf("token calls after expiry = %d, want 2", mock.tokenCalls)
	}
	mock.mu.Unlock()
}

func TestGATokenRefreshFailureIsSafe(t *testing.T) {
	svc, mock, _ := newTestGA(t)
	mock.mu.Lock()
	mock.tokenStatus = http.StatusBadRequest
	mock.mu.Unlock()
	_, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: ToolGetAccountSummaries})
	if err == nil {
		t.Fatal("token failure accepted")
	}
	var ge *mcpgateway.Error
	if !errors.As(err, &ge) || ge.Code != mcpgateway.CodeUpstreamError {
		t.Fatalf("token failure code: %v", err)
	}
	if strings.Contains(ge.Message, "test-secret") || strings.Contains(ge.Message, "test-refresh") {
		t.Fatalf("token error leaks credential material: %q", ge.Message)
	}
}

func TestGACredentialMissingDoesNotCallUpstream(t *testing.T) {
	mock := &mockGA{t: t}
	srv := httptest.NewServer(http.HandlerFunc(mock.api))
	t.Cleanup(srv.Close)
	svc := New(Options{
		CredentialRef: "secret:GA_ADSENSE_ADC",
		Credentials:   fakeSecrets{err: mcpgateway.ErrCredentialMissing},
		AdminBaseURL:  srv.URL + "/v1alpha", DataBaseURL: srv.URL + "/v1beta",
		DataAlphaBaseURL: srv.URL + "/v1alpha", TokenURL: srv.URL + "/token",
		MaxRows: 10,
	})
	params := json.RawMessage(`{"property_id":"470553943","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":["date"],"metrics":["sessions"]}`)
	_, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: ToolRunReport, Params: params})
	if err == nil {
		t.Fatal("missing credential accepted")
	}
	var ge *mcpgateway.Error
	if !errors.As(err, &ge) || ge.Code != mcpgateway.CodeCredentialMissing {
		t.Fatalf("missing credential code: %v", err)
	}
	if mock.apiCalls != 0 || mock.tokenCalls != 0 {
		t.Fatalf("missing credential reached upstream: token=%d api=%d", mock.tokenCalls, mock.apiCalls)
	}
}

func TestGAValidationRejectsBadParams(t *testing.T) {
	svc, mock, _ := newTestGA(t)
	ctx := context.Background()
	cases := []struct {
		tool   string
		params string
	}{
		{ToolRunReport, `{"property_id":"470553943","dimensions":["date"],"metrics":["sessions"]}`},
		{ToolRunReport, `{"property_id":"not-a-property","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":["date"],"metrics":["sessions"]}`},
		{ToolRunRealtimeReport, `{"property_id":"470553943","dimensions":["city"]}`},
		{ToolRunFunnelReport, `{"property_id":"470553943","funnel_steps":[]}`},
		{ToolRunFunnelReport, `{"property_id":"470553943","funnel_steps":[{"name":"no filter"}]}`},
		{ToolRunConversionsReport, `{"property_id":"470553943","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":["campaignName"],"metrics":["advertiserAdCost"]}`},
	}
	for _, tc := range cases {
		before := mock.apiCalls
		_, err := svc.Handle(ctx, mcpgateway.Call{Tool: tc.tool, Params: json.RawMessage(tc.params)})
		if err == nil {
			t.Fatalf("%s accepted bad params %s", tc.tool, tc.params)
		}
		if mock.apiCalls != before {
			t.Fatalf("%s bad params reached upstream", tc.tool)
		}
	}

	// A totals-only report (no dimensions, one metric) is valid in the Data API
	// and must remain callable; the dimensions field is still required.
	totals := json.RawMessage(`{"property_id":"470553943","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":[],"metrics":["sessions"]}`)
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolRunReport, Params: totals}); err != nil {
		t.Fatalf("totals-only report: %v", err)
	}
	mock.mu.Lock()
	body := mock.lastReport
	mock.mu.Unlock()
	if dims, ok := body["dimensions"].([]any); !ok || len(dims) != 0 {
		t.Fatalf("totals-only dimensions: %+v", body["dimensions"])
	}

	// Realtime must reject offset; conversions must reject invalid attribution_model; report rejects bad currency.
	for _, tc := range []struct {
		tool   string
		params string
	}{
		{ToolRunRealtimeReport, `{"property_id":"470553943","dimensions":["city"],"metrics":["activeUsers"],"offset":1}`},
		{ToolRunConversionsReport, `{"property_id":"470553943","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":["campaignName"],"metrics":["advertiserAdCost"],"conversion_spec":{"attribution_model":"BOGUS"}}`},
		{ToolRunReport, `{"property_id":"470553943","date_ranges":[{"start_date":"2026-11-01","end_date":"2026-11-02"}],"dimensions":["date"],"metrics":["sessions"],"currency_code":"US"}`},
	} {
		if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: tc.tool, Params: json.RawMessage(tc.params)}); err == nil {
			t.Fatalf("%s accepted bad params: %s", tc.tool, tc.params)
		}
	}
}
