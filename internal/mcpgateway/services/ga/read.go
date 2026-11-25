package ga

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

// CredentialSource is implemented by mcpgateway.Credentials.
type CredentialSource interface {
	Resolve(ref string) (string, error)
}

// Options wires the Google Analytics service. The three base URLs and the
// token URL are overridable so tests can point at httptest servers; production
// uses the official endpoints.
type Options struct {
	CredentialRef    string
	Credentials      CredentialSource
	Upstream         *mcpgateway.Upstream
	UpstreamTimeout  time.Duration
	HTTPClient       *http.Client
	AdminBaseURL     string
	DataBaseURL      string
	DataAlphaBaseURL string
	TokenURL         string
	MaxRows          int
	Now              func() time.Time
}

// Service implements mcpgateway.MCPService for the nine GA read tools.
type Service struct {
	credRef  string
	upstream *mcpgateway.Upstream
	adc      *mcpgateway.GoogleADC

	adminBase     string
	dataBase      string
	dataAlphaBase string
	maxRows       int
}

const (
	defaultAdminBaseURL     = "https://analyticsadmin.googleapis.com/v1alpha"
	defaultDataBaseURL      = "https://analyticsdata.googleapis.com/v1beta"
	defaultDataAlphaBaseURL = "https://analyticsdata.googleapis.com/v1alpha"
)

