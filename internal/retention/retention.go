// Package retention implements background storage hygiene (DESIGN §4.7, §10.2).
package retention

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Policy configures retention windows.
type Policy struct {
	MessageTTLDays         int
	PeerPruneAfterDays     int
	AuditRetentionDays     int
	AgentLogRetentionDays  int
	AgentLogMaxRowsPerPeer int
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
	// One-time pre-v1.10.1 audit noise cleanup. The store method is guarded by
	// a settings marker and does not scan the table again after the first run.
	if purge, err := r.st.PurgeAuditNoise(now); err != nil {
		return stats, err
	} else if purge != nil && !purge.AlreadyDone &&
		(purge.AutoWakeSuccess > 0 || purge.PeerOffline > 0) {
		stats["audit_noise_purged"] = purge.AutoWakeSuccess + purge.PeerOffline
		stats["audit_auto_wake_purged"] = purge.AutoWakeSuccess
		stats["audit_peer_offline_purged"] = purge.PeerOffline
	}
	if purge2, err := r.st.PurgeLogNoiseV2(now); err != nil {
		return stats, err
	} else if purge2 != nil && !purge2.AlreadyDone {
		total := purge2.FuseTripped + purge2.AutoWake502 + purge2.PeerOffline +
			purge2.RetentionSkipped + purge2.AgentDaemonStarted
		if total > 0 {
			stats["log_noise_purged"] = total
			stats["audit_fuse_tripped_purged"] = purge2.FuseTripped
			stats["audit_auto_wake_502_purged"] = purge2.AutoWake502
			stats["audit_peer_offline_v2_purged"] = purge2.PeerOffline
			stats["audit_retention_skipped_purged"] = purge2.RetentionSkipped
			stats["agent_daemon_started_purged"] = purge2.AgentDaemonStarted
		}
	}
	if purge3, err := r.st.PurgeStatusSpamV1(now); err != nil {
		return stats, err
	} else if purge3 != nil && !purge3.AlreadyDone && purge3.Deleted > 0 {
		stats["status_spam_purged"] = purge3.Deleted
		stats["status_spam_groups"] = purge3.Groups
	}
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
	if r.policy.AgentLogRetentionDays > 0 {
		cutoff := now - int64(r.policy.AgentLogRetentionDays)*86400
		n, err := r.st.PruneAgentLogs(cutoff)
		if err != nil {
			return stats, err
		}
		stats["agent_logs_pruned"] = n
	}
	if r.policy.AgentLogMaxRowsPerPeer > 0 {
		n, err := r.st.TrimAllAgentLogs(r.policy.AgentLogMaxRowsPerPeer)
		if err != nil {
			return stats, err
		}
		stats["agent_logs_trimmed"] = n
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
		// Ended tickets keep their projection row only until the same TTL;
		// open tickets never reach this path (their messages are pinned).
		if n, err := r.st.PruneTickets(now - int64(r.policy.MessageTTLDays)*86400); err != nil {
			return stats, err
		} else {
			stats["tickets_pruned"] = n
		}
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
	var opGlobalRead int64
	if raw, err := r.st.GetSetting("operator_read_seq"); err == nil && raw != "" {
		opGlobalRead, _ = strconv.ParseInt(raw, 10, 64)
	}
	opPeerReads := map[string]int64{}

	for _, root := range roots {
		// Pin the whole thread when its newest message is still inside the
		// retention window. Check the newest message BEFORE scanning the
		// thread so an active thread with >scanCap messages never triggers a
		// false-positive retention.skipped audit warning.
		newest, err := r.st.LastNInThread(root, 1)
		if err != nil {
			return 0, 0, skipped, err
		}
		if len(newest) > 0 && newest[0].CreatedAt >= cutoff {
			continue
		}
		msgs, err := r.st.ThreadMessages(root, scanCap+1)
		if err != nil {
			return 0, 0, skipped, err
		}
		if len(msgs) > scanCap {
			skipped++
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
			ok, err := r.fullyAcked(m, active, opGlobalRead, opPeerReads)
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
	// An open ticket pins its whole evidence chain: deleting its task or a
	// status message would make the work item impossible to rebuild.
	allSeqs := make([]int64, 0, len(toArchive))
	for _, m := range toArchive {
		allSeqs = append(allSeqs, m.Seq)
	}
	pinned, err := r.st.OpenTicketMessageSeqs(allSeqs)
	if err != nil {
		return 0, 0, skipped, err
	}
	if len(pinned) > 0 {
		kept := toArchive[:0]
		for _, m := range toArchive {
			if !pinned[m.Seq] {
				kept = append(kept, m)
			}
		}
		toArchive = kept
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

func (r *Runner) fullyAcked(m *store.Message, active map[string]bool, opGlobalRead int64, opPeerReads map[string]int64) (bool, error) {
	if m.Kind == "status" {
		// Lifecycle status logs older than cutoff can be archived once the
		// thread/ticket is no longer open (OpenTicketMessageSeqs still pins
		// any status row belonging to an open ticket).
		return true, nil
	}
	if m.Recipient == auth.OperatorID {
		// Operator reads advance settings watermarks instead of writing acks.
		peerRead, ok := opPeerReads[m.Sender]
		if !ok {
			if raw, err := r.st.GetSetting("operator_read_seq:" + m.Sender); err == nil && raw != "" {
				peerRead, _ = strconv.ParseInt(raw, 10, 64)
			}
			opPeerReads[m.Sender] = peerRead
		}
		return m.Seq <= opGlobalRead || m.Seq <= peerRead, nil
	}
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
	if strings.HasPrefix(m.Recipient, "grp_") {
		room, err := r.st.GetRoom(m.Recipient)
		if err != nil {
			return false, err
		}
		if room == nil || room.ArchivedAt > 0 {
			return true, nil
		}
		members, err := r.st.ListRoomMembers(m.Recipient)
		if err != nil {
			return false, err
		}
		for _, mem := range members {
			if mem.PeerID == m.Sender || !active[mem.PeerID] || m.Seq <= mem.StartSeq {
				continue
			}
			acked, err := r.st.IsAcked(m.Seq, mem.PeerID)
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
