// Package ga exposes the nine read-only Google Analytics tools. The upstream
// analytics-mcp server also ships create_property_annotation, but GA writes are
// out of scope here: it is deliberately not implemented, not exposed, and not
// reachable through this package. All upstream calls are read-only Admin API
// (v1alpha) / Data API (v1beta and v1alpha for funnel/conversions) requests.
package ga

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

const (
	ServiceID   = "ga"
	DisplayName = "Google Analytics"
	Version     = "1.0.0"
	Prefix      = "ga"
)

// Tool names are the upstream analytics-mcp read tool names, used verbatim.
const (
	ToolGetAccountSummaries        = "get_account_summaries"
	ToolGetPropertyDetails         = "get_property_details"
	ToolListGoogleAdsLinks         = "list_google_ads_links"
	ToolListPropertyAnnotations    = "list_property_annotations"
	ToolRunReport                  = "run_report"
	ToolRunFunnelReport            = "run_funnel_report"
	ToolRunConversionsReport       = "run_conversions_report"
	ToolGetCustomDimensionsMetrics = "get_custom_dimensions_and_metrics"
	ToolRunRealtimeReport          = "run_realtime_report"
)

// ReadToolNames returns the nine exposed read tools in stable order.
func ReadToolNames() []string {
	return []string{
		ToolGetAccountSummaries,
		ToolGetPropertyDetails,
		ToolListGoogleAdsLinks,
		ToolListPropertyAnnotations,
		ToolRunReport,
		ToolRunFunnelReport,
		ToolRunConversionsReport,
		ToolGetCustomDimensionsMetrics,
		ToolRunRealtimeReport,
	}
}

// Tools returns the exposed read-only tools. Every tool is RiskRead and
// ReadOnly; there are no write tools or placeholders in this package.
func (s *Service) Tools() []mcpgateway.Tool {
	return []mcpgateway.Tool{
		readTool(ToolGetAccountSummaries,
			"Retrieve the Google Analytics accounts and properties visible to the ADC, all pages.",
			mcpgateway.Schema(map[string]any{}), validateNoParams),
		readTool(ToolGetPropertyDetails,
			"Return details for one Google Analytics property (Admin API v1alpha).",
			propertySchema(), validatePropertyArgs),
		readTool(ToolListGoogleAdsLinks,
			"List the Google Ads links for one property (Admin API v1alpha).",
			propertySchema(), validatePropertyArgs),
		readTool(ToolListPropertyAnnotations,
			"List reporting data annotations for one property (Admin API v1alpha reportingDataAnnotations).",
			propertySchema(), validatePropertyArgs),
		readTool(ToolRunReport,
			"Run a Data API v1beta report. Field names are snake_case as in the upstream analytics-mcp tool; the service translates them to the REST request.",
			reportSchema(true, true, false), validateRunReport),
		readTool(ToolRunFunnelReport,
			"Run a Data API v1alpha funnel report. Each step needs either filter_expression or event.",
			funnelSchema(), validateRunFunnel),
		readTool(ToolRunConversionsReport,
			"Run a Data API v1alpha conversions report with conversion_spec.",
			reportSchema(true, true, true), validateRunConversions),
		readTool(ToolGetCustomDimensionsMetrics,
			"Return the property's custom dimensions and custom metrics from the Data API metadata endpoint.",
			propertySchema(), validatePropertyArgs),
		readTool(ToolRunRealtimeReport,
			"Run a Data API v1beta realtime report (last 30 minutes). Realtime custom metrics are not available.",
			reportSchema(false, false, false), validateRunRealtime),
	}
}

func readTool(name, desc string, schema json.RawMessage, validate func(json.RawMessage) error) mcpgateway.Tool {
	return mcpgateway.Tool{
		Name: name, Description: desc, Risk: mcpgateway.RiskRead, ReadOnly: true,
		Idempotent: true, OpenWorld: true, InputSchema: schema, Validate: validate,
	}
}

// propertyIDProp accepts both upstream forms: a number or properties/<number>.
func propertyIDProp() map[string]any {
	return map[string]any{
		"description": "Google Analytics property ID: a number or 'properties/<number>'.",
		"anyOf": []any{
			map[string]any{"type": "string"},
			map[string]any{"type": "integer"},
		},
	}
}

func propertySchema() json.RawMessage {
	return mcpgateway.Schema(map[string]any{"property_id": propertyIDProp()}, "property_id")
}

