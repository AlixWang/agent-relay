package mcpgateway

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type dummyService struct {
	id     string
	prefix string
	tools  []Tool
}

func (d *dummyService) ID() string               { return d.id }
func (d *dummyService) DisplayName() string      { return d.id }
func (d *dummyService) Version() string          { return "1" }
func (d *dummyService) Prefix() string           { return d.prefix }
func (d *dummyService) Tools() []Tool            { return d.tools }
func (d *dummyService) CredentialRefs() []string { return nil }
func (d *dummyService) Handle(ctx context.Context, call Call) (Result, error) {
	return Result{}, nil
}

func TestScopeParseAndMatch(t *testing.T) {
	valid := []string{
		"mcp:bing-webmaster:read:*",
		"mcp:bing-webmaster:write:submit_url",
		"mcp:adsense:read:list_accounts",
	}
	for _, s := range valid {
		if _, err := ParseScope(s); err != nil {
			t.Fatalf("valid scope %q: %v", s, err)
		}
	}
	invalid := []string{
		"mcp:*:read:*",
		"bing:read:*",
		"mcp:bing:execute:submit_url",
		"mcp:bing:read:Submit_URL",
		"mcp:bing:read:submit-url",
	}
	for _, s := range invalid {
		if _, err := ParseScope(s); err == nil {
			t.Fatalf("invalid scope accepted: %q", s)
		}
	}
	scopes := []string{"mcp:bing-webmaster:read:*", "mcp:bing-webmaster:write:submit_url"}
	if !ScopeMatch(scopes, "bing-webmaster", "read", "query_stats") {
		t.Fatal("read wildcard should match a read tool")
	}
	if ScopeMatch(scopes, "bing-webmaster", "write", "remove_feed") {
		t.Fatal("specific write must not grant another write")
	}
	if ScopeMatch(scopes, "bing-webmaster", "write", "remove_feed") {
		t.Fatal("read scope must not grant a write")
	}
	if ScopeMatch(scopes, "adsense", "read", "list_accounts") {
		t.Fatal("another service must not match")
	}
	if ScopeMatch([]string{"bad"}, "bing-webmaster", "read", "query_stats") {
		t.Fatal("malformed scope must never match")
	}
}

