package config

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config mirrors configs/config.toml.example. All durations are seconds
// unless the field name says otherwise.
type Config struct {
	ListenAddr string `toml:"listen_addr"` // "auto" | "tailnet" | explicit IP
	Port       int    `toml:"port"`
	DataDir    string `toml:"data_dir"`
	Public     bool   `toml:"public"`
	TLSCert    string `toml:"tls_cert"`
	TLSKey     string `toml:"tls_key"`

	// BehindProxy declares that TLS terminates at a reverse proxy
	// (caddy/nginx/traefik) in front of this server. Effects:
	//   - PublicAddr is advertised to assistants (onboarding prompts)
	//     instead of the bind address;
	//   - admin cookies get Secure (the outside is HTTPS even though the
	//     local hop is plain HTTP);
	//   - client IPs are read from X-Forwarded-For (RemoteAddr would
	//     always be the proxy otherwise, poisoning audit/token last_ip).
	// The proxy itself is configured out-of-band (install.sh writes the
	// caddy site file; see docs/DEPLOY.md for nginx equivalents).
	// Requires Public=false (double TLS is a misconfiguration).
	BehindProxy  bool   `toml:"behind_proxy"`
	PublicAddr   string `toml:"public_addr"`   // e.g. https://relay.example.com
	TrustedProxy string `toml:"trusted_proxy"` // CIDR allowed to set X-Forwarded-For, default 127.0.0.1/32

	AdminPasswordHash string `toml:"admin_password_hash"`

	Protocol  int `toml:"protocol"`
	MinClient int `toml:"min_client"`

	OnlineTimeoutSecs int `toml:"online_timeout_secs"`
	VerifyTimeoutSecs int `toml:"verify_timeout_secs"`
	FuseMaxMessages   int `toml:"fuse_max_messages"`
	FuseMaxAgeSecs    int `toml:"fuse_max_age_secs"`
	RatePerMinute     int `toml:"rate_per_minute"`
	// Permission handshake (§6.5): per-thread caps for execution-time
	// remote approval. Zero disables the corresponding cap.
	MaxOpenPermissions   int   `toml:"max_open_permissions"`
	ProgressThrottleSecs int64 `toml:"progress_throttle_secs"`
	PermissionTTLSecs    int64 `toml:"permission_ttl_secs"`
	MessageTTLDays       int   `toml:"message_ttl_days"`
	PeerPruneAfterDays   int   `toml:"peer_prune_after_days"`
	AuditRetentionDays   int   `toml:"audit_retention_days"`

	OfflineWebhookURL string `toml:"offline_webhook_url"`
	MaxBodyBytes      int    `toml:"max_body_bytes"`
}

// Default returns the documented defaults (DESIGN §6.1, §10.2).
func Default() *Config {
	return &Config{
		ListenAddr:           "auto",
		Port:                 18789,
		DataDir:              "/var/lib/agent-relay",
		Public:               false,
		Protocol:             1,
		MinClient:            1,
		OnlineTimeoutSecs:    300,
		VerifyTimeoutSecs:    600,
		FuseMaxMessages:      50,
		FuseMaxAgeSecs:       24 * 3600,
		RatePerMinute:        60,
		MaxOpenPermissions:   10,
		ProgressThrottleSecs: 10,
		PermissionTTLSecs:    600,
		MessageTTLDays:       30,
		PeerPruneAfterDays:   7,
		AuditRetentionDays:   90,
		MaxBodyBytes:         1 << 20,
	}
}

