package adsense

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
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

func newTestService(t *testing.T, handler http.HandlerFunc) (*Service, *mockAdSense) {
	t.Helper()
	mock := &mockAdSense{t: t}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			if r.Method != http.MethodPost {
				t.Errorf("token method = %s", r.Method)
			}
			if r.FormValue("grant_type") != "refresh_token" || r.FormValue("client_id") != "test-client" {
				t.Errorf("token form: %+v", r.Form)
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "access-123", "expires_in": 3600})
			return
		}
		if r.Header.Get("Authorization") != "Bearer access-123" {
			t.Errorf("bad auth header: %q", r.Header.Get("Authorization"))
		}
		mock.handler(w, r)
	}))
	t.Cleanup(srv.Close)
	svc := New(Options{
		CredentialRef: "secret:TEST_ADC", Credentials: fakeSecrets{value: testADC},
		Upstream: mcpgateway.NewUpstream(5 * time.Second),
		BaseURL:  srv.URL + "/v2", TokenURL: srv.URL + "/token",
		MaxRows: 2, Now: func() time.Time { return time.Unix(1_800_000_000, 0) },
	})
	return svc, mock
}

type mockAdSense struct {
	t      *testing.T
	calls  int32
	report url.Values
}

func (m *mockAdSense) handler(w http.ResponseWriter, r *http.Request) {
	atomic.AddInt32(&m.calls, 1)
	switch {
	case r.URL.Path == "/v2/accounts":
		_ = json.NewEncoder(w).Encode(map[string]any{"accounts": []any{
			map[string]any{"name": "accounts/pub-111", "displayName": "Main"},
			map[string]any{"name": "accounts/pub-222", "displayName": "Second"},
		}})
	case strings.HasSuffix(r.URL.Path, "/reports:generate"):
		m.report = r.URL.Query()
		rows := []any{}
		for i := 0; i < 3; i++ {
			rows = append(rows, map[string]any{"cells": []any{
				map[string]any{"value": "1.5"}, map[string]any{"value": "10"},
			}})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"headers": []any{map[string]any{"name": "ESTIMATED_EARNINGS"}, map[string]any{"name": "IMPRESSIONS"}},
			"rows":    rows,
			"totals": map[string]any{"cells": []any{
				map[string]any{"value": "4.5"}, map[string]any{"value": "30"},
			}},
		})
	case strings.HasSuffix(r.URL.Path, "/sites"):
		_ = json.NewEncoder(w).Encode(map[string]any{"sites": []any{
			map[string]any{"name": "sites/1", "domain": "x.test"},
		}})
	case strings.HasSuffix(r.URL.Path, "/alerts"):
		_ = json.NewEncoder(w).Encode(map[string]any{"alerts": []any{map[string]any{"name": "a1"}}})
	case strings.HasSuffix(r.URL.Path, "/policyIssues"):
		_ = json.NewEncoder(w).Encode(map[string]any{"policyIssues": []any{map[string]any{"name": "p1"}}})
	case strings.HasSuffix(r.URL.Path, "/payments"):
		_ = json.NewEncoder(w).Encode(map[string]any{"payments": []any{map[string]any{"name": "pay1"}}})
	case strings.HasSuffix(r.URL.Path, "/adclients"):
		_ = json.NewEncoder(w).Encode(map[string]any{"adClients": []any{
			map[string]any{"name": "accounts/pub-111/adclients/ca-1"},
		}})
	case strings.HasSuffix(r.URL.Path, "/adunits"):
		_ = json.NewEncoder(w).Encode(map[string]any{"adUnits": []any{
			map[string]any{"name": "u1"}, map[string]any{"name": "u2"}, map[string]any{"name": "u3"},
		}})
	default:
		m.t.Errorf("unexpected path %s", r.URL.Path)
		http.NotFound(w, r)
	}
}

func TestAdSenseToolsAreReadOnlyAndWritesHidden(t *testing.T) {
	svc, _ := newTestService(t, func(w http.ResponseWriter, r *http.Request) {})
	tools := svc.Tools()
	if len(tools) != 9 {
		t.Fatalf("read tools = %d", len(tools))
	}
	for _, tool := range tools {
		if tool.Risk != mcpgateway.RiskRead || !tool.ReadOnly {
			t.Fatalf("tool %s is not read-only: %+v", tool.Name, tool)
		}
	}
	if len(WriteToolNames()) != 5 {
		t.Fatalf("expected five write placeholders")
	}
	for _, p := range PlaceholderTools() {
		for _, exposed := range tools {
			if exposed.Name == p.Name {
				t.Fatalf("write placeholder %s must not be exposed", p.Name)
			}
		}
	}
	if svc.Prefix() != "adsense" || len(svc.CredentialRefs()) != 1 {
		t.Fatalf("service metadata: %+v %+v", svc.Prefix(), svc.CredentialRefs())
	}
}

