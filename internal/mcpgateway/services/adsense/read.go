package adsense

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/csv"
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
	format, ferr := normalizeReportFormat(p.Format)
	if ferr != nil {
		return mcpgateway.Result{}, ferr
	}
	params, pageSize, perr := reportParams(p, s.maxRows)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	account, aerr := s.resolveAccount(ctx, p.Account)
	if aerr != nil {
		return mcpgateway.Result{}, aerr
	}
	if format == reportFormatCSV {
		return s.generateReportCSV(ctx, account, params, p, pageSize)
	}
	rep, rows, err := s.report(ctx, account, params)
	if err != nil {
		return mcpgateway.Result{}, err
	}
	page, hasMore := sliceReportRows(rows, p.Offset, pageSize)
	total := totalMatchedRows(rep)
	if total >= 0 {
		if total > p.Offset+len(page) {
			hasMore = true
		}
	} else if len(rows) >= p.Offset+pageSize {
		// The upstream returned exactly the requested window and did not
		// report a total. There may be another page, so never claim the
		// report is complete in that case.
		hasMore = true
	}
	rep["rows"] = page
	rep["offset"] = p.Offset
	rep["limit"] = pageSize
	rep["returned"] = len(page)
	rep["has_more"] = hasMore
	if total >= 0 {
		rep["total_matched_rows"] = total
	}
	if hasMore {
		if len(page) > 0 {
			rep["next_offset"] = p.Offset + len(page)
			rep["note"] = "More rows matched this report. Call again with next_offset (and the same orderBy) to fetch the next page; offset+limit is bounded by the upstream row ceiling."
		} else {
			rep["note"] = "This offset is beyond the rows the upstream API returned for the requested window; narrow the report or lower the offset."
		}
	}
	return mcpgateway.Result{Data: rep, Truncated: hasMore, RowCount: len(page)}, nil
}

// reportParams validates report arguments and builds the upstream query. It
// returns the effective page size as well as the query: upstream receives
// limit=offset+pageSize (AdSense v2 has no offset/pageToken), and the caller
// slices the returned rows. It never silently lowers a requested limit: a
// request above mcp_gateway.max_rows_per_call or the 100,000-row upstream
// ceiling is rejected with invalid_params.
func reportParams(p reportArgs, maxRows int) (url.Values, int, *mcpgateway.Error) {
	if maxRows <= 0 {
		maxRows = 1000
	}
	if err := checkNames(p.Dimensions, allowedDimensions, "dimension"); err != nil {
		return nil, 0, mcpgateway.AsError(err)
	}
	if err := checkNames(p.Metrics, allowedMetrics, "metric"); err != nil {
		return nil, 0, mcpgateway.AsError(err)
	}
	if p.Limit < 0 || p.Limit > maxReportPage {
		return nil, 0, mcpgateway.InvalidParams("limit must be 1-%d", maxReportPage)
	}
	pageSize := p.Limit
	if pageSize <= 0 {
		pageSize = maxRows
	}
	if pageSize > maxRows {
		return nil, 0, mcpgateway.InvalidParams("limit must be 1-%d (mcp_gateway.max_rows_per_call)", maxRows)
	}
	if p.Offset < 0 || p.Offset >= maxReportWindow {
		return nil, 0, mcpgateway.InvalidParams("offset must be 0-%d", maxReportWindow-1)
	}
	if p.Offset+pageSize > maxReportWindow {
		return nil, 0, mcpgateway.InvalidParams("offset+limit must be <= %d (gateway report-window ceiling; AdSense generate itself allows 100,000 rows and CSV more, but the gateway's upstream response size cap still applies); use a smaller page or narrow the report", maxReportWindow)
	}
	if _, err := normalizeReportFormat(p.Format); err != nil {
		return nil, 0, mcpgateway.AsError(err)
	}
	for i, f := range p.Filters {
		if !validFilter(f) {
			return nil, 0, mcpgateway.InvalidParams("filters[%d] must be WHITELISTED_DIMENSION<==|=@>value", i)
		}
	}
	if p.OrderBy != "" {
		name := strings.TrimPrefix(p.OrderBy, "-")
		if !allowedDimensions[name] && !allowedMetrics[name] {
			return nil, 0, mcpgateway.InvalidParams("orderBy %q is not a whitelisted dimension/metric", p.OrderBy)
		}
	}
	v := url.Values{}
	if p.DateRange != "" {
		if p.StartDate != "" || p.EndDate != "" {
			return nil, 0, mcpgateway.InvalidParams("supply either dateRange or startDate+endDate, not both")
		}
		if !contains(dateRanges, p.DateRange) {
			return nil, 0, mcpgateway.InvalidParams("unsupported dateRange %q", p.DateRange)
		}
		v.Set("dateRange", p.DateRange)
	} else {
		sd, ed, err := mcpgateway.ValidateDateRange(p.StartDate, p.EndDate)
		if err != nil {
			return nil, 0, mcpgateway.InvalidParams("%s", err.Error())
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
	// AdSense reports.generate has no pageToken/offset. Ask for the whole
	// window in one call; generateReport slices it for the caller.
	v.Set("limit", strconv.Itoa(p.Offset+pageSize))
	return v, pageSize, nil
}

// report returns the raw upstream report and its rows without trimming, so
// the caller can apply the offset/page window itself.
func (s *Service) report(ctx context.Context, account string, params url.Values) (map[string]any, []any, *mcpgateway.Error) {
	var rep map[string]any
	path := account + "/reports:generate"
	if err := s.apiGet(ctx, path, params, &rep); err != nil {
		return nil, nil, err
	}
	if rep == nil {
		rep = map[string]any{}
	}
	rows, _ := rep["rows"].([]any)
	return rep, rows, nil
}

// generateReportCSV calls the separate reports:generateCsv method. The
// response is a Google HttpBody: {"contentType":"text/csv","data":"<base64>"}.
func (s *Service) generateReportCSV(ctx context.Context, account string, params url.Values, p reportArgs, pageSize int) (mcpgateway.Result, error) {
	var body struct {
		ContentType string `json:"contentType"`
		Data        string `json:"data"`
	}
	if err := s.apiGet(ctx, account+"/reports:generateCsv", params, &body); err != nil {
		return mcpgateway.Result{}, err
	}
	decoded, derr := base64.StdEncoding.DecodeString(strings.TrimSpace(body.Data))
	if derr != nil {
		return mcpgateway.Result{}, &mcpgateway.Error{
			Code: mcpgateway.CodeUpstreamError, Message: "AdSense returned invalid CSV data", HTTPStatus: 200,
		}
	}
	records, rerr := readCSV(string(decoded))
	if rerr != nil {
		return mcpgateway.Result{}, rerr
	}
	var header []string
	var dataRows [][]string
	if len(records) > 0 {
		header = records[0]
		dataRows = records[1:]
	}
	page, hasMore := sliceCSV(dataRows, p.Offset, pageSize)
	if !hasMore && len(dataRows) >= p.Offset+pageSize {
		// generateCsv has no totalMatchedRows: a full window is ambiguous,
		// so report has_more conservatively instead of silently claiming the
		// export is complete.
		hasMore = true
	}
	text := string(decoded)
	if p.Offset > 0 || hasMore || len(page) != len(dataRows) {
		text = encodeCSV(header, page)
	}
	data := map[string]any{
		"account":      account,
		"format":       reportFormatCSV,
		"content_type": body.ContentType,
		"csv":          text,
		"offset":       p.Offset,
		"limit":        pageSize,
		"returned":     len(page),
		"row_count":    len(page),
		"has_more":     hasMore,
	}
	if hasMore {
		data["next_offset"] = p.Offset + len(page)
		data["note"] = "More CSV rows were fetched than returned; call again with next_offset."
	} else {
		data["note"] = "AdSense generateCsv does not report total matched rows; if the CSV ends exactly at the requested limit there may be more rows (page with offset to confirm)."
	}
	return mcpgateway.Result{Data: data, Truncated: hasMore, RowCount: len(page)}, nil
}

// sliceReportRows applies a gateway-side offset/page window. hasMore is true
// when the upstream fetch returned rows beyond this page.
func sliceReportRows(rows []any, offset, pageSize int) ([]any, bool) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return []any{}, false
	}
	page := rows[offset:]
	hasMore := len(page) > pageSize
	if hasMore {
		page = page[:pageSize]
	}
	out := make([]any, len(page))
	copy(out, page)
	return out, hasMore
}

