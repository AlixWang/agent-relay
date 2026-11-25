package mcpgateway

import (
	"fmt"
	"regexp"
	"strings"
)

// Service ids and credential reference names are part of the deployed
// contract (design v5 §1.3/§6).
const (
	ServiceAdSense = "adsense"
	ServiceBing    = "bing-webmaster"
	ServiceGA      = "ga"

	CredentialAdSense = "secret:GA_ADSENSE_ADC"
	CredentialBing    = "secret:BING_WM_API_KEY"
	// GA reuses the AdSense ADC per design v4 §14.1/§14.4.
	CredentialGA = CredentialAdSense
)

var secretRefRe = regexp.MustCompile(`^secret:[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)

// Config is the [mcp_gateway] section. It is decoded by config.Load through
// UnmarshalTOML so unknown nested keys fail fast. Defaults are safe: the
// gateway and the write path are both off.
type Config struct {
	Enabled                 bool
	UpstreamTimeoutSecs     int
	GatewayTimeoutSecs      int
	RatePerMinutePerPeer    int
	RatePerHourPerPeer      int
	MaxRowsPerCall          int
	AuditRetentionDaysRead  int
	AuditRetentionDaysWrite int
	AuditMaxRows            int

	Services map[string]ServiceConfig
	Write    WriteConfig
	Keys     KeysConfig
}

// ServiceConfig is one [mcp_gateway.services.<id>] segment.
type ServiceConfig struct {
	Enabled       bool
	CredentialRef string
	AllowedTools  []string
}

// WriteConfig is [mcp_gateway.write] plus per-service child tables.
type WriteConfig struct {
	Enabled                   bool
	Services                  map[string]WriteServiceConfig
	BatchCap                  int
	ApprovalTTLSeconds        int
	ApprovalPendingTTLSeconds int
	LowRiskWritePerHour       int
	HighRiskWritePerHour      int
}

// WriteServiceConfig is one [mcp_gateway.write.<service>] table. The scalar
// knobs are optional per-service overrides (design v5 §6 places them under the
// Bing service table); zero means "use the [mcp_gateway.write] default".
type WriteServiceConfig struct {
	AllowedTools              []string
	BatchCap                  int
	ApprovalTTLSeconds        int
	ApprovalPendingTTLSeconds int
	LowRiskWritePerHour       int
	HighRiskWritePerHour      int
}

// KeysConfig holds key lifetime defaults.
type KeysConfig struct {
	ReadKeyTTLDays    int
	WriteKeyTTLDays   int
	PerToolKeyTTLDays int
	RotateGraceSecs   int
}

// DefaultConfig returns the documented defaults (#25 reads-only new keys,
// write master switch off, only the four low-risk Bing writes enabled).
func DefaultConfig() Config {
	return Config{
		Enabled:                 false,
		UpstreamTimeoutSecs:     15,
		GatewayTimeoutSecs:      30,
		RatePerMinutePerPeer:    30,
		RatePerHourPerPeer:      600,
		MaxRowsPerCall:          1000,
		AuditRetentionDaysRead:  90,
		AuditRetentionDaysWrite: 180,
		AuditMaxRows:            2_000_000,
		Services: map[string]ServiceConfig{
			ServiceAdSense: {Enabled: true, CredentialRef: CredentialAdSense, AllowedTools: []string{"*"}},
			ServiceBing:    {Enabled: true, CredentialRef: CredentialBing, AllowedTools: []string{"*"}},
			ServiceGA:      {Enabled: true, CredentialRef: CredentialGA, AllowedTools: []string{"*"}},
		},
		Write: WriteConfig{
			Enabled: false,
			Services: map[string]WriteServiceConfig{
				ServiceAdSense: {AllowedTools: []string{}},
				// GA is read-only in this gateway; the empty write allowlist
				// keeps create_property_annotation (and any future GA write)
				// behind all four write layers.
				ServiceGA: {AllowedTools: []string{}},
				// add_blocked_url is implemented but intentionally NOT in the
				// first-batch allowlist (design v5 decision).
				ServiceBing: {AllowedTools: []string{"submit_url", "submit_url_batch", "submit_feed", "remove_feed"}},
			},
			BatchCap:                  100,
			ApprovalTTLSeconds:        600,
			ApprovalPendingTTLSeconds: 86400,
			LowRiskWritePerHour:       50,
			HighRiskWritePerHour:      10,
		},
		Keys: KeysConfig{
			ReadKeyTTLDays:    90,
			WriteKeyTTLDays:   30,
			PerToolKeyTTLDays: 7,
			RotateGraceSecs:   0,
		},
	}
}

// Service returns the configured service segment (zero value if absent).
func (c Config) Service(id string) ServiceConfig { return c.Services[id] }

func (c Config) serviceEnabled(id string) bool {
	s, ok := c.Services[id]
	return ok && s.Enabled
}

func (c Config) writeService(id string) WriteServiceConfig {
	return c.Write.Services[id]
}

// writeToolAllowed applies the four-layer default-off rule for one write
// tool: gateway write switch, per-service allowlist, service switch.
func (c Config) writeToolAllowed(serviceID, tool string) bool {
	if !c.Write.Enabled || !c.serviceEnabled(serviceID) {
		return false
	}
	entry, ok := c.Write.Services[serviceID]
	if !ok {
		return false
	}
	for _, name := range entry.AllowedTools {
		if name == tool {
			return true
		}
	}
	return false
}

// readToolAllowed is the read allowlist; read paths remain usable while the
// write master switch is off.
func (c Config) readToolAllowed(serviceID, tool string) bool {
	svc, ok := c.Services[serviceID]
	if !ok || !svc.Enabled {
		return false
	}
	for _, name := range svc.AllowedTools {
		if name == "*" || name == tool {
			return true
		}
	}
	return false
}

// toolAllowed reports whether the config exposes this tool at all.
func (c Config) toolAllowed(serviceID string, t Tool) bool {
	if WriteTool(t) {
		return c.writeToolAllowed(serviceID, t.Name)
	}
	return c.readToolAllowed(serviceID, t.Name)
}

// BatchCapFor returns the configured batch ceiling for a service (per-service
// override first, then the write-table default).
func (c Config) BatchCapFor(serviceID string) int {
	if w, ok := c.Write.Services[serviceID]; ok && w.BatchCap > 0 {
		return w.BatchCap
	}
	if c.Write.BatchCap <= 0 {
		return 100
	}
	return c.Write.BatchCap
}

func (c Config) approvalTTLFor(serviceID string) int {
	if w, ok := c.Write.Services[serviceID]; ok && w.ApprovalTTLSeconds > 0 {
		return w.ApprovalTTLSeconds
	}
	if c.Write.ApprovalTTLSeconds <= 0 {
		return DefaultConfig().Write.ApprovalTTLSeconds
	}
	return c.Write.ApprovalTTLSeconds
}

func (c Config) approvalPendingTTLFor(serviceID string) int {
	if w, ok := c.Write.Services[serviceID]; ok && w.ApprovalPendingTTLSeconds > 0 {
		return w.ApprovalPendingTTLSeconds
	}
	if c.Write.ApprovalPendingTTLSeconds <= 0 {
		return DefaultConfig().Write.ApprovalPendingTTLSeconds
	}
	return c.Write.ApprovalPendingTTLSeconds
}

// ApprovalTTLFor returns the approved execution window for a service.
func (c Config) ApprovalTTLFor(serviceID string) int { return c.approvalTTLFor(serviceID) }

// ApprovalPendingTTLFor returns the pending-approval deadline for a service.
func (c Config) ApprovalPendingTTLFor(serviceID string) int {
	return c.approvalPendingTTLFor(serviceID)
}

// WriteLimitsFor returns the low/high write-per-hour limits for a service.
func (c Config) WriteLimitsFor(serviceID string) (low, high int) {
	return c.writeHourlyLimitsFor(serviceID)
}

func (c Config) writeHourlyLimitsFor(serviceID string) (low, high int) {
	low, high = c.Write.LowRiskWritePerHour, c.Write.HighRiskWritePerHour
	if w, ok := c.Write.Services[serviceID]; ok {
		if w.LowRiskWritePerHour > 0 {
			low = w.LowRiskWritePerHour
		}
		if w.HighRiskWritePerHour > 0 {
			high = w.HighRiskWritePerHour
		}
	}
	if low <= 0 {
		low = DefaultConfig().Write.LowRiskWritePerHour
	}
	if high <= 0 {
		high = DefaultConfig().Write.HighRiskWritePerHour
	}
	return low, high
}

// KeysConfigDefault fills zero values from documented defaults.
func (k KeysConfig) normalized() KeysConfig {
	d := DefaultConfig().Keys
	if k.ReadKeyTTLDays <= 0 {
		k.ReadKeyTTLDays = d.ReadKeyTTLDays
	}
	if k.WriteKeyTTLDays <= 0 {
		k.WriteKeyTTLDays = d.WriteKeyTTLDays
	}
	if k.PerToolKeyTTLDays <= 0 {
		k.PerToolKeyTTLDays = d.PerToolKeyTTLDays
	}
	if k.RotateGraceSecs < 0 {
		k.RotateGraceSecs = d.RotateGraceSecs
	}
	return k
}

// Validate enforces structural invariants (values, not registry contents).
func (c *Config) Validate() error {
	if c.UpstreamTimeoutSecs < 1 || c.UpstreamTimeoutSecs > 120 {
		return fmt.Errorf("mcp_gateway.upstream_timeout_secs must be 1-120")
	}
	if c.GatewayTimeoutSecs < c.UpstreamTimeoutSecs || c.GatewayTimeoutSecs > 600 {
		return fmt.Errorf("mcp_gateway.gateway_timeout_secs must be >= upstream_timeout_secs and <= 600")
	}
	if c.RatePerMinutePerPeer < 1 || c.RatePerHourPerPeer < c.RatePerMinutePerPeer {
		return fmt.Errorf("mcp_gateway rate limits must be positive, per-hour >= per-minute")
	}
	if c.MaxRowsPerCall < 1 || c.MaxRowsPerCall > 10000 {
		return fmt.Errorf("mcp_gateway.max_rows_per_call must be 1-10000")
	}
	if c.AuditRetentionDaysRead < 0 || c.AuditRetentionDaysWrite < 0 {
		return fmt.Errorf("mcp_gateway audit retention days must be >= 0")
	}
	if c.AuditMaxRows < 0 {
		return fmt.Errorf("mcp_gateway.audit_max_rows must be >= 0")
	}
	for id, svc := range c.Services {
		if !ValidServiceID(id) {
			return fmt.Errorf("mcp_gateway.services.%s: invalid service id", id)
		}
		if svc.CredentialRef != "" && !secretRefRe.MatchString(svc.CredentialRef) {
			return fmt.Errorf("mcp_gateway.services.%s.credential_ref must be a secret:<name> reference", id)
		}
		if len(svc.AllowedTools) == 0 {
			return fmt.Errorf("mcp_gateway.services.%s.allowed_tools must not be empty (use \"*\")", id)
		}
	}
	if err := c.Write.validate(); err != nil {
		return err
	}
	if err := c.Keys.normalized().validate(); err != nil {
		return err
	}
	return nil
}

func (w *WriteConfig) validate() error {
	if w.BatchCap < 1 || w.BatchCap > 500 {
		return fmt.Errorf("mcp_gateway.write.batch_cap must be 1-500")
	}
	if w.ApprovalTTLSeconds < 60 || w.ApprovalTTLSeconds > 3600 {
		return fmt.Errorf("mcp_gateway.write.approval_ttl_secs must be 60-3600")
	}
	if w.ApprovalPendingTTLSeconds < 3600 || w.ApprovalPendingTTLSeconds > 7*86400 {
		return fmt.Errorf("mcp_gateway.write.approval_pending_ttl_secs must be 1h-7d")
	}
	if w.LowRiskWritePerHour < 1 || w.HighRiskWritePerHour < 1 {
		return fmt.Errorf("mcp_gateway.write hourly limits must be >= 1")
	}
	for id, svc := range w.Services {
		for _, name := range svc.AllowedTools {
			if name == "*" {
				return fmt.Errorf("mcp_gateway.write.%s.allowed_tools must list explicit tools, not \"*\"", id)
			}
		}
		if svc.BatchCap < 0 || svc.BatchCap > 500 {
			return fmt.Errorf("mcp_gateway.write.%s.batch_cap must be 1-500 (or 0 for default)", id)
		}
		if svc.ApprovalTTLSeconds != 0 && (svc.ApprovalTTLSeconds < 60 || svc.ApprovalTTLSeconds > 3600) {
			return fmt.Errorf("mcp_gateway.write.%s.approval_ttl_secs must be 60-3600", id)
		}
		if svc.ApprovalPendingTTLSeconds != 0 && (svc.ApprovalPendingTTLSeconds < 3600 || svc.ApprovalPendingTTLSeconds > 7*86400) {
			return fmt.Errorf("mcp_gateway.write.%s.approval_pending_ttl_secs must be 1h-7d", id)
		}
		if svc.LowRiskWritePerHour < 0 || svc.HighRiskWritePerHour < 0 {
			return fmt.Errorf("mcp_gateway.write.%s hourly limits must be >= 0", id)
		}
	}
	return nil
}

func (k KeysConfig) validate() error {
	k = k.normalized()
	if k.ReadKeyTTLDays < 1 || k.ReadKeyTTLDays > 3650 {
		return fmt.Errorf("mcp_gateway.keys.read_key_ttl_days must be 1-3650")
	}
	if k.WriteKeyTTLDays < 1 || k.WriteKeyTTLDays > 3650 {
		return fmt.Errorf("mcp_gateway.keys.write_key_ttl_days must be 1-3650")
	}
	if k.PerToolKeyTTLDays < 1 || k.PerToolKeyTTLDays > 3650 {
		return fmt.Errorf("mcp_gateway.keys.per_tool_key_ttl_days must be 1-3650")
	}
	if k.RotateGraceSecs < 0 || k.RotateGraceSecs > 7*86400 {
		return fmt.Errorf("mcp_gateway.keys.rotate_grace_secs must be 0-7d")
	}
	return nil
}

// ValidateAgainst checks registry-dependent config rules at startup:
// configured services must exist, allowed tool names must exist, and a write
// allowlist may only contain write tools.
func (c *Config) ValidateAgainst(reg *Registry) error {
	for id := range c.Services {
		if _, ok := reg.Get(id); !ok {
			return fmt.Errorf("mcp_gateway.services.%s: service is not registered", id)
		}
	}
	for id, svc := range c.Services {
		for _, name := range svc.AllowedTools {
			if name == "*" {
				continue
			}
			if _, ok := reg.Tool(id, name); !ok {
				return fmt.Errorf("mcp_gateway.services.%s.allowed_tools: unknown tool %q", id, name)
			}
		}
	}
	for id, wsvc := range c.Write.Services {
		if _, ok := reg.Get(id); !ok {
			return fmt.Errorf("mcp_gateway.write.%s: service is not registered", id)
		}
		for _, name := range wsvc.AllowedTools {
			t, ok := reg.Tool(id, name)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.%s.allowed_tools: unknown tool %q", id, name)
			}
			if !WriteTool(t) {
				return fmt.Errorf("mcp_gateway.write.%s.allowed_tools: %q is not a write tool", id, name)
			}
		}
	}
	for id := range c.Write.Services {
		if _, ok := c.Services[id]; !ok {
			return fmt.Errorf("mcp_gateway.write.%s: no matching services.%s segment", id, id)
		}
	}
	return nil
}

// UnmarshalTOML decodes [mcp_gateway] with fail-fast unknown keys. It starts
// from DefaultConfig so omitted fields keep their safe defaults.
func (c *Config) UnmarshalTOML(v any) error {
	m, ok := v.(map[string]any)
	if !ok {
		return fmt.Errorf("mcp_gateway must be a table")
	}
	*c = DefaultConfig()
	for key, val := range m {
		switch key {
		case "enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("mcp_gateway.enabled must be a boolean")
			}
			c.Enabled = b
		case "upstream_timeout_secs":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.upstream_timeout_secs must be an integer")
			}
			c.UpstreamTimeoutSecs = n
		case "gateway_timeout_secs":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.gateway_timeout_secs must be an integer")
			}
			c.GatewayTimeoutSecs = n
		case "rate_per_minute_per_peer":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.rate_per_minute_per_peer must be an integer")
			}
			c.RatePerMinutePerPeer = n
		case "rate_per_hour_per_peer":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.rate_per_hour_per_peer must be an integer")
			}
			c.RatePerHourPerPeer = n
		case "max_rows_per_call":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.max_rows_per_call must be an integer")
			}
			c.MaxRowsPerCall = n
		case "audit_retention_days_read":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.audit_retention_days_read must be an integer")
			}
			c.AuditRetentionDaysRead = n
		case "audit_retention_days_write":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.audit_retention_days_write must be an integer")
			}
			c.AuditRetentionDaysWrite = n
		case "audit_max_rows":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.audit_max_rows must be an integer")
			}
			c.AuditMaxRows = n
		case "services":
			services, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("mcp_gateway.services must be a table")
			}
			if c.Services == nil {
				c.Services = map[string]ServiceConfig{}
			}
			for id, raw := range services {
				svc, err := decodeServiceConfig(id, raw)
				if err != nil {
					return err
				}
				c.Services[id] = svc
			}
		case "write":
			write, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("mcp_gateway.write must be a table")
			}
			if err := c.Write.decode(write); err != nil {
				return err
			}
		case "keys":
			keys, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("mcp_gateway.keys must be a table")
			}
			if err := c.Keys.decode(keys); err != nil {
				return err
			}
		default:
			return fmt.Errorf("unknown config key %q in [mcp_gateway]", key)
		}
	}
	return c.Validate()
}

func decodeServiceConfig(id string, raw any) (ServiceConfig, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return ServiceConfig{}, fmt.Errorf("mcp_gateway.services.%s must be a table", id)
	}
	svc := ServiceConfig{Enabled: true, AllowedTools: []string{"*"}}
	for key, val := range m {
		switch key {
		case "enabled":
			b, ok := val.(bool)
			if !ok {
				return svc, fmt.Errorf("mcp_gateway.services.%s.enabled must be a boolean", id)
			}
			svc.Enabled = b
		case "credential_ref":
			s, ok := val.(string)
			if !ok {
				return svc, fmt.Errorf("mcp_gateway.services.%s.credential_ref must be a string", id)
			}
			svc.CredentialRef = s
		case "allowed_tools":
			list, err := stringList(val)
			if err != nil {
				return svc, fmt.Errorf("mcp_gateway.services.%s.allowed_tools: %w", id, err)
			}
			svc.AllowedTools = list
		default:
			return svc, fmt.Errorf("unknown config key %q in [mcp_gateway.services.%s]", key, id)
		}
	}
	return svc, nil
}

func (w *WriteConfig) decode(m map[string]any) error {
	for key, val := range m {
		switch key {
		case "enabled":
			b, ok := val.(bool)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.enabled must be a boolean")
			}
			w.Enabled = b
		case "batch_cap":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.batch_cap must be an integer")
			}
			w.BatchCap = n
		case "approval_ttl_secs":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.approval_ttl_secs must be an integer")
			}
			w.ApprovalTTLSeconds = n
		case "approval_pending_ttl_secs":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.approval_pending_ttl_secs must be an integer")
			}
			w.ApprovalPendingTTLSeconds = n
		case "low_risk_write_per_hour":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.low_risk_write_per_hour must be an integer")
			}
			w.LowRiskWritePerHour = n
		case "high_risk_write_per_hour":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.write.high_risk_write_per_hour must be an integer")
			}
			w.HighRiskWritePerHour = n
		case "services":
			return fmt.Errorf("mcp_gateway.write.services is not a table; use [mcp_gateway.write.<service>]")
		default:
			child, ok := val.(map[string]any)
			if !ok {
				return fmt.Errorf("unknown config key %q in [mcp_gateway.write]", key)
			}
			if !ValidServiceID(key) {
				return fmt.Errorf("mcp_gateway.write.%s: invalid service id", key)
			}
			svc := w.Services[key]
			for ck, cv := range child {
				switch ck {
				case "allowed_tools":
					list, err := stringList(cv)
					if err != nil {
						return fmt.Errorf("mcp_gateway.write.%s.allowed_tools: %w", key, err)
					}
					svc.AllowedTools = list
				case "batch_cap":
					n, ok := intValue(cv)
					if !ok {
						return fmt.Errorf("mcp_gateway.write.%s.batch_cap must be an integer", key)
					}
					svc.BatchCap = n
				case "approval_ttl_secs":
					n, ok := intValue(cv)
					if !ok {
						return fmt.Errorf("mcp_gateway.write.%s.approval_ttl_secs must be an integer", key)
					}
					svc.ApprovalTTLSeconds = n
				case "approval_pending_ttl_secs":
					n, ok := intValue(cv)
					if !ok {
						return fmt.Errorf("mcp_gateway.write.%s.approval_pending_ttl_secs must be an integer", key)
					}
					svc.ApprovalPendingTTLSeconds = n
				case "low_risk_write_per_hour":
					n, ok := intValue(cv)
					if !ok {
						return fmt.Errorf("mcp_gateway.write.%s.low_risk_write_per_hour must be an integer", key)
					}
					svc.LowRiskWritePerHour = n
				case "high_risk_write_per_hour":
					n, ok := intValue(cv)
					if !ok {
						return fmt.Errorf("mcp_gateway.write.%s.high_risk_write_per_hour must be an integer", key)
					}
					svc.HighRiskWritePerHour = n
				default:
					return fmt.Errorf("unknown config key %q in [mcp_gateway.write.%s]", ck, key)
				}
			}
			if w.Services == nil {
				w.Services = map[string]WriteServiceConfig{}
			}
			w.Services[key] = svc
		}
	}
	return nil
}

func (k *KeysConfig) decode(m map[string]any) error {
	for key, val := range m {
		switch key {
		case "read_key_ttl_days":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.keys.read_key_ttl_days must be an integer")
			}
			k.ReadKeyTTLDays = n
		case "write_key_ttl_days":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.keys.write_key_ttl_days must be an integer")
			}
			k.WriteKeyTTLDays = n
		case "per_tool_key_ttl_days":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.keys.per_tool_key_ttl_days must be an integer")
			}
			k.PerToolKeyTTLDays = n
		case "rotate_grace_secs":
			n, ok := intValue(val)
			if !ok {
				return fmt.Errorf("mcp_gateway.keys.rotate_grace_secs must be an integer")
			}
			k.RotateGraceSecs = n
		default:
			return fmt.Errorf("unknown config key %q in [mcp_gateway.keys]", key)
		}
	}
	return nil
}

func intValue(v any) (int, bool) {
	switch n := v.(type) {
	case int64:
		return int(n), true
	case int:
		return n, true
	default:
		return 0, false
	}
}

func stringList(v any) ([]string, error) {
	raw, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return ss, nil
		}
		return nil, fmt.Errorf("must be an array of strings")
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok {
			return nil, fmt.Errorf("must be an array of strings")
		}
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("must not contain empty strings")
		}
		out = append(out, s)
	}
	return out, nil
}
