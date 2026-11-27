package bing

import (
	"context"
	"net/http"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

// Bing write tools (design v5 §5.4). Low-risk writes are called directly by
// the gateway. add_blocked_url is only reached after the approval state
// machine has claimed an approval row; this service itself cannot tell the
// difference, so the core is the sole approval gate.

func (s *Service) submitURL(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p submitURLArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.URL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("url must belong to the site")
	}
	if call.DryRun {
		return dryRunResult(call.Tool, false), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	p.URL = canonicalURL(p.URL)
	quota, qerr := s.quota(ctx, p.Site)
	if qerr != nil {
		return mcpgateway.Result{}, qerr
	}
	if quota.DailyQuota < 1 {
		return mcpgateway.Result{}, quotaError(quota)
	}
	body := map[string]any{"siteUrl": p.Site, "url": p.URL}
	if err := s.call(ctx, http.MethodPost, "SubmitUrl", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"submitted": 1, "site": p.Site, "url": p.URL, "daily_quota": quota.DailyQuota,
	}, RowCount: 1, UpstreamStatus: 200}, nil
}

func (s *Service) submitURLBatch(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p submitBatchArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	urls, err := mcpgateway.ValidateBatchURLs(p.Site, p.URLs, s.batchCap)
	if err != nil {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("%s", err.Error())
	}
	if call.DryRun {
		return dryRunResult(call.Tool, false), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	quota, qerr := s.quota(ctx, p.Site)
	if qerr != nil {
		return mcpgateway.Result{}, qerr
	}
	if quota.DailyQuota < len(urls) {
		return mcpgateway.Result{}, quotaError(quota)
	}
	body := map[string]any{"siteUrl": p.Site, "urlList": urls}
	if err := s.call(ctx, http.MethodPost, "SubmitUrlBatch", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"submitted": len(urls), "site": p.Site, "urls": urls, "daily_quota": quota.DailyQuota,
	}, RowCount: len(urls), UpstreamStatus: 200}, nil
}

func (s *Service) submitFeed(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p feedArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.FeedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("feedUrl must belong to the site")
	}
	if call.DryRun {
		return dryRunResult(call.Tool, false), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	p.FeedURL = canonicalURL(p.FeedURL)
	body := map[string]any{"siteUrl": p.Site, "feedUrl": p.FeedURL}
	if err := s.call(ctx, http.MethodPost, "SubmitFeed", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"submitted": true, "site": p.Site, "feed_url": p.FeedURL,
	}, RowCount: 1, UpstreamStatus: 200}, nil
}

func (s *Service) removeFeed(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p feedArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.FeedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("feedUrl must belong to the site")
	}
	if call.DryRun {
		return dryRunResult(call.Tool, false), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	p.FeedURL = canonicalURL(p.FeedURL)
	present, ferr := s.feedPresent(ctx, p.Site, p.FeedURL)
	if ferr != nil {
		return mcpgateway.Result{}, ferr
	}
	if !present {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("feedUrl is not in the submitted feed list")
	}
	body := map[string]any{"siteUrl": p.Site, "feedUrl": p.FeedURL}
	if err := s.call(ctx, http.MethodPost, "RemoveFeed", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"removed": true, "site": p.Site, "feed_url": p.FeedURL,
	}, RowCount: 1, UpstreamStatus: 200}, nil
}

func (s *Service) addBlockedURL(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p blockedArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.BlockedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("blockedUrl must belong to the site")
	}
	if isSiteRoot(p.Site, p.BlockedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("blockedUrl cannot be the site root")
	}
	entityType, eerr := blockedEntityType(p.EntityType)
	if eerr != nil {
		return mcpgateway.Result{}, eerr
	}
	if call.DryRun {
		return dryRunResult(call.Tool, true), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	p.BlockedURL = canonicalURL(p.BlockedURL)
	body := blockedURLBody(p.Site, p.BlockedURL, entityType, 1)
	if err := s.call(ctx, http.MethodPost, "AddBlockedUrl", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"blocked": true, "site": p.Site, "blocked_url": p.BlockedURL, "entity_type": p.EntityType,
	}, RowCount: 1, UpstreamStatus: 200}, nil
}

