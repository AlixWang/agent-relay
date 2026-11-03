package work

import (
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func m(seq int64, kind, status, approval string, at int64) *store.Message {
	return &store.Message{Seq: seq, Kind: kind, Status: status, ApprovalState: approval, CreatedAt: at}
}

func TestDeriveStages(t *testing.T) {
	task := &store.Message{Seq: 1, Kind: "task", ApprovalState: "n/a", CreatedAt: 100}
	tests := []struct {
		name      string
		task      *store.Message
		msgs      []*store.Message
		perm      bool
		wantState string
		wantStage string
	}{
		{"queued", task, []*store.Message{task}, false, "open", "queued"},
		{"approval", &store.Message{Seq: 1, Kind: "task", ApprovalState: "pending", CreatedAt: 100}, []*store.Message{task}, false, "open", "person/approval"},
		{"rejected", &store.Message{Seq: 1, Kind: "task", ApprovalState: "rejected", CreatedAt: 100}, []*store.Message{task}, false, "cancelled", "cancelled"},
		{"working", task, []*store.Message{task, m(2, "status", "started", "", 110)}, false, "open", "working"},
		{"progress", task, []*store.Message{task, m(2, "status", "progress", "", 110)}, false, "open", "working"},
		{"blocked", task, []*store.Message{task, m(2, "status", "blocked", "", 110)}, false, "open", "person/blocked"},
		{"answer", task, []*store.Message{task, m(2, "status", "blocked", "", 110)}, true, "open", "person/answer"},
		{"done", task, []*store.Message{task, m(2, "result", "", "", 120)}, false, "done", "done"},
		{"done_wins_over_blocked", task, []*store.Message{task, m(2, "status", "blocked", "", 110), m(3, "result", "", "", 120)}, false, "done", "done"},
		{"waiting", task, []*store.Message{task, m(2, "status", "waiting", "", 110)}, false, "open", "idle"},
		{"ready", task, []*store.Message{task, m(2, "status", "ready", "", 110)}, false, "open", "person/review"},
		{"failed", task, []*store.Message{task, m(2, "status", "failed", "", 110)}, false, "open", "person/failed"},
		{"dropped", task, []*store.Message{task, m(2, "status", "dropped", "", 110)}, false, "done", "done"},
		{"dropped_is_terminal", task, []*store.Message{task, m(2, "status", "dropped", "", 110), m(3, "status", "started", "", 120)}, false, "done", "done"},
		{"cancelled", task, []*store.Message{task, m(2, "status", "cancelled", "", 110)}, false, "cancelled", "cancelled"},
		{"cancelled_is_terminal", task, []*store.Message{task, m(2, "status", "cancelled", "", 110), m(3, "status", "resumed", "", 120)}, false, "cancelled", "cancelled"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Derive(tt.task, tt.msgs, tt.perm)
			if got.State != tt.wantState || got.Stage != tt.wantStage {
				t.Fatalf("Derive=%+v want state=%s stage=%s", got, tt.wantState, tt.wantStage)
			}
		})
	}
}

func TestEffectiveOfflineOverlay(t *testing.T) {
	tk := &store.Ticket{Stage: "working", AssigneeLastSeen: 1000}
	stage, attention, online := Effective(tk, 1200, 300)
	if !online || stage != "working" || attention {
		t.Fatalf("online: stage=%s attention=%v online=%v", stage, attention, online)
	}
	stage, attention, online = Effective(tk, 2000, 300)
	if online || stage != "person/offline" || !attention {
		t.Fatalf("offline: stage=%s attention=%v online=%v", stage, attention, online)
	}
	// A done ticket never becomes person/offline.
	tk.Stage = "done"
	stage, _, _ = Effective(tk, 2000, 300)
	if stage != "done" {
		t.Fatalf("done stage changed: %s", stage)
	}
	// An expired lease wins over a fresh heartbeat: the executor may be alive
	// but is no longer the authorized owner of the ticket.
	tk.Stage = "working"
	stage, attention, online = EffectiveLease(tk, true, 1200, 300)
	if online || stage != "person/offline" || !attention {
		t.Fatalf("lease: stage=%s attention=%v online=%v", stage, attention, online)
	}
}
