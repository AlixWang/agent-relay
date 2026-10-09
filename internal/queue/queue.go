// Package queue owns message acceptance, routing and delivery state (DESIGN §4.4).
package queue

import (
	"encoding/json"
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
	// Execution handshake (§6.5):
	Status        string `json:"status"`          // kind=status: started|progress|blocked|resumed|cancelled
	Op            string `json:"op"`              // kind=permission_request
	Target        string `json:"target"`          // kind=permission_request
	Detail        string `json:"detail"`          // kind=permission_request
	Decision      string `json:"decision"`        // kind=permission_decision: allow|deny
	ExpiresInSecs int64  `json:"expires_in_secs"` // kind=permission_request
	// Conversation fields (§v12):
	ConvID   string   `json:"conv_id"`   // conversation ID for group chat
	SeenSeq  int64    `json:"seen_seq"`  // freshness check for unsolicited replies
	Mentions []string `json:"mentions"`  // @-mentioned peer IDs
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

// Handshake TTL bounds (DESIGN §6.5). ExpiresInSecs above the max is
// clamped, not rejected — a B that overstates patience still gets a
// decision window instead of a confusing 400.
const (
	DefaultPermissionTTL = 600
	MaxPermissionTTL     = 3600
)

// defaultPermissionTTL is the effective default when a request omits
// expires_in_secs. main overrides it from config (permission_ttl_secs).
var defaultPermissionTTL = int64(DefaultPermissionTTL)

// SetDefaultPermissionTTL overrides the expires_in_secs default.
func SetDefaultPermissionTTL(secs int64) {
	if secs > 0 {
		defaultPermissionTTL = secs
	}
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
		Status:           req.Status,
		Op:               req.Op,
		Target:           req.Target,
		Detail:           req.Detail,
		Decision:         req.Decision,
		ExpiresInSecs:    req.ExpiresInSecs,
		ConvID:           req.ConvID,
		SeenSeq:          req.SeenSeq,
		Mentions:         req.Mentions,
	}
	verdict, err := s.guard.Check(env, now)
	if err != nil {
		return 0, false, "", err
	}
	// Permission decisions take the CAS path (first decision wins).
	if kind == "permission_decision" {
		return s.sendDecision(req, verdict.RootID, now)
	}
	expiresAt := int64(0)
	if kind == "permission_request" {
		ttl := req.ExpiresInSecs
		if ttl <= 0 {
			ttl = defaultPermissionTTL
		}
		if ttl > MaxPermissionTTL {
			ttl = MaxPermissionTTL
		}
		expiresAt = now + ttl
	}
	approvalState := "n/a"
	if verdict.Held {
		approvalState = "pending"
	}

	// Convert mentions to JSON
	mentionsJSON := ""
	if len(req.Mentions) > 0 {
		b, _ := json.Marshal(req.Mentions)
		mentionsJSON = string(b)
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
		Status:           req.Status,
		Op:               req.Op,
		Target:           req.Target,
		Detail:           req.Detail,
		Decision:         req.Decision,
		ExpiresAt:        expiresAt,
		ConvID:           req.ConvID,
		Mentions:         mentionsJSON,
	})
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, false, "", &guard.Rejection{Code: "duplicate_id", Reason: "sender already used id " + req.ID}
		}
		return 0, false, "", err
	}
	if kind == "permission_request" {
		if err := s.st.CreatePermissionRequest(&store.PermissionRequest{
			RequestID: req.ID, Thread: verdict.RootID,
			Requester: req.From, Approver: req.To, Status: "pending",
			Op: req.Op, Target: req.Target, Detail: req.Detail,
			CreatedAt: now, ExpiresAt: expiresAt,
		}); err != nil {
			// The message row is already in; a permission row failure is
			// a server error, not a silent half-write — surface 500 via
			// the generic path (no Rejection code).
			return 0, false, "", fmt.Errorf("permission row: %w", err)
		}
	}

	// Update conversation streak if this is a conversation message (§v12)
	if req.ConvID != "" {
		if err := s.guard.UpdateConversationStreak(req.ConvID, req.From); err != nil {
			// Non-fatal: log but continue
			// The message is already inserted, don't roll back for streak update failure
		}
	}

	s.guard.RecordHit(req.From, now)
	auditAction := "message.sent"
	switch kind {
	case "result":
		auditAction = "result.sent"
	case "status":
		auditAction = "task.status"
	case "permission_request":
		auditAction = "permission.requested"
	}
	_ = s.st.AppendAudit(req.From, auditAction,
		fmt.Sprintf("seq=%d thread=%s to=%s held=%v status=%s", seq, verdict.RootID, req.To, verdict.Held, req.Status), now)
	return seq, verdict.Held, verdict.RootID, nil
}

