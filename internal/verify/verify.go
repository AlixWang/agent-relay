// Package verify drives the onboarding state machine (DESIGN §8.4):
// pending → verifying → active | failed. The server never trusts "I'm set up".
package verify

import (
	"fmt"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Service checks smoke completion and timeouts.
type Service struct {
	st           store.Store
	verifyExpiry int64 // seconds
}

// New creates a verify Service.
func New(st store.Store, verifyTimeoutSecs int64) *Service {
	if verifyTimeoutSecs <= 0 {
		verifyTimeoutSecs = 600
	}
	return &Service{st: st, verifyExpiry: verifyTimeoutSecs}
}

// MarkVerifying flips pending → verifying when smoke is issued.
func (s *Service) MarkVerifying(peerID string, now int64) error {
	p, err := s.st.GetPeer(peerID)
	if err != nil || p == nil {
		return err
	}
	if p.Status == "pending" || p.Status == "failed" {
		return s.st.UpdatePeerStatus(peerID, "verifying")
	}
	return nil
}

// CheckCompletion returns true when the peer's smoke task has both a
// matching result (kind=result, in_reply_to=smokeID, from=peer) and an ack
// on the smoke message itself. On success it flips to active + audits.
func (s *Service) CheckCompletion(peerID string, now int64) (bool, error) {
	smokeID, _, err := s.st.GetSmoke(peerID)
	if err != nil || smokeID == "" {
		return false, err
	}
	// Smoke message(s) with this id (sender=system).
	smokes, err := s.st.GetByIDAnySender(smokeID)
	if err != nil {
		return false, err
	}
	var smokeSeq int64
	for _, m := range smokes {
		if m.Sender == "system" && m.Recipient == peerID {
			smokeSeq = m.Seq
			break
		}
	}
	if smokeSeq == 0 {
		return false, nil
	}
	acked, err := s.st.IsAcked(smokeSeq, peerID)
	if err != nil || !acked {
		return false, err
	}
	// Results carry their own ids; search for in_reply_to == smokeID from peer.
	// Thread scan: smoke lives in verify/<peer>, scan that thread.
	thread, err := s.st.ThreadMessages("verify/"+peerID, 50)
	if err != nil {
		return false, err
	}
	hasResult := false
	for _, m := range thread {
		if m.Sender == peerID && m.Kind == "result" && m.InReplyTo == smokeID {
			hasResult = true
			break
		}
	}
	if !hasResult {
		return false, nil
	}
	// Only pending/verifying peers graduate. A peer the admin suspended
	// (or already failed) must stay as-is — verification must not override
	// a human decision.
	p, err := s.st.GetPeer(peerID)
	if err != nil || p == nil {
		return false, err
	}
	if p.Status != "pending" && p.Status != "verifying" {
		return false, nil
	}
	if err := s.st.UpdatePeerStatus(peerID, "active"); err != nil {
		return false, err
	}
	_ = s.st.ClearSmoke(peerID)
	_ = s.st.AppendAudit("system", "peer.verified", "peer="+peerID, now)
	return true, nil
}

// Sweep marks verifying peers past the timeout as failed, with a reason
// distinguishing "registered, never polled" from "polled, never acked".
func (s *Service) Sweep(now int64) {
	peers, err := s.st.ListPeers()
	if err != nil {
		return
	}
	for _, p := range peers {
		if p.Status != "verifying" && p.Status != "pending" {
			continue
		}
		smokeID, smokeTs, err := s.st.GetSmoke(p.ID)
		if err != nil || smokeID == "" {
			// Never even requested smoke: only fail pending peers that are old.
			if p.Status == "pending" && now-p.CreatedAt > s.verifyExpiry*2 {
				_ = s.st.UpdatePeerStatus(p.ID, "failed")
				_ = s.st.AppendAudit("system", "peer.verify_failed",
					fmt.Sprintf("peer=%s reason=registered_never_verified", p.ID), now)
			}
			continue
		}
		if now-smokeTs < s.verifyExpiry {
			continue
		}
		reason := "registered_never_polled"
		if p.LastSeen > smokeTs {
			reason = "polled_never_acked"
		}
		_ = s.st.UpdatePeerStatus(p.ID, "failed")
		_ = s.st.AppendAudit("system", "peer.verify_failed",
			fmt.Sprintf("peer=%s reason=%s smoke=%s", p.ID, reason, smokeID), now)
	}
}
