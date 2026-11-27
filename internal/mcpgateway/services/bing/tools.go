// Package bing implements the Bing Webmaster Tools MCP service: 14 read tools
// and six write tools (four low-risk direct; add_blocked_url and
// remove_blocked_url high-risk via the gateway approval state machine).
// Upstream is the JSON/HTTP API at
// https://ssl.bing.com/webmaster/api.svc/json/<Method>.
package bing

import (
	"encoding/json"
	"net/url"
	"regexp"
	"strings"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

const (
	ServiceID   = "bing-webmaster"
	DisplayName = "Bing Webmaster"
	Version     = "1.0.0"
	Prefix      = "bing"
)

const (
	ToolListSites      = "list_sites"
	ToolQueryStats     = "query_stats"
	ToolPageStats      = "page_stats"
	ToolRankTraffic    = "rank_traffic"
	ToolCrawlStats     = "crawl_stats"
	ToolCrawlIssues    = "crawl_issues"
	ToolListFeeds      = "list_feeds"
	ToolPageQueries    = "page_queries"
	ToolQueryPages     = "query_pages"
	ToolKeyword        = "keyword"
	ToolRelatedKeyword = "related_keywords"
	ToolKeywordStats   = "keyword_stats"

	ToolGetURLSubmissionQuota = "get_url_submission_quota"
	ToolGetBlockedURLs        = "get_blocked_urls"

	ToolSubmitURL        = "submit_url"
	ToolSubmitURLBatch   = "submit_url_batch"
	ToolSubmitFeed       = "submit_feed"
	ToolRemoveFeed       = "remove_feed"
	ToolAddBlockedURL    = "add_blocked_url"
	ToolRemoveBlockedURL = "remove_blocked_url"
)

var (
	countryRe = regexp.MustCompile(`^[A-Z]{2}$`)
	langRe    = regexp.MustCompile(`^[a-z]{2}$`)
	dateRe    = regexp.MustCompile(`^/Date\((-?\d+)([+-]\d{4})?\)/$`)
)

// Tools returns all twenty registered tools. Whether a write tool is
// callable still depends on [mcp_gateway.write.*] and key scope.
func (s *Service) Tools() []mcpgateway.Tool {
	siteSchema := func(extra map[string]any, required ...string) json.RawMessage {
		props := map[string]any{"site": mcpgateway.Prop("string", "Verified site URL exactly as returned by list_sites, e.g. https://example.com/.")}
		for k, v := range extra {
			props[k] = v
		}
		return mcpgateway.Schema(props, required...)
	}
	limitProp := mcpgateway.Prop("integer", "Rows to return (default 100, max 1000).")
	writeSchema := func(extra map[string]any, required ...string) json.RawMessage {
		props := map[string]any{
			"site":        mcpgateway.Prop("string", "Verified site URL exactly as returned by list_sites."),
			"dry_run":     mcpgateway.Prop("boolean", "Validate params/scope only; never call upstream."),
			"approval_id": mcpgateway.Prop("string", "Approval id when executing a previously approved high-risk write."),
		}
		for k, v := range extra {
			props[k] = v
		}
		return mcpgateway.Schema(props, required...)
	}
	return []mcpgateway.Tool{
		readTool(ToolListSites, "List sites verified under this Bing Webmaster account.", mcpgateway.Schema(map[string]any{}), s.validateNoParams),
		readTool(ToolQueryStats, "Query-level traffic stats for a verified site.", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolPageStats, "Page-level traffic stats (Bing returns the page in its Query field; renamed to Page).", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolRankTraffic, "Daily rank and traffic stats.", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolCrawlStats, "Crawl stats for a verified site.", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolCrawlIssues, "Crawl issues for a verified site.", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolListFeeds, "List submitted sitemaps/feeds.", siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),
		readTool(ToolPageQueries, "Queries driving traffic to one page (Bing returns the first 1000 rows, time ascending).",
			siteSchema(map[string]any{
				"page":  mcpgateway.Prop("string", "Full page URL belonging to the site."),
				"limit": limitProp,
			}, "site", "page"), s.validatePageQueries),
		readTool(ToolQueryPages, "Pages ranking for one query (small result, recent weeks).",
			siteSchema(map[string]any{
				"q":     mcpgateway.Prop("string", "Search query."),
				"limit": limitProp,
			}, "site", "q"), s.validateQueryPages),
		readTool(ToolKeyword, "Aggregate Bing keyword research (no site ownership needed).",
			keywordSchema(true), s.validateKeyword),
		readTool(ToolRelatedKeyword, "Related keywords for a query.",
			keywordSchema(true), s.validateKeyword),
		readTool(ToolKeywordStats, "Keyword traffic stats for a query.",
			keywordSchema(false), s.validateKeywordStats),
		readTool(ToolGetURLSubmissionQuota, "Daily and monthly URL submission quota remaining for a verified site.",
			siteSchema(map[string]any{}, "site"), s.validateSiteRead),
		readTool(ToolGetBlockedURLs, "List pages/directories currently blocked from Bing results.",
			siteSchema(map[string]any{"limit": limitProp}, "site"), s.validateSiteRead),

		writeTool(ToolSubmitURL, "Submit one URL for indexing. Low risk; direct execution with a write scope.",
			writeSchema(map[string]any{"url": mcpgateway.Prop("string", "URL belonging to the site.")}, "site", "url"),
			false, false, false, s.validateSubmitURL),
		writeTool(ToolSubmitURLBatch, "Submit up to the configured batch cap (default 100; upstream 500) URLs. Duplicates are removed.",
			writeSchema(map[string]any{"urls": mcpgateway.ArrayProp("string", "URLs belonging to the site; deduplicated.")}, "site", "urls"),
			false, false, false, s.validateSubmitURLBatch),
		writeTool(ToolSubmitFeed, "Submit a sitemap/RSS/Atom/text feed for a verified site.",
			writeSchema(map[string]any{"feedUrl": mcpgateway.Prop("string", "Feed/sitemap URL belonging to the site.")}, "site", "feedUrl"),
			false, false, false, s.validateSubmitFeed),
		writeTool(ToolRemoveFeed, "Remove a previously submitted feed. Reversible by submitting it again.",
			writeSchema(map[string]any{"feedUrl": mcpgateway.Prop("string", "Feed URL currently in the submitted feed list.")}, "site", "feedUrl"),
			true, false, false, s.validateRemoveFeed),
		writeTool(ToolAddBlockedURL, "Block a page or directory from Bing results. High risk: requires an approval_id after operator approval.",
			writeSchema(map[string]any{
				"blockedUrl":  mcpgateway.Prop("string", "Page or directory URL belonging to the site (cannot be the site root)."),
				"entity_type": mcpgateway.EnumProp("Block scope: page (default) or directory.", "page", "directory"),
			}, "site", "blockedUrl"),
			true, true, true, s.validateAddBlockedURL),
		writeTool(ToolRemoveBlockedURL, "Unblock a page or directory from Bing results. High risk: requires an approval_id after operator approval.",
			writeSchema(map[string]any{
				"blockedUrl":  mcpgateway.Prop("string", "Blocked page or directory URL belonging to the site (cannot be the site root)."),
				"entity_type": mcpgateway.EnumProp("Block scope: page (default) or directory.", "page", "directory"),
			}, "site", "blockedUrl"),
			true, true, true, s.validateRemoveBlockedURL),
	}
}

func readTool(name, desc string, schema json.RawMessage, validate func(json.RawMessage) error) mcpgateway.Tool {
	return mcpgateway.Tool{
		Name: name, Description: desc, Risk: mcpgateway.RiskRead, ReadOnly: true,
		Idempotent: true, OpenWorld: true, InputSchema: schema, Validate: validate,
	}
}

func writeTool(name, desc string, schema json.RawMessage, destructive, highRisk, idempotent bool, validate func(json.RawMessage) error) mcpgateway.Tool {
	risk := mcpgateway.RiskLow
	if highRisk {
		risk = mcpgateway.RiskHigh
	}
	return mcpgateway.Tool{
		Name: name, Description: desc, Risk: risk, ReadOnly: false,
		Destructive: destructive, Idempotent: idempotent, OpenWorld: true,
		InputSchema: schema, Validate: validate,
	}
}

func keywordSchema(withDates bool) json.RawMessage {
	props := map[string]any{
		"q":        mcpgateway.Prop("string", "Search query."),
		"country":  mcpgateway.Prop("string", "Two-letter country code, default US."),
		"language": mcpgateway.Prop("string", "Two-letter language code, default en."),
	}
	if withDates {
		props["startDate"] = mcpgateway.Prop("string", "Optional start date YYYY-MM-DD.")
		props["endDate"] = mcpgateway.Prop("string", "Optional end date YYYY-MM-DD.")
	}
	return mcpgateway.Schema(props, "q")
}

// ---- validators ----

func (s *Service) validateNoParams(raw json.RawMessage) error {
	var p struct{}
	return mcpgateway.DecodeStrict(raw, &p)
}

type siteArgs struct {
	Site  string `json:"site"`
	Limit int    `json:"limit"`
}

func (s *Service) validateSiteRead(raw json.RawMessage) error {
	var p siteArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if _, err := mcpgateway.ValidateSiteURL(p.Site); err != nil {
		return mcpgateway.InvalidParams("%s", err.Error())
	}
	if p.Limit < 0 || p.Limit > 1000 {
		return mcpgateway.InvalidParams("limit must be 1-1000")
	}
	return nil
}

type pageQueriesArgs struct {
	Site  string `json:"site"`
	Page  string `json:"page"`
	Limit int    `json:"limit"`
}

func (s *Service) validatePageQueries(raw json.RawMessage) error {
	var p pageQueriesArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if _, err := mcpgateway.ValidateSiteURL(p.Site); err != nil {
		return mcpgateway.InvalidParams("%s", err.Error())
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.Page) {
		return mcpgateway.InvalidParams("page must belong to the site")
	}
	if p.Limit < 0 || p.Limit > 1000 {
		return mcpgateway.InvalidParams("limit must be 1-1000")
	}
	return nil
}

type queryPagesArgs struct {
	Site  string `json:"site"`
	Q     string `json:"q"`
	Limit int    `json:"limit"`
}

func (s *Service) validateQueryPages(raw json.RawMessage) error {
	var p queryPagesArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if _, err := mcpgateway.ValidateSiteURL(p.Site); err != nil {
		return mcpgateway.InvalidParams("%s", err.Error())
	}
	p.Q = strings.TrimSpace(p.Q)
	if p.Q == "" || len(p.Q) > 200 {
		return mcpgateway.InvalidParams("q is required (max 200 bytes)")
	}
	if p.Limit < 0 || p.Limit > 1000 {
		return mcpgateway.InvalidParams("limit must be 1-1000")
	}
	return nil
}

type keywordArgs struct {
	Q         string `json:"q"`
	Country   string `json:"country"`
	Language  string `json:"language"`
	StartDate string `json:"startDate"`
	EndDate   string `json:"endDate"`
}

func (s *Service) validateKeyword(raw json.RawMessage) error {
	p, err := decodeKeyword(raw)
	if err != nil {
		return err
	}
	if p.StartDate != "" || p.EndDate != "" {
		if p.StartDate == "" || p.EndDate == "" {
			return mcpgateway.InvalidParams("startDate and endDate must be supplied together")
		}
		if _, _, err := mcpgateway.ValidateDateRange(p.StartDate, p.EndDate); err != nil {
			return mcpgateway.InvalidParams("%s", err.Error())
		}
	}
	return nil
}

func (s *Service) validateKeywordStats(raw json.RawMessage) error {
	// GetKeywordStats does not accept date filters; reject them rather than
	// silently dropping fields.
	var p struct {
		Q        string `json:"q"`
		Country  string `json:"country"`
		Language string `json:"language"`
	}
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	p.Q = strings.TrimSpace(p.Q)
	if p.Q == "" || len(p.Q) > 200 {
		return mcpgateway.InvalidParams("q is required (max 200 bytes)")
	}
	return validateCountryLang(p.Country, p.Language)
}

func decodeKeyword(raw json.RawMessage) (keywordArgs, error) {
	var p keywordArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return p, err
	}
	p.Q = strings.TrimSpace(p.Q)
	if p.Q == "" || len(p.Q) > 200 {
		return p, mcpgateway.InvalidParams("q is required (max 200 bytes)")
	}
	if err := validateCountryLang(p.Country, p.Language); err != nil {
		return p, err
	}
	return p, nil
}

