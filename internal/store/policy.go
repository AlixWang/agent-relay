package store

import "database/sql"

// Mandate is the authorization record for one ticket. Phase 3 records the
// policy version and requester basis; later phases may add scope checks.
type Mandate struct {
	TicketID      int64  `json:"ticket_id"`
	PolicyVersion string `json:"policy_version"`
	Scope         string `json:"scope"`
	Requester     string `json:"requester"`
	RequesterKind string `json:"requester_kind"` // operator|system|peer
	CreatedAt     int64  `json:"created_at"`
}

// PolicyVersion is an immutable snapshot of the deterministic admission rules.
type PolicyVersion struct {
	Version   string `json:"version"`
	Rules     string `json:"rules"`
	CreatedAt int64  `json:"created_at"`
}

func (s *sqliteStore) UpsertPolicyVersion(version, rules string, now int64) error {
	_, err := s.db.Exec(`INSERT INTO policy_versions(version,rules,created_at) VALUES(?,?,?)
		ON CONFLICT(version) DO NOTHING`, version, rules, now)
	return err
}

func (s *sqliteStore) GetPolicyVersion(version string) (*PolicyVersion, error) {
	var p PolicyVersion
	err := s.db.QueryRow(`SELECT version,rules,created_at FROM policy_versions WHERE version=?`, version).
		Scan(&p.Version, &p.Rules, &p.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *sqliteStore) CreateMandate(m *Mandate) error {
	if m.Scope == "" {
		m.Scope = "task"
	}
	if m.RequesterKind == "" {
		m.RequesterKind = "peer"
	}
	_, err := s.db.Exec(`INSERT INTO mandates(ticket_id,policy_version,scope,requester,requester_kind,created_at)
		VALUES(?,?,?,?,?,?) ON CONFLICT(ticket_id) DO NOTHING`,
		m.TicketID, m.PolicyVersion, m.Scope, m.Requester, m.RequesterKind, m.CreatedAt)
	return err
}

func (s *sqliteStore) GetMandate(ticketID int64) (*Mandate, error) {
	var m Mandate
	err := s.db.QueryRow(`SELECT ticket_id,policy_version,scope,requester,requester_kind,created_at
		FROM mandates WHERE ticket_id=?`, ticketID).
		Scan(&m.TicketID, &m.PolicyVersion, &m.Scope, &m.Requester, &m.RequesterKind, &m.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}
