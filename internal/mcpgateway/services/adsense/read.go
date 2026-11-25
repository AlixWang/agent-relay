package adsense

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// Options wires the AdSense service. BaseURL/TokenURL are overridable so tests
// can point at httptest servers; production uses the official endpoints.
type Options struct {
	CredentialRef string
	Credentials   CredentialSource
	Upstream      *mcpgateway.Upstream
	HTTPClient    *http.Client
	BaseURL       string
	TokenURL      string
	MaxRows       int
	Now           func() time.Time
}

// Service implements mcpgateway.MCPService for AdSense reads.
type Service struct {
	credRef  string
	creds    CredentialSource
	upstream *mcpgateway.Upstream
	client   *http.Client
	baseURL  string
	tokenURL string
	maxRows  int
	now      func() time.Time

	mu      sync.Mutex
	token   string
	expires time.Time
}

const defaultBaseURL = "https://adsense.googleapis.com/v2"
const defaultTokenURL = "https://oauth2.googleapis.com/token"

func New(opts Options) *Service {
	timeout := 15 * time.Second
	client := opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: timeout}
	}
	up := opts.Upstream
	if up == nil {
		up = mcpgateway.NewUpstream(timeout)
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
	}
	tokenURL := opts.TokenURL
	if tokenURL == "" {
		tokenURL = defaultTokenURL
	}
	maxRows := opts.MaxRows
	if maxRows <= 0 {
		maxRows = 1000
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		credRef: opts.CredentialRef, creds: opts.Credentials, upstream: up, client: client,
		baseURL: base, tokenURL: tokenURL, maxRows: maxRows, now: now,
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

// Handle dispatches only the nine read tools. The write placeholders return
// not_implemented and are deliberately absent from Tools(). The wrapper tags
// successful calls with the upstream HTTP status for audit/meta.
func (s *Service) Handle(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	res, err := s.handle(ctx, call)
	if err == nil && res.UpstreamStatus == 0 {
		res.UpstreamStatus = http.StatusOK
	}
	return res, err
}

func (s *Service) handle(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	switch call.Tool {
	case ToolListAccounts:
		return s.listAccounts(ctx)
	case ToolEarningsSummary:
		return s.earningsSummary(ctx, call.Params)
	case ToolGenerateReport:
		return s.generateReport(ctx, call.Params)
	case ToolComparePeriods:
		return s.comparePeriods(ctx, call.Params)
	case ToolListSites:
		return s.listCollection(ctx, call.Params, "sites", "sites")
	case ToolListAlerts:
		return s.listCollection(ctx, call.Params, "alerts", "alerts")
	case ToolListPolicy:
		return s.listCollection(ctx, call.Params, "policyIssues", "policyIssues")
	case ToolListPayments:
		return s.listCollection(ctx, call.Params, "payments", "payments")
	case ToolListAdUnits:
		return s.listAdUnits(ctx, call.Params)
	case WriteCreateAdUnit, WritePatchAdUnit, WriteCreateCustomChannel,
		WriteDeleteCustomChannel, WritePatchCustomChannel:
		return mcpgateway.Result{}, &mcpgateway.Error{
			Code:       mcpgateway.CodeNotImplemented,
			Message:    "AdSense writes are not implemented (readonly ADC and AFP project are required)",
			HTTPStatus: http.StatusOK,
		}
	default:
		return mcpgateway.Result{}, mcpgateway.InvalidParams("unknown AdSense tool %q", call.Tool)
	}
}

// ---- auth ----

type adcFile struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
}