func TestAdSenseReadsAndReportTruncation(t *testing.T) {
	svc, mock := newTestService(t, nil)
	ctx := context.Background()
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolListAccounts})
	if err != nil {
		t.Fatalf("list accounts: %v", err)
	}
	data := res.Data.(map[string]any)
	if len(data["accounts"].([]map[string]any)) != 2 {
		t.Fatalf("accounts: %+v", data)
	}
	// Account resolution + report query shape.
	raw := json.RawMessage(`{"account":"pub-111","startDate":"2026-11-01","endDate":"2026-11-02","dimensions":["DATE","COUNTRY_NAME"],"metrics":["ESTIMATED_EARNINGS","IMPRESSIONS"],"limit":100}`)
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolGenerateReport, Params: raw})
	if err != nil {
		t.Fatalf("report: %v", err)
	}
	if !res.Truncated || res.RowCount != 2 {
		t.Fatalf("truncation: %+v", res)
	}
	if mock.report.Get("startDate.year") != "2026" || mock.report.Get("startDate.month") != "11" ||
		mock.report.Get("startDate.day") != "1" || mock.report.Get("limit") != "100" {
		t.Fatalf("report query: %+v", mock.report)
	}
	if len(mock.report["dimensions"]) != 2 || len(mock.report["metrics"]) != 2 {
		t.Fatalf("repeated params: %+v", mock.report)
	}
	// Ad units span ad clients and honour max rows.
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolListAdUnits, Params: json.RawMessage(`{"account":"accounts/pub-111"}`)})
	if err != nil {
		t.Fatalf("ad units: %v", err)
	}
	units := res.Data.(map[string]any)["adUnits"].([]any)
	if len(units) != 2 || !res.Truncated {
		t.Fatalf("ad units: %+v", res)
	}
	// Compare periods returns absolute delta and base.
	raw = json.RawMessage(`{"account":"pub-111","period1Start":"2026-11-01","period1End":"2026-11-01","period2Start":"2026-11-02","period2End":"2026-11-02","metrics":["ESTIMATED_EARNINGS"]}`)
	res, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolComparePeriods, Params: raw})
	if err != nil {
		t.Fatalf("compare: %v", err)
	}
	cd := res.Data.(map[string]any)
	delta := cd["delta"].(map[string]any)
	if delta["ESTIMATED_EARNINGS"].(float64) != 0 {
		t.Fatalf("delta: %+v", delta)
	}
	if cd["base"].(map[string]any)["ESTIMATED_EARNINGS"].(float64) != 4.5 {
		t.Fatalf("base: %+v", cd["base"])
	}
}

func TestAdSensePageURLDimension(t *testing.T) {
	// Anti-drift guard: PAGE_URL exists in the official v2 enum; it must stay
	// in our whitelist so page-level earnings reports keep working.
	if !allowedDimensions["PAGE_URL"] {
		t.Fatal("PAGE_URL missing from allowedDimensions")
	}
	svc, mock := newTestService(t, nil)
	raw := json.RawMessage(`{"account":"pub-111","startDate":"2026-11-01","endDate":"2026-11-02","dimensions":["PAGE_URL"],"metrics":["ESTIMATED_EARNINGS"],"limit":10}`)
	if err := validateReportArg(raw); err != nil {
		t.Fatalf("PAGE_URL rejected: %v", err)
	}
	if _, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: ToolGenerateReport, Params: raw}); err != nil {
		t.Fatalf("PAGE_URL report: %v", err)
	}
	dims := mock.report["dimensions"]
	if len(dims) != 1 || dims[0] != "PAGE_URL" {
		t.Fatalf("PAGE_URL not passed upstream: %+v", mock.report)
	}
	// Validation still precedes credential resolution; a valid PAGE_URL report
	// with no credential fails as credential_missing.
	missing := New(Options{CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing}, MaxRows: 10})
	_, err := missing.Handle(context.Background(), mcpgateway.Call{Tool: ToolGenerateReport, Params: raw})
	if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
		t.Fatalf("PAGE_URL missing credential: %v", err)
	}
}

