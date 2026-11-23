package store

import (
	"fmt"
	"strings"
)

// AuditNoisePurge counts one one-time cleanup pass. AlreadyDone means the
// settings marker was present and nothing was scanned.
type AuditNoisePurge struct {
	AutoWakeSuccess int64
	PeerOffline     int64
	AutoWakeKept    int64
	AlreadyDone     bool
}

// auditNoisePurgeMarker makes this cleanup one-time per database. It is a
// settings marker (like migrated.scope_v1) rather than an every-boot job: the
// scan is only useful for rows written before v1.10.1 started suppressing
// them.
const auditNoisePurgeMarker = "migrated.audit_noise_purge_v1"

// auditFieldValue extracts one `key=value` field from an audit detail line.
// next is the following field name, used to keep spaces inside an error
// message; empty means "rest of the line". The second return value reports
// whether the field was present at all.
func auditFieldValue(detail, field, next string) (string, bool) {
	marker := " " + field + "="
	idx := strings.Index(detail, marker)
	if idx < 0 {
		return "", false
	}
	rest := detail[idx+len(marker):]
	if next != "" {
		if end := strings.Index(rest, " "+next+"="); end >= 0 {
			rest = rest[:end]
		}
	}
	return strings.TrimSpace(rest), true
}

// autoWakeAuditSucceeded reports whether an existing muse.auto_wake detail
// describes a routine success. The historical format is
//
//	peer=<id> account=<id> ok=true vm=<state> wake_ok=<bool> wake_state=<s> error=<msg> wake_error=<msg>
//
// Empty error fields are written as `error= wake_error=`, so a plain
// "no error= substring" test would classify failures wrongly. Parse the
// fields instead; error messages may contain spaces and even "ok=true", so
// only the actual fields count.
func autoWakeAuditSucceeded(detail string) bool {
	ok, found := auditFieldValue(detail, "ok", "vm")
	if !found || ok != "true" {
		return false
	}
	if msg, found := auditFieldValue(detail, "error", "wake_error"); found && msg != "" {
		return false
	}
	if msg, found := auditFieldValue(detail, "wake_error", ""); found && msg != "" {
		return false
	}
	return true
}