func (s *Service) accessToken(ctx context.Context) (string, *mcpgateway.Error) {
	s.mu.Lock()
	if s.token != "" && s.now().Before(s.expires) {
		tok := s.token
		s.mu.Unlock()
		return tok, nil
	}
	s.mu.Unlock()
	if s.creds == nil || s.credRef == "" {
		return "", credentialError()
	}
	raw, err := s.creds.Resolve(s.credRef)
	if err != nil {
		return "", credentialError()
	}
	var adc adcFile
	if err := json.Unmarshal([]byte(raw), &adc); err != nil || adc.Type != "authorized_user" ||
		adc.ClientID == "" || adc.ClientSecret == "" || adc.RefreshToken == "" {
		return "", credentialError()
	}
	form := url.Values{
		"client_id":     {adc.ClientID},
		"client_secret": {adc.ClientSecret},
		"refresh_token": {adc.RefreshToken},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", mcpgateway.UpstreamFailure(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "relay-mcp/1.0")
	resp, err := s.client.Do(req)
	if err != nil {
		// The token endpoint uses form encoding, so it has its own client;
		// still avoid echoing the URL/body or any credential material.
		if errors.Is(err, context.DeadlineExceeded) {
			return "", mcpgateway.UpstreamFailure(mcpgateway.ErrUpstreamTimeout)
		}
		return "", mcpgateway.UpstreamFailure(mcpgateway.ErrUpstreamNetwork)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", mcpgateway.UpstreamFailure(mcpgateway.ErrUpstreamNetwork)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &mcpgateway.Error{
			Code: mcpgateway.CodeUpstreamError, HTTPStatus: 200,
			Message: fmt.Sprintf("AdSense token refresh failed (HTTP %d); operator must re-authorize the ADC", resp.StatusCode),
			Details: map[string]any{"upstream_status": resp.StatusCode},
		}
	}
	var tok struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err := json.Unmarshal(data, &tok); err != nil || tok.AccessToken == "" {
		return "", &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "AdSense token refresh returned no access token", HTTPStatus: 200}
	}
	ttl := tok.ExpiresIn
	if ttl <= 0 {
		ttl = 3000
	}
	s.mu.Lock()
	s.token = tok.AccessToken
	s.expires = s.now().Add(time.Duration(ttl-60) * time.Second)
	s.mu.Unlock()
	return tok.AccessToken, nil
}

func (s *Service) clearToken() {
	s.mu.Lock()
	s.token = ""
	s.expires = time.Time{}
	s.mu.Unlock()
}

func (s *Service) apiGet(ctx context.Context, path string, params url.Values, out any) *mcpgateway.Error {
	token, terr := s.accessToken(ctx)
	if terr != nil {
		return terr
	}
	u := strings.TrimSuffix(s.baseURL, "/") + "/" + strings.TrimPrefix(path, "/")
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	resp, err := s.upstream.DoRead(ctx, http.MethodGet, u, nil, map[string]string{"Authorization": "Bearer " + token})
	if err != nil {
		return mcpgateway.UpstreamFailure(err)
	}
	if resp.Status != http.StatusOK {
		if resp.Status == http.StatusUnauthorized || resp.Status == http.StatusForbidden {
			s.clearToken()
		}
		return mcpgateway.UpstreamError(resp.Status)
	}
	if err := json.Unmarshal(resp.Body, out); err != nil {
		return &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "AdSense returned invalid JSON", HTTPStatus: 200}
	}
	return nil
}

// ---- account resolution ----

func (s *Service) listAccounts(ctx context.Context) (mcpgateway.Result, error) {
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := s.apiGet(ctx, "accounts", nil, &out); err != nil {
		return mcpgateway.Result{}, err
	}
	data, truncated := trimSlice(out.Accounts, s.maxRows)
	return mcpgateway.Result{Data: map[string]any{"accounts": data}, Truncated: truncated, RowCount: len(data)}, nil
}

func (s *Service) accounts(ctx context.Context) ([]map[string]any, *mcpgateway.Error) {
	var out struct {
		Accounts []map[string]any `json:"accounts"`
	}
	if err := s.apiGet(ctx, "accounts", nil, &out); err != nil {
		return nil, err
	}
	return out.Accounts, nil
}

func (s *Service) resolveAccount(ctx context.Context, requested string) (string, *mcpgateway.Error) {
	accounts, err := s.accounts(ctx)
	if err != nil {
		return "", err
	}
	if len(accounts) == 0 {
		return "", &mcpgateway.Error{Code: mcpgateway.CodeInvalidParams, Message: "no AdSense accounts found for this credential", HTTPStatus: 400}
	}
	if requested == "" {
		if name, ok := accounts[0]["name"].(string); ok && name != "" {
			return name, nil
		}
		return "", &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "AdSense account list has no name", HTTPStatus: 200}
	}
	want := normalizeAccount(requested)
	for _, a := range accounts {
		if name, ok := a["name"].(string); ok && name == want {
			return name, nil
		}
	}
	return "", &mcpgateway.Error{Code: mcpgateway.CodeInvalidParams, Message: "account not found in this credential", HTTPStatus: 400}
}

// ---- reports ----

func (s *Service) generateReport(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p reportArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	params, perr := reportParams(p)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	rep, truncated, rows, err := s.report(ctx, account, params)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: rep, Truncated: truncated, RowCount: rows}, nil
}

