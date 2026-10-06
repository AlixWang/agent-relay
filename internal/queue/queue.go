// Package queue owns message acceptance, routing and delivery state (DESIGN §4.4).
package queue

import (
	"fmt"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/guard"
	"github.com/AlixWang/agent-relay/internal/store"
)

// SendRequest is the wire envelope (DESIGN §5.2, Appendix A).
type SendRequest struct {
	ID               string `json:"id"`
	To               string `json:"to"`
	From             string `json:"from"`
	Kind             string `json:"kind"`
	InReplyTo        string `json:"in_reply_to"`
	RequiresApproval bool   `json:"requires_approval"`
	Payload          string `json:"payload"`
	ProtocolVersion  int    `json:"protocol_version"`
}

// Service ties guard + store together.
type Service struct {
	st    store.Store
	guard *guard.Guard
}

// New creates a queue Service.
func New(st store.Store, g *guard.Guard) *Service {
	return &Service{st: st, guard: g}
}

// Send validates (guard first), inserts, and returns (seq, held, rootID).
func (s *Service) Send(req *SendRequest, now int64) (int64, bool, string, error) {
	if !auth.ValidID(req.ID) {
		return 0, false, "", &guard.Rejection{Code: "bad_message", Reason: "bad id"}
	}
	if !auth.ValidTarget(req.To) {
		return 0, false, "", &guard.Rejection{Code: "bad_message", Reason: "bad to"}
	}
	if !auth.ValidID(req.From) {
		return 0, false, "", &guard.Rejection{Code: "bad_message", Reason: "bad from"}
	}
	kind := req.Kind
	if kind == "" {
		kind = "task"
	}
	env := &guard.Envelope{
		ID:               req.ID,
		To:               req.To,
		From:             req.From,
		Kind:             kind,
		InReplyTo:        req.InReplyTo,
		RequiresApproval: req.RequiresApproval,
		Payload:          req.Payload,
	}
	verdict, err := s.guard.Check(env, now)
	if err != nil {
		return 0, false, "", err
	}
	approvalState := "n/a"
	if verdict.Held {
		approvalState = "pending"
	}
	seq, err := s.st.InsertMessage(&store.Message{
		ID:               req.ID,
		Sender:           req.From,
		Recipient:        req.To,
		Kind:             kind,
		InReplyTo:        req.InReplyTo,
		RootID:           verdict.RootID,
		RequiresApproval: req.RequiresApproval,
		ApprovalState:    approvalState,
		Payload:          req.Payload,
		CreatedAt:        now,
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, false, "", &guard.Rejection{Code: "duplicate_id", Reason: "sender already used id " + req.ID}
		}
		return 0, false, "", err
	}
	s.guard.RecordHit(req.From, now)
	auditAction := "message.sent"
	if kind == "result" {
		auditAction = "result.sent"
	}
	_ = s.st.AppendAudit(req.From, auditAction,
		fmt.Sprintf("seq=%d thread=%s to=%s held=%v", seq, verdict.RootID, req.To, verdict.Held), now)
	return seq, verdict.Held, verdict.RootID, nil
}

// Visible returns messages visible to peerID with seq > since (DESIGN §4.4).
func (s *Service) Visible(peerID string, since int64, limit int) ([]*store.Message, error) {
	return s.st.VisibleTo(peerID, since, limit)
}

// AckByID marks every message with client id visible to `by` as acked.
// Unknown ids are a no-op success (idempotent, mirrors relay v2).
func (s *Service) AckByID(messageID, by string, now int64) (bool, error) {
	cands, err := s.st.GetByIDAnySender(messageID)
	if err != nil {
		return false, err
	}
	acked := false
	for _, m := range cands {
		visible := m.Recipient == by || (m.Recipient == "*" && m.Sender != by)
		if !visible {
			continue
		}
		if m.ApprovalState == "pending" {
			continue
		}
		if err := s.st.Ack(m.Seq, by, now); err != nil {
			return false, err
		}
		acked = true
	}
	return acked, nil
}

// Thread returns one thread oldest-first capped at limit.
func (s *Service) Thread(rootID string, limit int) ([]*store.Message, error) {
	return s.st.ThreadMessages(rootID, limit)
}

// Smoke creates the server-issued verification message (§8.4).
// A repeat call while a smoke is still pending (5s window) returns the
// existing smoke instead of UNIQUE-conflicting, so impatient assistants
// can't wedge onboarding with rapid retries.
func (s *Service) Smoke(peerID string) (seq int64, smokeID string, err error) {
	now := time.Now().Unix()
	if prev, prevTs, _ := s.st.GetSmoke(peerID); prev != "" && now-prevTs < 5 {
		msgs, _ := s.st.GetByIDAnySender(prev)
		for _, m := range msgs {
			if m.Sender == "system" && m.Recipient == peerID {
				return m.Seq, prev, nil
			}
		}
	}
	smokeID = fmt.Sprintf("smoke-%s-%d", peerID, now)
	if err := s.st.SetSmoke(peerID, smokeID, now); err != nil {
		return 0, "", err
	}
	seq, err = s.st.InsertMessage(&store.Message{
		ID:            smokeID,
		Sender:        "system",
		Recipient:     peerID,
		Kind:          "task",
		RootID:        "verify/" + peerID,
		ApprovalState: "n/a",
		Payload:       "冒烟测试：收到请回复『收到』并 ack 本条。",
		CreatedAt:     now,
	})
	if err != nil {
		return 0, "", err
	}
	_ = s.st.AppendAudit("system", "verify.smoke_issued", "peer="+peerID+" id="+smokeID, now)
	return seq, smokeID, nil
}
