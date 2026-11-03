// Package work projects work items from the immutable message log.
// See docs/adr/0001-work-items-as-projection.md.
package work

import (
	"github.com/AlixWang/agent-relay/internal/stages"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Derived is the stage/state computed from one ticket's message history.
type Derived struct {
	State     string
	Stage     string
	UpdatedAt int64
	EndedAt   int64
	LastSeq   int64
}

// Derive implements the stage table in docs/SUPERAGENT.md. The opening task is
// always the highest-priority source; a result or a terminal status closes the
// ticket and later status cannot reopen it — new work needs a new task message.
func Derive(task *store.Message, msgs []*store.Message, hasOpenPermission bool) Derived {
	d := Derived{State: "open", Stage: stages.Queued}
	if task == nil {
		return d
	}
	if len(msgs) == 0 {
		msgs = []*store.Message{task}
	}
	last := msgs[0]
	var latestStatus, terminal *store.Message
	terminalState, terminalStage := "", ""
	for _, m := range msgs {
		if m.Seq > last.Seq {
			last = m
		}
		switch m.Kind {
		case "status":
			if latestStatus == nil || m.Seq > latestStatus.Seq {
				latestStatus = m
			}
			// dropped/cancelled are terminal even if a stray started/resumed
			// arrives later: reopening would silently undo a human decision.
			state, stage := "", ""
			switch m.Status {
			case "dropped":
				state, stage = "done", stages.Done
			case "cancelled":
				state, stage = "cancelled", stages.Cancelled
			}
			if stage != "" && (terminal == nil || m.Seq < terminal.Seq) {
				terminal, terminalState, terminalStage = m, state, stage
			}
		case "result":
			if terminal == nil || m.Seq < terminal.Seq {
				terminal, terminalState, terminalStage = m, "done", stages.Done
			}
		}
	}
	d.UpdatedAt = last.CreatedAt
	d.LastSeq = last.Seq

	switch task.ApprovalState {
	case "pending":
		d.Stage = stages.PersonApproval
		return d
	case "rejected":
		d.State = "cancelled"
		d.Stage = stages.Cancelled
		d.EndedAt = last.CreatedAt
		return d
	}

	// A result or a terminal status closes this phase's ticket. This is
	// checked before an open permission so an answer arriving after a
	// close cannot resurrect the work item.
	if terminal != nil {
		d.State = terminalState
		d.Stage = terminalStage
		d.EndedAt = terminal.CreatedAt
		return d
	}
	if hasOpenPermission {
		d.Stage = stages.PersonAnswer
		return d
	}
	if latestStatus == nil {
		return d
	}
	switch latestStatus.Status {
	case "blocked":
		d.Stage = stages.PersonBlocked
	case "started", "progress", "resumed":
		d.Stage = stages.Working
	case "waiting":
		d.Stage = stages.Idle
	case "ready":
		d.Stage = stages.PersonReview
	case "failed":
		d.Stage = stages.PersonFailed
	}
	return d
}

// Effective applies the presence overlay used by the console. person/offline
// is never stored: a peer can come back at any time.
func Effective(t *store.Ticket, now, onlineSecs int64) (stage string, attention, online bool) {
	return EffectiveLease(t, false, now, onlineSecs)
}

// EffectiveLease is Effective plus the expired-assignment overlay. Keeping the
// two-argument form lets callers that do not load assignments stay simple.
func EffectiveLease(t *store.Ticket, leaseExpired bool, now, onlineSecs int64) (stage string, attention, online bool) {
	return stages.Effective(t.Stage, t.AssigneeLastSeen, now, onlineSecs, leaseExpired)
}
