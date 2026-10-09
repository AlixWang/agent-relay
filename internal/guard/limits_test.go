package guard

import (
	"testing"

	"github.com/AlixWang/agent-relay/internal/config"
)

// A configurable limit that never reaches the guard is a knob that silently
// does nothing in production. v0.14.0 shipped exactly that: the conversation
// caps were wired into a hand-built test fixture but not into main, so its
// guards were dead while its tests passed. This pins the mapping.
func TestLimitsFromConfigCarriesEveryKnob(t *testing.T) {
	cfg := &config.Config{
		FuseMaxMessages:      11,
		FuseMaxAgeSecs:       22,
		RatePerMinute:        33,
		MaxOpenPermissions:   44,
		ProgressThrottleSecs: 55,
		RoomFuseMaxMessages:  66,
		RoomFuseWindowSecs:   77,
	}
	l := LimitsFromConfig(cfg)
	if l.FuseMaxMessages != 11 || l.FuseMaxAgeSecs != 22 || l.RatePerMinute != 33 ||
		l.MaxOpenPermissions != 44 || l.ProgressThrottleSecs != 55 ||
		l.RoomFuseMaxMessages != 66 || l.RoomFuseWindowSecs != 77 {
		t.Fatalf("limits did not carry every knob: %+v", l)
	}
	if got := LimitsFromConfig(nil); got != (Limits{}) {
		t.Fatalf("nil config should give zero limits, got %+v", got)
	}
	// The shipped defaults must actually bound a room.
	d := LimitsFromConfig(config.Default())
	if d.RoomFuseMaxMessages <= 0 || d.RoomFuseWindowSecs <= 0 {
		t.Fatalf("room caps missing from defaults: %+v", d)
	}
}
