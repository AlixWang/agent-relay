package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/AlixWang/agent-relay/internal/stages"
)

// Ticket is a read-model row projected from messages. It is not an
// independent source of truth: see docs/adr/0001-work-items-as-projection.md.
type Ticket struct {
	ID               int64  `json:"id"`
	ThreadRoot       string `json:"thread_root"`
	Requester        string `json:"requester"`
	Assignee         string `json:"assignee"`
	State            string `json:"state"` // open|done|cancelled
	Stage            string `json:"stage"` // queued|working|person/*|done|cancelled
	CreatedAt        int64  `json:"created_at"`
	UpdatedAt        int64  `json:"updated_at"`
	EndedAt          int64  `json:"ended_at"`
	LastSeq          int64  `json:"last_seq"`
	AssigneeLastSeen int64  `json:"-"`
	LastSender       string `json:"last_sender,omitempty"`
	LastKind         string `json:"last_kind,omitempty"`
	LastPreview      string `json:"last_preview,omitempty"`
}

// TicketFilter narrows the work-item list. Stage accepts an exact stage or
// "attention" for every person/* stage.
type TicketFilter struct {
	Stage    string
	State    string
	Assignee string
	Q        string
}

// TicketCounts feeds the console badges and /admin/stats.
type TicketCounts struct {
	Open      int `json:"open"`
	Attention int `json:"attention"`
}

func (s *sqliteStore) CreateTicket(t *Ticket) error {
	if t.State == "" {
		t.State = "open"
	}
	if t.Stage == "" {
		t.Stage = "queued"
	}
	_, err := s.db.Exec(`INSERT INTO tickets(id,thread_root,requester,assignee,state,stage,created_at,updated_at,ended_at,last_seq)
		VALUES(?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO NOTHING`,
		t.ID, t.ThreadRoot, t.Requester, t.Assignee, t.State, t.Stage,
		t.CreatedAt, t.UpdatedAt, t.EndedAt, t.LastSeq)
	return err
}

func (s *sqliteStore) UpdateTicket(t *Ticket) error {
	_, err := s.db.Exec(`UPDATE tickets
		SET thread_root=?, requester=?, assignee=?, state=?, stage=?, updated_at=?, ended_at=?, last_seq=?
		WHERE id=?`,
		t.ThreadRoot, t.Requester, t.Assignee, t.State, t.Stage,
		t.UpdatedAt, t.EndedAt, t.LastSeq, t.ID)
	return err
}

func (s *sqliteStore) GetTicket(id int64) (*Ticket, error) {
	row := s.db.QueryRow(ticketSelect+` WHERE t.id=?`, id)
	return scanTicket(row)
}

const ticketSelect = `SELECT t.id,t.thread_root,t.requester,t.assignee,t.state,t.stage,
	t.created_at,t.updated_at,t.ended_at,t.last_seq,
	COALESCE(p.last_seen,0),
	COALESCE(m.sender,''),COALESCE(m.kind,''),COALESCE(substr(m.payload,1,160),'')
	FROM tickets t
	LEFT JOIN messages m ON m.seq=t.last_seq
	LEFT JOIN peers p ON p.id=t.assignee`

func scanTicket(row rowScanner) (*Ticket, error) {
	var t Ticket
	if err := row.Scan(&t.ID, &t.ThreadRoot, &t.Requester, &t.Assignee, &t.State, &t.Stage,
		&t.CreatedAt, &t.UpdatedAt, &t.EndedAt, &t.LastSeq, &t.AssigneeLastSeen,
		&t.LastSender, &t.LastKind, &t.LastPreview); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &t, nil
}

