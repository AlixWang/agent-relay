package store

// RecordAuditAndEffect writes one audit row and, when kind is non-empty, one
// durable effect in the same transaction. It is the transactional boundary for
// intents whose trigger lives outside SQLite (a presence transition): either
// both the audit trail and the retryable side effect exist, or neither does.
func (s *sqliteStore) RecordAuditAndEffect(actor, action, detail, kind, key, payload string, now int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`,
		now, actor, action, detail); err != nil {
		return err
	}
	if kind != "" {
		if _, err := tx.Exec(`INSERT INTO effects(kind,key,payload,attempts,next_at,created_at)
			VALUES(?,?,?,0,?,?)
			ON CONFLICT(kind,key) DO UPDATE SET payload=excluded.payload, next_at=excluded.next_at`,
			kind, key, payload, now, now); err != nil {
			return err
		}
	}
	return tx.Commit()
}