// reportSchema builds the common run_report / run_realtime_report /
// run_conversions_report surface. Realtime has no date ranges, currency or
// conversion spec; conversions adds conversion_spec.
func reportSchema(withDateRanges, withCurrency, withConversionSpec bool) json.RawMessage {
	props := map[string]any{
		"property_id":           propertyIDProp(),
		"dimensions":            mcpgateway.ArrayProp("string", "Data API dimension API names, e.g. date, country, sessionSource."),
		"metrics":               mcpgateway.ArrayProp("string", "Data API metric API names, e.g. activeUsers, sessions, totalRevenue."),
		"dimension_filter":      filterProp("Data API FilterExpression applied to dimensions (snake_case field names)."),
		"metric_filter":         filterProp("Data API FilterExpression applied to metrics (snake_case field names)."),
		"order_bys":             orderBysProp(),
		"limit":                 mcpgateway.Prop("integer", "Maximum rows to return (positive, <=250000)."),
		"offset":                mcpgateway.Prop("integer", "Zero-based start row for pagination."),
		"return_property_quota": mcpgateway.Prop("boolean", "Include property quota details in the response."),
	}
	required := []string{"property_id", "dimensions", "metrics"}
	if withDateRanges {
		props["date_ranges"] = dateRangesProp()
		required = append(required, "date_ranges")
	}
	if withCurrency {
		props["currency_code"] = mcpgateway.Prop("string", "Optional ISO4217 currency code, e.g. USD.")
	}
	if withConversionSpec {
		props["conversion_spec"] = conversionSpecProp()
		required = append(required, "conversion_spec")
	}
	return mcpgateway.Schema(props, required...)
}

func funnelSchema() json.RawMessage {
	return mcpgateway.Schema(map[string]any{
		"property_id": propertyIDProp(),
		"funnel_steps": map[string]any{
			"type":        "array",
			"description": "Funnel steps; each has name plus either filter_expression (object) or event (event name).",
			"items": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"name":              mcpgateway.Prop("string", "Display name for the step."),
					"event":             mcpgateway.Prop("string", "Event name for a simple event-based step."),
					"filter_expression": map[string]any{"type": "object", "description": "FunnelFilterExpression (snake_case)."},
				},
				"required":             []string{},
				"additionalProperties": false,
			},
		},
		"date_ranges": dateRangesProp(),
		"funnel_breakdown": map[string]any{
			"type": "object", "description": "Optional breakdown dimension, e.g. {\"breakdown_dimension\":\"deviceCategory\"}.",
			"properties": map[string]any{
				"breakdown_dimension": mcpgateway.Prop("string", "Dimension API name to break the funnel down by."),
			},
			"required": []string{"breakdown_dimension"}, "additionalProperties": false,
		},
		"funnel_next_action": map[string]any{
			"type": "object", "description": "Optional next-action analysis, e.g. {\"next_action_dimension\":\"eventName\",\"limit\":5}.",
			"properties": map[string]any{
				"next_action_dimension": mcpgateway.Prop("string", "Dimension API name for the next action."),
				"limit":                 mcpgateway.Prop("integer", "Maximum next-action rows."),
			},
			"required": []string{"next_action_dimension"}, "additionalProperties": false,
		},
		"segments":              mcpgateway.ArrayProp("object", "Optional Data API segments."),
		"return_property_quota": mcpgateway.Prop("boolean", "Include property quota details in the response."),
	}, "property_id", "funnel_steps")
}

func dateRangesProp() map[string]any {
	return map[string]any{
		"type":        "array",
		"description": "One or more Data API DateRange objects (start_date/end_date, optional name). Relative values such as today, yesterday and 30daysAgo are accepted by the API.",
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"start_date": mcpgateway.Prop("string", "Inclusive start date, YYYY-MM-DD or a relative value."),
				"end_date":   mcpgateway.Prop("string", "Inclusive end date, YYYY-MM-DD or a relative value."),
				"name":       mcpgateway.Prop("string", "Optional display name for the range."),
			},
			"required": []string{"start_date", "end_date"}, "additionalProperties": false,
		},
	}
}

func filterProp(desc string) map[string]any {
	return map[string]any{"type": "object", "description": desc, "additionalProperties": true}
}

func orderBysProp() map[string]any {
	return map[string]any{
		"type": "array", "description": "Data API OrderBy objects (snake_case), e.g. [{\"metric\":{\"metric_name\":\"sessions\"},\"desc\":true}].",
		"items": map[string]any{"type": "object", "additionalProperties": true},
	}
}

