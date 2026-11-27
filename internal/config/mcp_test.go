package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestMCPGatewayConfigDefaultsAndSegments(t *testing.T) {
	cfg := Default()
	if cfg.MCPGateway.Enabled || cfg.MCPGateway.Write.Enabled {
		t.Fatal("mcp gateway and write must default off")
	}
	bing := cfg.MCPGateway.Write.Services["bing-webmaster"]
	if strings.Join(bing.AllowedTools, ",") != "submit_url,submit_url_batch,submit_feed,remove_feed,remove_blocked_url" {
		t.Fatalf("default write set: %+v", bing.AllowedTools)
	}
	if cfg.MCPGateway.Service("adsense").CredentialRef != "secret:GA_ADSENSE_ADC" {
		t.Fatalf("adsense credential ref: %+v", cfg.MCPGateway.Service("adsense"))
	}

	path := writeConfig(t, `
[mcp_gateway]
enabled = true
max_rows_per_call = 500

[mcp_gateway.services.adsense]
enabled = false
credential_ref = "secret:MY_ADSENSE"
allowed_tools = ["list_accounts"]

[mcp_gateway.write]
enabled = true
batch_cap = 50

[mcp_gateway.write.bing-webmaster]
allowed_tools = ["submit_url", "add_blocked_url"]

[mcp_gateway.keys]
read_key_ttl_days = 30
`)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	m := loaded.MCPGateway
	if !m.Enabled || !m.Write.Enabled || m.MaxRowsPerCall != 500 || m.Write.BatchCap != 50 {
		t.Fatalf("scalars not applied: %+v", m)
	}
	if m.Services["adsense"].Enabled || m.Services["adsense"].CredentialRef != "secret:MY_ADSENSE" ||
		len(m.Services["adsense"].AllowedTools) != 1 {
		t.Fatalf("service segment: %+v", m.Services["adsense"])
	}
	if m.Services["bing-webmaster"].CredentialRef != "secret:BING_WM_API_KEY" {
		t.Fatalf("unmentioned service should keep its default: %+v", m.Services["bing-webmaster"])
	}
	if strings.Join(m.Write.Services["bing-webmaster"].AllowedTools, ",") != "submit_url,add_blocked_url" {
		t.Fatalf("write segment: %+v", m.Write.Services["bing-webmaster"])
	}
	if m.Keys.ReadKeyTTLDays != 30 || m.Keys.WriteKeyTTLDays != 30 {
		t.Fatalf("keys segment: %+v", m.Keys)
	}
}

func TestMCPGatewayPerServiceWriteKnobs(t *testing.T) {
	path := writeConfig(t, `
[mcp_gateway.write.bing-webmaster]
batch_cap = 7
approval_ttl_secs = 120
approval_pending_ttl_secs = 7200
low_risk_write_per_hour = 11
high_risk_write_per_hour = 3
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := cfg.MCPGateway.BatchCapFor("bing-webmaster"); got != 7 {
		t.Fatalf("batch cap = %d", got)
	}
	if got := cfg.MCPGateway.ApprovalTTLFor("bing-webmaster"); got != 120 {
		t.Fatalf("approval ttl = %d", got)
	}
	if got := cfg.MCPGateway.ApprovalPendingTTLFor("bing-webmaster"); got != 7200 {
		t.Fatalf("pending ttl = %d", got)
	}
	if low, high := cfg.MCPGateway.WriteLimitsFor("bing-webmaster"); low != 11 || high != 3 {
		t.Fatalf("limits = %d/%d", low, high)
	}
	// The global default still applies to a service without overrides.
	if got := cfg.MCPGateway.BatchCapFor("adsense"); got != 100 {
		t.Fatalf("adsense batch cap = %d", got)
	}
}

func TestMCPGatewayConfigRejectsUnknownAndPlaintextSecrets(t *testing.T) {
	path := writeConfig(t, `
[mcp_gateway]
enabled = true
nope = 1
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "unknown config key") {
		t.Fatalf("unknown nested key: %v", err)
	}

	path = writeConfig(t, `
[mcp_gateway.services.adsense]
credential_ref = "ya29.plaintext-key"
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "credential_ref") {
		t.Fatalf("plaintext credential accepted: %v", err)
	}

	path = writeConfig(t, `
[mcp_gateway.write.bing-webmaster]
allowed_tools = ["*"]
`)
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "explicit tools") {
		t.Fatalf("write wildcard accepted: %v", err)
	}

	path = writeConfig(t, `
[mcp_gateway.keys]
read_key_ttl_days = -5
`)
	if _, err := Load(path); err == nil {
		t.Fatal("negative read_key_ttl_days accepted")
	}

	// Partial service override must preserve default credential_ref.
	path = writeConfig(t, `
[mcp_gateway.services.adsense]
allowed_tools = ["list_accounts"]
`)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("partial service override failed: %v", err)
	}
	if loaded.MCPGateway.Services["adsense"].CredentialRef != "secret:GA_ADSENSE_ADC" {
		t.Fatalf("partial override wiped credential_ref: %+v", loaded.MCPGateway.Services["adsense"])
	}
}

func TestExampleConfigParses(t *testing.T) {
	path := filepath.Join("..", "..", "configs", "config.toml.example")
	if _, err := os.Stat(path); err != nil {
		t.Skip("example config not present")
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("example config: %v", err)
	}
	if cfg.MCPGateway.Enabled {
		t.Fatal("example must ship disabled")
	}
	if got := cfg.MCPGateway.Service("ga"); !got.Enabled || got.CredentialRef != "secret:GA_ADSENSE_ADC" {
		t.Fatalf("example ga segment: %+v", got)
	}
	if got := cfg.MCPGateway.Write.Services["ga"]; len(got.AllowedTools) != 0 {
		t.Fatalf("example ga write allowlist: %+v", got)
	}
}
