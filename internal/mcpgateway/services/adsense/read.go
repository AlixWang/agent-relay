package adsense

import (
	"context"
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

// Options wires the AdSense service. BaseURL/TokenURL are overridable so tests
// can point at httptest servers; production uses the official endpoints.
type Options struct {
	CredentialRef   string
	Credentials     CredentialSource
	Upstream        *mcpgateway.Upstream
	UpstreamTimeout time.Duration
	HTTPClient      *http.Client
	BaseURL         string
	TokenURL        string
	MaxRows         int
	Now             func() time.Time
}

// Service implements mcpgateway.MCPService for AdSense reads.
type Service struct {
	credRef  string
	upstream *mcpgateway.Upstream
	adc      *mcpgateway.GoogleADC
	baseURL  string
	maxRows  int
	now      func() time.Time
}

const defaultBaseURL = "https://adsense.googleapis.com/v2"

func New(opts Options) *Service {
	timeout := opts.UpstreamTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	up := opts.Upstream
	if up == nil {
		up = mcpgateway.NewUpstream(timeout)
	}
	base := strings.TrimSuffix(opts.BaseURL, "/")
	if base == "" {
		base = defaultBaseURL
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
		credRef:  opts.CredentialRef,
		upstream: up,
		adc: mcpgateway.NewGoogleADC(mcpgateway.GoogleADCOptions{
			ServiceName:   DisplayName,
			CredentialRef: opts.CredentialRef,
			Credentials:   opts.Credentials,
			HTTPClient:    opts.HTTPClient,
			Timeout:       timeout,
			TokenURL:      opts.TokenURL,
			Now:           now,
		}),
		baseURL: base,
		maxRows: maxRows,
		now:     now,
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

func (s *Service) apiGet(ctx context.Context, path string, params url.Values, out any) *mcpgateway.Error {
	u := strings.TrimSuffix(s.baseURL, "/") + "/" + strings.TrimPrefix(path, "/")
	if len(params) > 0 {
		u += "?" + params.Encode()
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, terr := s.adc.Token(ctx)
		if terr != nil {
			return terr
		}
		resp, err := s.upstream.DoRead(ctx, http.MethodGet, u, nil, map[string]string{"Authorization": "Bearer " + token})
		if err != nil {
			return mcpgateway.UpstreamFailure(err)
		}
		if resp.Status == http.StatusUnauthorized {
			s.adc.Invalidate(token)
			if attempt == 0 {
				continue
			}
			return mcpgateway.UpstreamError(resp.Status)
		}
		if resp.Status != http.StatusOK {
			return mcpgateway.UpstreamError(resp.Status)
		}
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "AdSense returned invalid JSON", HTTPStatus: 200}
		}
		return nil
	}
	return mcpgateway.UpstreamError(http.StatusUnauthorized)
}

// ---- account resolution ----

func (s *Service) listAccounts(ctx context.Context) (mcpgateway.Result, error) {
	all, truncated, err := s.accountsPaged(ctx, s.maxRows)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{"accounts": all}, Truncated: truncated, RowCount: len(all)}, nil
}

func (s *Service) accountsPaged(ctx context.Context, maxRows int) ([]map[string]any, bool, *mcpgateway.Error) {
	all := []map[string]any{}
	seen := map[string]bool{}
	pageToken := ""
	for pageNum := 0; pageNum < 50; pageNum++ {
		q := url.Values{}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var out struct {
			Accounts      []map[string]any `json:"accounts"`
			NextPageToken string           `json:"nextPageToken"`
		}
		if err := s.apiGet(ctx, "accounts", q, &out); err != nil {
			return nil, false, err
		}
		for _, a := range out.Accounts {
			if maxRows > 0 && len(all) >= maxRows {
				return all, true, nil
			}
			all = append(all, a)
		}
		next := out.NextPageToken
		if next == "" || seen[next] {
			break
		}
		if maxRows > 0 && len(all) >= maxRows {
			return all, true, nil
		}
		seen[next] = true
		pageToken = next
	}
	return all, false, nil
}

