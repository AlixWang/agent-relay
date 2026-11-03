package store

import "database/sql"

// PromptRevision is one immutable template snapshot. BaseRevision is the
// revision this snapshot was derived from, so a diff can be rendered without
// scanning history; it is empty only for the first snapshot of a type.
type PromptRevision struct {
	Revision     string `json:"revision"`
	AgentType    string `json:"agent_type"`
	BaseRevision string `json:"base_revision,omitempty"`
	Content      string `json:"content,omitempty"`
	CreatedAt    int64  `json:"created_at"`
}

const promptRevisionSelect = `SELECT revision,agent_type,base_revision,content,created_at FROM prompt_revisions`

func scanPromptRevision(row rowScanner) (*PromptRevision, error) {
	var p PromptRevision
	if err := row.Scan(&p.Revision, &p.AgentType, &p.BaseRevision, &p.Content, &p.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

func (s *sqliteStore) UpsertPromptRevision(revision, agentType, baseRevision, content string, now int64) error {
	_, err := s.db.Exec(`INSERT INTO prompt_revisions(revision,agent_type,base_revision,content,created_at)
		VALUES(?,?,?,?,?) ON CONFLICT(revision) DO NOTHING`,
		revision, agentType, baseRevision, content, now)
	return err
}

func (s *sqliteStore) ListPromptRevisions() ([]*PromptRevision, error) {
	rows, err := s.db.Query(promptRevisionSelect + ` ORDER BY agent_type, created_at DESC, rowid DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PromptRevision{}
	for rows.Next() {
		p, err := scanPromptRevision(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *sqliteStore) GetPromptRevision(revision string) (*PromptRevision, error) {
	return scanPromptRevision(s.db.QueryRow(promptRevisionSelect+` WHERE revision=?`, revision))
}

// LatestPromptRevision returns the newest snapshot for one agent type. It is
// used when recording a new revision so its base link is stable.
func (s *sqliteStore) LatestPromptRevision(agentType string) (*PromptRevision, error) {
	return scanPromptRevision(s.db.QueryRow(promptRevisionSelect+
		` WHERE agent_type=? ORDER BY created_at DESC, rowid DESC LIMIT 1`, agentType))
}
