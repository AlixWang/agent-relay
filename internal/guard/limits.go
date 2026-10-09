package guard

import "github.com/AlixWang/agent-relay/internal/config"

// LimitsFromConfig maps the configurable counters onto Limits. It exists so a
// knob that a human can set in config.toml always reaches the guard, and so
// tests that need prod-like behaviour can use the exact same mapping instead of
// hand-building a Limits literal that silently drifts from production (the
// v0.14.0 conversation caps were wired in a test fixture but never in main, so
// the feature had no limits at all while its tests passed).
func LimitsFromConfig(cfg *config.Config) Limits {
	if cfg == nil {
		return Limits{}
	}
	return Limits{
		FuseMaxMessages:      cfg.FuseMaxMessages,
		FuseMaxAgeSecs:       int64(cfg.FuseMaxAgeSecs),
		RatePerMinute:        cfg.RatePerMinute,
		MaxOpenPermissions:   cfg.MaxOpenPermissions,
		ProgressThrottleSecs: cfg.ProgressThrottleSecs,
		RoomFuseMaxMessages:  cfg.RoomFuseMaxMessages,
		RoomFuseWindowSecs:   int64(cfg.RoomFuseWindowSecs),
	}
}