// ListTickets returns one page ordered by attention priority, then activity.
func (s *sqliteStore) ListTickets(f TicketFilter, limit, offset int) ([]*Ticket, int, error) {
	limit, offset = clampPage(limit, offset, 20, 200)
	where := []string{"1=1"}
	var args []any
	if st := strings.TrimSpace(f.Stage); st != "" {
		if st == "attention" {
			where = append(where, "t.stage LIKE 'person/%'")
		} else {
			where = append(where, "t.stage=?")
			args = append(args, st)
		}
	}
	if st := strings.TrimSpace(f.State); st != "" {
		where = append(where, "t.state=?")
		args = append(args, st)
	}
	if a := strings.TrimSpace(f.Assignee); a != "" {
		where = append(where, "t.assignee=?")
		args = append(args, a)
	}
	if q := strings.TrimSpace(f.Q); q != "" {
		like := "%" + likeEscape(q) + "%"
		where = append(where, `(t.thread_root LIKE ? ESCAPE '\' OR t.requester LIKE ? ESCAPE '\'
			OR t.assignee LIKE ? ESCAPE '\' OR COALESCE(m.payload,'') LIKE ? ESCAPE '\')`)
		args = append(args, like, like, like, like)
	}
	cond := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tickets t LEFT JOIN messages m ON m.seq=t.last_seq LEFT JOIN peers p ON p.id=t.assignee WHERE `+cond, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	q := ticketSelect + ` WHERE ` + cond + ` ORDER BY ` + ticketOrder + ` LIMIT ? OFFSET ?`
	rows, err := s.db.Query(q, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*Ticket{}
	for rows.Next() {
		t, err := scanTicket(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, t)
	}
	return out, total, rows.Err()
}

// ticketOrder is generated from internal/stages, the single ordering source
// shared with the work service and the console response.
var ticketOrder = stages.OrderCase("t.stage") + `, t.updated_at DESC, t.id DESC`

// TicketMessages returns one ticket's messages oldest-first.
func (s *sqliteStore) TicketMessages(ticketID int64, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 2000 {
		limit = 1000
	}
	rows, err := s.db.Query(visibleSelect+`
		JOIN message_tickets mt ON mt.message_seq=m.seq
		WHERE mt.ticket_id=? ORDER BY m.seq ASC LIMIT ?`, ticketID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// MessagesAfter is the projector's cursor scan over the immutable message log.
func (s *sqliteStore) MessagesAfter(afterSeq int64, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 500
	}
	rows, err := s.db.Query(visibleSelect+` WHERE m.seq > ? ORDER BY m.seq ASC LIMIT ?`, afterSeq, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *sqliteStore) MessageTicket(seq int64) (int64, error) {
	var id int64
	err := s.db.QueryRow(`SELECT ticket_id FROM message_tickets WHERE message_seq=?`, seq).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	return id, err
}

func (s *sqliteStore) SetMessageTicket(seq, ticketID int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO message_tickets(message_seq,ticket_id) VALUES(?,?)`, seq, ticketID)
	return err
}

// ResetTickets clears only the message→ticket mapping before a full rebuild.
// Ticket rows (and their assignment/subscription/mandate history) survive; the
// projector updates matching rows and PruneOrphanTickets removes rows whose
// source messages no longer exist.
func (s *sqliteStore) ResetTickets() error {
	_, err := s.db.Exec(`DELETE FROM message_tickets`)
	return err
}

