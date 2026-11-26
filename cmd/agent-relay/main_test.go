package main

import (
	"bytes"
	"log"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/mcpgateway"
)

type mapSecrets map[string]string

func (m mapSecrets) RevealServerSecret(name string) (string, error) {
	v, ok := m[name]
	if !ok {
		return "", mcpgateway.ErrCredentialMissing
	}
	return v, nil
}

func TestMCPCredentialGatingIsPerService(t *testing.T) {
	cfg := mcpgateway.DefaultConfig()
	cfg.Enabled = true
	cfg.Services[mcpgateway.ServiceAdSense] = mcpgateway.ServiceConfig{
		Enabled: true, CredentialRef: "secret:TEST_ADSENSE", AllowedTools: []string{"*"},
	}
	cfg.Services[mcpgateway.ServiceBing] = mcpgateway.ServiceConfig{
		Enabled: true, CredentialRef: "secret:TEST_BING", AllowedTools: []string{"*"},
	}
	cfg.Services[mcpgateway.ServiceGA] = mcpgateway.ServiceConfig{
		Enabled: true, CredentialRef: "secret:TEST_GA", AllowedTools: []string{"*"},
	}

	var logs bytes.Buffer
	oldWriter := log.Writer()
	log.SetOutput(&logs)
	defer log.SetOutput(oldWriter)

	creds := mcpgateway.NewCredentials(mapSecrets{
		"TEST_ADSENSE": "adsense-value-not-to-log",
		"TEST_BING":    "bing-value-not-to-log",
	})
	warnMissingMCPCredentials(cfg, creds)

	// Missing credentials must warn without disabling the service segment so
	// configuring the secret live in the console works immediately without restart.
	if !cfg.Enabled {
		t.Fatal("gateway-wide switch must stay on")
	}
	for _, id := range []string{mcpgateway.ServiceAdSense, mcpgateway.ServiceBing, mcpgateway.ServiceGA} {
		if !cfg.Service(id).Enabled {
			t.Fatalf("%s must stay enabled in config so console secret activation works without restart", id)
		}
	}
	if !strings.Contains(logs.String(), "service ga credential secret:TEST_GA not yet configured") {
		t.Fatalf("missing ga credential warning: %q", logs.String())
	}
	for _, secret := range []string{"adsense-value-not-to-log", "bing-value-not-to-log"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("credential value leaked to logs")
		}
	}

	logs.Reset()
	all := mcpgateway.NewCredentials(mapSecrets{
		"TEST_ADSENSE": "a", "TEST_BING": "b", "TEST_GA": "c",
	})
	warnMissingMCPCredentials(cfg, all)
	if logs.Len() != 0 {
		t.Fatalf("expected no warnings when all credentials exist, got %q", logs.String())
	}
}
