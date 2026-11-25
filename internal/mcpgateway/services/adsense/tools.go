// Package adsense exposes the nine AdSense read tools. The five AdSense write
// methods exist only as placeholders by design v5 §1.3/§5.3: the deployed ADC
// is readonly and adunits.create/patch additionally require an AdSense for
// Platforms project. Nothing in this package can POST/PATCH/DELETE upstream.
package adsense

import (
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

const (
	ServiceID   = "adsense"
	DisplayName = "AdSense"
	Version     = "1.0.0"
	Prefix      = "adsense"
)

var accountRe = regexp.MustCompile(`^accounts/pub-[A-Za-z0-9]+$`)

// Tool names (read, exposed).
const (
	ToolListAccounts    = "list_accounts"
	ToolEarningsSummary = "earnings_summary"
	ToolGenerateReport  = "generate_report"
	ToolComparePeriods  = "compare_periods"
	ToolListSites       = "list_sites"
	ToolListAlerts      = "list_alerts"
	ToolListPolicy      = "list_policy_issues"
	ToolListPayments    = "list_payments"
	ToolListAdUnits     = "list_ad_units"
)

// AdSense write placeholders (design v5 §5.3: present in the upstream API but
// deliberately NOT exposed by Tools()). Kept here so the non-implementation is
// explicit and greppable.
const (
	WriteCreateAdUnit        = "create_ad_unit"
	WritePatchAdUnit         = "patch_ad_unit"
	WriteCreateCustomChannel = "create_custom_channel"
	WriteDeleteCustomChannel = "delete_custom_channel"
	WritePatchCustomChannel  = "patch_custom_channel"
)

// WriteToolNames lists the five placeholders for admin/docs purposes.
func WriteToolNames() []string {
	return []string{
		WriteCreateAdUnit, WritePatchAdUnit, WriteCreateCustomChannel,
		WriteDeleteCustomChannel, WritePatchCustomChannel,
	}
}

// Tools returns the exposed read-only tools. Write placeholders are never
// returned: AdSense writes are disabled until a full adsense-scope ADC and an
// AFP project are confirmed.
func (s *Service) Tools() []mcpgateway.Tool {
	return []mcpgateway.Tool{
		readTool(ToolListAccounts, "List the AdSense accounts visible to this credential.",
			mcpgateway.Schema(map[string]any{}), validateEmpty),
		readTool(ToolEarningsSummary, "Earnings/impressions/clicks summary for today, yesterday, last 7 days, this month and last month. The most recent day is provisional.",
			mcpgateway.Schema(map[string]any{
				"account": mcpgateway.Prop("string", "Optional account name (accounts/pub-...); defaults to the first account."),
			}), validateAccountArg),
		readTool(ToolGenerateReport, "Generate an AdSense report with whitelisted dimensions/metrics, dates or dateRange, filters, orderBy and limit (<=1000).",
			mcpgateway.Schema(map[string]any{
				"account":    mcpgateway.Prop("string", "Optional account name; defaults to the first account."),
				"startDate":  mcpgateway.Prop("string", "Start date YYYY-MM-DD (with endDate)."),
				"endDate":    mcpgateway.Prop("string", "End date YYYY-MM-DD (with startDate)."),
				"dateRange":  mcpgateway.EnumProp("Predefined range when startDate/endDate are omitted.", dateRanges...),
				"dimensions": mcpgateway.ArrayProp("string", "Whitelisted dimension names, e.g. DATE, COUNTRY_NAME, AD_UNIT_NAME."),
				"metrics":    mcpgateway.ArrayProp("string", "Whitelisted metric names; defaults to the standard six."),
				"filters":    mcpgateway.ArrayProp("string", "Filters like COUNTRY_NAME==US (comma-free)."),
				"orderBy":    mcpgateway.Prop("string", "Whitelisted dimension/metric, optionally prefixed with '-'."),
				"limit":      mcpgateway.Prop("integer", "Row limit 1-1000 (default max_rows_per_call)."),
			}), validateReportArg),
		readTool(ToolComparePeriods, "Compare two explicit date ranges: absolute totals for both periods plus the delta and base.",
			mcpgateway.Schema(map[string]any{
				"account":      mcpgateway.Prop("string", "Optional account name."),
				"period1Start": mcpgateway.Prop("string", "First period start YYYY-MM-DD."),
				"period1End":   mcpgateway.Prop("string", "First period end YYYY-MM-DD."),
				"period2Start": mcpgateway.Prop("string", "Second period start YYYY-MM-DD."),
				"period2End":   mcpgateway.Prop("string", "Second period end YYYY-MM-DD."),
				"dimensions":   mcpgateway.ArrayProp("string", "Whitelisted dimensions."),
				"metrics":      mcpgateway.ArrayProp("string", "Whitelisted metrics."),
			}, "period1Start", "period1End", "period2Start", "period2End"), validateCompareArg),
		readTool(ToolListSites, "List sites for an AdSense account.", accountSchema(), validateAccountArg),
		readTool(ToolListAlerts, "List alerts for an AdSense account.", accountSchema(), validateAccountArg),
		readTool(ToolListPolicy, "List policy issues for an AdSense account.", accountSchema(), validateAccountArg),
		readTool(ToolListPayments, "List payments for an AdSense account.", accountSchema(), validateAccountArg),
		readTool(ToolListAdUnits, "List ad units across the account's ad clients.", accountSchema(), validateAccountArg),
	}
}

func readTool(name, desc string, schema json.RawMessage, validate func(json.RawMessage) error) mcpgateway.Tool {
	return mcpgateway.Tool{
		Name: name, Description: desc, Risk: mcpgateway.RiskRead, ReadOnly: true,
		OpenWorld: true, InputSchema: schema, Validate: validate,
	}
}

func accountSchema() json.RawMessage {
	return mcpgateway.Schema(map[string]any{
		"account": mcpgateway.Prop("string", "Optional account name; defaults to the first account."),
	})
}

var dateRanges = []string{
	"TODAY", "YESTERDAY", "LAST_7_DAYS", "LAST_30_DAYS", "MONTH_TO_DATE", "YEAR_TO_DATE",
}

var allowedDimensions = map[string]bool{
	"DATE": true, "WEEK": true, "MONTH": true, "ACCOUNT_NAME": true, "AD_CLIENT_ID": true,
	"PRODUCT_NAME": true, "PRODUCT_CODE": true, "AD_UNIT_NAME": true, "AD_UNIT_ID": true,
	"AD_UNIT_SIZE_NAME": true, "AD_UNIT_SIZE_CODE": true,
	"CUSTOM_CHANNEL_NAME": true, "CUSTOM_CHANNEL_ID": true,
	"OWNED_SITE_DOMAIN_NAME": true, "OWNED_SITE_ID": true,
	"URL_CHANNEL_NAME": true, "URL_CHANNEL_ID": true,
	"BUYER_NETWORK_NAME": true, "BUYER_NETWORK_ID": true,
	"BID_TYPE_NAME": true, "BID_TYPE_CODE": true,
	"CREATIVE_SIZE_NAME": true, "CREATIVE_SIZE_CODE": true,
	"DOMAIN_NAME": true, "DOMAIN_CODE": true,
	"COUNTRY_NAME": true, "COUNTRY_CODE": true,
	"PLATFORM_TYPE_NAME": true, "PLATFORM_TYPE_CODE": true,
	"TARGETING_TYPE_NAME": true, "TARGETING_TYPE_CODE": true,
	"CONTENT_PLATFORM_NAME": true, "CONTENT_PLATFORM_CODE": true,
	"AD_PLACEMENT_NAME": true, "AD_PLACEMENT_CODE": true,
	"REQUESTED_AD_TYPE_NAME": true, "REQUESTED_AD_TYPE_CODE": true,
	"SERVED_AD_TYPE_NAME": true, "SERVED_AD_TYPE_CODE": true,
	"AD_FORMAT_NAME": true, "AD_FORMAT_CODE": true,
	"CUSTOM_SEARCH_STYLE_NAME": true, "CUSTOM_SEARCH_STYLE_ID": true,
	"DOMAIN_REGISTRANT":          true,
	"WEBSEARCH_RESULT_TYPE_NAME": true, "WEBSEARCH_RESULT_TYPE_CODE": true,
}

var allowedMetrics = map[string]bool{
	"PAGE_VIEWS": true, "AD_REQUESTS": true, "MATCHED_AD_REQUESTS": true,
	"TOTAL_IMPRESSIONS": true, "IMPRESSIONS": true, "INDIVIDUAL_AD_IMPRESSIONS": true, "CLICKS": true,
	"PAGE_VIEWS_SPAM_RATIO": true, "AD_REQUESTS_SPAM_RATIO": true, "MATCHED_AD_REQUESTS_SPAM_RATIO": true,
	"IMPRESSIONS_SPAM_RATIO": true, "INDIVIDUAL_AD_IMPRESSIONS_SPAM_RATIO": true, "CLICKS_SPAM_RATIO": true,
	"AD_REQUESTS_COVERAGE": true,
	"PAGE_VIEWS_CTR":       true, "AD_REQUESTS_CTR": true, "MATCHED_AD_REQUESTS_CTR": true,
	"IMPRESSIONS_CTR": true, "INDIVIDUAL_AD_IMPRESSIONS_CTR": true,
	"ACTIVE_VIEW_MEASURABILITY": true, "ACTIVE_VIEW_VIEWABILITY": true, "ACTIVE_VIEW_TIME": true,
	"ESTIMATED_EARNINGS": true,
	"PAGE_VIEWS_RPM":     true, "AD_REQUESTS_RPM": true, "MATCHED_AD_REQUESTS_RPM": true,
	"IMPRESSIONS_RPM": true, "INDIVIDUAL_AD_IMPRESSIONS_RPM": true,
	"COST_PER_CLICK": true, "ADS_PER_IMPRESSION": true, "TOTAL_EARNINGS": true,
	"WEBSEARCH_RESULT_PAGES": true, "FUNNEL_REQUESTS": true, "FUNNEL_IMPRESSIONS": true,
	"FUNNEL_CLICKS": true, "FUNNEL_RPM": true,
}

var defaultMetrics = []string{
	"ESTIMATED_EARNINGS", "IMPRESSIONS", "CLICKS", "PAGE_VIEWS", "PAGE_VIEWS_CTR", "PAGE_VIEWS_RPM",
}

func validateEmpty(raw json.RawMessage) error {
	var p struct{}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	return nil
}

func validateAccountArg(raw json.RawMessage) error {
	var p struct {
		Account string `json:"account"`
	}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if p.Account != "" && !accountRe.MatchString(normalizeAccount(p.Account)) {
		return mcpgateway.InvalidParams("account must be an AdSense account (accounts/pub-...)")
	}
	return nil
}

type reportArgs struct {
	Account    string   `json:"account"`
	StartDate  string   `json:"startDate"`
	EndDate    string   `json:"endDate"`
	DateRange  string   `json:"dateRange"`
	Dimensions []string `json:"dimensions"`
	Metrics    []string `json:"metrics"`
	Filters    []string `json:"filters"`
	OrderBy    string   `json:"orderBy"`
	Limit      int      `json:"limit"`
}

func validateReportArg(raw json.RawMessage) error {
	var p reportArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if p.Account != "" && !accountRe.MatchString(normalizeAccount(p.Account)) {
		return mcpgateway.InvalidParams("account must be an AdSense account (accounts/pub-...)")
	}
	if p.DateRange != "" {
		if p.StartDate != "" || p.EndDate != "" {
			return mcpgateway.InvalidParams("supply either dateRange or startDate+endDate, not both")
		}
		if !contains(dateRanges, p.DateRange) {
			return mcpgateway.InvalidParams("unsupported dateRange %q", p.DateRange)
		}
	} else {
		if p.StartDate == "" || p.EndDate == "" {
			return mcpgateway.InvalidParams("generate_report needs startDate+endDate or dateRange")
		}
		if _, _, err := mcpgateway.ValidateDateRange(p.StartDate, p.EndDate); err != nil {
			return mcpgateway.InvalidParams("%s", err.Error())
		}
	}
	if err := checkNames(p.Dimensions, allowedDimensions, "dimension"); err != nil {
		return err
	}
	if err := checkNames(p.Metrics, allowedMetrics, "metric"); err != nil {
		return err
	}
	if p.Limit < 0 || p.Limit > 1000 {
		return mcpgateway.InvalidParams("limit must be 1-1000")
	}
	for i, f := range p.Filters {
		if !validFilter(f) {
			return mcpgateway.InvalidParams("filters[%d] must be FIELD<op>value", i)
		}
	}
	if p.OrderBy != "" {
		name := strings.TrimPrefix(p.OrderBy, "-")
		if !allowedDimensions[name] && !allowedMetrics[name] {
			return mcpgateway.InvalidParams("orderBy %q is not a whitelisted dimension/metric", p.OrderBy)
		}
	}
	return nil
}

type compareArgs struct {
	Account      string   `json:"account"`
	Period1Start string   `json:"period1Start"`
	Period1End   string   `json:"period1End"`
	Period2Start string   `json:"period2Start"`
	Period2End   string   `json:"period2End"`
	Dimensions   []string `json:"dimensions"`
	Metrics      []string `json:"metrics"`
}

func validateCompareArg(raw json.RawMessage) error {
	var p compareArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if p.Account != "" && !accountRe.MatchString(normalizeAccount(p.Account)) {
		return mcpgateway.InvalidParams("account must be an AdSense account (accounts/pub-...)")
	}
	if _, _, err := mcpgateway.ValidateDateRange(p.Period1Start, p.Period1End); err != nil {
		return mcpgateway.InvalidParams("period1: %s", err.Error())
	}
	if _, _, err := mcpgateway.ValidateDateRange(p.Period2Start, p.Period2End); err != nil {
		return mcpgateway.InvalidParams("period2: %s", err.Error())
	}
	if err := checkNames(p.Dimensions, allowedDimensions, "dimension"); err != nil {
		return err
	}
	if err := checkNames(p.Metrics, allowedMetrics, "metric"); err != nil {
		return err
	}
	return nil
}

func checkNames(names []string, allowed map[string]bool, kind string) error {
	for _, n := range names {
		if !allowed[n] {
			return mcpgateway.InvalidParams("%s %q is not whitelisted", kind, n)
		}
	}
	return nil
}

var filterRe = regexp.MustCompile(`^([A-Z][A-Z0-9_]*)(==|=@)([^,]{1,128})$`)

func validFilter(s string) bool {
	m := filterRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return false
	}
	return allowedDimensions[m[1]]
}

func contains(list []string, v string) bool {
	for _, s := range list {
		if s == v {
			return true
		}
	}
	return false
}

func normalizeAccount(account string) string {
	account = strings.TrimSpace(account)
	if account == "" {
		return ""
	}
	if !strings.HasPrefix(account, "accounts/") {
		return "accounts/" + account
	}
	return account
}

func defaultMetricsOr(metrics []string) []string {
	if len(metrics) == 0 {
		return append([]string(nil), defaultMetrics...)
	}
	return metrics
}

// credentialError is a stable no-secret error for credential problems.
func credentialError() *mcpgateway.Error {
	return &mcpgateway.Error{
		Code: mcpgateway.CodeCredentialMissing, Message: "AdSense credential is not available", HTTPStatus: http.StatusOK,
	}
}

func asInt(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case int:
		return float64(n), true
	default:
		return 0, false
	}
}

func describePeriod(start, end string) string {
	return fmt.Sprintf("%s..%s", start, end)
}
