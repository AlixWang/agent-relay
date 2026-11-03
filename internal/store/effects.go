package store

// Effect is one durable side effect. The key makes enqueueing idempotent:
// re-enqueueing a waiting effect replaces its payload but keeps its place.
type Effect struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Key       string `json:"key"`
	Payload   string `json:"payload"`
	Attempts  int    `json:"attempts"`
	NextAt    int64  `json:"next_at"`
	CreatedAt int64  `json:"created_at"`
	LastError string `json:"last_error"`
}

func (s *sqliteStore) EnqueueEffect(kind, key, payload string, now int64) error {
	_, err := s.db.Exec(`INSERT INTO effects(kind,key,payload,attempts,next_at,created_at)
		VALUES(?,?,?,0,?,?)
		ON CONFLICT(kind,key) DO UPDATE SET payload=excluded.payload, next_at=excluded.next_at`,
		kind, key, payload, now, now)
	return err
}

func (s *sqliteStore) DueEffects(now int64, limit int) ([]*Effect, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id,kind,key,payload,attempts,next_at,created_at,last_error
		FROM effects WHERE next_at <= ? ORDER BY next_at, id LIMIT ?`, now, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Effect{}
	for rows.Next() {
		var e Effect
		if err := rows.Scan(&e.ID, &e.Kind, &e.Key, &e.Payload, &e.Attempts, &e.NextAt, &e.CreatedAt, &e.LastError); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) CompleteEffect(id int64) error {
	_, err := s.db.Exec(`DELETE FROM effects WHERE id=?`, id)
	return err
}

func (s *sqliteStore) FailEffect(id int64, msg string, nextAt int64) error {
	_, err := s.db.Exec(`UPDATE effects SET attempts=attempts+1, last_error=?, next_at=? WHERE id=?`, msg, nextAt, id)
	return err
}

// EffectKindHealth is one effect kind's queue state. Failed counts rows that
// exhausted the first three attempts; LastError is the newest stored error so
// an operator can see why without opening the database.
type EffectKindHealth struct {
	Pending   int    `json:"pending"`
	Due       int    `json:"due"`
	Failed    int    `json:"failed"`
	LastError string `json:"last_error,omitempty"`
}

// EffectHealth is the per-kind durable-side-effect health view.
type EffectHealth struct {
	Pending int                         `json:"pending"`
	Due     int                         `json:"due"`
	Failed  int                         `json:"failed"`
	Kinds   map[string]EffectKindHealth `json:"kinds"`
}

// EffectStats returns every kind's queue state. Pending counts all stored
// rows, including rows whose next_at is already due but whose worker has not
// run yet — the previous implementation counted only future rows, which made
// a stopped worker look healthy.
func (s *sqliteStore) EffectStats(now int64) (*EffectHealth, error) {
	rows, err := s.db.Query(`SELECT id,kind,next_at,attempts,last_error FROM effects ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	h := &EffectHealth{Kinds: map[string]EffectKindHealth{}}
	for rows.Next() {
		var id, nextAt int64
		var kind string
		var attempts int
		var lastErr string
		if err := rows.Scan(&id, &kind, &nextAt, &attempts, &lastErr); err != nil {
			return nil, err
		}
		k := h.Kinds[kind]
		k.Pending++
		h.Pending++
		if nextAt <= now {
			k.Due++
			h.Due++
		}
		if attempts >= 3 {
			k.Failed++
			h.Failed++
		}
		if lastErr != "" {
			k.LastError = lastErr
		}
		h.Kinds[kind] = k
	}
	return h, rows.Err()
}