func TestAdSenseValidationAndCredentialFailure(t *testing.T) {
	svc, mock := newTestService(t, nil)
	bad := json.RawMessage(`{"startDate":"2026-11-01","endDate":"2026-11-02","dimensions":["DROP_TABLE"]}`)
	if err := svc.Tools()[2].Validate(bad); err == nil {
		t.Fatal("unknown dimension accepted")
	}
	bad = json.RawMessage(`{"site":"not a url"}`)
	_ = bad
	// A report that fails validation must not hit upstream.
	before := atomic.LoadInt32(&mock.calls)
	if _, err := svc.Handle(context.Background(), mcpgateway.Call{Tool: ToolGenerateReport, Params: json.RawMessage(`{"startDate":"2026-11-02","endDate":"2026-11-01"}`)}); err == nil {
		t.Fatal("reversed dates accepted at handler")
	}
	// The handler decodes, but reportParams is the guard; no upstream call.
	if atomic.LoadInt32(&mock.calls) != before {
		t.Fatalf("validation failure reached upstream")
	}
	missing := New(Options{CredentialRef: "secret:NOPE", Credentials: fakeSecrets{err: mcpgateway.ErrCredentialMissing}, MaxRows: 10})
	_, err := missing.Handle(context.Background(), mcpgateway.Call{Tool: ToolListAccounts})
	if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeCredentialMissing) {
		t.Fatalf("missing credential: %v", err)
	}
	// Write placeholders return not_implemented and never call upstream.
	_, err = svc.Handle(context.Background(), mcpgateway.Call{Tool: WriteCreateAdUnit})
	if err == nil || !strings.Contains(err.Error(), mcpgateway.CodeNotImplemented) {
		t.Fatalf("write placeholder: %v", err)
	}
}

func TestAdSenseFixesEnumsDatesPagingAndToken(t *testing.T) {
	var tokenCalls int32
	var force403 atomic.Bool
	var force401Once atomic.Bool
	var lastReport atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/token" {
			atomic.AddInt32(&tokenCalls, 1)
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "tok-" + string(rune('0'+atomic.LoadInt32(&tokenCalls))), "expires_in": 3600})
			return
		}
		if force403.Load() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if force401Once.CompareAndSwap(true, false) {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v2/accounts":
			if r.URL.Query().Get("pageToken") == "" {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accounts":      []any{map[string]any{"name": "accounts/pub-1"}},
					"nextPageToken": "p2",
				})
			} else {
				_ = json.NewEncoder(w).Encode(map[string]any{
					"accounts": []any{map[string]any{"name": "accounts/pub-2"}},
				})
			}
		default:
			lastReport.Store(r.URL.Query())
			_ = json.NewEncoder(w).Encode(map[string]any{
				"headers": []any{map[string]any{"name": "ESTIMATED_EARNINGS"}},
				"totals":  map[string]any{"cells": []any{map[string]any{"value": "2.0"}}},
			})
		}
	}))
	t.Cleanup(srv.Close)
	svc := New(Options{
		CredentialRef: "secret:TEST", Credentials: fakeSecrets{value: testADC},
		Upstream: mcpgateway.NewUpstream(5 * time.Second), BaseURL: srv.URL + "/v2", TokenURL: srv.URL + "/token", MaxRows: 10,
	})
	ctx := context.Background()

	// Paging follows nextPageToken.
	res, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolListAccounts})
	if err != nil || res.RowCount != 2 {
		t.Fatalf("paged accounts: %+v err=%v", res, err)
	}
	// compare_periods canonicalizes RFC3339 dates instead of sending year=1.
	_, err = svc.Handle(ctx, mcpgateway.Call{Tool: ToolComparePeriods, Params: json.RawMessage(`{
		"account":"pub-1","period1Start":"2026-11-01T00:00:00Z","period1End":"2026-11-02T00:00:00Z",
		"period2Start":"2026-11-03T00:00:00Z","period2End":"2026-11-04T00:00:00Z"
	}`)})
	if err != nil {
		t.Fatalf("compare_periods rfc3339: %v", err)
	}
	q := lastReport.Load().(url.Values)
	if q.Get("startDate.year") != "2026" || q.Get("startDate.month") != "11" {
		t.Fatalf("compare_periods sent bad date: %+v", q)
	}
	// 403 must not evict cached token; 401 refreshes and retries once.
	force403.Store(true)
	_, _ = svc.Handle(ctx, mcpgateway.Call{Tool: ToolListAccounts})
	force403.Store(false)
	if atomic.LoadInt32(&tokenCalls) != 1 {
		t.Fatalf("403 evicted token: calls=%d", atomic.LoadInt32(&tokenCalls))
	}
	force401Once.Store(true)
	if _, err := svc.Handle(ctx, mcpgateway.Call{Tool: ToolListAccounts}); err != nil {
		t.Fatalf("401 retry failed: %v", err)
	}
	if atomic.LoadInt32(&tokenCalls) != 2 {
		t.Fatalf("401 did not refresh once: calls=%d", atomic.LoadInt32(&tokenCalls))
	}
	// Invalid v2 enums and mixed date params are rejected.
	for _, bad := range []string{
		`{"dateRange":"LAST_14_DAYS"}`,
		`{"dateRange":"TODAY","startDate":"2026-11-01","endDate":"2026-11-01"}`,
		`{"dateRange":"TODAY","metrics":["CTR"]}`,
		`{"dateRange":"TODAY","filters":["DROP_TABLE==1"]}`,
		`{"dateRange":"TODAY","filters":["COUNTRY_CODE!=US"]}`,
	} {
		if err := validateReportArg(json.RawMessage(bad)); err == nil {
			t.Fatalf("accepted invalid report arg: %s", bad)
		}
	}
}
