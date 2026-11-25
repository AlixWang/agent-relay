package bing

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

// CredentialSource is implemented by mcpgateway.Credentials.
type CredentialSource interface {
	Resolve(ref string) (string, error)
}

// Options wires the Bing service. BaseURL is overridable for httptest mocks.
type Options struct {
	CredentialRef string
	Credentials   CredentialSource
	Upstream      *mcpgateway.Upstream
	BaseURL       string
	MaxRows       int
	BatchCap      int
	Now           func() time.Time
}

// Service implements mcpgateway.MCPService.
type Service struct {
	credRef  string
	creds    CredentialSource
	upstream *mcpgateway.Upstream
	baseURL  string
	maxRows  int
	batchCap int
	now      func() time.Time

	refreshMu sync.Mutex
	mu        sync.Mutex
	sites     map[string]bool
	sitesAt   time.Time
	sitesHash [32]byte
}

const defaultBaseURL = "https://ssl.bing.com/webmaster/api.svc/json"

func New(opts Options) *Service {
	up := opts.Upstream
	if up == nil {
		up = mcpgateway.NewUpstream(15 * time.Second)
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = 1000
	}
	batchCap := opts.BatchCap
	if batchCap <= 0 {
		batchCap = 100
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		credRef: opts.CredentialRef, creds: opts.Credentials, upstream: up,
		baseURL: base, maxRows: maxRows, batchCap: batchCap, now: now,
	}
}

func (s *Service) ID() string          { return ServiceID }
func (s *Service) DisplayName() string { return DisplayName }
func (s *Service) Version() string     { return Version }
func (s *Service) Prefix() string      { return Prefix }
func (s *Service) CredentialRefs() []string {
	if s.credRef == "" {
		return nil
	}
	return []string{s.credRef}
}

func (s *Service) Handle(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	switch call.Tool {
	case ToolListSites:
		return s.listSites(ctx)
	case ToolQueryStats, ToolPageStats, ToolRankTraffic, ToolCrawlStats,
		ToolCrawlIssues, ToolListFeeds, ToolPageQueries, ToolQueryPages:
		return s.siteRead(ctx, call)
	case ToolKeyword, ToolRelatedKeyword, ToolKeywordStats:
		return s.keywordRead(ctx, call)
	case ToolSubmitURL:
		return s.submitURL(ctx, call)
	case ToolSubmitURLBatch:
		return s.submitURLBatch(ctx, call)
	case ToolSubmitFeed:
		return s.submitFeed(ctx, call)
	case ToolRemoveFeed:
		return s.removeFeed(ctx, call)
	case ToolAddBlockedURL:
		return s.addBlockedURL(ctx, call)
	default:
		return mcpgateway.Result{}, mcpgateway.InvalidParams("unknown Bing tool %q", call.Tool)
	}
}

// ---- upstream ----

func (s *Service) apiKey() (string, *mcpgateway.Error) {
	if s.creds == nil || s.credRef == "" {
		return "", credentialError()
	}
	v, err := s.creds.Resolve(s.credRef)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", credentialError()
	}
	return strings.TrimSpace(v), nil
}

// call issues one JSON/HTTP request. The key rides in the query string exactly
// as the Microsoft API requires; the URL is never returned in an error and
// never logged.
func (s *Service) call(ctx context.Context, httpMethod, method string, params url.Values, body any, out any) *mcpgateway.Error {
	key, kerr := s.apiKey()
	if kerr != nil {
		return kerr
	}
	q := url.Values{}
	for k, vals := range params {
		for _, v := range vals {
			q.Add(k, v)
		}
	}
	q.Set("apikey", key)
	rawURL := strings.TrimSuffix(s.baseURL, "/") + "/" + method + "?" + q.Encode()
	var resp *mcpgateway.Response
	var err error
	if httpMethod == http.MethodPost {
		resp, err = s.upstream.Do(ctx, httpMethod, rawURL, body, nil)
	} else {
		resp, err = s.upstream.DoRead(ctx, httpMethod, rawURL, nil, nil)
	}
	if err != nil {
		return mcpgateway.UpstreamFailure(err)
	}
	if resp.Status != http.StatusOK {
		return mcpgateway.UpstreamError(resp.Status)
	}
	var wrapper struct {
		D json.RawMessage `json:"d"`
	}
	if len(resp.Body) > 0 {
		if err := json.Unmarshal(resp.Body, &wrapper); err != nil {
			return &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "Bing returned invalid JSON", HTTPStatus: 200}
		}
	}
	if out != nil && len(wrapper.D) > 0 && string(wrapper.D) != "null" {
		if err := json.Unmarshal(wrapper.D, out); err != nil {
			return &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "Bing returned an unexpected payload", HTTPStatus: 200}
		}
	}
	return nil
}

