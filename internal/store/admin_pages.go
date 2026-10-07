package store

import (
	"database/sql"
	"strings"
)

// Paged/aggregated read models for the operations console (DESIGN §9).
// These exist so the console never pulls whole tables (or N+1 per-thread
// scans) just to render one page.

// ThreadSummary is one row of the console thread list.
type ThreadSummary struct {
	RootID       string   `json:"root_id"`
	Count        int      `json:"count"`
	Held         int      `json:"held"`
	SinceReset   int      `json:"since_reset"` // messages after the fuse watermark
	FirstAt      int64    `json:"first_at"`
	LastAt       int64    `json:"last_at"`
	LastSeq      int64    `json:"last_seq"`
	LastSender   string   `json:"last_sender"`
	LastKind     string   `json:"last_kind"`
	LastPreview  string   `json:"last_preview"`
	Participants []string `json:"participants"`
}

// ThreadFilter narrows the thread list. Q matches root_id substring or an
// exact participant id. Held keeps threads with pending approvals; FusedMin
// (>0) keeps threads whose post-watermark count reached the fuse limit.
type ThreadFilter struct {
	Q        string
	Held     bool
	FusedMin int
}

// AuditFilter narrows the audit log. Actor is exact, Action is a prefix
// ("peer." matches peer.status/peer.deleted), Q is a detail substring.
type AuditFilter struct {
	Actor  string
	Action string
	Q      string
}

// AdminCounts feeds the console overview cards.
type AdminCounts struct {
	Threads          int `json:"threads_total"`
	PendingApprovals int `json:"pending_approvals"`
	TokensActive     int `json:"tokens_active"`
	AuditTotal       int `json:"audit_total"`
}

func likeEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

func clampPage(limit, offset, def, max int) (int, int) {
	if limit <= 0 || limit > max {
		limit = def
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (s *sqliteStore) ThreadSummaries(f ThreadFilter, limit, offset int) ([]*ThreadSummary, int, error) {
	limit, offset = clampPage(limit, offset, 20, 500)
	where := ""
	var args []any
	if q := strings.TrimSpace(f.Q); q != "" {
		where = `WHERE m.root_id IN (SELECT DISTINCT root_id FROM messages
			WHERE root_id LIKE ? ESCAPE '\' OR sender=? OR recipient=?)`
		args = append(args, "%"+likeEscape(q)+"%", q, q)
	}
	var having []string
	if f.Held {
		having = append(having, "held>0")
	}
	if f.FusedMin > 0 {
		having = append(having, "since_reset>=?")
	}
	grouped := `SELECT m.root_id AS root_id, COUNT(*) AS cnt,
		SUM(CASE WHEN m.approval_state='pending' THEN 1 ELSE 0 END) AS held,
		SUM(CASE WHEN m.seq > COALESCE(f.cleared_seq,0) THEN 1 ELSE 0 END) AS since_reset,
		MIN(m.created_at) AS first_at, MAX(m.created_at) AS last_at, MAX(m.seq) AS mx,
		GROUP_CONCAT(DISTINCT m.sender) AS senders,
		GROUP_CONCAT(DISTINCT m.recipient) AS recipients
		FROM messages m LEFT JOIN thread_fuses f ON f.root_id=m.root_id ` + where + `
		GROUP BY m.root_id`
	if len(having) > 0 {
		grouped += " HAVING " + strings.Join(having, " AND ")
		if f.FusedMin > 0 {
			args = append(args, f.FusedMin)
		}
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM (`+grouped+`)`, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := `SELECT t.root_id, t.cnt, t.held, t.since_reset, t.first_at, t.last_at, t.mx,
		COALESCE(t.senders,''), COALESCE(t.recipients,''),
		COALESCE(lm.sender,''), COALESCE(lm.kind,''), COALESCE(substr(lm.payload,1,160),'')
		FROM (` + grouped + `) t LEFT JOIN messages lm ON lm.seq=t.mx
		ORDER BY t.mx DESC LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*ThreadSummary{}
	for rows.Next() {
		var t ThreadSummary
		var senders, recips string
		if err := rows.Scan(&t.RootID, &t.Count, &t.Held, &t.SinceReset, &t.FirstAt, &t.LastAt, &t.LastSeq,
			&senders, &recips, &t.LastSender, &t.LastKind, &t.LastPreview); err != nil {
			return nil, 0, err
		}
		seen := map[string]bool{}
		t.Participants = []string{}
		for _, p := range strings.Split(senders+","+recips, ",") {
			if p == "" || p == "*" || seen[p] {
				continue
			}
			seen[p] = true
			t.Participants = append(t.Participants, p)
		}
		out = append(out, &t)
	}
	return out, total, rows.Err()
}

func (s *sqliteStore) SearchMessagesPage(query string, limit, offset int) ([]*Message, int, error) {
	limit, offset = clampPage(limit, offset, 20, 200)
	pat := "%" + likeEscape(query) + "%"
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE payload LIKE ? ESCAPE '\'`, pat).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE payload LIKE ? ESCAPE '\' ORDER BY seq DESC LIMIT ? OFFSET ?`, pat, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Message{}
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, m)
	}
	return out, total, rows.Err()
}

func (s *sqliteStore) ListAuditPage(f AuditFilter, limit, offset int) ([]*AuditEntry, int, error) {
	limit, offset = clampPage(limit, offset, 20, 1000)
	where := ` WHERE 1=1`
	var args []any
	if f.Actor != "" {
		where += ` AND actor=?`
		args = append(args, f.Actor)
	}
	if f.Action != "" {
		where += ` AND action LIKE ? ESCAPE '\'`
		args = append(args, likeEscape(f.Action)+"%")
	}
	if f.Q != "" {
		where += ` AND detail LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscape(f.Q)+"%")
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM audit_log`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT seq,ts,actor,action,detail FROM audit_log`+where+
		` ORDER BY seq DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*AuditEntry{}
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.Ts, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, 0, err
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}

func (s *sqliteStore) AuditFacets() (actors, actions []string, err error) {
	read := func(q string) ([]string, error) {
		rows, err := s.db.Query(q)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		out := []string{}
		for rows.Next() {
			var v string
			if err := rows.Scan(&v); err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, rows.Err()
	}
	if actors, err = read(`SELECT DISTINCT actor FROM audit_log ORDER BY actor LIMIT 200`); err != nil {
		return nil, nil, err
	}
	if actions, err = read(`SELECT DISTINCT action FROM audit_log ORDER BY action LIMIT 200`); err != nil {
		return nil, nil, err
	}
	return actors, actions, nil
}

func (s *sqliteStore) AdminCounts() (*AdminCounts, error) {
	var c AdminCounts
	scan := func(q string, dst *int) error {
		var v sql.NullInt64
		if err := s.db.QueryRow(q).Scan(&v); err != nil {
			return err
		}
		*dst = int(v.Int64)
		return nil
	}
	if err := scan(`SELECT COUNT(DISTINCT root_id) FROM messages`, &c.Threads); err != nil {
		return nil, err
	}
	if err := scan(`SELECT COUNT(*) FROM messages WHERE approval_state='pending'`, &c.PendingApprovals); err != nil {
		return nil, err
	}
	if err := scan(`SELECT COUNT(*) FROM tokens WHERE revoked_at=0 OR revoked_at IS NULL`, &c.TokensActive); err != nil {
		return nil, err
	}
	if err := scan(`SELECT COUNT(*) FROM audit_log`, &c.AuditTotal); err != nil {
		return nil, err
	}
	return &c, nil
}