func validateCountryLang(country, language string) error {
	if country != "" && !countryRe.MatchString(country) {
		return mcpgateway.InvalidParams("country must be a two-letter uppercase code")
	}
	if language != "" && !langRe.MatchString(language) {
		return mcpgateway.InvalidParams("language must be a two-letter lowercase code")
	}
	return nil
}

// ---- write validators ----

type submitURLArgs struct {
	Site       string `json:"site"`
	URL        string `json:"url"`
	DryRun     bool   `json:"dry_run"`
	ApprovalID string `json:"approval_id"`
}

func (s *Service) validateSubmitURL(raw json.RawMessage) error {
	var p submitURLArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.URL) {
		return mcpgateway.InvalidParams("url must belong to the site")
	}
	return nil
}

type submitBatchArgs struct {
	Site       string   `json:"site"`
	URLs       []string `json:"urls"`
	DryRun     bool     `json:"dry_run"`
	ApprovalID string   `json:"approval_id"`
}

func (s *Service) validateSubmitURLBatch(raw json.RawMessage) error {
	var p submitBatchArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if _, err := mcpgateway.ValidateBatchURLs(p.Site, p.URLs, s.batchCap); err != nil {
		return mcpgateway.InvalidParams("%s", err.Error())
	}
	return nil
}