func (s *Service) accounts(ctx context.Context) ([]map[string]any, *mcpgateway.Error) {
	all, _, err := s.accountsPaged(ctx, 0)
	return all, err
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
	params, perr := reportParams(p, s.maxRows)
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

func reportParams(p reportArgs, maxRows int) (url.Values, *mcpgateway.Error) {
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
			return nil, mcpgateway.InvalidParams("filters[%d] must be WHITELISTED_DIMENSION<==|=@>value", i)
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
		if p.StartDate != "" || p.EndDate != "" {
			return nil, mcpgateway.InvalidParams("supply either dateRange or startDate+endDate, not both")
		}
		if !contains(dateRanges, p.DateRange) {
			return nil, mcpgateway.InvalidParams("unsupported dateRange %q", p.DateRange)
		}
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
		limit = maxRows
		if limit <= 0 || limit > 1000 {
			limit = 1000
		}
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
	monthStart := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	lastMonthEnd := monthStart.AddDate(0, 0, -1)
	lastMonthStart := time.Date(lastMonthEnd.Year(), lastMonthEnd.Month(), 1, 0, 0, 0, 0, time.UTC)
	periods := []struct {
		name      string
		dateRange string
		start     string
		end       string
	}{
		{name: "today", dateRange: "TODAY"},
		{name: "yesterday", dateRange: "YESTERDAY"},
		{name: "last7Days", dateRange: "LAST_7_DAYS"},
		{name: "thisMonth", dateRange: "MONTH_TO_DATE"},
		{name: "lastMonth", start: lastMonthStart.Format("2006-01-02"), end: lastMonthEnd.Format("2006-01-02")},
	}
	out := map[string]any{"account": account}
	var mu sync.Mutex
	var wg sync.WaitGroup
	var firstErr error
	for _, period := range periods {
		period := period
		args := reportArgs{DateRange: period.dateRange, StartDate: period.start, EndDate: period.end}
		params, perr := reportParams(args, s.maxRows)
		if perr != nil {
			return mcpgateway.Result{}, perr
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, _, _, err := s.report(ctx, account, params)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			out[period.name] = totalsOf(rep, true)
		}()
	}
	wg.Wait()
	if firstErr != nil {
		return mcpgateway.Result{}, firstErr
	}
	out["provisional"] = "today is provisional; not a trend by itself"
	return mcpgateway.Result{Data: out, RowCount: len(periods)}, nil
}

func totalsOf(rep map[string]any, allowRowFallback bool) map[string]any {
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
	if len(cells) == 0 && allowRowFallback {
		if rows, ok := rep["rows"].([]any); ok && len(rows) == 1 {
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
	p1s, p1e, err := mcpgateway.ValidateDateRange(p.Period1Start, p.Period1End)
	if err != nil {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("period1: %s", err.Error())
	}
	p2s, p2e, err := mcpgateway.ValidateDateRange(p.Period2Start, p.Period2End)
	if err != nil {
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
		return totalsOf(rep, len(p.Dimensions) == 0), nil
	}
	p1, rerr := run(p1s, p1e)
	if rerr != nil {
		return mcpgateway.Result{}, rerr
	}
	p2, rerr := run(p2s, p2e)
	if rerr != nil {
		return mcpgateway.Result{}, rerr
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
			"period1": describePeriod(p1s, p1e),
			"period2": describePeriod(p2s, p2e),
		},
	}}, nil
}

// ---- collection reads ----

func (s *Service) listCollectionPaged(ctx context.Context, path, key string, maxRows int) ([]any, bool, *mcpgateway.Error) {
	items := []any{}
	seen := map[string]bool{}
	pageToken := ""
	for pageNum := 0; pageNum < 50; pageNum++ {
		q := url.Values{}
		if pageToken != "" {
			q.Set("pageToken", pageToken)
		}
		var out map[string]any
		if err := s.apiGet(ctx, path, q, &out); err != nil {
			return nil, false, err
		}
		if pageItems, ok := out[key].([]any); ok {
			for _, item := range pageItems {
				if maxRows > 0 && len(items) >= maxRows {
					return items, true, nil
				}
				items = append(items, item)
			}
		}
		next, _ := out["nextPageToken"].(string)
		if next == "" || seen[next] {
			break
		}
		if maxRows > 0 && len(items) >= maxRows {
			return items, true, nil
		}
		seen[next] = true
		pageToken = next
	}
	return items, false, nil
}

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
	items, truncated, err := s.listCollectionPaged(ctx, account+"/"+path, key, s.maxRows)
	if err != nil {
		return mcpgateway.Result{}, err
	}
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
	adClients, _, cerr := s.listCollectionPaged(ctx, account+"/adclients", "adClients", 0)
	if cerr != nil {
		return mcpgateway.Result{}, cerr
	}
	units := []any{}
	truncated := false
	for _, rawClient := range adClients {
		c, _ := rawClient.(map[string]any)
		name, _ := c["name"].(string)
		if name == "" {
			continue
		}
		rem := s.maxRows - len(units)
		if rem <= 0 {
			truncated = true
			break
		}
		pageUnits, pageTrunc, uerr := s.listCollectionPaged(ctx, name+"/adunits", "adUnits", rem)
		if uerr != nil {
			return mcpgateway.Result{}, uerr
		}
		units = append(units, pageUnits...)
		if pageTrunc {
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
