package config

import (
	"fmt"
	"net"
	"net/url"
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
	// Deterministic task admission (docs/SUPERAGENT.md Phase 3). Defaults
	// preserve the historical behaviour: peers may delegate tasks, and a
	// peer-created task is not automatically held for the operator.
	AllowPeerTasks               bool `toml:"allow_peer_tasks"`
	RequireTaskApprovalFromPeers bool `toml:"require_task_approval_from_peers"`
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

	// SSE push (DESIGN §4.4b): outbound-only event stream assistants can
	// hold instead of short-polling. Zero disables the corresponding knob.
	StreamKeepaliveSecs int `toml:"stream_keepalive_secs"`
	StreamMaxPerPeer    int `toml:"stream_max_per_peer"`

	// Assistant profile (§8.7): self-reported capabilities + usual tasks.
	// The server nudges online peers to refresh via heartbeat when the
	// profile is older than this. 0 disables the nudge.
	ProfileRefreshDays int `toml:"profile_refresh_days"`

	// Rooms (§6.8): group chat rides the normal message path — a room is a
	// routing alias with a member list, not a peer. These bound the blast
	// radius: member cap, and a time-windowed message cap per room thread
	// (rooms are long-lived, so the whole-thread fuse would wedge them).
	MaxRoomMembers      int `toml:"max_room_members"`
	RoomFuseMaxMessages int `toml:"room_fuse_max_messages"`
	RoomFuseWindowSecs  int `toml:"room_fuse_window_secs"`

	// Muse account authorization + keepalive (DESIGN §4.9). Optional: with
	// no authorized accounts the loop performs no network calls. A zero
	// keepalive interval disables the background loop (manual actions still
	// work); a zero wake cooldown means the default 300s.
	MuseKeepaliveSecs    int    `toml:"muse_keepalive_secs"`
	MuseWakeCooldownSecs int    `toml:"muse_wake_cooldown_secs"`
	MuseHTTPTimeoutSecs  int    `toml:"muse_http_timeout_secs"`
	MuseAutoWake         bool   `toml:"muse_auto_wake"`
	MuseBaseURL          string `toml:"muse_base_url"`
	MuseProxyURL         string `toml:"muse_proxy_url"`
}

// Default returns the documented defaults (DESIGN §6.1, §10.2).
func Default() *Config {
	return &Config{
		ListenAddr:                   "auto",
		Port:                         18789,
		DataDir:                      "/var/lib/agent-relay",
		Public:                       false,
		Protocol:                     1,
		MinClient:                    1,
		OnlineTimeoutSecs:            300,
		VerifyTimeoutSecs:            600,
		FuseMaxMessages:              500,
		FuseMaxAgeSecs:               24 * 3600,
		RatePerMinute:                60,
		AllowPeerTasks:               true,
		RequireTaskApprovalFromPeers: false,
		MaxOpenPermissions:           10,
		ProgressThrottleSecs:         10,
		PermissionTTLSecs:            600,
		MessageTTLDays:               30,
		PeerPruneAfterDays:           7,
		AuditRetentionDays:           90,
		MaxBodyBytes:                 1 << 20,
		StreamKeepaliveSecs:          20,
		StreamMaxPerPeer:             3,
		ProfileRefreshDays:           7,
		MaxRoomMembers:               16,
		RoomFuseMaxMessages:          60,
		RoomFuseWindowSecs:           3600,
		MuseKeepaliveSecs:            900,
		MuseWakeCooldownSecs:         300,
		MuseHTTPTimeoutSecs:          12,
		MuseAutoWake:                 true,
		MuseBaseURL:                  "https://muse.ai",
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
		"rate_per_minute": true, "allow_peer_tasks": true,
		"require_task_approval_from_peers": true, "max_open_permissions": true,
		"progress_throttle_secs": true, "permission_ttl_secs": true, "message_ttl_days": true,
		"peer_prune_after_days": true, "audit_retention_days": true,
		"offline_webhook_url": true, "max_body_bytes": true,
		"stream_keepalive_secs": true, "stream_max_per_peer": true,
		"profile_refresh_days": true,
		"max_room_members":     true, "room_fuse_max_messages": true,
		"room_fuse_window_secs": true,
		"muse_keepalive_secs":   true, "muse_wake_cooldown_secs": true,
		"muse_http_timeout_secs": true, "muse_auto_wake": true,
		"muse_base_url": true, "muse_proxy_url": true,
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
	if c.StreamKeepaliveSecs < 0 || c.StreamKeepaliveSecs > 120 {
		return fmt.Errorf("stream_keepalive_secs must be 0-120")
	}
	if c.StreamMaxPerPeer < 0 {
		return fmt.Errorf("stream_max_per_peer must be >= 0")
	}
	if c.ProfileRefreshDays < 0 {
		return fmt.Errorf("profile_refresh_days must be >= 0")
	}
	if c.MaxRoomMembers < 2 || c.MaxRoomMembers > 64 {
		return fmt.Errorf("max_room_members must be 2-64")
	}
	if c.RoomFuseMaxMessages < 2 {
		return fmt.Errorf("room_fuse_max_messages must be >= 2")
	}
	if c.RoomFuseWindowSecs < 60 {
		return fmt.Errorf("room_fuse_window_secs must be >= 60")
	}
	if c.MuseKeepaliveSecs < 0 || (c.MuseKeepaliveSecs > 0 && c.MuseKeepaliveSecs < 60) {
		return fmt.Errorf("muse_keepalive_secs must be 0 (disabled) or >= 60")
	}
	if c.MuseWakeCooldownSecs < 0 || (c.MuseWakeCooldownSecs > 0 && c.MuseWakeCooldownSecs < 60) {
		return fmt.Errorf("muse_wake_cooldown_secs must be 0 (default) or >= 60")
	}
	if c.MuseHTTPTimeoutSecs < 1 || c.MuseHTTPTimeoutSecs > 60 {
		return fmt.Errorf("muse_http_timeout_secs must be 1-60")
	}
	if c.MuseBaseURL != "" {
		if err := validateMuseBaseURL(c.MuseBaseURL); err != nil {
			return err
		}
	}
	if c.MuseProxyURL != "" {
		if err := validateMuseProxyURL(c.MuseProxyURL); err != nil {
			return err
		}
	}
	return nil
}

// validateMuseBaseURL keeps the cookie-bearing control plane on TLS except for
// loopback test servers. A path/query is rejected so the endpoint join is
// unambiguous.
func validateMuseBaseURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("muse_base_url is not a valid URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return fmt.Errorf("muse_base_url must be http(s)")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("muse_base_url must be an origin only (no userinfo/path/query/fragment)")
	}
	if u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		return fmt.Errorf("muse_base_url must use https unless the host is loopback")
	}
	return nil
}

func validateMuseProxyURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("muse_proxy_url is not a valid URL")
	}
	switch u.Scheme {
	case "http", "https", "socks5":
	default:
		return fmt.Errorf("muse_proxy_url must be http(s) or socks5")
	}
	if u.Fragment != "" {
		return fmt.Errorf("muse_proxy_url must not carry a fragment")
	}
	return nil
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
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
