package store

import (
	"encoding/json"
	"strings"
)

// AgentLog is one runtime diagnostic uploaded by a client (DESIGN §4.11).
// Fields is the raw JSON object; MarshalJSON exposes it as an object for the
// console and NDJSON export.
type AgentLog struct {
	Seq           int64  `json:"seq"`
	EventID       string `json:"event_id"`
	Ts            int64  `json:"ts"`
	ReceivedAt    int64  `json:"received_at"`
	PeerID        string `json:"peer_id"`
	Level         string `json:"level"`
	Source        string `json:"source"`
	Event         string `json:"event"`
	Message       string `json:"message"`
	TaskID        string `json:"task_id,omitempty"`
	Thread        string `json:"thread,omitempty"`
	Fields        string `json:"-"`
	ClientVersion string `json:"client_version,omitempty"`
	ClientRev     string `json:"client_rev,omitempty"`
}

// MarshalJSON exposes the raw fields object as JSON instead of the storage
// string. It keeps the console/export representation on the store type so the
// gateway never has to re-list every column.
func (e AgentLog) MarshalJSON() ([]byte, error) {
	type alias AgentLog
	b, err := json.Marshal(alias(e))
	if err != nil {
		return nil, err
	}
	var out map[string]json.RawMessage
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, err
	}
	fields := json.RawMessage(e.Fields)
	if !json.Valid(fields) {
		fields = json.RawMessage(`{}`)
	}
	out["fields"] = fields
	return json.Marshal(out)
}

// AgentLogFilter narrows a console log query. Since/Until are inclusive
// server-received unix seconds; zero means unbounded on that side.
type AgentLogFilter struct {
	PeerID string
	Level  []string // exact values (OR)
	Source []string // exact values (OR)
	Event  string   // prefix match
	TaskID string
	Thread string
	Q      string // message/event/fields/task_id/thread substring
	Since  int64
	Until  int64
}

// AgentLogStats powers the console overview cards.
type AgentLogStats struct {
	Errors         int   `json:"errors"`
	Warnings       int   `json:"warnings"`
	Peers          int   `json:"peers"`
	LastReceivedAt int64 `json:"last_received_at"`
}

func agentLogWhere(f AgentLogFilter) (string, []any) {
	where := ` WHERE 1=1`
	var args []any
	if f.PeerID != "" {
		where += ` AND peer_id=?`
		args = append(args, f.PeerID)
	}
	for _, dim := range []struct {
		column string
		values []string
	}{{"level", f.Level}, {"source", f.Source}} {
		if len(dim.values) == 0 {
			continue
		}
		where += ` AND ` + dim.column + ` IN (` + placeholders(len(dim.values)) + `)`
		for _, v := range dim.values {
			args = append(args, v)
		}
	}
	if f.Event != "" {
		where += ` AND event LIKE ? ESCAPE '\'`
		args = append(args, likeEscape(f.Event)+"%")
	}
	if f.TaskID != "" {
		where += ` AND task_id=?`
		args = append(args, f.TaskID)
	}
	if f.Thread != "" {
		where += ` AND thread=?`
		args = append(args, f.Thread)
	}
	if f.Q != "" {
		pat := "%" + likeEscape(f.Q) + "%"
		where += ` AND (message LIKE ? ESCAPE '\' OR event LIKE ? ESCAPE '\' OR fields LIKE ? ESCAPE '\' OR task_id LIKE ? ESCAPE '\' OR thread LIKE ? ESCAPE '\')`
		args = append(args, pat, pat, pat, pat, pat)
	}
	if f.Since > 0 {
		where += ` AND received_at>=?`
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where += ` AND received_at<=?`
		args = append(args, f.Until)
	}
	return where, args
}

func placeholders(n int) string {
	if n <= 0 {
		return ""
	}
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}