func conversionSpecProp() map[string]any {
	return map[string]any{
		"type":        "object",
		"description": "ConversionSpec, e.g. {\"conversion_actions\":[],\"attribution_model\":\"DATA_DRIVEN\"}.",
		"properties": map[string]any{
			"conversion_actions": mcpgateway.ArrayProp("string", "Conversion action resource names; empty means all actions."),
			"attribution_model":  mcpgateway.EnumProp("Attribution model.", "DATA_DRIVEN", "LAST_CLICK", "ATTRIBUTION_MODEL_UNSPECIFIED"),
		},
		"required": []string{}, "additionalProperties": false,
	}
}

// ---- argument types ----

// propertyID accepts the upstream int-or-string form.
type propertyID string

func (p *propertyID) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var s string
		if err := json.Unmarshal(b, &s); err != nil {
			return err
		}
		*p = propertyID(s)
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("property_id must be a number or string")
	}
	*p = propertyID(n.String())
	return nil
}

type propertyArgs struct {
	PropertyID propertyID `json:"property_id"`
}

type dateRangeArg struct {
	StartDate string `json:"start_date"`
	EndDate   string `json:"end_date"`
	Name      string `json:"name"`
}

type runReportArgs struct {
	PropertyID          propertyID       `json:"property_id"`
	DateRanges          []dateRangeArg   `json:"date_ranges"`
	Dimensions          []string         `json:"dimensions"`
	Metrics             []string         `json:"metrics"`
	DimensionFilter     map[string]any   `json:"dimension_filter"`
	MetricFilter        map[string]any   `json:"metric_filter"`
	OrderBys            []map[string]any `json:"order_bys"`
	Limit               int              `json:"limit"`
	Offset              int              `json:"offset"`
	CurrencyCode        string           `json:"currency_code"`
	ReturnPropertyQuota bool             `json:"return_property_quota"`
}

type runRealtimeArgs struct {
	PropertyID          propertyID       `json:"property_id"`
	Dimensions          []string         `json:"dimensions"`
	Metrics             []string         `json:"metrics"`
	DimensionFilter     map[string]any   `json:"dimension_filter"`
	MetricFilter        map[string]any   `json:"metric_filter"`
	OrderBys            []map[string]any `json:"order_bys"`
	Limit               int              `json:"limit"`
	Offset              int              `json:"offset"`
	ReturnPropertyQuota bool             `json:"return_property_quota"`
}

type runConversionsArgs struct {
	PropertyID          propertyID       `json:"property_id"`
	DateRanges          []dateRangeArg   `json:"date_ranges"`
	Dimensions          []string         `json:"dimensions"`
	Metrics             []string         `json:"metrics"`
	ConversionSpec      map[string]any   `json:"conversion_spec"`
	DimensionFilter     map[string]any   `json:"dimension_filter"`
	MetricFilter        map[string]any   `json:"metric_filter"`
	OrderBys            []map[string]any `json:"order_bys"`
	Limit               int              `json:"limit"`
	Offset              int              `json:"offset"`
	CurrencyCode        string           `json:"currency_code"`
	ReturnPropertyQuota bool             `json:"return_property_quota"`
}

type funnelStepArg struct {
	Name             string         `json:"name"`
	Event            string         `json:"event"`
	FilterExpression map[string]any `json:"filter_expression"`
}

type funnelBreakdownArg struct {
	BreakdownDimension string `json:"breakdown_dimension"`
}

type funnelNextActionArg struct {
	NextActionDimension string `json:"next_action_dimension"`
	Limit               int    `json:"limit"`
}

type runFunnelArgs struct {
	PropertyID          propertyID           `json:"property_id"`
	FunnelSteps         []funnelStepArg      `json:"funnel_steps"`
	DateRanges          []dateRangeArg       `json:"date_ranges"`
	FunnelBreakdown     *funnelBreakdownArg  `json:"funnel_breakdown"`
	FunnelNextAction    *funnelNextActionArg `json:"funnel_next_action"`
	Segments            []map[string]any     `json:"segments"`
	ReturnPropertyQuota bool                 `json:"return_property_quota"`
}

// ---- validators ----

var propertyNumberRe = regexp.MustCompile(`^[0-9]+$`)

func normalizePropertyID(raw propertyID) (string, error) {
	s := strings.TrimSpace(string(raw))
	if s == "" {
		return "", mcpgateway.InvalidParams("property_id is required")
	}
	num := s
	if strings.HasPrefix(num, "properties/") {
		num = strings.TrimPrefix(num, "properties/")
	}
	if !propertyNumberRe.MatchString(num) {
		return "", mcpgateway.InvalidParams("property_id must be a number or 'properties/<number>'")
	}
	return num, nil
}