func reportParams(p reportArgs) (url.Values, *mcpgateway.Error) {
	if err := checkNames(p.Dimensions, allowedDimensions, "dimension"); err != nil {
		return nil, mcpgateway.AsError(err)
	}
	if err := checkNames(p.Metrics, allowedMetrics, "metric"); err != nil {
		return nil, mcpgateway.AsError(err)
	}
	if p.Limit < 0 || p.Limit > 1000 {
		return nil, mcpgateway.InvalidParams("limit must be 1-1000")
	}
	for i, f := range p.Filters {
		if !validFilter(f) {
			return nil, mcpgateway.InvalidParams("filters[%d] must be FIELD<op>value", i)
		}
	}
	if p.OrderBy != "" {
		name := strings.TrimPrefix(p.OrderBy, "-")
		if !allowedDimensions[name] && !allowedMetrics[name] {
			return nil, mcpgateway.InvalidParams("orderBy %q is not a whitelisted dimension/metric", p.OrderBy)
		}
	}
	v := url.Values{}
	if p.DateRange != "" {
		v.Set("dateRange", p.DateRange)
	} else {
		sd, ed, err := mcpgateway.ValidateDateRange(p.StartDate, p.EndDate)
		if err != nil {
			return nil, mcpgateway.InvalidParams("%s", err.Error())
		}
		addDate(v, "startDate", sd)
		addDate(v, "endDate", ed)
	}
	for _, d := range p.Dimensions {
		v.Add("dimensions", d)
	}
	for _, m := range defaultMetricsOr(p.Metrics) {
		v.Add("metrics", m)
	}
	for _, f := range p.Filters {
		v.Add("filters", strings.TrimSpace(f))
	}
	if p.OrderBy != "" {
		v.Set("orderBy", p.OrderBy)
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 1000
	}
	v.Set("limit", strconv.Itoa(limit))
	return v, nil
}

func addDate(v url.Values, prefix, date string) {
	y, m, d := dateParts(date)
	v.Set(prefix+".year", strconv.Itoa(y))
	v.Set(prefix+".month", strconv.Itoa(m))
	v.Set(prefix+".day", strconv.Itoa(d))
}

func dateParts(date string) (int, int, int) {
	t, _ := time.Parse("2006-01-02", date)
	return t.Year(), int(t.Month()), t.Day()
}

func (s *Service) report(ctx context.Context, account string, params url.Values) (map[string]any, bool, int, *mcpgateway.Error) {
	var rep map[string]any
	path := account + "/reports:generate"
	if err := s.apiGet(ctx, path, params, &rep); err != nil {
		return nil, false, 0, err
	}
	if rep == nil {
		rep = map[string]any{}
	}
	rows, truncated := trimAnyRows(rep["rows"], s.maxRows)
	rep["rows"] = rows
	return rep, truncated, len(rows), nil
}

func (s *Service) earningsSummary(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p struct {
		Account string `json:"account"`
	}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	now := s.now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonthEnd := monthStart.AddDate(0, 0, -1)
	lastMonthStart := time.Date(lastMonthEnd.Year(), lastMonthEnd.Month(), 1, 0, 0, 0, 0, time.UTC)
	periods := []struct {
		name       string
		start, end time.Time
	}{
		{"today", today, today},
		{"yesterday", yesterday, yesterday},
		{"last7Days", today.AddDate(0, 0, -6), today},
		{"thisMonth", monthStart, today},
		{"lastMonth", lastMonthStart, lastMonthEnd},
	}
	out := map[string]any{"account": account}
	for _, period := range periods {
		args := reportArgs{
			StartDate: period.start.Format("2006-01-02"),
			EndDate:   period.end.Format("2006-01-02"),
		}
		params, perr := reportParams(args)
		if perr != nil {
			return mcpgateway.Result{}, perr
		}
		rep, _, _, err := s.report(ctx, account, params)
		if err != nil {
			return mcpgateway.Result{}, err
		}
		out[period.name] = totalsOf(rep)
	}
	out["provisional"] = "today is provisional; not a trend by itself"
	return mcpgateway.Result{Data: out, RowCount: len(periods)}, nil
}