func (s *Service) removeBlockedURL(ctx context.Context, call mcpgateway.Call) (mcpgateway.Result, error) {
	var p blockedArgs
	if err := mcpgateway.DecodeStrict(call.Params, &p); err != nil {
		return mcpgateway.Result{}, err
	}
	if !mcpgateway.URLBelongsToSite(p.Site, p.BlockedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("blockedUrl must belong to the site")
	}
	if isSiteRoot(p.Site, p.BlockedURL) {
		return mcpgateway.Result{}, mcpgateway.InvalidParams("blockedUrl cannot be the site root")
	}
	entityType, eerr := blockedEntityType(p.EntityType)
	if eerr != nil {
		return mcpgateway.Result{}, eerr
	}
	if call.DryRun {
		return dryRunResult(call.Tool, true), nil
	}
	if verr := s.verifiedSite(ctx, p.Site); verr != nil {
		return mcpgateway.Result{}, verr
	}
	p.BlockedURL = canonicalURL(p.BlockedURL)
	body := blockedURLBody(p.Site, p.BlockedURL, entityType, 0)
	if err := s.call(ctx, http.MethodPost, "RemoveBlockedUrl", nil, body, nil); err != nil {
		return mcpgateway.Result{}, err
	}
	return mcpgateway.Result{Data: map[string]any{
		"removed": true, "site": p.Site, "blocked_url": p.BlockedURL, "entity_type": p.EntityType,
	}, RowCount: 1, UpstreamStatus: 200}, nil
}

// blockedEntityType maps the public scope name to Bing's BlockedUrl.EntityType.
func blockedEntityType(raw string) (int, *mcpgateway.Error) {
	switch raw {
	case "", "page":
		return 0, nil
	case "directory":
		return 1, nil
	default:
		return 0, mcpgateway.InvalidParams("entity_type must be page or directory")
	}
}

// blockedURLBody builds the wrapped BlockedUrl payload shared by AddBlockedUrl
// (RequestType 1) and RemoveBlockedUrl (RequestType 0).
func blockedURLBody(site, blockedURL string, entityType, requestType int) map[string]any {
	return map[string]any{
		"siteUrl": site,
		"blockedUrl": map[string]any{
			"__type":      "BlockedUrl:#Microsoft.Bing.Webmaster.Api",
			"Date":        "/Date(-62135568000000-0800)/",
			"EntityType":  entityType,
			"RequestType": requestType,
			"Url":         blockedURL,
		},
	}
}

type submissionQuota struct {
	DailyQuota   int `json:"DailyQuota"`
	MonthlyQuota int `json:"MonthlyQuota"`
}

func (s *Service) quota(ctx context.Context, site string) (submissionQuota, *mcpgateway.Error) {
	var q submissionQuota
	if err := s.call(ctx, http.MethodGet, "GetUrlSubmissionQuota", siteParam(site), nil, &q); err != nil {
		return q, err
	}
	return q, nil
}

func quotaError(q submissionQuota) *mcpgateway.Error {
	return &mcpgateway.Error{
		Code: mcpgateway.CodeQuotaExceeded, Message: "Bing URL submission quota is exhausted",
		HTTPStatus: http.StatusOK,
		Details:    map[string]any{"daily_quota": q.DailyQuota, "monthly_quota": q.MonthlyQuota},
	}
}

func (s *Service) feedPresent(ctx context.Context, site, feedURL string) (bool, *mcpgateway.Error) {
	var feeds []map[string]any
	if err := s.call(ctx, http.MethodGet, "GetFeeds", siteParam(site), nil, &feeds); err != nil {
		return false, err
	}
	want := canonicalURL(feedURL)
	for _, f := range feeds {
		for _, key := range []string{"Url", "FeedUrl", "URL"} {
			if v, ok := f[key].(string); ok && canonicalURL(v) == want {
				return true, nil
			}
		}
	}
	return false, nil
}

func canonicalURL(raw string) string {
	u, err := mcpgateway.NormalizeURL(raw)
	if err != nil {
		return raw
	}
	return u.String()
}

func dryRunResult(tool string, highRisk bool) mcpgateway.Result {
	return mcpgateway.Result{Data: map[string]any{
		"dry_run": true, "valid": true, "tool": tool,
		"would_require_approval": highRisk,
	}}
}
