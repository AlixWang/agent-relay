package services_test

import (
	"testing"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
	"github.com/AlixWang/agent-relay/internal/mcpgateway/services/adsense"
	"github.com/AlixWang/agent-relay/internal/mcpgateway/services/bing"
	"github.com/AlixWang/agent-relay/internal/mcpgateway/services/ga"
)

func TestExplicitServiceRegistrationAndDefaultPolicy(t *testing.T) {
	reg := mcpgateway.NewRegistry()
	if err := reg.Register(adsense.New(adsense.Options{})); err != nil {
		t.Fatalf("register adsense: %v", err)
	}
	if err := reg.Register(bing.New(bing.Options{})); err != nil {
		t.Fatalf("register bing: %v", err)
	}
	if err := reg.Register(ga.New(ga.Options{})); err != nil {
		t.Fatalf("register ga: %v", err)
	}
	tools := reg.AggregateTools()
	// 9 AdSense reads + 23 Bing reads + 6 Bing writes + 10 GA reads = 48.
	// CSV, offset paging and the AdSense dimensions are parameters, not tools.
	if len(tools) != 48 {
		t.Fatalf("registered tool count = %d, want 48", len(tools))
	}
	names := map[string]bool{}
	for _, tool := range tools {
		names[tool.Name] = true
	}
	for _, want := range []string{
		"adsense_list_accounts", "adsense_list_ad_units",
		"bing_list_sites", "bing_submit_url", "bing_submit_url_batch",
		"bing_submit_feed", "bing_remove_feed", "bing_add_blocked_url",
		"bing_get_url_submission_quota", "bing_get_blocked_urls", "bing_remove_blocked_url",
		"bing_get_url_info", "bing_get_url_traffic_info",
		"bing_get_query_page_detail_stats", "bing_get_query_traffic_stats",
		"bing_get_children_url_info", "bing_get_children_url_traffic_info",
		"bing_get_link_counts", "bing_get_url_links", "bing_get_feed_details",
		"ga_get_account_summaries", "ga_list_key_events", "ga_run_report", "ga_run_realtime_report",
		"ga_run_funnel_report", "ga_run_conversions_report",
	} {
		if !names[want] {
			t.Fatalf("missing aggregate tool %q", want)
		}
	}
	for _, hidden := range []string{
		"adsense_create_ad_unit", "adsense_patch_ad_unit",
		"ga_create_property_annotation",
	} {
		if names[hidden] {
			t.Fatalf("AdSense write placeholder exposed as %q", hidden)
		}
	}
	cfg := mcpgateway.DefaultConfig()
	if err := cfg.ValidateAgainst(reg); err != nil {
		t.Fatalf("default config must validate: %v", err)
	}
	// GA resolves on both endpoints and its scopes use the existing read-only
	// namespace; a read scope can never grant a write.
	if rt, ok := reg.ResolveTool("", "ga_run_report", true); !ok || rt.ServiceID != "ga" || rt.Name != "run_report" {
		t.Fatalf("aggregate ga resolve: %+v %v", rt, ok)
	}
	if rt, ok := reg.ResolveTool("ga", "run_report", false); !ok || rt.AggregateName != "ga_run_report" {
		t.Fatalf("service ga resolve: %+v %v", rt, ok)
	}
	if _, err := mcpgateway.ValidateScopes(reg, []string{"mcp:ga:read:*"}); err != nil {
		t.Fatalf("ga read scope: %v", err)
	}
	if !mcpgateway.ScopeMatch([]string{"mcp:ga:read:*"}, "ga", "read", "run_report") {
		t.Fatal("ga read scope did not match")
	}
	if mcpgateway.ScopeMatch([]string{"mcp:ga:read:*"}, "ga", "write", "run_report") {
		t.Fatal("ga read scope granted a write")
	}

	// add_blocked_url is implemented but deliberately not in the first batch;
	// remove_blocked_url is the approval-gated safety valve and is enabled.
	write := cfg.Write.Services["bing-webmaster"]
	if len(write.AllowedTools) != 5 {
		t.Fatalf("default bing write allowlist: %+v", write.AllowedTools)
	}
	allowed := map[string]bool{}
	for _, name := range write.AllowedTools {
		allowed[name] = true
	}
	if allowed[bing.ToolAddBlockedURL] {
		t.Fatal("add_blocked_url must be off in the first batch")
	}
	if !allowed[bing.ToolRemoveBlockedURL] {
		t.Fatal("remove_blocked_url must be in the default bing write allowlist")
	}
	// The second wave is a config-only change and validates.
	cfg.Write.Services["bing-webmaster"] = mcpgateway.WriteServiceConfig{
		AllowedTools: []string{"submit_url", "submit_url_batch", "submit_feed", "remove_feed", "add_blocked_url", "remove_blocked_url"},
	}
	if err := cfg.ValidateAgainst(reg); err != nil {
		t.Fatalf("second-wave config must validate: %v", err)
	}
	// A read tool in the write allowlist is rejected.
	cfg.Write.Services["bing-webmaster"] = mcpgateway.WriteServiceConfig{AllowedTools: []string{"query_stats"}}
	if err := cfg.ValidateAgainst(reg); err == nil {
		t.Fatal("read tool in write allowlist accepted")
	}
}