func totalsOf(rep map[string]any) map[string]any {
	out := map[string]any{}
	headers, _ := rep["headers"].([]any)
	names := make([]string, 0, len(headers))
	for _, h := range headers {
		if m, ok := h.(map[string]any); ok {
			if n, ok := m["name"].(string); ok {
				names = append(names, n)
			}
		}
	}
	var cells []any
	if totals, ok := rep["totals"].(map[string]any); ok {
		cells, _ = totals["cells"].([]any)
	}
	if len(cells) == 0 {
		if rows, ok := rep["rows"].([]any); ok && len(rows) > 0 {
			if row, ok := rows[0].(map[string]any); ok {
				cells, _ = row["cells"].([]any)
			}
		}
	}
	for i, cell := range cells {
		if i >= len(names) {
			break
		}
		if m, ok := cell.(map[string]any); ok {
			val := m["value"]
			if s, ok := val.(string); ok {
				if f, err := strconv.ParseFloat(s, 64); err == nil {
					out[names[i]] = f
					continue
				}
			}
			out[names[i]] = val
		}
	}
	return out
}

func (s *Service) comparePeriods(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p compareArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if _, _, err := mcpgateway.ValidateDateRange(p.Period1Start, p.Period1End); err != nil {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("period1: %s", err.Error())
	}
	if _, _, err := mcpgateway.ValidateDateRange(p.Period2Start, p.Period2End); err != nil {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("period2: %s", err.Error())
	}
	if err := checkNames(p.Dimensions, allowedDimensions, "dimension"); err != nil {
		return mcpgateway.Result{}, mcpgateway.AsError(err)
	}
	if err := checkNames(p.Metrics, allowedMetrics, "metric"); err != nil {
		return mcpgateway.Result{}, mcpgateway.AsError(err)
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	run := func(start, end string) (map[string]any, *mcpgateway.Error) {
		v := url.Values{}
		addDate(v, "startDate", start)
		addDate(v, "endDate", end)
		for _, d := range p.Dimensions {
			v.Add("dimensions", d)
		}
		for _, m := range defaultMetricsOr(p.Metrics) {
			v.Add("metrics", m)
		}
		v.Set("limit", strconv.Itoa(s.maxRows))
		rep, _, _, err := s.report(ctx, account, v)
		if err != nil {
			return nil, err
		}
		return totalsOf(rep), nil
	}
	p1, err := run(p.Period1Start, p.Period1End)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	p2, err := run(p.Period2Start, p.Period2End)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	delta := map[string]any{}
	base := map[string]any{}
	for k, v1 := range p1 {
		f1, ok1 := asInt(v1)
		f2, ok2 := asInt(p2[k])
		if ok1 && ok2 {
			delta[k] = f2 - f1
			base[k] = f1
		}
	}
	return mcpgateway.Result{Data: map[string]any{
		"account": account, "period1": p1, "period2": p2,
		"delta": delta, "base": base,
		"periods": map[string]string{
			"period1": describePeriod(p.Period1Start, p.Period1End),
			"period2": describePeriod(p.Period2Start, p.Period2End),
		},
	}}, nil
}

// ---- collection reads ----

func (s *Service) listCollection(ctx context.Context, raw json.RawMessage, path, key string) (mcpgateway.Result, error) {
	var p struct {
		Account string `json:"account"`
	}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	var out map[string]any
	if err := s.apiGet(ctx, account+"/"+path, nil, &out); err != nil {
		return mcpgateway.Result{}, err
	}
	items, truncated := trimAnyRows(out[key], s.maxRows)
	return mcpgateway.Result{Data: map[string]any{key: items}, Truncated: truncated, RowCount: len(items)}, nil
}

func (s *Service) listAdUnits(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p struct {
		Account string `json:"account"`
	}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	var clients struct {
		AdClients []map[string]any `json:"adClients"`
	}
	if err := s.apiGet(ctx, account+"/adclients", nil, &clients); err != nil {
		return mcpgateway.Result{}, err
	}
	var units []any
	truncated := false
	for _, c := range clients.AdClients {
		name, _ := c["name"].(string)
		if name == "" {
			continue
		}
		var out struct {
			AdUnits []any `json:"adUnits"`
		}
		if err := s.apiGet(ctx, name+"/adunits", nil, &out); err != nil {
			return mcpgateway.Result{}, err
		}
		units = append(units, out.AdUnits...)
		if len(units) >= s.maxRows {
			units = units[:s.maxRows]
			truncated = true
			break
		}
	}
	return mcpgateway.Result{Data: map[string]any{"adUnits": units}, Truncated: truncated, RowCount: len(units)}, nil
}

// ---- helpers ----

func trimSlice(in []map[string]any, max int) ([]map[string]any, bool) {
	if len(in) <= max {
		return in, false
	}
	return in[:max], true
}

func trimAnyRows(v any, max int) ([]any, bool) {
	rows, ok := v.([]any)
	if !ok {
		return []any{}, false
	}
	if len(rows) <= max {
		return rows, false
	}
	return rows[:max], true
}