type bingSite struct {
	URL        string `json:"Url"`
	IsVerified bool   `json:"IsVerified"`
}

// verifiedSite enforces the v5 contract: the site must be verified under the
// same API key. A 60s cache avoids one GetUserSites call per row, without
// holding s.mu across upstream HTTP I/O.
func (s *Service) verifiedSite(ctx context.Context, site string) *mcpgateway.Error {
	key, kerr := s.apiKey()
	if kerr != nil {
		return kerr
	}
	keyHash := sha256.Sum256([]byte(key))
	now := s.now()

	s.mu.Lock()
	if s.sites != nil && s.sitesHash == keyHash && now.Sub(s.sitesAt) < 60*time.Second {
		verified := s.sites[site]
		s.mu.Unlock()
		if verified {
			return nil
		}
		return mcpgateway.InvalidParams("site is not verified under this API key")
	}
	s.mu.Unlock()

	s.refreshMu.Lock()
	defer s.refreshMu.Unlock()

	now = s.now()
	s.mu.Lock()
	if s.sites != nil && s.sitesHash == keyHash && now.Sub(s.sitesAt) < 60*time.Second {
		verified := s.sites[site]
		s.mu.Unlock()
		if verified {
			return nil
		}
		return mcpgateway.InvalidParams("site is not verified under this API key")
	}
	s.mu.Unlock()

	var sites []bingSite
	if err := s.call(ctx, http.MethodGet, "GetUserSites", nil, nil, &sites); err != nil {
		return err
	}
	nextSites := map[string]bool{}
	for _, st := range sites {
		if st.URL != "" {
			nextSites[st.URL] = st.IsVerified
		}
	}
	s.mu.Lock()
	s.sites = nextSites
	s.sitesAt = s.now()
	s.sitesHash = keyHash
	verified := s.sites[site]
	s.mu.Unlock()

	if !verified {
		return mcpgateway.InvalidParams("site is not verified under this API key")
	}
	return nil
}

// ---- read tools ----

func (s *Service) listSites(ctx context.Context) (mcpgateway.Result, error) {
	var sites []map[string]any
	if err := s.call(ctx, http.MethodGet, "GetUserSites", nil, nil, &sites); err != nil {
		return mcpgateway.Result{}, err
	}
	if sites == nil {
		sites = []map[string]any{}
	}
	items, truncated := trimRows(sites, s.maxRows)
	return mcpgateway.Result{Data: map[string]any{"sites": items}, Truncated: truncated, RowCount: len(items), UpstreamStatus: 200}, nil
}