// PurgeAuditNoise removes the historical rows that the v1.10.1 noise rules
// make redundant, exactly once per database:
//
//   - audit_log action='muse.auto_wake' with a successful, error-free detail;
//   - every audit_log action='peer.offline' row.
//
// Failed auto-wake rows and rows carrying an error are kept. The cleanup and
// its settings marker commit in one transaction, so a crash cannot leave a
// half-deleted table looking done. A retention.purged audit row records the
// counts when anything was deleted; repeated calls delete 0 rows.
func (s *sqliteStore) PurgeAuditNoise(now int64) (*AuditNoisePurge, error) {
	out := &AuditNoisePurge{}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM settings WHERE key=?`, auditNoisePurgeMarker).Scan(&done); err != nil {
		return nil, err
	}
	if done > 0 {
		out.AlreadyDone = true
		return out, nil
	}

	var total int64
	if err := tx.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE action='muse.auto_wake'`).Scan(&total); err != nil {
		return nil, err
	}
	rows, err := tx.Query(`SELECT seq,detail FROM audit_log
		WHERE action='muse.auto_wake' AND detail LIKE '%ok=true%' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		var detail string
		if err := rows.Scan(&seq, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		if autoWakeAuditSucceeded(detail) {
			seqs = append(seqs, seq)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	const deleteBatch = 400 // stay well below SQLite's variable limit
	for start := 0; start < len(seqs); start += deleteBatch {
		end := start + deleteBatch
		if end > len(seqs) {
			end = len(seqs)
		}
		batch := seqs[start:end]
		args := make([]any, len(batch))
		for i, seq := range batch {
			args[i] = seq
		}
		placeholders := strings.Repeat("?,", len(batch)-1) + "?"
		res, err := tx.Exec(`DELETE FROM audit_log WHERE seq IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		out.AutoWakeSuccess += n
	}
	out.AutoWakeKept = total - out.AutoWakeSuccess

	res, err := tx.Exec(`DELETE FROM audit_log WHERE action='peer.offline'`)
	if err != nil {
		return nil, err
	}
	if out.PeerOffline, err = res.RowsAffected(); err != nil {
		return nil, err
	}

	if deleted := out.AutoWakeSuccess + out.PeerOffline; deleted > 0 {
		detail := fmt.Sprintf("muse_auto_wake_success=%d peer_offline=%d muse_auto_wake_kept=%d",
			out.AutoWakeSuccess, out.PeerOffline, out.AutoWakeKept)
		if _, err := tx.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`,
			now, "system", "retention.purged", detail); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(`INSERT OR IGNORE INTO settings(key,value,updated_at) VALUES(?, '1', ?)`,
		auditNoisePurgeMarker, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// LogNoisePurge counts one one-time v2 noise cleanup pass.
type LogNoisePurge struct {
	FuseTripped        int64
	AutoWake502        int64
	PeerOffline        int64
	RetentionSkipped   int64
	AgentDaemonStarted int64
	AlreadyDone        bool
}

const logNoisePurgeV2Marker = "migrated.log_noise_purge_v2"

// PurgeLogNoiseV2 removes historical storm/noise rows in one transaction and
// records a settings marker so subsequent boots are no-ops:
//   - audit_log action='thread.fuse_tripped' (historical loop storms)
//   - audit_log action='peer.offline' (routine sleep-cycle flaps)
//   - audit_log action='retention.skipped' (false-positive active thread warnings)
//   - audit_log action='muse.auto_wake' 502 single-attempt noise or routine successes
//   - agent_logs level='info' AND event='watchdog.daemon_started'
func (s *sqliteStore) PurgeLogNoiseV2(now int64) (*LogNoisePurge, error) {
	out := &LogNoisePurge{}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM settings WHERE key=?`, logNoisePurgeV2Marker).Scan(&done); err != nil {
		return nil, err
	}
	if done > 0 {
		out.AlreadyDone = true
		return out, nil
	}

	delAudit := func(action string) (int64, error) {
		res, err := tx.Exec(`DELETE FROM audit_log WHERE action=?`, action)
		if err != nil {
			return 0, err
		}
		return res.RowsAffected()
	}
	if out.FuseTripped, err = delAudit("thread.fuse_tripped"); err != nil {
		return nil, err
	}
	if out.PeerOffline, err = delAudit("peer.offline"); err != nil {
		return nil, err
	}
	if out.RetentionSkipped, err = delAudit("retention.skipped"); err != nil {
		return nil, err
	}

	rows, err := tx.Query(`SELECT seq,detail FROM audit_log WHERE action='muse.auto_wake' ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	var seqs []int64
	for rows.Next() {
		var seq int64
		var detail string
		if err := rows.Scan(&seq, &detail); err != nil {
			rows.Close()
			return nil, err
		}
		if strings.Contains(detail, "wake_error=noise dial: 502 (attempt 1/3") || autoWakeAuditSucceeded(detail) {
			seqs = append(seqs, seq)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	const deleteBatch = 400
	for start := 0; start < len(seqs); start += deleteBatch {
		end := start + deleteBatch
		if end > len(seqs) {
			end = len(seqs)
		}
		batch := seqs[start:end]
		args := make([]any, len(batch))
		for i, seq := range batch {
			args[i] = seq
		}
		placeholders := strings.Repeat("?,", len(batch)-1) + "?"
		res, err := tx.Exec(`DELETE FROM audit_log WHERE seq IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		out.AutoWake502 += n
	}

	res, err := tx.Exec(`DELETE FROM agent_logs WHERE level='info' AND event='watchdog.daemon_started'`)
	if err != nil {
		return nil, err
	}
	if out.AgentDaemonStarted, err = res.RowsAffected(); err != nil {
		return nil, err
	}

	total := out.FuseTripped + out.PeerOffline + out.RetentionSkipped + out.AutoWake502 + out.AgentDaemonStarted
	if total > 0 {
		detail := fmt.Sprintf("fuse_tripped=%d peer_offline=%d retention_skipped=%d muse_auto_wake_502=%d agent_daemon_started=%d",
			out.FuseTripped, out.PeerOffline, out.RetentionSkipped, out.AutoWake502, out.AgentDaemonStarted)
		if _, err := tx.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`,
			now, "system", "retention.purged", detail); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(`INSERT OR IGNORE INTO settings(key,value,updated_at) VALUES(?, '1', ?)`,
		logNoisePurgeV2Marker, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}

// StatusSpamPurge counts one one-time status-storm cleanup pass.
type StatusSpamPurge struct {
	Deleted     int64
	Groups      int64
	AlreadyDone bool
}

const statusSpamPurgeV1Marker = "migrated.status_spam_purge_v1"

// PurgeStatusSpamV1 removes historical repeated kind='status' storms in one
// transaction and records a settings marker so subsequent runs are no-ops.
// For every (root_id, sender, in_reply_to, status) group with > 2 rows, it
// keeps the first (MIN(seq)) and last (MAX(seq)) row as lifecycle evidence and
// deletes the redundant intermediate rows.
func (s *sqliteStore) PurgeStatusSpamV1(now int64) (*StatusSpamPurge, error) {
	out := &StatusSpamPurge{}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	var done int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM settings WHERE key=?`, statusSpamPurgeV1Marker).Scan(&done); err != nil {
		return nil, err
	}
	if done > 0 {
		out.AlreadyDone = true
		return out, nil
	}

	type spamGroup struct {
		rootID    string
		sender    string
		inReplyTo string
		status    string
		minSeq    int64
		maxSeq    int64
	}
	grows, err := tx.Query(`SELECT root_id, sender, in_reply_to, status, MIN(seq), MAX(seq)
		FROM messages
		WHERE kind='status' AND in_reply_to!=''
		GROUP BY root_id, sender, in_reply_to, status
		HAVING COUNT(*) > 2`)
	if err != nil {
		return nil, err
	}
	var groups []spamGroup
	for grows.Next() {
		var g spamGroup
		if err := grows.Scan(&g.rootID, &g.sender, &g.inReplyTo, &g.status, &g.minSeq, &g.maxSeq); err != nil {
			grows.Close()
			return nil, err
		}
		groups = append(groups, g)
	}
	if err := grows.Err(); err != nil {
		grows.Close()
		return nil, err
	}
	grows.Close()

	var seqs []int64
	for _, g := range groups {
		srows, err := tx.Query(`SELECT seq FROM messages
			WHERE root_id=? AND sender=? AND in_reply_to=? AND kind='status' AND status=?
			  AND seq > ? AND seq < ?
			ORDER BY seq`, g.rootID, g.sender, g.inReplyTo, g.status, g.minSeq, g.maxSeq)
		if err != nil {
			return nil, err
		}
		for srows.Next() {
			var seq int64
			if err := srows.Scan(&seq); err != nil {
				srows.Close()
				return nil, err
			}
			seqs = append(seqs, seq)
		}
		if err := srows.Err(); err != nil {
			srows.Close()
			return nil, err
		}
		srows.Close()
	}
	out.Groups = int64(len(groups))

	const deleteBatch = 400
	for start := 0; start < len(seqs); start += deleteBatch {
		end := start + deleteBatch
		if end > len(seqs) {
			end = len(seqs)
		}
		batch := seqs[start:end]
		args := make([]any, len(batch))
		for i, seq := range batch {
			args[i] = seq
		}
		placeholders := strings.Repeat("?,", len(batch)-1) + "?"
		if _, err := tx.Exec(`DELETE FROM acks WHERE message_seq IN (`+placeholders+`)`, args...); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(`DELETE FROM message_tickets WHERE message_seq IN (`+placeholders+`)`, args...); err != nil {
			return nil, err
		}
		res, err := tx.Exec(`DELETE FROM messages WHERE seq IN (`+placeholders+`)`, args...)
		if err != nil {
			return nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, err
		}
		out.Deleted += n
	}

	if out.Deleted > 0 {
		detail := fmt.Sprintf("status_spam_messages=%d status_spam_groups=%d", out.Deleted, out.Groups)
		if _, err := tx.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`,
			now, "system", "retention.purged", detail); err != nil {
			return nil, err
		}
	}

	if _, err := tx.Exec(`INSERT OR IGNORE INTO settings(key,value,updated_at) VALUES(?, '1', ?)`,
		statusSpamPurgeV1Marker, now); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return out, nil
}
