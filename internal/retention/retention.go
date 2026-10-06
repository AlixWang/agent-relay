// Package retention implements background storage hygiene (DESIGN §4.7, §10.2).
package retention

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

// Policy configures retention windows.
type Policy struct {
	MessageTTLDays     int
	PeerPruneAfterDays int
	AuditRetentionDays int
	// PermissionTTLSecs overrides the queue default (600s) for how long a
	// pending permission request stays decidable. <=0 keeps the default.
	PermissionTTLSecs int64
	DataDir           string
}

// Runner executes retention passes.
type Runner struct {
	st     store.Store
	policy Policy
}

// New creates a Runner.
func New(st store.Store, p Policy) *Runner { return &Runner{st: st, policy: p} }

// RunOnce performs a single pass: archive+delete old fully-acked messages,
// prune stale peers, prune audit log. Returns human-readable counts.
func (r *Runner) RunOnce(now int64) (map[string]int64, error) {
	stats := map[string]int64{}
	msgCutoff := now - int64(r.policy.MessageTTLDays)*86400
	if r.policy.MessageTTLDays > 0 {
		archived, deleted, skipped, err := r.archiveOldMessages(msgCutoff, now)
		if err != nil {
			return stats, err
		}
		stats["messages_archived"] = int64(archived)
		stats["messages_deleted"] = int64(deleted)
		// Threads beyond the scan cap are reported, not silently kept.
		if skipped > 0 {
			stats["threads_skipped"] = int64(skipped)
			_ = r.st.AppendAudit("system", "retention.skipped",
				fmt.Sprintf("threads=%d cap=%d", skipped, scanCap), now)
		}
	}
	if r.policy.PeerPruneAfterDays > 0 {
		cutoff := now - int64(r.policy.PeerPruneAfterDays)*86400
		n, err := r.st.PrunePeers(cutoff)
		if err != nil {
			return stats, err
		}
		stats["peers_pruned"] = n
		// NOTE: tokens persist until revoked (DESIGN §10.2) — no token delete here.
	}
	if r.policy.AuditRetentionDays > 0 {
		cutoff := now - int64(r.policy.AuditRetentionDays)*86400
		n, err := r.st.PruneAudit(cutoff)
		if err != nil {
			return stats, err
		}
		stats["audit_pruned"] = n
	}
	// Decided/expired permission rows older than the message TTL are
	// dropped; pending rows are never pruned here (expiry is enforced at
	// decision time, and CountOpenPermissions ignores expired rows).
	if r.policy.MessageTTLDays > 0 {
		n, err := r.st.PrunePermissions(now - int64(r.policy.MessageTTLDays)*86400)
		if err != nil {
			return stats, err
		}
		stats["permissions_pruned"] = n
	}
	return stats, nil
}

// archiveOldMessages exports fully-acked messages older than cutoff to a
// JSONL archive, then deletes them. "Fully acked" for direct = recipient
// acked; for broadcast ('*') = every active peer acked. Held (pending)
// messages are never archived.
// scanCap bounds a single retention pass. Threads beyond the cap are counted
// in threads_skipped (audited) instead of silently blocking TTL forever.
const scanCap = 1000

func (r *Runner) archiveOldMessages(cutoff, now int64) (archived, deleted, skipped int, err error) {
	roots, err := r.st.ThreadRoots(scanCap + 1)
	if err != nil {
		return 0, 0, 0, err
	}
	if len(roots) > scanCap {
		skipped = len(roots) - scanCap
		roots = roots[:scanCap]
	}
	var toArchive []*store.Message
	peers, err := r.st.ListPeers()
	if err != nil {
		return 0, 0, skipped, err
	}
	active := map[string]bool{}
	for _, p := range peers {
		if p.Status == "active" {
			active[p.ID] = true
		}
	}
	for _, root := range roots {
		msgs, err := r.st.ThreadMessages(root, scanCap+1)
		if err != nil {
			return 0, 0, skipped, err
		}
		if len(msgs) > scanCap {
			skipped++
			msgs = msgs[:scanCap]
		}
		for _, m := range msgs {
			if m.CreatedAt > cutoff {
				continue
			}
			if m.ApprovalState == "pending" {
				continue
			}
			if m.ApprovalState == "rejected" {
				// Human-rejected holds are terminal: nobody will ever
				// ack them, so archive unconditionally after TTL.
				toArchive = append(toArchive, m)
				continue
			}
			ok, err := r.fullyAcked(m, active)
			if err != nil {
				return 0, 0, skipped, err
			}
			if ok {
				toArchive = append(toArchive, m)
			}
		}
	}
	if len(toArchive) == 0 {
		return 0, 0, skipped, nil
	}
	if err := r.appendArchive(toArchive, now); err != nil {
		return 0, 0, skipped, err
	}
	seqs := make([]int64, 0, len(toArchive))
	for _, m := range toArchive {
		seqs = append(seqs, m.Seq)
	}
	if err := r.st.DeleteMessages(seqs); err != nil {
		return len(toArchive), 0, skipped, err
	}
	_ = r.st.AppendAudit("system", "retention.archived",
		fmt.Sprintf("messages=%d cutoff=%d", len(toArchive), cutoff), now)
	return len(toArchive), len(toArchive), skipped, nil
}

func (r *Runner) fullyAcked(m *store.Message, active map[string]bool) (bool, error) {
	if m.Recipient == "*" {
		for peerID := range active {
			if peerID == m.Sender {
				continue
			}
			acked, err := r.st.IsAcked(m.Seq, peerID)
			if err != nil {
				return false, err
			}
			if !acked {
				return false, nil
			}
		}
		return true, nil
	}
	return r.st.IsAcked(m.Seq, m.Recipient)
}

func (r *Runner) appendArchive(msgs []*store.Message, now int64) error {
	dir := filepath.Join(r.policy.DataDir, "archive")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, time.Unix(now, 0).UTC().Format("2006-01")+".jsonl")
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := json.NewEncoder(f)
	for _, m := range msgs {
		if err := enc.Encode(m); err != nil {
			return err
		}
	}
	return nil
}