func (s *sqliteStore) InsertAgentLogs(events []*AgentLog) (accepted, duplicates int, err error) {
	if len(events) == 0 {
		return 0, 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR IGNORE INTO agent_logs
		(event_id,ts,received_at,peer_id,level,source,event,message,task_id,thread,fields,client_version,client_rev)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`)
	if err != nil {
		return 0, 0, err
	}
	defer stmt.Close()
	for _, e := range events {
		fields := e.Fields
		if strings.TrimSpace(fields) == "" {
			fields = "{}"
		}
		res, err := stmt.Exec(e.EventID, e.Ts, e.ReceivedAt, e.PeerID, e.Level, e.Source,
			e.Event, e.Message, e.TaskID, e.Thread, fields, e.ClientVersion, e.ClientRev)
		if err != nil {
			return 0, 0, err
		}
		n, _ := res.RowsAffected()
		accepted += int(n)
	}
	duplicates = len(events) - accepted
	return accepted, duplicates, tx.Commit()
}

func (s *sqliteStore) ListAgentLogsPage(f AgentLogFilter, limit, offset int) ([]*AgentLog, int, error) {
	limit, offset = clampPage(limit, offset, 50, 10000)
	where, args := agentLogWhere(f)
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM agent_logs`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := s.db.Query(`SELECT seq,event_id,ts,received_at,peer_id,level,source,event,message,task_id,thread,fields,client_version,client_rev
		FROM agent_logs`+where+` ORDER BY seq DESC LIMIT ? OFFSET ?`, append(args, limit, offset)...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := []*AgentLog{}
	for rows.Next() {
		var e AgentLog
		if err := rows.Scan(&e.Seq, &e.EventID, &e.Ts, &e.ReceivedAt, &e.PeerID, &e.Level, &e.Source,
			&e.Event, &e.Message, &e.TaskID, &e.Thread, &e.Fields, &e.ClientVersion, &e.ClientRev); err != nil {
			return nil, 0, err
		}
		out = append(out, &e)
	}
	return out, total, rows.Err()
}

func (f AgentLogFilter) withoutLevel() AgentLogFilter {
	f.Level = nil
	return f
}

func (f AgentLogFilter) withoutSource() AgentLogFilter {
	f.Source = nil
	return f
}

func (f AgentLogFilter) withoutEvent() AgentLogFilter {
	f.Event = ""
	return f
}

func (s *sqliteStore) AgentLogFacets(f AgentLogFilter) (sources, events []string, levels map[string]int, err error) {
	readStrings := func(q string, args []any) ([]string, error) {
		rows, err := s.db.Query(q, args...)
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
	swhere, sargs := agentLogWhere(f.withoutSource())
	if sources, err = readStrings(`SELECT DISTINCT source FROM agent_logs`+swhere+` ORDER BY source LIMIT 100`, sargs); err != nil {
		return nil, nil, nil, err
	}
	ewhere, eargs := agentLogWhere(f.withoutEvent())
	if events, err = readStrings(`SELECT DISTINCT event FROM agent_logs`+ewhere+` ORDER BY event LIMIT 200`, eargs); err != nil {
		return nil, nil, nil, err
	}
	lwhere, largs := agentLogWhere(f.withoutLevel())
	rows, err := s.db.Query(`SELECT level, COUNT(*) FROM agent_logs`+lwhere+` GROUP BY level`, largs...)
	if err != nil {
		return nil, nil, nil, err
	}
	defer rows.Close()
	levels = map[string]int{}
	for rows.Next() {
		var level string
		var n int
		if err := rows.Scan(&level, &n); err != nil {
			return nil, nil, nil, err
		}
		levels[level] = n
	}
	return sources, events, levels, rows.Err()
}

func (s *sqliteStore) AgentLogStats(since, until int64) (*AgentLogStats, error) {
	q := `SELECT
		COALESCE(SUM(CASE WHEN level='error' THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN level='warn' THEN 1 ELSE 0 END),0),
		COUNT(DISTINCT peer_id),
		COALESCE(MAX(received_at),0)
		FROM agent_logs WHERE received_at>=?`
	args := []any{since}
	if until > 0 {
		q += ` AND received_at<=?`
		args = append(args, until)
	}
	var st AgentLogStats
	err := s.db.QueryRow(q, args...).Scan(&st.Errors, &st.Warnings, &st.Peers, &st.LastReceivedAt)
	if err != nil {
		return nil, err
	}
	return &st, nil
}

func (s *sqliteStore) PruneAgentLogs(olderThan int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM agent_logs WHERE received_at<?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *sqliteStore) TrimAgentLogs(peerID string, maxRows int) (int64, error) {
	if maxRows <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM agent_logs WHERE peer_id=? AND seq <= COALESCE(
		(SELECT seq FROM agent_logs WHERE peer_id=? ORDER BY seq DESC LIMIT 1 OFFSET ?), 0)`,
		peerID, peerID, maxRows)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TrimAllAgentLogs enforces the per-peer row cap globally. The retention
// runner uses it so a lowered cap also applies to peers that stopped reporting.
func (s *sqliteStore) TrimAllAgentLogs(maxRows int) (int64, error) {
	if maxRows <= 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM agent_logs WHERE seq IN (
		SELECT seq FROM (
			SELECT seq, ROW_NUMBER() OVER (PARTITION BY peer_id ORDER BY seq DESC) AS rn
			FROM agent_logs
		) WHERE rn > ?)`, maxRows)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