// Load reads a TOML file over the defaults. Unknown keys are rejected so
// typos fail fast instead of silently running with defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	if path == "" {
		return cfg, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config %s: %w", path, err)
	}
	var raw map[string]any
	if err := toml.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse config %s: %w", path, err)
	}
	known := map[string]bool{
		"listen_addr": true, "port": true, "data_dir": true,
		"public": true, "tls_cert": true, "tls_key": true,
		"behind_proxy": true, "public_addr": true, "trusted_proxy": true,
		"admin_password_hash": true, "protocol": true, "min_client": true,
		"online_timeout_secs": true, "verify_timeout_secs": true,
		"fuse_max_messages": true, "fuse_max_age_secs": true,
		"rate_per_minute": true, "max_open_permissions": true,
		"progress_throttle_secs": true, "permission_ttl_secs": true, "message_ttl_days": true,
		"peer_prune_after_days": true, "audit_retention_days": true,
		"offline_webhook_url": true, "max_body_bytes": true,
	}
	for k := range raw {
		if !known[k] {
			return nil, fmt.Errorf("unknown config key %q in %s", k, path)
		}
	}
	if err := toml.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("decode config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Validate enforces the safety invariants (DESIGN §7.3, §10.1).
func (c *Config) Validate() error {
	if c.Port < 1 || c.Port > 65535 {
		return fmt.Errorf("port %d out of range", c.Port)
	}
	if c.Public && (c.TLSCert == "" || c.TLSKey == "") {
		return fmt.Errorf("public mode requires tls_cert and tls_key")
	}
	if c.BehindProxy {
		if c.Public {
			return fmt.Errorf("behind_proxy and public are mutually exclusive (TLS would terminate twice)")
		}
		if c.PublicAddr == "" {
			return fmt.Errorf("behind_proxy requires public_addr (e.g. https://relay.example.com)")
		}
		if !strings.HasPrefix(c.PublicAddr, "https://") {
			return fmt.Errorf("public_addr must start with https://")
		}
		if _, _, err := net.ParseCIDR(orCIDR(c.TrustedProxy)); err != nil {
			return fmt.Errorf("bad trusted_proxy CIDR: %w", err)
		}
	}
	if c.FuseMaxMessages < 2 {
		return fmt.Errorf("fuse_max_messages must be >= 2")
	}
	if c.RatePerMinute < 1 {
		return fmt.Errorf("rate_per_minute must be >= 1")
	}
	if c.MaxOpenPermissions < 0 {
		return fmt.Errorf("max_open_permissions must be >= 0")
	}
	if c.ProgressThrottleSecs < 0 {
		return fmt.Errorf("progress_throttle_secs must be >= 0")
	}
	if c.PermissionTTLSecs < 0 {
		return fmt.Errorf("permission_ttl_secs must be >= 0")
	}
	return nil
}

// BindAddr resolves listen_addr to a concrete "ip:port".
// "auto"/"tailnet" mean the Tailscale IPv4 (mirrors relay v2 behaviour);
// anything else is used verbatim (e.g. "127.0.0.1" for tests).
func (c *Config) BindAddr() string {
	switch c.ListenAddr {
	case "", "auto", "tailnet":
		if ip := tailscaleIPv4(); ip != "" {
			return fmt.Sprintf("%s:%d", ip, c.Port)
		}
		return fmt.Sprintf("127.0.0.1:%d", c.Port)
	default:
		if strings.Contains(c.ListenAddr, ":") {
			return c.ListenAddr
		}
		return fmt.Sprintf("%s:%d", c.ListenAddr, c.Port)
	}
}

// orCIDR defaults an empty trusted_proxy to loopback-only.
func orCIDR(s string) string {
	if s == "" {
		return "127.0.0.1/32"
	}
	return s
}

// TrustedProxyNet parses the configured trusted-proxy CIDR.
func (c *Config) TrustedProxyNet() *net.IPNet {
	_, n, err := net.ParseCIDR(orCIDR(c.TrustedProxy))
	if err != nil {
		_, n, _ = net.ParseCIDR("127.0.0.1/32")
	}
	return n
}

func tailscaleIPv4() string {
	out, err := exec.Command("tailscale", "ip", "-4").Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		ip := strings.TrimSpace(line)
		if strings.HasPrefix(ip, "100.") {
			return ip
		}
	}
	return ""
}