func New(opts Options) *Service {
	timeout := opts.UpstreamTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	up := opts.Upstream
	if up == nil {
		up = mcpgateway.NewUpstream(timeout)
	}
	adminBase := strings.TrimSuffix(opts.AdminBaseURL, "/")
	if adminBase == "" {
		adminBase = defaultAdminBaseURL
	}
	dataBase := strings.TrimSuffix(opts.DataBaseURL, "/")
	if dataBase == "" {
		dataBase = defaultDataBaseURL
	}
	dataAlphaBase := strings.TrimSuffix(opts.DataAlphaBaseURL, "/")
	if dataAlphaBase == "" {
		dataAlphaBase = defaultDataAlphaBaseURL
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
		adminBase: adminBase, dataBase: dataBase, dataAlphaBase: dataAlphaBase,
		maxRows: maxRows,
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

// Handle dispatches only the nine read tools. There are no write tools in this
// service; create_property_annotation is intentionally absent.
func (s *Service) Handle(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	res, err := s.handle(ctx, call)
	if err == nil && res.UpstreamStatus == 0 {
		res.UpstreamStatus = http.StatusOK
	}
	return res, err
}

func (s *Service) handle(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	switch call.Tool {
	case ToolGetAccountSummaries:
		return s.getAccountSummaries(ctx)
	case ToolGetPropertyDetails:
		return s.getPropertyDetails(ctx, call.Params)
	case ToolListGoogleAdsLinks:
		return s.listGoogleAdsLinks(ctx, call.Params)
	case ToolListPropertyAnnotations:
		return s.listPropertyAnnotations(ctx, call.Params)
	case ToolGetCustomDimensionsMetrics:
		return s.getCustomDimensionsAndMetrics(ctx, call.Params)
	case ToolRunReport:
		return s.runReport(ctx, call.Params)
	case ToolRunRealtimeReport:
		return s.runRealtimeReport(ctx, call.Params)
	case ToolRunConversionsReport:
		return s.runConversionsReport(ctx, call.Params)
	case ToolRunFunnelReport:
		return s.runFunnelReport(ctx, call.Params)
	default:
		return mcpgateway.Result{}, mcpgateway.InvalidParams("unknown Google Analytics tool %q", call.Tool)
	}
}

// ---- HTTP ----

func (s *Service) apiGet(ctx context.Context, base, path string, query url.Values, out any) *mcpgateway.Error {
	return s.apiDo(ctx, http.MethodGet, base, path, query, nil, out)
}

func (s *Service) apiPost(ctx context.Context, base, path string, body any, out any) *mcpgateway.Error {
	return s.apiDo(ctx, http.MethodPost, base, path, nil, body, out)
}

func (s *Service) apiDo(ctx context.Context, method, base, path string, query url.Values, body any, out any) *mcpgateway.Error {
	u := strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(path, "/")
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	for attempt := 0; attempt < 2; attempt++ {
		token, terr := s.adc.Token(ctx)
		if terr != nil {
			return terr
		}
		resp, err := s.upstream.DoRead(ctx, method, u, body, map[string]string{"Authorization": "Bearer " + token})
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
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(resp.Body, out); err != nil {
			return &mcpgateway.Error{Code: mcpgateway.CodeUpstreamError, Message: "Google Analytics returned invalid JSON", HTTPStatus: 200}
		}
		return nil
	}
	return mcpgateway.UpstreamError(http.StatusUnauthorized)
}

// ---- Admin API reads ----

func decodeProperty(raw json.RawMessage) (string, *mcpgateway.Error) {
	var p propertyArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return "", mcpgateway.AsError(err)
	}
	num, err := normalizePropertyID(p.PropertyID)
	if err != nil {
		return "", mcpgateway.AsError(err)
	}
	return num, nil
}

func (s *Service) getAccountSummaries(ctx context.Context) (mcpgateway.Result, error) {
	items, truncated, err := s.listPaged(ctx, s.adminBase, "/accountSummaries", "accountSummaries")
	if err != nil {
		return mcpgateway.Result{}, err
	}
	data, _ := snakeValue(items).([]any)
	return mcpgateway.Result{Data: data, Truncated: truncated, RowCount: len(data)}, nil
}

func (s *Service) getPropertyDetails(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	num, perr := decodeProperty(raw)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	var property map[string]any
	if err := s.apiGet(ctx, s.adminBase, "/properties/"+num, nil, &property); err != nil {
		return mcpgateway.Result{}, err
	}
	data := snakeMap(property)
	return mcpgateway.Result{Data: data, RowCount: 1}, nil
}

func (s *Service) listGoogleAdsLinks(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	num, perr := decodeProperty(raw)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	items, truncated, err := s.listPaged(ctx, s.adminBase,
		"/properties/"+num+"/googleAdsLinks", "googleAdsLinks")
	if err != nil {
		return mcpgateway.Result{}, err
	}
	data, _ := snakeValue(items).([]any)
	return mcpgateway.Result{Data: data, Truncated: truncated, RowCount: len(data)}, nil
}

func (s *Service) listPropertyAnnotations(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	num, perr := decodeProperty(raw)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	items, truncated, err := s.listPaged(ctx, s.adminBase,
		"/properties/"+num+"/reportingDataAnnotations", "reportingDataAnnotations")
	if err != nil {
		return mcpgateway.Result{}, err
	}
	data, _ := snakeValue(items).([]any)
	return mcpgateway.Result{Data: data, Truncated: truncated, RowCount: len(data)}, nil
}

func (s *Service) listPaged(ctx context.Context, base, path, key string) ([]any, bool, *mcpgateway.Error) {
	out := []any{}
	seen := map[string]bool{}
	pageToken := ""
	for pageNum := 0; pageNum < 50; pageNum++ {
		query := url.Values{"pageSize": {"200"}}
		if pageToken != "" {
			query.Set("pageToken", pageToken)
		}
		var page map[string]any
		if err := s.apiGet(ctx, base, path, query, &page); err != nil {
			return nil, false, err
		}
		if items, ok := page[key].([]any); ok {
			for _, item := range items {
				if len(out) >= s.maxRows {
					return out, true, nil
				}
				out = append(out, item)
			}
		}
		next, _ := page["nextPageToken"].(string)
		if next == "" || seen[next] {
			return out, false, nil
		}
		if len(out) >= s.maxRows {
			return out, true, nil
		}
		seen[next] = true
		pageToken = next
	}
	return out, true, nil
}

// ---- Data API metadata ----

func (s *Service) getCustomDimensionsAndMetrics(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	num, perr := decodeProperty(raw)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	var meta map[string]any
	if err := s.apiGet(ctx, s.dataBase, "/properties/"+num+"/metadata", nil, &meta); err != nil {
		return mcpgateway.Result{}, err
	}
	dimensions := customDefinitions(meta["dimensions"])
	metrics := customDefinitions(meta["metrics"])
	truncated := false
	if len(dimensions) > s.maxRows {
		dimensions = dimensions[:s.maxRows]
		truncated = true
	}
	rem := s.maxRows - len(dimensions)
	if rem < len(metrics) {
		if rem < 0 {
			rem = 0
		}
		metrics = metrics[:rem]
		truncated = true
	}
	data := map[string]any{
		"custom_dimensions": snakeValue(dimensions),
		"custom_metrics":    snakeValue(metrics),
	}
	return mcpgateway.Result{Data: data, Truncated: truncated, RowCount: len(dimensions) + len(metrics)}, nil
}

func customDefinitions(v any) []any {
	items, _ := v.([]any)
	out := []any{}
	for _, item := range items {
		m, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if custom, ok := m["customDefinition"].(bool); ok && custom {
			out = append(out, item)
		}
	}
	return out
}

// ---- reports ----

func (s *Service) effectiveLimit(requested int) int {
	if requested <= 0 || requested > s.maxRows {
		return s.maxRows + 1
	}
	return requested
}

func (s *Service) runReport(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p runReportArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if err := p.validate(); err != nil {
		return mcpgateway.Result{}, err
	}
	path, perr := propertyPath(p.PropertyID)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	body := reportBody(p.DateRanges, p.Dimensions, p.Metrics, p.DimensionFilter,
		p.MetricFilter, p.OrderBys, s.effectiveLimit(p.Limit), p.Offset, strings.TrimSpace(p.CurrencyCode), p.ReturnPropertyQuota)
	var rep map[string]any
	if err := s.apiPost(ctx, s.dataBase, "/"+path+":runReport", body, &rep); err != nil {
		return mcpgateway.Result{}, err
	}
	rowCount, truncated := trimRowsField(rep, s.maxRows)
	rep = snakeMap(rep)
	return mcpgateway.Result{Data: rep, Truncated: truncated, RowCount: rowCount}, nil
}

func (s *Service) runRealtimeReport(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p runRealtimeArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if err := p.validate(); err != nil {
		return mcpgateway.Result{}, err
	}
	path, perr := propertyPath(p.PropertyID)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	body := reportBody(nil, p.Dimensions, p.Metrics, p.DimensionFilter,
		p.MetricFilter, p.OrderBys, s.effectiveLimit(p.Limit), 0, "", p.ReturnPropertyQuota)
	var rep map[string]any
	if err := s.apiPost(ctx, s.dataBase, "/"+path+":runRealtimeReport", body, &rep); err != nil {
		return mcpgateway.Result{}, err
	}
	rowCount, truncated := trimRowsField(rep, s.maxRows)
	rep = snakeMap(rep)
	return mcpgateway.Result{Data: rep, Truncated: truncated, RowCount: rowCount}, nil
}

func (s *Service) runConversionsReport(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p runConversionsArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if err := p.validate(); err != nil {
		return mcpgateway.Result{}, err
	}
	path, perr := propertyPath(p.PropertyID)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	body := reportBody(p.DateRanges, p.Dimensions, p.Metrics, p.DimensionFilter,
		p.MetricFilter, p.OrderBys, s.effectiveLimit(p.Limit), p.Offset, strings.TrimSpace(p.CurrencyCode), p.ReturnPropertyQuota)
	actions := p.ConversionSpec.ConversionActions
	if actions == nil {
		actions = []string{}
	}
	spec := map[string]any{"conversionActions": actions}
	if p.ConversionSpec.AttributionModel != "" {
		spec["attributionModel"] = p.ConversionSpec.AttributionModel
	}
	body["conversionSpec"] = spec
	var rep map[string]any
	if err := s.apiPost(ctx, s.dataAlphaBase, "/"+path+":runReport", body, &rep); err != nil {
		return mcpgateway.Result{}, err
	}
	rowCount, truncated := trimRowsField(rep, s.maxRows)
	rep = snakeMap(rep)
	return mcpgateway.Result{Data: rep, Truncated: truncated, RowCount: rowCount}, nil
}

func (s *Service) runFunnelReport(ctx context.Context, raw json.RawMessage) (mcpgateway.Result, error) {
	var p runFunnelArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if err := p.validate(); err != nil {
		return mcpgateway.Result{}, err
	}
	path, perr := propertyPath(p.PropertyID)
	if perr != nil {
		return mcpgateway.Result{}, perr
	}
	body, berr := funnelBody(&p)
	if berr != nil {
		return mcpgateway.Result{}, berr
	}
	var rep map[string]any
	if err := s.apiPost(ctx, s.dataAlphaBase, "/"+path+":runFunnelReport", body, &rep); err != nil {
		return mcpgateway.Result{}, err
	}
	rep = snakeMap(rep)
	rowCount, truncated := trimFunnelRows(rep, s.maxRows)
	return mcpgateway.Result{Data: rep, Truncated: truncated, RowCount: rowCount}, nil
}

func funnelBody(p *runFunnelArgs) (map[string]any, *mcpgateway.Error) {
	steps := make([]map[string]any, 0, len(p.FunnelSteps))
	for i, step := range p.FunnelSteps {
		name := step.Name
		if name == "" {
			name = fmt.Sprintf("Step %d", i+1)
		}
		var filter any
		if step.FilterExpression != nil {
			filter = camelize(step.FilterExpression)
		} else if strings.TrimSpace(step.Event) != "" {
			filter = map[string]any{
				"funnelEventFilter": map[string]any{"eventName": strings.TrimSpace(step.Event)},
			}
		} else {
			return nil, mcpgateway.InvalidParams("funnel_steps[%d] needs filter_expression or event", i)
		}
		steps = append(steps, map[string]any{
			"name":             name,
			"filterExpression": filter,
		})
	}
	body := map[string]any{
		"funnel": map[string]any{"steps": steps},
	}
	if p.DateRanges != nil {
		body["dateRanges"] = dateRangesJSON(p.DateRanges)
	}
	if p.FunnelBreakdown != nil {
		body["funnelBreakdown"] = map[string]any{
			"breakdownDimension": map[string]any{"name": strings.TrimSpace(p.FunnelBreakdown.BreakdownDimension)},
		}
	}
	if p.FunnelNextAction != nil {
		next := map[string]any{
			"nextActionDimension": map[string]any{"name": strings.TrimSpace(p.FunnelNextAction.NextActionDimension)},
		}
		if p.FunnelNextAction.Limit > 0 {
			next["limit"] = p.FunnelNextAction.Limit
		}
		body["funnelNextAction"] = next
	}
	if len(p.Segments) > 0 {
		body["segments"] = camelize(p.Segments)
	}
	if p.ReturnPropertyQuota {
		body["returnPropertyQuota"] = true
	}
	return body, nil
}

// ---- response helpers ----

func trimRowsField(container map[string]any, max int) (int, bool) {
	v, ok := container["rows"]
	if !ok {
		return 0, false
	}
	rows, ok := v.([]any)
	if !ok {
		return 0, false
	}
	if len(rows) > max {
		container["rows"] = rows[:max]
		return max, true
	}
	return len(rows), false
}

func trimFunnelRows(rep map[string]any, max int) (int, bool) {
	rowCount := 0
	truncated := false
	if table, ok := rep["funnel_table"].(map[string]any); ok {
		n, truncatedTable := trimRowsField(table, max)
		rowCount = n
		truncated = truncated || truncatedTable
	}
	if viz, ok := rep["funnel_visualization"].(map[string]any); ok {
		_, truncatedViz := trimRowsField(viz, max)
		truncated = truncated || truncatedViz
	}
	return rowCount, truncated
}

// snakeValue mirrors the upstream analytics-mcp proto_to_dict output, which
// preserves protobuf snake_case field names for every nested message.
func snakeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[snakeKey(k)] = snakeValue(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = snakeValue(val)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = snakeValue(val)
		}
		return out
	default:
		return v
	}
}

func snakeMap(m map[string]any) map[string]any {
	if m == nil {
		return map[string]any{}
	}
	out, _ := snakeValue(m).(map[string]any)
	return out
}

func snakeKey(k string) string {
	var b strings.Builder
	for i, r := range k {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			b.WriteRune(unicode.ToLower(r))
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
