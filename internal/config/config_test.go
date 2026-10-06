package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefault(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default must validate: %v", err)
	}
	if cfg.Port != 18789 || cfg.Protocol != 1 || cfg.FuseMaxMessages != 50 {
		t.Fatalf("unexpected defaults: %+v", cfg)
	}
}

func TestLoadRoundTrip(t *testing.T) {
	cfg := Default()
	cfg.Port = 18790
	cfg.AdminPasswordHash = "x"
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	data := "port = 18790\nadmin_password_hash = \"x\"\n"
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Port != 18790 || loaded.AdminPasswordHash != "x" {
		t.Fatalf("not applied: %+v", loaded)
	}
	// Untouched keys keep defaults.
	if loaded.FuseMaxMessages != cfg.FuseMaxMessages || loaded.DataDir != cfg.DataDir {
		t.Fatalf("defaults lost: %+v", loaded)
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("ports = 1234\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown-key error")
	}
}

func TestValidate(t *testing.T) {
	cfg := Default()
	cfg.Port = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected port error")
	}
	cfg = Default()
	cfg.Public = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected public-without-tls error")
	}
	cfg = Default()
	cfg.RatePerMinute = 0
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected rate error")
	}
}

func TestBehindProxyValidation(t *testing.T) {
	cfg := Default()
	cfg.BehindProxy = true
	cfg.Public = true
	cfg.PublicAddr = "https://relay.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("behind_proxy+public must conflict")
	}
	cfg = Default()
	cfg.BehindProxy = true
	if err := cfg.Validate(); err == nil {
		t.Fatal("behind_proxy without public_addr must fail")
	}
	cfg.PublicAddr = "http://relay.example.com"
	if err := cfg.Validate(); err == nil {
		t.Fatal("non-https public_addr must fail")
	}
	cfg.PublicAddr = "https://relay.example.com"
	cfg.TrustedProxy = "not-a-cidr"
	if err := cfg.Validate(); err == nil {
		t.Fatal("bad trusted_proxy must fail")
	}
	cfg.TrustedProxy = ""
	if err := cfg.Validate(); err != nil {
		t.Fatalf("valid behind_proxy: %v", err)
	}
	if got := cfg.TrustedProxyNet().String(); got != "127.0.0.1/32" {
		t.Fatalf("default trusted proxy: %s", got)
	}
}

func TestBindAddr(t *testing.T) {
	cfg := Default()
	cfg.ListenAddr = "127.0.0.1"
	cfg.Port = 9999
	if got := cfg.BindAddr(); got != "127.0.0.1:9999" {
		t.Fatalf("bind: %s", got)
	}
	cfg.ListenAddr = "127.0.0.1:9998"
	if got := cfg.BindAddr(); got != "127.0.0.1:9998" {
		t.Fatalf("verbatim bind: %s", got)
	}
	// auto without tailscale falls back to loopback (no tailscale in CI).
	cfg.ListenAddr = "auto"
	if got := cfg.BindAddr(); got == "" {
		t.Fatal("empty bind")
	}
}

func TestHandshakeDefaults(t *testing.T) {
	cfg := Default()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("default must validate: %v", err)
	}
	if cfg.MaxOpenPermissions != 10 || cfg.ProgressThrottleSecs != 10 || cfg.PermissionTTLSecs != 600 {
		t.Fatalf("handshake defaults: %+v", cfg)
	}
	cfg.MaxOpenPermissions = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative max_open_permissions must fail")
	}
	cfg = Default()
	cfg.ProgressThrottleSecs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative progress_throttle_secs must fail")
	}
	cfg = Default()
	cfg.PermissionTTLSecs = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("negative permission_ttl_secs must fail")
	}
	// Unknown keys still rejected; known new keys load.
	dir := t.TempDir()
	path := dir + "/c.toml"
	os.WriteFile(path, []byte("max_open_permissions = 3\nprogress_throttle_secs = 5\npermission_ttl_secs = 120\n"), 0o644)
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.MaxOpenPermissions != 3 || loaded.ProgressThrottleSecs != 5 || loaded.PermissionTTLSecs != 120 {
		t.Fatalf("not applied: %+v", loaded)
	}
}
