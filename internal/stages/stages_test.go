package stages

import (
	"strconv"
	"strings"
	"testing"
)

func TestRankAndOrderCase(t *testing.T) {
	seen := map[string]bool{}
	for i, stage := range All {
		if seen[stage] {
			t.Fatalf("duplicate stage %q", stage)
		}
		seen[stage] = true
		if Rank(stage) != i {
			t.Fatalf("Rank(%q)=%d want %d", stage, Rank(stage), i)
		}
		if !strings.Contains(OrderCase("t.stage"), "WHEN '"+stage+"' THEN "+strconv.Itoa(i)) {
			t.Fatalf("OrderCase missing %q rank %d", stage, i)
		}
	}
	if Rank("unknown") != len(All) {
		t.Fatalf("unknown rank: %d", Rank("unknown"))
	}
	if !Attention(PersonApproval) || Attention(Working) {
		t.Fatal("attention prefix rule broken")
	}
}