// requireFields checks presence before decoding. Empty arrays satisfy a
// required field (GA allows metrics-only reports with no dimensions), while a
// missing field does not.
func requireFields(raw json.RawMessage, fields ...string) error {
	if len(strings.TrimSpace(string(raw))) == 0 {
		raw = json.RawMessage(`{}`)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return mcpgateway.InvalidParams("params must be a JSON object")
	}
	for _, field := range fields {
		if _, ok := m[field]; !ok {
			return mcpgateway.InvalidParams("%s is required", field)
		}
	}
	return nil
}

func validateNoParams(raw json.RawMessage) error {
	var p struct{}
	return mcpgateway.DecodeStrict(raw, &p)
}

func validatePropertyArgs(raw json.RawMessage) error {
	var p propertyArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	_, err := normalizePropertyID(p.PropertyID)
	return err
}

func validateRunReport(raw json.RawMessage) error {
	if err := requireFields(raw, "property_id", "date_ranges", "dimensions", "metrics"); err != nil {
		return err
	}
	var p runReportArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	return p.validate()
}

func validateRunRealtime(raw json.RawMessage) error {
	if err := requireFields(raw, "property_id", "dimensions", "metrics"); err != nil {
		return err
	}
	var p runRealtimeArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	return p.validate()
}

func validateRunConversions(raw json.RawMessage) error {
	if err := requireFields(raw, "property_id", "date_ranges", "dimensions", "metrics", "conversion_spec"); err != nil {
		return err
	}
	var p runConversionsArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	return p.validate()
}

func validateRunFunnel(raw json.RawMessage) error {
	if err := requireFields(raw, "property_id", "funnel_steps"); err != nil {
		return err
	}
	var p runFunnelArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	return p.validate()
}

func (p *runReportArgs) validate() error {
	if _, err := normalizePropertyID(p.PropertyID); err != nil {
		return err
	}
	if err := validateDateRanges(p.DateRanges); err != nil {
		return err
	}
	if p.Dimensions == nil {
		return mcpgateway.InvalidParams("dimensions must be an array")
	}
	if err := validateNames(p.Dimensions, "dimensions"); err != nil {
		return err
	}
	if err := validateMetrics(p.Metrics); err != nil {
		return err
	}
	return validateWindow(p.Limit, p.Offset)
}

func (p *runRealtimeArgs) validate() error {
	if _, err := normalizePropertyID(p.PropertyID); err != nil {
		return err
	}
	if p.Dimensions == nil {
		return mcpgateway.InvalidParams("dimensions must be an array")
	}
	if err := validateNames(p.Dimensions, "dimensions"); err != nil {
		return err
	}
	if err := validateMetrics(p.Metrics); err != nil {
		return err
	}
	return validateWindow(p.Limit, p.Offset)
}

func (p *runConversionsArgs) validate() error {
	if _, err := normalizePropertyID(p.PropertyID); err != nil {
		return err
	}
	if err := validateDateRanges(p.DateRanges); err != nil {
		return err
	}
	if p.Dimensions == nil {
		return mcpgateway.InvalidParams("dimensions must be an array")
	}
	if err := validateNames(p.Dimensions, "dimensions"); err != nil {
		return err
	}
	if err := validateMetrics(p.Metrics); err != nil {
		return err
	}
	if p.ConversionSpec == nil {
		return mcpgateway.InvalidParams("conversion_spec is required")
	}
	return validateWindow(p.Limit, p.Offset)
}

func (p *runFunnelArgs) validate() error {
	if _, err := normalizePropertyID(p.PropertyID); err != nil {
		return err
	}
	if len(p.FunnelSteps) == 0 {
		return mcpgateway.InvalidParams("funnel_steps must contain at least one step")
	}
	for i, step := range p.FunnelSteps {
		if step.FilterExpression == nil && strings.TrimSpace(step.Event) == "" {
			return mcpgateway.InvalidParams("funnel_steps[%d] needs filter_expression or event", i)
		}
	}
	if err := validateOptionalDateRanges(p.DateRanges); err != nil {
		return err
	}
	if p.FunnelBreakdown != nil && strings.TrimSpace(p.FunnelBreakdown.BreakdownDimension) == "" {
		return mcpgateway.InvalidParams("funnel_breakdown.breakdown_dimension is required")
	}
	if p.FunnelNextAction != nil {
		if strings.TrimSpace(p.FunnelNextAction.NextActionDimension) == "" {
			return mcpgateway.InvalidParams("funnel_next_action.next_action_dimension is required")
		}
		if p.FunnelNextAction.Limit < 0 {
			return mcpgateway.InvalidParams("funnel_next_action.limit must be >= 0")
		}
	}
	return nil
}

