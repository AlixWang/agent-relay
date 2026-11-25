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
	disableMCPServicesWithoutCredentials(&cfg, creds)

	if !cfg.Enabled {
		t.Fatal("gateway-wide switch must stay on")
	}
	if !cfg.Service(mcpgateway.ServiceAdSense).Enabled {
		t.Fatal("adsense must stay enabled when its own credential exists")
	}
	if !cfg.Service(mcpgateway.ServiceBing).Enabled {
		t.Fatal("bing must stay enabled when its own credential exists")
	}
	if cfg.Service(mcpgateway.ServiceGA).Enabled {
		t.Fatal("ga must be disabled when its credential is missing")
	}
	if !strings.Contains(logs.String(), "service ga disabled") {
		t.Fatalf("missing ga disable warning: %q", logs.String())
	}
	for _, secret := range []string{"adsense-value-not-to-log", "bing-value-not-to-log"} {
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("credential value leaked to logs")
		}
	}

	// With every referenced secret present, all three stay enabled. Re-enable
	// the segment first: a disabled segment is intentionally not re-probed on a
	// later pass (startup runs this once).
	for id, svc := range cfg.Services {
		svc.Enabled = true
		cfg.Services[id] = svc
	}
	all := mcpgateway.NewCredentials(mapSecrets{
		"TEST_ADSENSE": "a", "TEST_BING": "b", "TEST_GA": "c",
	})
	disableMCPServicesWithoutCredentials(&cfg, all)
	for _, id := range []string{mcpgateway.ServiceAdSense, mcpgateway.ServiceBing, mcpgateway.ServiceGA} {
		if !cfg.Service(id).Enabled {
			t.Fatalf("%s disabled although every credential exists", id)
		}
	}
}