func (s *Service) siteRead(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	site, limit, page, query, err := decodeSiteRead(call.Tool, call.Params)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	if _, err := mcpgateway.ValidateSiteURL(site); err != nil {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("%s", err.Error())
	}
	switch call.Tool {
	case ToolPageQueries:
		if !mcpgateway.URLBelongsToSite(site, page) {
			return mcpgateway.Result{}, mcpgateway.InvalidParams("page must belong to the site")
		}
		if u, uerr := mcpgateway.NormalizeURL(page); uerr == nil {
			page = u.String()
		}
	case ToolQueryPages:
		if strings.TrimSpace(query) == "" {
			return mcpgateway.Result{}, mcpgateway.InvalidParams("q is required")
		}
	}
	if verr := s.verifiedSite(ctx, site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	method, params := siteMethod(call.Tool, site, page, query)
	var rows []map[string]any
	if err := s.call(ctx, http.MethodGet, method, params, nil, &rows); err != nil {
		return mcpgateway.Result{}, err
	}
	if call.Tool == ToolPageStats {
		for _, row := range rows {
			if v, ok := row["Query"]; ok {
				delete(row, "Query")
				row["Page"] = v
			}
		}
	}
	rows = normalizeRows(rows)
	max := s.rowLimit(limit)
	items, truncated := trimRows(rows, max)
	data := map[string]any{"site": site, "rows": items}
	switch call.Tool {
	case ToolQueryStats, ToolPageStats, ToolRankTraffic, ToolPageQueries, ToolQueryPages:
		data["totals"] = totalsOf(rows)
	case ToolListFeeds:
		data["feeds"] = items
	}
	if call.Tool == ToolPageQueries || call.Tool == ToolQueryPages {
		data["note"] = "Bing returns at most its top rows; page-queries is time-ascending and capped near 1000."
	}
	return mcpgateway.Result{Data: data, Truncated: truncated, RowCount: len(items), UpstreamStatus: 200}, nil
}

func decodeSiteRead(tool string, raw json.RawMessage) (site string, limit int, page, query string, err error) {
	switch tool {
	case ToolPageQueries:
		var p pageQueriesArgs
		if e := mcpgateway.DecodeStrict(raw, &p); e != nil {
			return "", 0, "", "", e
		}
		return p.Site, p.Limit, p.Page, "", nil
	case ToolQueryPages:
		var p queryPagesArgs
		if e := mcpgateway.DecodeStrict(raw, &p); e != nil {
			return "", 0, "", "", e
		}
		return p.Site, p.Limit, "", strings.TrimSpace(p.Q), nil
	default:
		var p siteArgs
		if e := mcpgateway.DecodeStrict(raw, &p); e != nil {
			return "", 0, "", "", e
		}
		return p.Site, p.Limit, "", "", nil
	}
}

func siteMethod(tool, site, page, query string) (string, url.Values) {
	params := siteParam(site)
	switch tool {
	case ToolQueryStats:
		return "GetQueryStats", params
	case ToolPageStats:
		return "GetPageStats", params
	case ToolRankTraffic:
		return "GetRankAndTrafficStats", params
	case ToolCrawlStats:
		return "GetCrawlStats", params
	case ToolCrawlIssues:
		return "GetCrawlIssues", params
	case ToolListFeeds:
		return "GetFeeds", params
	case ToolPageQueries:
		params.Set("page", page)
		return "GetPageQueryStats", params
	case ToolQueryPages:
		params.Set("query", query)
		return "GetQueryPageStats", params
	default:
		return "", params
	}
}

func (s *Service) keywordRead(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	switch call.Tool {
	case ToolKeyword, ToolRelatedKeyword:
		p, err := decodeKeyword(call.Params)
		if err != nil {
			return mcpgateway.Result{}, err
		}
		country, language := p.Country, p.Language
		if country == "" {
			country = "US"
		}
		if language == "" {
			language = "en"
		}
		params := url.Values{"q": {p.Q}, "country": {country}, "language": {language}}
		if p.StartDate != "" || p.EndDate != "" {
			if p.StartDate == "" || p.EndDate == "" {
				return mcpgateway.Result{}, mcpgateway.InvalidParams("startDate and endDate must be supplied together")
			}
			start, end, err := mcpgateway.ValidateDateRange(p.StartDate, p.EndDate)
			if err != nil {
				return mcpgateway.Result{}, mcpgateway.InvalidParams("%s", err.Error())
			}
			params.Set("startDate", start)
			params.Set("endDate", end)
		}
		method := "GetKeyword"
		if call.Tool == ToolRelatedKeyword {
			method = "GetRelatedKeywords"
		}
		var data any
		if err := s.call(ctx, http.MethodGet, method, params, nil, &data); err != nil {
			return mcpgateway.Result{}, err
		}
		rows, truncated := normalizeAndTrimAny(data, s.maxRows)
		return mcpgateway.Result{Data: map[string]any{"rows": rows}, Truncated: truncated, RowCount: len(rows), UpstreamStatus: 200}, nil
	case ToolKeywordStats:
		var p struct {
			Q        string `json:"q"`
			Country  string `json:"country"`
			Language string `json:"language"`
		}
		if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
			return mcpgateway.Result{}, err
		}
		p.Q = strings.TrimSpace(p.Q)
		if p.Q == "" || len(p.Q) > 200 {
			return mcpgateway.Result{}, mcpgateway.InvalidParams("q is required (max 200 bytes)")
		}
		if err := validateCountryLang(p.Country, p.Language); err != nil {
			return mcpgateway.Result{}, err
		}
		country, language := p.Country, p.Language
		if country == "" {
			country = "US"
		}
		if language == "" {
			language = "en"
		}
		params := url.Values{"q": {p.Q}, "country": {country}, "language": {language}}
		var data any
		if err := s.call(ctx, http.MethodGet, "GetKeywordStats", params, nil, &data); err != nil {
			return mcpgateway.Result{}, err
		}
		rows, truncated := normalizeAndTrimAny(data, s.maxRows)
		return mcpgateway.Result{Data: map[string]any{"rows": rows}, Truncated: truncated, RowCount: len(rows), UpstreamStatus: 200}, nil
	}
	return mcpgateway.Result{}, mcpgateway.InvalidParams("unknown keyword tool")
}

