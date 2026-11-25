package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// Relay MCP gateway v5 storage (design v5 §2). The schema lives in
// schema.sql / migrations/001_init.sql; this file is the only code that
// touches those three tables. Enum validation and crypto live in
// internal/mcpgateway, not here.

// MCPKey is one row of mcp_keys. PrincipalType is "key" (an external client
// key, KeyHash set) or "peer" (a relay assistant grant, one per peer, no
// key material). Scopes are JSON in SQLite and []string in Go.
type MCPKey struct {
	ID            string   `json:"id"`
	PrincipalType string   `json:"principal_type"`
	PeerID        string   `json:"peer_id"`
	Name          string   `json:"name"`
	KeyPrefix     string   `json:"key_prefix"`
	KeyHash       string   `json:"-"`
	Scopes        []string `json:"scopes"`
	Status        string   `json:"status"`
	Note          string   `json:"note"`
	CreatedBy     string   `json:"created_by"`
	CreatedAt     int64    `json:"created_at"`
	ExpiresAt     int64    `json:"expires_at"`
	RevokedAt     int64    `json:"revoked_at"`
	RotatedFrom   string   `json:"rotated_from"`
	LastUsedAt    int64    `json:"last_used_at"`
	LastUsedIP    string   `json:"last_used_ip"`
	UseCount      int64    `json:"use_count"`
}

// MCPApproval is one frozen high-risk write request.
type MCPApproval struct {
	ID             string `json:"id"`
	KeyID          string `json:"key_id"`
	KeyName        string `json:"key_name"`
	Principal      string `json:"principal"`
	AuthKind       string `json:"auth_kind"`
	Endpoint       string `json:"endpoint"`
	ServiceID      string `json:"service_id"`
	Tool           string `json:"tool"`
	Risk           string `json:"risk"`
	ParamsJSON     string `json:"params_json"`
	ParamsHash     string `json:"params_hash"`
	Status         string `json:"status"`
	RequestedAt    int64  `json:"requested_at"`
	ExpiresAt      int64  `json:"expires_at"`
	DecidedAt      int64  `json:"decided_at"`
	DecidedBy      string `json:"decided_by"`
	DecisionNote   string `json:"decision_note"`
	ExecuteBefore  int64  `json:"execute_before"`
	ExecutedAt     int64  `json:"executed_at"`
	Outcome        string `json:"outcome"`
	ErrorCode      string `json:"error_code"`
	UpstreamStatus int    `json:"upstream_status"`
	RequestID      string `json:"request_id"`
}

// MCPAuditEntry is one gateway call/transition audit row.
type MCPAuditEntry struct {
	Seq            int64  `json:"seq"`
	Ts             int64  `json:"ts"`
	RequestID      string `json:"request_id"`
	Actor          string `json:"actor"`
	AuthKind       string `json:"auth_kind"`
	KeyID          string `json:"key_id"`
	KeyName        string `json:"key_name"`
	Endpoint       string `json:"endpoint"`
	ServiceID      string `json:"service_id"`
	Tool           string `json:"tool"`
	Risk           string `json:"risk"`
	ParamsHash     string `json:"params_hash"`
	ParamsSummary  string `json:"params_summary"`
	ApprovalID     string `json:"approval_id"`
	Outcome        string `json:"outcome"`
	ErrorCode      string `json:"error_code"`
	HTTPStatus     int    `json:"http_status"`
	UpstreamStatus int    `json:"upstream_status"`
	LatencyMS      int    `json:"latency_ms"`
	ClientIP       string `json:"client_ip"`
	Detail         string `json:"detail"`
}

// MCPApprovalFilter selects approval rows for the admin console.
type MCPApprovalFilter struct {
	Status    string
	Principal string
	ServiceID string
	Tool      string
}

// MCPAuditFilter selects audit rows. Empty strings and zero timestamps mean
// "no filter".
type MCPAuditFilter struct {
	Actor      string
	ServiceID  string
	Tool       string
	Outcome    string
	ApprovalID string
	Since      int64
	Until      int64
}

const mcpKeyColumns = `id,principal_type,peer_id,name,key_prefix,key_hash,scopes,status,note,
	created_by,created_at,expires_at,revoked_at,rotated_from,last_used_at,last_used_ip,use_count`