func TestValidateScopesAgainstRegistry(t *testing.T) {
	reg := NewRegistry()
	err := reg.Register(&dummyService{id: "mock", prefix: "mock", tools: []Tool{
		{Name: "read_one", Risk: RiskRead, ReadOnly: true},
		{Name: "write_one", Risk: RiskLow},
	}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ValidateScopes(reg, []string{"mcp:mock:read:read_one"}); err != nil {
		t.Fatalf("valid read scope: %v", err)
	}
	if _, err := ValidateScopes(reg, []string{"mcp:mock:read:write_one"}); err == nil {
		t.Fatal("read class on a write tool must fail")
	}
	if _, err := ValidateScopes(reg, []string{"mcp:mock:write:nope"}); err == nil {
		t.Fatal("unknown tool must fail")
	}
	if _, err := ValidateScopes(reg, []string{"mcp:nope:read:*"}); err == nil {
		t.Fatal("unknown service must fail")
	}
	if _, err := ValidateScopes(reg, nil); err == nil {
		t.Fatal("empty scope set must fail")
	}
}

func TestRegistryConflicts(t *testing.T) {
	tools := []Tool{{Name: "same", Risk: RiskRead, ReadOnly: true}}
	reg := NewRegistry()
	if err := reg.Register(&dummyService{id: "one", prefix: "dup", tools: tools}); err != nil {
		t.Fatal(err)
	}
	// Same prefix.
	if err := reg.Register(&dummyService{id: "two", prefix: "dup", tools: tools}); err == nil {
		t.Fatal("duplicate prefix must fail")
	}
	// Duplicate service id.
	if err := reg.Register(&dummyService{id: "one", prefix: "one", tools: tools}); err == nil {
		t.Fatal("duplicate service id must fail")
	}
	// Invalid tool name.
	if err := reg.Register(&dummyService{id: "three", prefix: "three", tools: []Tool{{Name: "Bad-Name", Risk: RiskRead, ReadOnly: true}}}); err == nil {
		t.Fatal("invalid tool name must fail")
	}
	// Aggregate collision is impossible with distinct prefixes+tool names, but a
	// too-long aggregate name must fail.
	long := strings.Repeat("x", 65)
	if err := reg.Register(&dummyService{id: "four", prefix: "four", tools: []Tool{{Name: long, Risk: RiskRead, ReadOnly: true}}}); err == nil {
		t.Fatal("invalid (too long) tool name must fail")
	}
}

func TestFreezeParamsNormalization(t *testing.T) {
	a, h1, err := FreezeParams("bing-webmaster", "submit_url", json.RawMessage(`{"b":2,"a":{"y":1.0,"x":" q "}}`))
	if err != nil {
		t.Fatal(err)
	}
	b, h2, err := FreezeParams("bing-webmaster", "submit_url", json.RawMessage(`{"a":{"x":" q ","y":1},"b":2.0,"approval_id":"ignored","dry_run":true}`))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) || h1 != h2 {
		t.Fatalf("normalization mismatch:\n%s\n%s\n%s %s", a, b, h1, h2)
	}
	_, h3, _ := FreezeParams("bing-webmaster", "submit_url", json.RawMessage(`{"a":{"x":"q","y":1},"b":2}`))
	if h1 == h3 {
		t.Fatal("whitespace/string change must change the hash")
	}
	_, h4, _ := FreezeParams("bing-webmaster", "remove_feed", json.RawMessage(`{"a":1,"b":2}`))
	if h1 == h4 {
		t.Fatal("tool name must be part of the frozen hash")
	}
	_, h5, _ := FreezeParams("other-svc", "submit_url", json.RawMessage(`{"a":{"x":" q ","y":1},"b":2}`))
	if h1 == h5 {
		t.Fatal("service id must be part of the frozen hash")
	}
}

func TestParamsSummaryRedactsSecrets(t *testing.T) {
	got := ParamsSummary(json.RawMessage(`{"site":"https://x.test/","api_key":"sk-secret","token":"t","url":"https://x.test/a"}`))
	if strings.Contains(got, "sk-secret") || strings.Contains(got, `"t"`) {
		t.Fatalf("summary leaked secret: %s", got)
	}
	if !strings.Contains(got, "https://x.test/") {
		t.Fatalf("summary lost non-secret params: %s", got)
	}
	got = ParamsSummary(json.RawMessage(`{"url":"https://x.test/a?token=very-secret"}`))
	if strings.Contains(got, "very-secret") || strings.Contains(got, "token=") {
		t.Fatalf("URL query leaked into summary: %s", got)
	}
	got = ParamsSummary(json.RawMessage(`{"private_key":"pk-1","x-api-key":"xk-2","note":"leak rmk_abcdef123456"}`))
	if strings.Contains(got, "pk-1") || strings.Contains(got, "xk-2") || strings.Contains(got, "rmk_abcdef123456") {
		t.Fatalf("extended secret patterns leaked: %s", got)
	}
	// Multi-byte UTF-8 truncation must produce valid UTF-8.
	longCN := json.RawMessage(`{"q":"` + strings.Repeat("中", 100) + `"}`)
	got = ParamsSummary(longCN)
	if !strings.Contains(got, "…") || strings.ContainsRune(got, '\uFFFD') {
		t.Fatalf("UTF-8 truncation corrupted rune: %q", got)
	}
}

func TestStrictJSONAndURLBelongsToSiteSecurity(t *testing.T) {
	// Case-colliding / duplicate keys rejected by both DecodeStrict and FreezeParams.
	type args struct {
		Site string `json:"site"`
		URL  string `json:"url"`
	}
	for _, raw := range []string{
		`{"site":"https://x.test/","URL":"https://x.test/","url":"https://x.test/a"}`,
		`{"site":"https://x.test/","site":"https://y.test/"}`,
	} {
		var a args
		if err := DecodeStrict(json.RawMessage(raw), &a); err == nil {
			t.Fatalf("DecodeStrict accepted %s", raw)
		}
		if _, _, err := FreezeParams("bing-webmaster", "add_blocked_url", json.RawMessage(raw)); err == nil {
			t.Fatalf("FreezeParams accepted %s", raw)
		}
	}
	// Integral floats >= 1e6 and -0.0 normalize cleanly; Inf fails.
	_, h1, err := FreezeParams("svc", "tool", json.RawMessage(`{"n":1000000,"z":0}`))
	if err != nil {
		t.Fatal(err)
	}
	_, h2, err := FreezeParams("svc", "tool", json.RawMessage(`{"n":1000000.0,"z":-0.0}`))
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("large int / -0.0 normalization mismatch")
	}
	if _, _, err := FreezeParams("svc", "tool", json.RawMessage(`{"n":1e999}`)); err == nil {
		t.Fatal("overflow float must be rejected")
	}

	// URLBelongsToSite rejects traversal and scheme mismatch.
	if URLBelongsToSite("https://example.com/blog/", "https://example.com/blog/../admin") {
		t.Fatal("dot-dot traversal accepted")
	}
	if URLBelongsToSite("https://example.com/blog/", "https://example.com/blog/%2e%2e/admin") {
		t.Fatal("encoded dot-dot traversal accepted")
	}
	if URLBelongsToSite("https://example.com/", "http://example.com/page") {
		t.Fatal("scheme downgrade accepted")
	}
	if !URLBelongsToSite("https://example.com/blog/", "https://EXAMPLE.COM/blog/post") {
		t.Fatal("valid subpath with uppercase host rejected")
	}
	batch, err := ValidateBatchURLs("https://example.com/", []string{"https://EXAMPLE.COM/a", "https://example.com/a"}, 10)
	if err != nil || len(batch) != 1 {
		t.Fatalf("batch host normalization dedup failed: %+v err=%v", batch, err)
	}
}
