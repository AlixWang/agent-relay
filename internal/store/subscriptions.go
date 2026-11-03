package store

// TicketSubscription is a ticket's follow relation to a thread.
type TicketSubscription struct {
	TicketID   int64  `json:"ticket_id"`
	ThreadRoot string `json:"thread_root"`
	Mode       string `json:"mode"` // own|context
	CreatedAt  int64  `json:"created_at"`
}

func (s *sqliteStore) SubscribeTicket(ticketID int64, threadRoot, mode string, now int64) error {
	if mode != "context" {
		mode = "own"
	}
	_, err := s.db.Exec(`INSERT INTO ticket_subscriptions(ticket_id,thread_root,mode,created_at)
		VALUES(?,?,?,?) ON CONFLICT(ticket_id,thread_root) DO NOTHING`, ticketID, threadRoot, mode, now)
	return err
}

func (s *sqliteStore) TicketSubscriptions(ticketID int64) ([]*TicketSubscription, error) {
	rows, err := s.db.Query(`SELECT ticket_id,thread_root,mode,created_at
		FROM ticket_subscriptions WHERE ticket_id=? ORDER BY thread_root`, ticketID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*TicketSubscription{}
	for rows.Next() {
		var sub TicketSubscription
		if err := rows.Scan(&sub.TicketID, &sub.ThreadRoot, &sub.Mode, &sub.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &sub)
	}
	return out, rows.Err()
}

// PeerInThread reports whether peerID has sent or received a message in root.
// It is the privacy predicate for ticket subscriptions.
func (s *sqliteStore) PeerInThread(rootID, peerID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE root_id=? AND (sender=? OR recipient=?)`,
		rootID, peerID, peerID).Scan(&n)
	return n > 0, err
}