func sliceCSV(rows [][]string, offset, pageSize int) ([][]string, bool) {
	if offset < 0 {
		offset = 0
	}
	if offset >= len(rows) {
		return nil, false
	}
	page := rows[offset:]
	hasMore := len(page) > pageSize
	if hasMore {
		page = page[:pageSize]
	}
	return page, hasMore
}

// totalMatchedRows parses the upstream ReportResult.totalMatchedRows count
// (a string in the discovery document) and returns -1 when it is absent.
func totalMatchedRows(rep map[string]any) int {
	v, ok := rep["totalMatchedRows"]
	if !ok {
		return -1
	}
	switch n := v.(type) {
	case string:
		i, err := strconv.ParseInt(strings.TrimSpace(n), 10, 64)
		if err != nil {
			return -1
		}
		return int(i)
	case float64:
		return int(n)
	case json.Number:
		i, err := n.Int64()
		if err != nil {
			return -1
		}
		return int(i)
	default:
		return -1
	}
}

func readCSV(text string) ([][]string, *mcpgateway.Error) {
	if strings.TrimSpace(text) == "" {
		return nil, nil
	}
	r := csv.NewReader(strings.NewReader(text))
	r.FieldsPerRecord = -1
	records, err := r.ReadAll()
	if err != nil {
		return nil, &mcpgateway.Error{
			Code: mcpgateway.CodeUpstreamError, Message: "AdSense returned malformed CSV", HTTPStatus: 200,
		}
	}
	return records, nil
}

func encodeCSV(header []string, rows [][]string) string {
	var buf bytes.Buffer
	w := csv.NewWriter(&buf)
	if header != nil {
		if err := w.Write(header); err != nil {
			return ""
		}
	}
	_ = w.WriteAll(rows)
	w.Flush()
	return buf.String()
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
		params, _, perr := reportParams(args, s.maxRows)
		if perr != nil {
			return mcpgateway.Result{}, perr
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			rep, _, err := s.report(ctx, account, params)
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
		rep, _, err := s.report(ctx, account, v)
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