func scanMCPKey(sc rowScanner) (*MCPKey, error) {
	var k MCPKey
	var scopes string
	if err := sc.Scan(&k.ID, &k.PrincipalType, &k.PeerID, &k.Name, &k.KeyPrefix, &k.KeyHash,
		&scopes, &k.Status, &k.Note, &k.CreatedBy, &k.CreatedAt, &k.ExpiresAt, &k.RevokedAt,
		&k.RotatedFrom, &k.LastUsedAt, &k.LastUsedIP, &k.UseCount); err != nil {
		return nil, err
	}
	if err := json.Unmarshal([]byte(scopes), &k.Scopes); err != nil {
		return nil, fmt.Errorf("mcp_keys %s scopes: %w", k.ID, err)
	}
	if k.Scopes == nil {
		k.Scopes = []string{}
	}
	return &k, nil
}

func marshalScopes(scopes []string) string {
	if scopes == nil {
		scopes = []string{}
	}
	b, _ := json.Marshal(scopes)
	return string(b)
}

// CreateMCPKey inserts a key or peer-grant row. Unique index violations
// (key_hash, peer_id) are returned to the caller for a clear API error.
func (s *sqliteStore) CreateMCPKey(k *MCPKey) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO mcp_keys(`+mcpKeyColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		k.ID, k.PrincipalType, k.PeerID, k.Name, k.KeyPrefix, k.KeyHash, marshalScopes(k.Scopes),
		k.Status, k.Note, k.CreatedBy, k.CreatedAt, k.ExpiresAt, k.RevokedAt, k.RotatedFrom,
		k.LastUsedAt, k.LastUsedIP, k.UseCount)
	return err
}

func (s *sqliteStore) GetMCPKey(id string) (*MCPKey, error) {
	k, err := scanMCPKey(s.db.QueryRow(`SELECT `+mcpKeyColumns+` FROM mcp_keys WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// GetMCPKeyByHash is the external-client auth lookup. It does not filter
// status/expiry: internal/mcpgateway owns those decisions and records the
// matching denial reason.
func (s *sqliteStore) GetMCPKeyByHash(hash string) (*MCPKey, error) {
	k, err := scanMCPKey(s.db.QueryRow(`SELECT `+mcpKeyColumns+` FROM mcp_keys WHERE key_hash=?`, hash))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

// GetMCPPeerGrant returns the materialized peer authorization row.
func (s *sqliteStore) GetMCPPeerGrant(peerID string) (*MCPKey, error) {
	k, err := scanMCPKey(s.db.QueryRow(`SELECT `+mcpKeyColumns+`
		FROM mcp_keys WHERE principal_type='peer' AND peer_id=?`, peerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return k, err
}

func (s *sqliteStore) ListMCPKeys() ([]*MCPKey, error) {
	rows, err := s.db.Query(`SELECT ` + mcpKeyColumns + ` FROM mcp_keys WHERE principal_type='key' ORDER BY created_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPKey
	for rows.Next() {
		k, err := scanMCPKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ListMCPPeerGrants() ([]*MCPKey, error) {
	rows, err := s.db.Query(`SELECT ` + mcpKeyColumns + ` FROM mcp_keys WHERE principal_type='peer' ORDER BY peer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*MCPKey
	for rows.Next() {
		k, err := scanMCPKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SaveMCPPeerGrant inserts or updates the one peer row. The caller supplies a
// fresh ID for the insert path. Re-enabling a revoked row clears revoked_at.
func (s *sqliteStore) SaveMCPPeerGrant(k *MCPKey) (*MCPKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var id string
	err = tx.QueryRow(`SELECT id FROM mcp_keys WHERE principal_type='peer' AND peer_id=?`, k.PeerID).Scan(&id)
	switch {
	case err == sql.ErrNoRows:
		_, err = tx.Exec(`INSERT INTO mcp_keys(`+mcpKeyColumns+`)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			k.ID, "peer", k.PeerID, k.Name, "", "", marshalScopes(k.Scopes), k.Status, k.Note,
			k.CreatedBy, k.CreatedAt, 0, k.RevokedAt, "", 0, "", 0)
		if err != nil {
			return nil, err
		}
		id = k.ID
	case err != nil:
		return nil, err
	default:
		_, err = tx.Exec(`UPDATE mcp_keys SET name=?,scopes=?,status=?,note=?,revoked_at=?
			WHERE id=? AND principal_type='peer'`,
			k.Name, marshalScopes(k.Scopes), k.Status, k.Note, k.RevokedAt, id)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.GetMCPPeerGrant(k.PeerID)
}

func (s *sqliteStore) UpdateMCPKeyScopes(id string, scopes []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE mcp_keys SET scopes=? WHERE id=?`, marshalScopes(scopes), id)
	return err
}

// UpdateMCPKeyUsage is best effort on the request path; callers ignore errors.
func (s *sqliteStore) UpdateMCPKeyUsage(id string, ts int64, ip string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE mcp_keys SET last_used_at=?,last_used_ip=?,use_count=use_count+1 WHERE id=?`, ts, ip, id)
	return err
}

// RevokeMCPKey CASes active -> revoked. false means already revoked/missing.
func (s *sqliteStore) RevokeMCPKey(id string, ts int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_keys SET status='revoked',revoked_at=? WHERE id=? AND status='active'`, ts, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// SetMCPKeyRevokedAt schedules an active key's expiry without flipping the
// status yet (rotation grace, design v5 §4.4). Auth treats revoked_at<=now as
// dead even if the row still says active.
func (s *sqliteStore) SetMCPKeyRevokedAt(id string, at int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_keys SET revoked_at=? WHERE id=? AND status='active' AND revoked_at=0`, at, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RotateMCPKey atomically retires/schedules-expiry on oldID and inserts newKey.
// Returns (false, nil) if oldID is missing, not an external key, already
// revoked/in-grace, or expired.
func (s *sqliteStore) RotateMCPKey(oldID string, newKey *MCPKey, now, graceRevokeAt int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var res sql.Result
	if graceRevokeAt > now {
		res, err = tx.Exec(`UPDATE mcp_keys SET revoked_at=?
			WHERE id=? AND principal_type='key' AND status='active' AND revoked_at=0 AND (expires_at=0 OR expires_at>?)`,
			graceRevokeAt, oldID, now)
	} else {
		res, err = tx.Exec(`UPDATE mcp_keys SET status='revoked',revoked_at=?
			WHERE id=? AND principal_type='key' AND status='active' AND revoked_at=0 AND (expires_at=0 OR expires_at>?)`,
			now, oldID, now)
	}
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, nil
	}
	if _, err := tx.Exec(`INSERT INTO mcp_keys(`+mcpKeyColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		newKey.ID, newKey.PrincipalType, newKey.PeerID, newKey.Name, newKey.KeyPrefix, newKey.KeyHash,
		marshalScopes(newKey.Scopes), newKey.Status, newKey.Note, newKey.CreatedBy, newKey.CreatedAt,
		newKey.ExpiresAt, newKey.RevokedAt, newKey.RotatedFrom, newKey.LastUsedAt, newKey.LastUsedIP,
		newKey.UseCount); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *sqliteStore) upgradeMCPIndexes() {
	var sqlText string
	err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='index' AND name='idx_mcp_approvals_open'`).Scan(&sqlText)
	if err == nil && !strings.Contains(sqlText, "service_id") {
		_, _ = s.db.Exec(`DROP INDEX IF EXISTS idx_mcp_approvals_open`)
	}
}

const mcpApprovalColumns = `id,key_id,key_name,principal,auth_kind,endpoint,service_id,tool,risk,
	params_json,params_hash,status,requested_at,expires_at,decided_at,decided_by,decision_note,
	execute_before,executed_at,outcome,error_code,upstream_status,request_id`

func scanMCPApproval(sc rowScanner) (*MCPApproval, error) {
	var a MCPApproval
	err := sc.Scan(&a.ID, &a.KeyID, &a.KeyName, &a.Principal, &a.AuthKind, &a.Endpoint, &a.ServiceID,
		&a.Tool, &a.Risk, &a.ParamsJSON, &a.ParamsHash, &a.Status, &a.RequestedAt, &a.ExpiresAt,
		&a.DecidedAt, &a.DecidedBy, &a.DecisionNote, &a.ExecuteBefore, &a.ExecutedAt, &a.Outcome,
		&a.ErrorCode, &a.UpstreamStatus, &a.RequestID)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *sqliteStore) CreateMCPApproval(a *MCPApproval) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO mcp_approvals(`+mcpApprovalColumns+`)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.KeyID, a.KeyName, a.Principal, a.AuthKind, a.Endpoint, a.ServiceID, a.Tool, a.Risk,
		a.ParamsJSON, a.ParamsHash, a.Status, a.RequestedAt, a.ExpiresAt, a.DecidedAt, a.DecidedBy,
		a.DecisionNote, a.ExecuteBefore, a.ExecutedAt, a.Outcome, a.ErrorCode, a.UpstreamStatus, a.RequestID)
	return err
}

func (s *sqliteStore) GetMCPApproval(id string) (*MCPApproval, error) {
	a, err := scanMCPApproval(s.db.QueryRow(`SELECT `+mcpApprovalColumns+` FROM mcp_approvals WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

// GetOpenMCPApproval returns the existing pending/approved row for the same
// principal+service_id+tool+frozen params, so a re-issued request reuses the
// approval instead of stacking duplicates.
func (s *sqliteStore) GetOpenMCPApproval(principal, serviceID, tool, paramsHash string) (*MCPApproval, error) {
	a, err := scanMCPApproval(s.db.QueryRow(`SELECT `+mcpApprovalColumns+`
		FROM mcp_approvals WHERE principal=? AND service_id=? AND tool=? AND params_hash=?
		AND status IN ('pending','approved') ORDER BY requested_at DESC LIMIT 1`,
		principal, serviceID, tool, paramsHash))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return a, err
}

func (s *sqliteStore) ListMCPApprovals(f MCPApprovalFilter, limit, offset int) ([]*MCPApproval, int, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Status != "" {
		where = append(where, "status=?")
		args = append(args, f.Status)
	}
	if f.Principal != "" {
		where = append(where, "principal=?")
		args = append(args, f.Principal)
	}
	if f.ServiceID != "" {
		where = append(where, "service_id=?")
		args = append(args, f.ServiceID)
	}
	if f.Tool != "" {
		where = append(where, "tool=?")
		args = append(args, f.Tool)
	}
	if limit <= 0 {
		limit = 50
	}
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_approvals WHERE `+strings.Join(where, " AND "), args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(`SELECT `+mcpApprovalColumns+` FROM mcp_approvals WHERE `+
		strings.Join(where, " AND ")+` ORDER BY requested_at DESC, id LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*MCPApproval
	for rows.Next() {
		a, err := scanMCPApproval(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

func (s *sqliteStore) CountMCPApprovals(status string) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_approvals WHERE ?='' OR status=?`, status, status).Scan(&n)
	return n, err
}

func (s *sqliteStore) CountPendingMCPApprovals(now int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_approvals WHERE status='pending' AND expires_at>?`, now).Scan(&n)
	return n, err
}

// ApproveMCPApproval CASes pending -> approved inside the request TTL.
func (s *sqliteStore) ApproveMCPApproval(id string, now, executeBefore int64, by, note string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_approvals SET status='approved',decided_at=?,decided_by=?,
		decision_note=?,execute_before=? WHERE id=? AND status='pending' AND expires_at>?`,
		now, by, note, executeBefore, id, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *sqliteStore) RejectMCPApproval(id string, now int64, by, note string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_approvals SET status='rejected',decided_at=?,decided_by=?,
		decision_note=? WHERE id=? AND status='pending' AND expires_at>?`, now, by, note, id, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ClaimMCPApprovalExecution is the one-shot execution gate: approved ->
// executed only inside the 10-minute window. It runs BEFORE the upstream call
// so two concurrent replays cannot both execute. The outcome is filled in
// afterwards; a crash leaves a terminal executed row (no retry).
func (s *sqliteStore) ClaimMCPApprovalExecution(id string, now int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_approvals SET status='executed',executed_at=?
		WHERE id=? AND status='approved' AND execute_before>?`, now, id, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (s *sqliteStore) UpdateMCPApprovalOutcome(id, outcome, errCode string, upstreamStatus int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`UPDATE mcp_approvals SET outcome=?,error_code=?,upstream_status=?
		WHERE id=? AND status='executed'`, outcome, errCode, upstreamStatus, id)
	return err
}

// ExpireMCPApproval CASes one due pending/approved row to expired.
func (s *sqliteStore) ExpireMCPApproval(id string, now int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`UPDATE mcp_approvals SET status='expired',
		decision_note=CASE WHEN decision_note='' THEN 'expired' ELSE decision_note END
		WHERE id=? AND ((status='pending' AND expires_at<=?) OR (status='approved' AND execute_before<=?))`,
		id, now, now)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// ExpireDueMCPApprovals returns the rows it actually transitioned, so the
// caller can write one mcp.write_expired audit per row.
func (s *sqliteStore) ExpireDueMCPApprovals(now int64) ([]*MCPApproval, error) {
	rows, err := s.db.Query(`SELECT `+mcpApprovalColumns+` FROM mcp_approvals
		WHERE (status='pending' AND expires_at<=?) OR (status='approved' AND execute_before<=?)
		ORDER BY id`, now, now)
	if err != nil {
		return nil, err
	}
	var due []*MCPApproval
	for rows.Next() {
		a, err := scanMCPApproval(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		due = append(due, a)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []*MCPApproval
	for _, a := range due {
		ok, err := s.ExpireMCPApproval(a.ID, now)
		if err != nil {
			return nil, err
		}
		if ok {
			a.Status = "expired"
			out = append(out, a)
		}
	}
	return out, nil
}

const mcpAuditColumns = `seq,ts,request_id,actor,auth_kind,key_id,key_name,endpoint,service_id,tool,
	risk,params_hash,params_summary,approval_id,outcome,error_code,http_status,upstream_status,
	latency_ms,client_ip,detail`

func scanMCPAudit(sc rowScanner) (*MCPAuditEntry, error) {
	var a MCPAuditEntry
	err := sc.Scan(&a.Seq, &a.Ts, &a.RequestID, &a.Actor, &a.AuthKind, &a.KeyID, &a.KeyName,
		&a.Endpoint, &a.ServiceID, &a.Tool, &a.Risk, &a.ParamsHash, &a.ParamsSummary,
		&a.ApprovalID, &a.Outcome, &a.ErrorCode, &a.HTTPStatus, &a.UpstreamStatus,
		&a.LatencyMS, &a.ClientIP, &a.Detail)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

func (s *sqliteStore) AppendMCPAudit(e *MCPAuditEntry) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO mcp_audit(ts,request_id,actor,auth_kind,key_id,key_name,
		endpoint,service_id,tool,risk,params_hash,params_summary,approval_id,outcome,error_code,
		http_status,upstream_status,latency_ms,client_ip,detail)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		e.Ts, e.RequestID, e.Actor, e.AuthKind, e.KeyID, e.KeyName, e.Endpoint, e.ServiceID,
		e.Tool, e.Risk, e.ParamsHash, e.ParamsSummary, e.ApprovalID, e.Outcome, e.ErrorCode,
		e.HTTPStatus, e.UpstreamStatus, e.LatencyMS, e.ClientIP, e.Detail)
	return err
}

func (s *sqliteStore) ListMCPAudit(f MCPAuditFilter, limit, offset int) ([]*MCPAuditEntry, int, error) {
	where, args := []string{"1=1"}, []any{}
	if f.Actor != "" {
		where = append(where, "actor=?")
		args = append(args, f.Actor)
	}
	if f.ServiceID != "" {
		where = append(where, "service_id=?")
		args = append(args, f.ServiceID)
	}
	if f.Tool != "" {
		where = append(where, "tool=?")
		args = append(args, f.Tool)
	}
	if f.Outcome != "" {
		where = append(where, "outcome=?")
		args = append(args, f.Outcome)
	}
	if f.ApprovalID != "" {
		where = append(where, "approval_id=?")
		args = append(args, f.ApprovalID)
	}
	if f.Since > 0 {
		where = append(where, "ts>=?")
		args = append(args, f.Since)
	}
	if f.Until > 0 {
		where = append(where, "ts<=?")
		args = append(args, f.Until)
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 10000 {
		limit = 10000
	}
	clause := strings.Join(where, " AND ")
	var total int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM mcp_audit WHERE `+clause, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := s.db.Query(`SELECT `+mcpAuditColumns+` FROM mcp_audit WHERE `+clause+
		` ORDER BY seq DESC LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []*MCPAuditEntry
	for rows.Next() {
		a, err := scanMCPAudit(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// PruneMCPAudit deletes read rows older than readBefore and write/approval
// rows older than writeBefore. One statement keeps it atomic.
func (s *sqliteStore) PruneMCPAudit(readBefore, writeBefore int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`DELETE FROM mcp_audit
		WHERE (risk='read' AND ts<?) OR (risk<>'read' AND ts<?)`, readBefore, writeBefore)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// TrimMCPAudit keeps the newest maxRows rows (row-count ceiling #37).
func (s *sqliteStore) TrimMCPAudit(maxRows int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if maxRows <= 0 {
		return 0, nil
	}
	res, err := s.db.Exec(`DELETE FROM mcp_audit WHERE seq <= COALESCE(
		(SELECT seq FROM mcp_audit ORDER BY seq DESC LIMIT 1 OFFSET ?), 0)`, maxRows)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