// sendDecision runs the first-decision-wins CAS. A lost race, an expiry, or
// an already-decided row returns won=false; the row is re-read so the
// caller can answer idempotent replay (same value → 200) vs flip (409).
func (s *Service) sendDecision(req *SendRequest, rootID string, now int64) (int64, bool, string, error) {
	msg := &store.Message{
		ID: req.ID, Sender: req.From, Recipient: req.To,
		Kind: "permission_decision", InReplyTo: req.InReplyTo,
		RootID: rootID, ApprovalState: "n/a",
		Payload: req.Payload, CreatedAt: now, Decision: req.Decision,
	}
	pr, err := s.st.GetPermissionRequest(req.InReplyTo)
	if err != nil {
		return 0, false, "", err
	}
	if pr == nil {
		return 0, false, "", &guard.Rejection{Code: "bad_permission_ref", Reason: "unknown permission request"}
	}
	// Same value twice: idempotent replay of a won race returns the
	// original decision seq. Matches on value (not message id) so a
	// retried decision with a fresh id is still idempotent.
	if pr.Status != "pending" {
		if (pr.Status == "allowed") == (req.Decision == "allow") {
			if won, err := s.st.GetBySenderID(pr.Approver, pr.DecisionID); err == nil && won != nil {
				s.guard.RecordHit(req.From, now)
				return won.Seq, false, rootID, nil
			}
		}
		return 0, false, "", &guard.Rejection{Code: "permission_already_decided", Reason: "request already decided"}
	}
	if now >= pr.ExpiresAt {
		return 0, false, "", &guard.Rejection{Code: "permission_expired", Reason: "request expired; the recipient must treat it as denied"}
	}
	won, err := s.st.DecidePermission(msg, req.InReplyTo, req.Decision, now)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return 0, false, "", &guard.Rejection{Code: "duplicate_id", Reason: "sender already used id " + req.ID}
		}
		return 0, false, "", err
	}
	if !won {
		// Lost the CAS race after passing the check: re-read and answer
		// same as above (idempotent vs conflict).
		if pr2, err := s.st.GetPermissionRequest(req.InReplyTo); err == nil && pr2 != nil && pr2.Status != "pending" {
			if (pr2.Status == "allowed") == (req.Decision == "allow") {
				if orig, err := s.st.GetBySenderID(pr2.Approver, pr2.DecisionID); err == nil && orig != nil {
					s.guard.RecordHit(req.From, now)
					return orig.Seq, false, rootID, nil
				}
			}
		}
		return 0, false, "", &guard.Rejection{Code: "permission_already_decided", Reason: "request already decided"}
	}
	s.guard.RecordHit(req.From, now)
	_ = s.st.AppendAudit(req.From, "permission.decided",
		fmt.Sprintf("thread=%s request=%s decision=%s", rootID, req.InReplyTo, req.Decision), now)
	decided, err := s.st.GetBySenderID(req.From, req.ID)
	if err != nil || decided == nil {
		return 0, false, "", fmt.Errorf("decision row missing after CAS win")
	}
	return decided.Seq, false, rootID, nil
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
