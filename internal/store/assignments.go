package store

import "database/sql"

// AssignmentStatus values. ended_reason records why a non-active row stopped
// being authoritative, so an expired lease is not silently re-armed by the
// next projector refresh.
const (
	AssignmentPending        = "pending"
	AssignmentRunning        = "running"
	AssignmentEnded          = "ended"
	AssignmentReasonReassign = "reassigned"
	AssignmentReasonLease    = "lease_expired"
	AssignmentReasonClosed   = "ticket_closed"
)

// TicketAssignment is one executor binding. A ticket can have several over
// time: reassignment ends the old row and starts a new pending one.
type TicketAssignment struct {
	ID             int64  `json:"id"`
	TicketID       int64  `json:"ticket_id"`
	Assignee       string `json:"assignee"`
	Status         string `json:"status"` // pending|running|ended
	LeaseExpiresAt int64  `json:"lease_expires_at,omitempty"`
	EndedReason    string `json:"ended_reason,omitempty"`
	CreatedAt      int64  `json:"created_at"`
	StartedAt      int64  `json:"started_at,omitempty"`
	EndedAt        int64  `json:"ended_at,omitempty"`
}

// AssignmentCommand bundles the ticket/assignee/lease tuple shared by
// EnsureAssignment and ReassignTicket. A bare (ticketID, assignee, now)
// triple repeated in three signatures was the data clump this replaces.
type AssignmentCommand struct {
	TicketID  int64
	Assignee  string
	LeaseSecs int64
	Now       int64
}

const assignmentSelect = `SELECT id,ticket_id,assignee,status,lease_expires_at,ended_reason,created_at,started_at,ended_at
	FROM ticket_assignments`

func scanAssignment(row rowScanner) (*TicketAssignment, error) {
	var a TicketAssignment
	if err := row.Scan(&a.ID, &a.TicketID, &a.Assignee, &a.Status, &a.LeaseExpiresAt,
		&a.EndedReason, &a.CreatedAt, &a.StartedAt, &a.EndedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &a, nil
}

func (s *sqliteStore) createAssignment(cmd AssignmentCommand) (*TicketAssignment, error) {
	lease := int64(0)
	if cmd.LeaseSecs > 0 {
		lease = cmd.Now + cmd.LeaseSecs
	}
	res, err := s.db.Exec(`INSERT INTO ticket_assignments(ticket_id,assignee,status,lease_expires_at,created_at)
		VALUES(?,?,'pending',?,?)`, cmd.TicketID, cmd.Assignee, lease, cmd.Now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetAssignment(id)
}

func (s *sqliteStore) GetAssignment(id int64) (*TicketAssignment, error) {
	return scanAssignment(s.db.QueryRow(assignmentSelect+` WHERE id=?`, id))
}

// LatestAssignment returns the newest binding regardless of status, so the
// projector can distinguish "never assigned" from "lease expired".
func (s *sqliteStore) LatestAssignment(ticketID int64) (*TicketAssignment, error) {
	return scanAssignment(s.db.QueryRow(assignmentSelect+` WHERE ticket_id=? ORDER BY id DESC LIMIT 1`, ticketID))
}

// ListAssignments returns the full assignment history for one ticket.
func (s *sqliteStore) ListAssignments(ticketID int64) ([]*TicketAssignment, error) {
	rows, err := s.db.Query(assignmentSelect+` WHERE ticket_id=? ORDER BY id`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TicketAssignment{}
	for rows.Next() {
		a, err := scanAssignment(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// EnsureAssignment keeps the current binding in sync with the ticket's
// assignee. It is idempotent and is called by the projector on every refresh.
//
// A lease that already expired is deliberately not re-armed: renewal must be
// an explicit reassignment, otherwise the lease would mean nothing.
func (s *sqliteStore) EnsureAssignment(cmd AssignmentCommand) (*TicketAssignment, error) {
	latest, err := s.LatestAssignment(cmd.TicketID)
	if err != nil {
		return nil, err
	}
	if latest != nil {
		if latest.Status != AssignmentEnded && latest.Assignee == cmd.Assignee {
			return latest, nil
		}
		if latest.Status == AssignmentEnded && latest.EndedReason == AssignmentReasonLease &&
			latest.Assignee == cmd.Assignee {
			return nil, nil
		}
		if latest.Status != AssignmentEnded {
			if err := s.endAssignment(latest.ID, AssignmentReasonReassign, cmd.Now); err != nil {
				return nil, err
			}
		}
	}
	return s.createAssignment(cmd)
}

func (s *sqliteStore) endAssignment(id int64, reason string, now int64) error {
	_, err := s.db.Exec(`UPDATE ticket_assignments SET status='ended', ended_reason=?, ended_at=?
		WHERE id=? AND status != 'ended'`, reason, now, id)
	return err
}

func (s *sqliteStore) MarkAssignmentRunning(ticketID, now int64) error {
	_, err := s.db.Exec(`UPDATE ticket_assignments SET status='running', started_at=?
		WHERE ticket_id=? AND status='pending'`, now, ticketID)
	return err
}

// EndAssignments closes every live binding for a ticket. reason is recorded so
// a later refresh can tell a normal close from an expired lease.
func (s *sqliteStore) EndAssignments(ticketID int64, reason string, now int64) error {
	if reason == "" {
		reason = AssignmentReasonClosed
	}
	_, err := s.db.Exec(`UPDATE ticket_assignments SET status='ended', ended_reason=?, ended_at=?
		WHERE ticket_id=? AND status != 'ended'`, reason, now, ticketID)
	return err
}

// ExpireAssignmentLeases ends every live binding whose lease deadline passed.
// The projector calls it on each refresh; the ticket is then shown as
// person/offline until an operator reassigns it.
func (s *sqliteStore) ExpireAssignmentLeases(now int64) (int64, error) {
	res, err := s.db.Exec(`UPDATE ticket_assignments SET status='ended', ended_reason=?, ended_at=?
		WHERE status != 'ended' AND lease_expires_at > 0 AND lease_expires_at <= ?`,
		AssignmentReasonLease, now, now)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ReassignTicket ends the current binding(s) and starts a new pending one in a
// single transaction; the ticket id never changes. An empty lease means no
// deadline.
func (s *sqliteStore) ReassignTicket(cmd AssignmentCommand) (*TicketAssignment, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE ticket_assignments SET status='ended', ended_reason=?, ended_at=?
		WHERE ticket_id=? AND status != 'ended'`, AssignmentReasonReassign, cmd.Now, cmd.TicketID); err != nil {
		return nil, err
	}
	lease := int64(0)
	if cmd.LeaseSecs > 0 {
		lease = cmd.Now + cmd.LeaseSecs
	}
	res, err := tx.Exec(`INSERT INTO ticket_assignments(ticket_id,assignee,status,lease_expires_at,created_at)
		VALUES(?,?,'pending',?,?)`, cmd.TicketID, cmd.Assignee, lease, cmd.Now)
	if err != nil {
		return nil, err
	}
	if _, err := tx.Exec(`UPDATE tickets SET assignee=?, updated_at=? WHERE id=?`, cmd.Assignee, cmd.Now, cmd.TicketID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return s.GetAssignment(id)
}