type feedArgs struct {
	Site       string `json:"site"`
	FeedURL    string `json:"feedUrl"`
	DryRun     bool   `json:"dry_run"`
	ApprovalID string `json:"approval_id"`
}

func (s *Service) validateSubmitFeed(raw json.RawMessage) error {
	var p feedArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.FeedURL) {
		return mcpgateway.InvalidParams("feedUrl must belong to the site")
	}
	return nil
}

func (s *Service) validateRemoveFeed(raw json.RawMessage) error { return s.validateSubmitFeed(raw) }

type blockedArgs struct {
	Site       string `json:"site"`
	BlockedURL string `json:"blockedUrl"`
	EntityType string `json:"entity_type"`
	DryRun     bool   `json:"dry_run"`
	ApprovalID string `json:"approval_id"`
}

func (s *Service) validateAddBlockedURL(raw json.RawMessage) error {
	var p blockedArgs
	if err := mcpgateway.DecodeStrict(raw, &p); err != nil {
		return err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.BlockedURL) {
		return mcpgateway.InvalidParams("blockedUrl must belong to the site")
	}
	if isSiteRoot(p.Site, p.BlockedURL) {
		return mcpgateway.InvalidParams("blockedUrl cannot be the site root")
	}
	switch p.EntityType {
	case "", "page", "directory":
	default:
		return mcpgateway.InvalidParams("entity_type must be page or directory")
	}
	return nil
}

// validateRemoveBlockedURL reuses the add_blocked_url contract: same params,
// same site-ownership and entity-type checks.
func (s *Service) validateRemoveBlockedURL(raw json.RawMessage) error {
	return s.validateAddBlockedURL(raw)
}

func isSiteRoot(site, target string) bool {
	su, err1 := mcpgateway.NormalizeURL(site)
	tu, err2 := mcpgateway.NormalizeURL(target)
	if err1 != nil || err2 != nil {
		return false
	}
	sp := strings.TrimSuffix(su.EscapedPath(), "/")
	tp := strings.TrimSuffix(tu.EscapedPath(), "/")
	return sp == tp && tu.RawQuery == ""
}

// helper used by read/write paths to build query params.
func siteParam(site string) url.Values {
	return url.Values{"siteUrl": {site}}
}