// PruneOrphanTickets drops ticket rows that no message maps to any more.
func (s *sqliteStore) PruneOrphanTickets() (int64, error) {
	res, err := s.db.Exec(`DELETE FROM tickets WHERE id NOT IN (SELECT DISTINCT ticket_id FROM message_tickets)`)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// OpenTicketMessageSeqs reports which of seqs belong to an open ticket.
// Retention uses this to pin evidence for work that is still in progress.
func (s *sqliteStore) OpenTicketMessageSeqs(seqs []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	for start := 0; start < len(seqs); start += 400 {
		end := start + 400
		if end > len(seqs) {
			end = len(seqs)
		}
		chunk := seqs[start:end]
		ph := strings.Repeat("?,", len(chunk))
		ph = strings.TrimSuffix(ph, ",")
		args := make([]any, 0, len(chunk))
		for _, seq := range chunk {
			args = append(args, seq)
		}
		rows, err := s.db.Query(`SELECT mt.message_seq FROM message_tickets mt
			JOIN tickets t ON t.id=mt.ticket_id
			WHERE t.state='open' AND mt.message_seq IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var seq int64
			if err := rows.Scan(&seq); err != nil {
				rows.Close()
				return nil, err
			}
			out[seq] = true
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

// PruneTickets drops ended tickets older than cutoff. Mappings cascade.
func (s *sqliteStore) PruneTickets(olderThan int64) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM message_tickets WHERE ticket_id IN
		(SELECT id FROM tickets WHERE state != 'open' AND updated_at < ?)`, olderThan); err != nil {
		return 0, err
	}
	res, err := tx.Exec(`DELETE FROM tickets WHERE state != 'open' AND updated_at < ?`, olderThan)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}

// ticketEffectiveStages scans every open ticket once and applies the same
// overlays the console uses (presence + expired assignment lease). The store
// owns the SQL and internal/stages owns the rule, so counts and the HTTP view
// cannot drift.
func (s *sqliteStore) ticketEffectiveStages(now, onlineSecs int64) (map[string]int, error) {
	rows, err := s.db.Query(`SELECT t.stage, COALESCE(p.last_seen,0),
		CASE WHEN a.status='ended' AND a.ended_reason='lease_expired' THEN 1 ELSE 0 END
		FROM tickets t
		LEFT JOIN peers p ON p.id=t.assignee
		LEFT JOIN ticket_assignments a ON a.id=(SELECT MAX(id) FROM ticket_assignments WHERE ticket_id=t.id)
		WHERE t.state='open'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var stored string
		var lastSeen int64
		var leaseExpired int
		if err := rows.Scan(&stored, &lastSeen, &leaseExpired); err != nil {
			return nil, err
		}
		stage, _, _ := stages.Effective(stored, lastSeen, now, onlineSecs, leaseExpired != 0)
		out[stage]++
	}
	return out, rows.Err()
}

// TicketCounts counts open work, including the presence and lease overlays
// used by the console: a queued/working ticket whose assignee is offline, or
// whose assignment lease expired, needs a person.
func (s *sqliteStore) TicketCounts(now, onlineSecs int64) (*TicketCounts, error) {
	stagesByCount, err := s.ticketEffectiveStages(now, onlineSecs)
	if err != nil {
		return nil, err
	}
	c := &TicketCounts{}
	for stage, n := range stagesByCount {
		c.Open += n
		if stages.Attention(stage) {
			c.Attention += n
		}
	}
	return c, nil
}

// TicketStats is a small debug helper used by the work service health line.
func (s *sqliteStore) TicketStats() (tickets, mappings int, err error) {
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM tickets`).Scan(&tickets); err != nil {
		return 0, 0, fmt.Errorf("count tickets: %w", err)
	}
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM message_tickets`).Scan(&mappings); err != nil {
		return 0, 0, fmt.Errorf("count message_tickets: %w", err)
	}
	return tickets, mappings, nil
}

// OpenTicketIDs lists every open ticket, used by the periodic refresh.
func (s *sqliteStore) OpenTicketIDs() ([]int64, error) {
	rows, err := s.db.Query(`SELECT id FROM tickets WHERE state='open' ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TicketHasOpenPermission reports whether one ticket has a pending, unexpired
// permission request. Used by the projector's single-ticket refresh.
func (s *sqliteStore) TicketHasOpenPermission(ticketID, now int64) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*)
		FROM permission_requests pr
		JOIN messages m ON m.id=pr.request_id
		JOIN message_tickets mt ON mt.message_seq=m.seq
		WHERE mt.ticket_id=? AND pr.status='pending' AND pr.expires_at > ?`, ticketID, now).Scan(&n)
	return n > 0, err
}

// TicketStageCounts returns effective stage counts, applying the same
// presence and lease overlays as the console without mutating stored stages.
func (s *sqliteStore) TicketStageCounts(now, onlineSecs int64) (map[string]int, error) {
	return s.ticketEffectiveStages(now, onlineSecs)
}