func (s *Service) rowLimit(requested int) int {
	if requested <= 0 {
		return 100
	}
	if requested > s.maxRows {
		return s.maxRows
	}
	return requested
}

func totalsOf(rows []map[string]any) map[string]any {
	var clicks, impressions float64
	for _, row := range rows {
		if v, ok := row["Clicks"]; ok {
			if f, ok := number(v); ok {
				clicks += f
			}
		}
		if v, ok := row["Impressions"]; ok {
			if f, ok := number(v); ok {
				impressions += f
			}
		}
	}
	return map[string]any{"rows": len(rows), "clicks": clicks, "impressions": impressions}
}

func normalizeRows(rows []map[string]any) []map[string]any {
	out := make([]map[string]any, len(rows))
	for i, row := range rows {
		m := make(map[string]any, len(row))
		for k, v := range row {
			m[k] = normalizeValue(v)
		}
		out[i] = m
	}
	return out
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case string:
		if m := dateRe.FindStringSubmatch(x); m != nil {
			ms, err := strconv.ParseInt(m[1], 10, 64)
			if err == nil {
				return time.Unix(ms/1000, 0).UTC().Format("2006-01-02")
			}
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = normalizeValue(item)
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = normalizeValue(item)
		}
		return out
	default:
		return v
	}
}

func normalizeAndTrimAny(v any, max int) ([]any, bool) {
	if v == nil {
		return []any{}, false
	}
	arr, ok := v.([]any)
	if !ok {
		return []any{normalizeValue(v)}, false
	}
	norm := make([]any, len(arr))
	for i, item := range arr {
		norm[i] = normalizeValue(item)
	}
	if len(norm) > max {
		return norm[:max], true
	}
	return norm, false
}

func trimRows(rows []map[string]any, max int) ([]map[string]any, bool) {
	if max <= 0 {
		max = 1000
	}
	if len(rows) <= max {
		return rows, false
	}
	return rows[:max], true
}

func number(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func credentialError() *mcpgateway.Error {
	return &mcpgateway.Error{Code: mcpgateway.CodeCredentialMissing, Message: "Bing API key is not available", HTTPStatus: 200}
}

func toolError(format string, args ...any) *mcpgateway.Error {
	return mcpgateway.InvalidParams(format, args...)
}