func validateDateRanges(rs []dateRangeArg) error {
	if len(rs) == 0 {
		return mcpgateway.InvalidParams("date_ranges must contain at least one range")
	}
	return validateOptionalDateRanges(rs)
}

func validateOptionalDateRanges(rs []dateRangeArg) error {
	for i, r := range rs {
		if strings.TrimSpace(r.StartDate) == "" || strings.TrimSpace(r.EndDate) == "" {
			return mcpgateway.InvalidParams("date_ranges[%d] needs start_date and end_date", i)
		}
	}
	return nil
}

// validateNames allows an empty list (dimensions are optional in a totals-only
// report) but rejects empty strings.
func validateNames(names []string, field string) error {
	for i, name := range names {
		if strings.TrimSpace(name) == "" {
			return mcpgateway.InvalidParams("%s[%d] must not be empty", field, i)
		}
	}
	return nil
}

func validateMetrics(metrics []string) error {
	if len(metrics) == 0 {
		return mcpgateway.InvalidParams("metrics must contain at least one name")
	}
	return validateNames(metrics, "metrics")
}

func validateWindow(limit, offset int) error {
	if limit < 0 || limit > 250000 {
		return mcpgateway.InvalidParams("limit must be between 1 and 250000 (0 means upstream default)")
	}
	if offset < 0 {
		return mcpgateway.InvalidParams("offset must be >= 0")
	}
	return nil
}

// ---- request rendering helpers ----

func propertyPath(raw propertyID) (string, *mcpgateway.Error) {
	num, err := normalizePropertyID(raw)
	if err != nil {
		return "", mcpgateway.AsError(err)
	}
	return "properties/" + num, nil
}

func dateRangesJSON(rs []dateRangeArg) []map[string]any {
	out := make([]map[string]any, 0, len(rs))
	for _, r := range rs {
		m := map[string]any{"startDate": r.StartDate, "endDate": r.EndDate}
		if r.Name != "" {
			m["name"] = r.Name
		}
		out = append(out, m)
	}
	return out
}

func dimensionsJSON(names []string) []map[string]any {
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"name": name})
	}
	return out
}

// camelize converts the upstream snake_case protobuf field names in user
// supplied filter/order/segment objects to the lowerCamelCase JSON names the
// REST endpoints use. Values are left untouched.
func camelize(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			out[camelKey(k)] = camelize(val)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = camelize(val)
		}
		return out
	case []map[string]any:
		out := make([]any, len(x))
		for i, val := range x {
			out[i] = camelize(val)
		}
		return out
	default:
		return v
	}
}

func camelKey(k string) string {
	if !strings.Contains(k, "_") {
		if k == "" {
			return k
		}
		r := []rune(k)
		r[0] = unicode.ToLower(r[0])
		return string(r)
	}
	parts := strings.Split(k, "_")
	var b strings.Builder
	b.WriteString(strings.ToLower(parts[0]))
	for _, part := range parts[1:] {
		if part == "" {
			continue
		}
		b.WriteString(strings.ToUpper(part[:1]))
		b.WriteString(part[1:])
	}
	return b.String()
}

func optionalInt(v int) (int, bool) {
	if v == 0 {
		return 0, false
	}
	return v, true
}

func addWindow(body map[string]any, limit, offset int, currency string) {
	if n, ok := optionalInt(limit); ok {
		body["limit"] = n
	}
	if n, ok := optionalInt(offset); ok {
		body["offset"] = n
	}
	if currency != "" {
		body["currencyCode"] = currency
	}
}

func reportBody(dateRanges []dateRangeArg, dimensions, metrics []string,
	dimFilter, metFilter map[string]any, orderBys []map[string]any,
	limit, offset int, currency string, quota bool) map[string]any {
	body := map[string]any{
		"dimensions": dimensionsJSON(dimensions),
		"metrics":    dimensionsJSON(metrics),
	}
	if dateRanges != nil {
		body["dateRanges"] = dateRangesJSON(dateRanges)
	}
	if dimFilter != nil {
		body["dimensionFilter"] = camelize(dimFilter)
	}
	if metFilter != nil {
		body["metricFilter"] = camelize(metFilter)
	}
	if len(orderBys) > 0 {
		body["orderBys"] = camelize(orderBys)
	}
	addWindow(body, limit, offset, currency)
	if quota {
		body["returnPropertyQuota"] = true
	}
	return body
}
