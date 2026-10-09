// Package store owns all SQLite access (DESIGN §4.6).
// No other module may import database/sql or write SQL.
package store

import (
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

//go:embed schema.sql
var schemaSQL string

// ---- Domain types (DESIGN §5.1) ----

type Peer struct {
	ID               string
	DisplayName      string
	AgentType        string
	ProtocolVer      int
	Capabilities     string // JSON
	Status           string // pending|verifying|active|suspended|failed
	CreatedAt        int64
	LastSeen         int64
	PromptVersion    int    // worker-instruction revision the peer runs (§8.6)
	PromptUpdatedAt  int64  // when the peer confirmed the current revision
	Profile          string // self-reported capabilities + usual tasks (§8.7)
	ProfileUpdatedAt int64  // when the profile was last set/refreshed
	ClientVersion    string // receiver client version tag (§8.9); "" = not reporting
	ClientUpdatedAt  int64  // when the client version was last reported
	ClientRev        string // receiver source revision (§8.9); "" = not reporting
	// Memory reconciliation (§8.6, prompt v11): the assistant's own summary
	// of which relay rules it purged from memory, and for which revision.
	MemoryNote         string
	MemoryVersion      int
	MemoryReconciledAt int64
}

type TokenRow struct {
	ID         int64  `json:"id"`
	PeerID     string `json:"peer_id"`
	TokenHash  string `json:"token_hash"`
	Label      string `json:"label"`
	CreatedAt  int64  `json:"created_at"`
	LastUsedAt int64  `json:"last_used_at"`
	LastIP     string `json:"last_ip"`
	RevokedAt  int64  `json:"revoked_at"`
}

type Invite struct {
	CodeHash   string
	IntendedID string
	AgentType  string
	CreatedBy  string
	CreatedAt  int64
	ExpiresAt  int64
	UsedBy     string
	UsedAt     int64
}

type Message struct {
	Seq              int64
	ID               string
	Sender           string
	Recipient        string
	Kind             string // task|result|chat|system|status|permission_request|permission_decision
	InReplyTo        string
	RootID           string
	RequiresApproval bool
	ApprovalState    string // n/a|pending|approved|rejected
	Payload          string
	CreatedAt        int64
	// Execution-state handshake (§6.5). Only set on the new kinds:
	// status → Status; permission_request → Op/Target/Detail + ExpiresAt;
	// permission_decision → Decision.
	Status    string
	Op        string
	Target    string
	Detail    string
	Decision  string // allow|deny
	ExpiresAt int64
}

// PermissionRequest is the server-side row for one permission_request
// message (request_id = message id). The decision CAS runs against this
// table: first decision wins, repeats are idempotent, flips are rejected.
type PermissionRequest struct {
	RequestID  string `json:"request_id"`
	Thread     string `json:"thread"`
	Requester  string `json:"requester"`
	Approver   string `json:"approver"`
	Status     string `json:"status"` // pending|allowed|denied|expired
	Op         string `json:"op"`
	Target     string `json:"target"`
	Detail     string `json:"detail"`
	CreatedAt  int64  `json:"created_at"`
	ExpiresAt  int64  `json:"expires_at"`
	DecidedAt  int64  `json:"decided_at"`
	DecisionID string `json:"decision_id"`
}

type AuditEntry struct {
	Seq    int64  `json:"seq"`
	Ts     int64  `json:"ts"`
	Actor  string `json:"actor"`
	Action string `json:"action"`
	Detail string `json:"detail"`
}

// Store is the repository interface. Postgres can implement this later (P2)
// without touching any other module.
type Store interface {
	// peers
	CreatePeer(p *Peer) error
	GetPeer(id string) (*Peer, error)
	ListPeers() ([]*Peer, error)
	UpdatePeerStatus(id, status string) error
	UpdatePeerMeta(id, displayName string) error
	UpdatePeerCapabilities(id string, protocolVer int, caps string) error
	// UpdatePeerProfile records a peer's self-reported profile (§8.7).
	// Empty profile keeps the stored value (same "not reporting" rule as
	// capabilities): only a non-empty string overwrites.
	UpdatePeerProfile(id, profile string, ts int64) error
	// UpdatePeerPrompt records a peer's confirmed worker-instruction
	// revision (§8.6). Only moves forward: stale replays are ignored.
	UpdatePeerPrompt(id string, promptVersion int, ts int64) error
	// UpdatePeerClientRev records the receiver source revision (§8.9): the
	// value the update nudge actually compares, so releases that do not
	// touch cmd/relay-tail never nag a current client.
	UpdatePeerClientRev(id, clientRev string, ts int64) error
	// UpdatePeerClient records the receiver client version (§8.9). Empty
	// keeps the stored value (shell scripts don't report); non-empty
	// overwrites unconditionally — client builds are opaque tags, not
	// ordered revisions, so no forward-only gate.
	UpdatePeerClient(id, clientVersion string, ts int64) error
	// UpdatePeerMemory records the assistant's memory-reconciliation report
	// (§8.6). Forward-only on version, same rule as UpdatePeerPrompt.
	UpdatePeerMemory(id, note string, version int, ts int64) error
	TouchPeer(id string, ts int64) error
	PrunePeers(olderThan int64) (int64, error)
	// DeletePeer removes the peer row, revokes all its tokens and clears
	// verify state in one transaction. Message history is intentionally
	// kept (threads stay readable, sender shown as-is).
	DeletePeer(id string, ts int64) (tokensRevoked int64, err error)
	// tokens
	CreateToken(peerID, hash, label string, ts int64) (int64, error)
	FindPeerByTokenHash(hash string) (*Peer, *TokenRow, error)
	ListTokens() ([]*TokenRow, error)
	ListTokensByPeer(peerID string) ([]*TokenRow, error)
	TouchToken(id int64, ts int64, ip string) error
	RevokeToken(id int64, ts int64) error
	// invites
	CreateInvite(inv *Invite) error
	GetInvite(codeHash string) (*Invite, error)
	ListInvites() ([]*Invite, error)
	MarkInviteUsed(codeHash, usedBy string, ts int64) error
	DeleteInvite(codeHash string) error
	// messages
	InsertMessage(m *Message) (int64, error)
	GetBySenderID(sender, id string) (*Message, error)
	GetBySeq(seq int64) (*Message, error)
	GetByIDAnySender(id string) ([]*Message, error)
	VisibleTo(peerID string, since int64, limit int) ([]*Message, error)
	ThreadMessages(rootID string, limit int) ([]*Message, error)
	LastNInThread(rootID string, n int) ([]*Message, error)
	// LastHandshakeTs returns MAX(created_at) for a handshake slice of a
	// thread (kind='status' + status value, one sender). Zero when absent.
	// Used for the progress per-thread throttle (§6.5).
	LastHandshakeTs(rootID, sender, status string) (int64, error)
	CountThreadSince(rootID string, sinceSeq int64) (int, error)
	ThreadOldestTs(rootID string) (int64, error)
	MaxSeq() (int64, error)
	ThreadRoots(limit int) ([]string, error)
	SearchMessages(query string, limit int) ([]*Message, error)
	ApproveMessage(seq int64, approved bool) error
	DeleteMessages(seqs []int64) error
	// permission handshake (§6.5)
	CreatePermissionRequest(pr *PermissionRequest) error
	GetPermissionRequest(requestID string) (*PermissionRequest, error)
	CountOpenPermissions(thread string, now int64) (int, error)
	// DecidePermission atomically inserts a decision message and CASes a
	// pending, unexpired row to decided. won=false (no insert) when the row
	// is missing, expired, or already decided — the caller re-reads the row
	// to distinguish duplicate-value idempotency from conflicting flips.
	DecidePermission(msg *Message, requestID, decision string, decidedAt int64) (won bool, err error)
	MarkPermissionExpired(requestID string, now int64) error
	PrunePermissions(olderThan int64) (int64, error)
	// acks
	Ack(seq int64, peerID string, ts int64) error
	IsAcked(seq int64, peerID string) (bool, error)
	AckedCount(seq int64) (int, error)
	// fuses (DESIGN §6.1 reset without deleting history)
	FuseWatermark(rootID string) (int64, error)
	SetFuseWatermark(rootID string, seq int64) error
	ClearFuseWatermark(rootID string) error
	// Delivery visibility (§4.4): one rule shared by VisibleTo, acking and any
	// future reader — see visibilityExpr.
	MessageVisibleTo(seq int64, peerID string) (bool, error)
	AckIfVisible(messageSeq int64, peerID string, ts int64) (bool, error)
	AcksForSeqs(seqs []int64) (map[int64][]string, error)
	// rooms (§6.8): group chat as a routing alias with a member list.
	CreateRoom(r *Room) error
	GetRoom(id string) (*Room, error)
	ListRoomsWithStats(includeArchived bool) ([]*RoomStats, error)
	UpdateRoom(id, name, note string) error
	SetRoomArchived(id string, ts int64) error
	AddRoomMember(roomID, peerID string, startSeq, ts int64) error
	RemoveRoomMember(roomID, peerID string) error
	ListRoomMembers(roomID string) ([]*RoomMember, error)
	IsRoomMember(roomID, peerID string) (bool, error)
	CountRoomMessagesSince(roomID string, sinceSeq, sinceTs int64) (int, error)
	// settings + operator inbox (§9.5)
	GetSetting(key string) (string, error)
	SetSetting(key, value string, ts int64) error
	InboxMessages(limit, offset int) ([]*Message, int, error)
	CountInboxSince(seq int64) (int, error)
	// verify (§8.4)
	SetSmoke(peerID, smokeID string, ts int64) error
	GetSmoke(peerID string) (smokeID string, ts int64, err error)
	ClearSmoke(peerID string) error
	// audit
	AppendAudit(actor, action, detail string, ts int64) error
	ListAudit(actor, action string, since, limit int) ([]*AuditEntry, error)
	PruneAudit(olderThan int64) (int64, error)
	// console read models (admin_pages.go): paged, total-counted queries
	ThreadSummaries(f ThreadFilter, limit, offset int) ([]*ThreadSummary, int, error)
	SearchMessagesPage(query string, limit, offset int) ([]*Message, int, error)
	ListAuditPage(f AuditFilter, limit, offset int) ([]*AuditEntry, int, error)
	AuditFacets() (actors, actions []string, err error)
	AdminCounts() (*AdminCounts, error)
	// ops
	DBSize() (int64, error)
	Vacuum() error
	Close() error
}

type sqliteStore struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (Store, error) {
	dsn := "file:" + path + "?cache=shared"
	if path == ":memory:" {
		dsn = ":memory:"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON;`); err != nil {
		db.Close()
		return nil, fmt.Errorf("pragma: %w", err)
	}
	s := &sqliteStore{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *sqliteStore) migrate() error {
	if err := s.dropTokensFK(); err != nil {
		return err
	}
	s.dropConversationsV12()
	// Strip full-line comments so each semicolon chunk is one statement.
	var b strings.Builder
	for _, line := range strings.Split(schemaSQL, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "--") {
			continue
		}
		b.WriteString(line)
		b.WriteString("\n")
	}
	for _, stmt := range strings.Split(b.String(), ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := s.db.Exec(stmt); err != nil && !isDupErr(err) {
			return fmt.Errorf("migrate stmt %.60q: %w", stmt, err)
		}
	}
	return nil
}

func isDupErr(err error) bool {
	msg := err.Error()
	// "already exists" covers CREATE IF NOT EXISTS races; schema.sql also
	// carries ALTER TABLE ... ADD COLUMN for pre-existing databases, which
	// fail with "duplicate column name" on every boot after the first.
	return strings.Contains(msg, "already exists") || strings.Contains(msg, "duplicate column")
}

// dropConversationsV12 removes the v12 conversations tables and the two
// message columns that release added. v0.14.0 was never deployed, but local
// databases that ran it keep the leftovers and nothing reads them any more:
// drop them so every database converges on the current schema. Best-effort —
// an SQLite without DROP COLUMN just keeps two unused columns.
func (s *sqliteStore) dropConversationsV12() {
	for _, stmt := range []string{
		`DROP TABLE IF EXISTS conversation_members`,
		`DROP TABLE IF EXISTS conversations`,
		`ALTER TABLE messages DROP COLUMN conv_id`,
		`ALTER TABLE messages DROP COLUMN mentions`,
	} {
		_, _ = s.db.Exec(stmt)
	}
}

// dropTokensFK rebuilds the tokens table without the peers FK for databases
// created before the FK was removed from the schema. Tokens must outlive
// peers (DESIGN §10.2); otherwise PrunePeers fails on the FK and a whole
// retention pass aborts. New databases already match the schema: no-op.
func (s *sqliteStore) dropTokensFK() error {
	var sql string
	err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='tokens'`).Scan(&sql)
	if err != nil {
		return nil // table doesn't exist yet; fresh migrate creates it right
	}
	if !strings.Contains(sql, "REFERENCES") {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmts := []string{
		`CREATE TABLE tokens_new (id INTEGER PRIMARY KEY AUTOINCREMENT, peer_id TEXT NOT NULL, token_hash TEXT NOT NULL UNIQUE, label TEXT DEFAULT '', created_at INTEGER NOT NULL, last_used_at INTEGER DEFAULT 0, last_ip TEXT DEFAULT '', revoked_at INTEGER DEFAULT 0)`,
		`INSERT INTO tokens_new(id,peer_id,token_hash,label,created_at,last_used_at,last_ip,revoked_at) SELECT id,peer_id,token_hash,label,created_at,last_used_at,last_ip,revoked_at FROM tokens`,
		`DROP TABLE tokens`,
		`ALTER TABLE tokens_new RENAME TO tokens`,
	}
	for _, stmt := range stmts {
		if _, err := tx.Exec(stmt); err != nil {
			return fmt.Errorf("drop tokens FK: %w", err)
		}
	}
	return tx.Commit()
}

// ---- peers ----

func (s *sqliteStore) CreatePeer(p *Peer) error {
	_, err := s.db.Exec(`INSERT INTO peers(id,display_name,agent_type,protocol_ver,capabilities,status,created_at,last_seen)
		VALUES(?,?,?,?,?,?,?,?)`, p.ID, p.DisplayName, p.AgentType, p.ProtocolVer, p.Capabilities, p.Status, p.CreatedAt, p.LastSeen)
	return err
}

func (s *sqliteStore) GetPeer(id string) (*Peer, error) {
	var p Peer
	err := s.db.QueryRow(`SELECT id,display_name,agent_type,protocol_ver,capabilities,status,created_at,last_seen,
		COALESCE(prompt_version,0),COALESCE(prompt_updated_at,0),
		COALESCE(profile,''),COALESCE(profile_updated_at,0),
		COALESCE(client_version,''),COALESCE(client_updated_at,0),COALESCE(client_rev,''),
		COALESCE(memory_note,''),COALESCE(memory_version,0),COALESCE(memory_reconciled_at,0) FROM peers WHERE id=?`, id).
		Scan(&p.ID, &p.DisplayName, &p.AgentType, &p.ProtocolVer, &p.Capabilities, &p.Status, &p.CreatedAt, &p.LastSeen,
			&p.PromptVersion, &p.PromptUpdatedAt, &p.Profile, &p.ProfileUpdatedAt,
			&p.ClientVersion, &p.ClientUpdatedAt, &p.ClientRev,
			&p.MemoryNote, &p.MemoryVersion, &p.MemoryReconciledAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (s *sqliteStore) ListPeers() ([]*Peer, error) {
	rows, err := s.db.Query(`SELECT id,display_name,agent_type,protocol_ver,capabilities,status,created_at,last_seen,
		COALESCE(prompt_version,0),COALESCE(prompt_updated_at,0),
		COALESCE(profile,''),COALESCE(profile_updated_at,0),
		COALESCE(client_version,''),COALESCE(client_updated_at,0),COALESCE(client_rev,''),
		COALESCE(memory_note,''),COALESCE(memory_version,0),COALESCE(memory_reconciled_at,0) FROM peers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Peer
	for rows.Next() {
		var p Peer
		if err := rows.Scan(&p.ID, &p.DisplayName, &p.AgentType, &p.ProtocolVer, &p.Capabilities, &p.Status, &p.CreatedAt, &p.LastSeen,
			&p.PromptVersion, &p.PromptUpdatedAt, &p.Profile, &p.ProfileUpdatedAt,
			&p.ClientVersion, &p.ClientUpdatedAt, &p.ClientRev,
			&p.MemoryNote, &p.MemoryVersion, &p.MemoryReconciledAt); err != nil {
			return nil, err
		}
		out = append(out, &p)
	}
	return out, rows.Err()
}

func (s *sqliteStore) UpdatePeerStatus(id, status string) error {
	_, err := s.db.Exec(`UPDATE peers SET status=? WHERE id=?`, status, id)
	return err
}

func (s *sqliteStore) UpdatePeerMeta(id, displayName string) error {
	_, err := s.db.Exec(`UPDATE peers SET display_name=? WHERE id=?`, displayName, id)
	return err
}

func (s *sqliteStore) UpdatePeerCapabilities(id string, protocolVer int, caps string) error {
	_, err := s.db.Exec(`UPDATE peers SET protocol_ver=?, capabilities=? WHERE id=?`, protocolVer, caps, id)
	return err
}

func (s *sqliteStore) UpdatePeerProfile(id, profile string, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET profile=?, profile_updated_at=? WHERE id=?`, profile, ts, id)
	return err
}

func (s *sqliteStore) UpdatePeerPrompt(id string, promptVersion int, ts int64) error {
	// Forward-only: a delayed heartbeat carrying an older revision must not
	// clobber a newer confirmation.
	_, err := s.db.Exec(`UPDATE peers SET prompt_version=?, prompt_updated_at=?
		WHERE id=? AND COALESCE(prompt_version,0)<=?`, promptVersion, ts, id, promptVersion)
	return err
}

func (s *sqliteStore) UpdatePeerClient(id, clientVersion string, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET client_version=?, client_updated_at=? WHERE id=?`,
		clientVersion, ts, id)
	return err
}

func (s *sqliteStore) UpdatePeerClientRev(id, clientRev string, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET client_rev=?, client_updated_at=? WHERE id=?`,
		clientRev, ts, id)
	return err
}

func (s *sqliteStore) UpdatePeerMemory(id, note string, version int, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET memory_note=?, memory_version=?, memory_reconciled_at=?
		WHERE id=? AND COALESCE(memory_version,0)<=?`, note, version, ts, id, version)
	return err
}

func (s *sqliteStore) TouchPeer(id string, ts int64) error {
	_, err := s.db.Exec(`UPDATE peers SET last_seen=? WHERE id=?`, ts, id)
	return err
}

func (s *sqliteStore) PrunePeers(olderThan int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM peers WHERE last_seen>0 AND last_seen<?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s *sqliteStore) DeletePeer(id string, ts int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE tokens SET revoked_at=? WHERE peer_id=? AND revoked_at=0`, ts, id)
	if err != nil {
		return 0, err
	}
	revoked, _ := res.RowsAffected()
	if _, err := tx.Exec(`DELETE FROM verify_state WHERE peer_id=?`, id); err != nil {
		return 0, err
	}
	res, err = tx.Exec(`DELETE FROM peers WHERE id=?`, id)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, fmt.Errorf("unknown peer %q", id)
	}
	return revoked, tx.Commit()
}

// ---- tokens ----

func (s *sqliteStore) CreateToken(peerID, hash, label string, ts int64) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO tokens(peer_id,token_hash,label,created_at) VALUES(?,?,?,?)`, peerID, hash, label, ts)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *sqliteStore) FindPeerByTokenHash(hash string) (*Peer, *TokenRow, error) {
	var t TokenRow
	err := s.db.QueryRow(`SELECT id,peer_id,token_hash,label,created_at,last_used_at,last_ip,revoked_at FROM tokens WHERE token_hash=?`, hash).
		Scan(&t.ID, &t.PeerID, &t.TokenHash, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.LastIP, &t.RevokedAt)
	if err == sql.ErrNoRows {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	if t.RevokedAt != 0 {
		return nil, nil, nil
	}
	p, err := s.GetPeer(t.PeerID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		// Peer row pruned but token survives (DESIGN §10.2). Return the
		// token with a nil peer so the auth layer can recreate it.
		return nil, &t, nil
	}
	if p.Status == "suspended" {
		return nil, nil, nil
	}
	return p, &t, nil
}

func (s *sqliteStore) ListTokens() ([]*TokenRow, error) {
	rows, err := s.db.Query(`SELECT id,peer_id,token_hash,label,created_at,last_used_at,last_ip,revoked_at FROM tokens ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TokenRow
	for rows.Next() {
		var t TokenRow
		if err := rows.Scan(&t.ID, &t.PeerID, &t.TokenHash, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.LastIP, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *sqliteStore) ListTokensByPeer(peerID string) ([]*TokenRow, error) {
	rows, err := s.db.Query(`SELECT id,peer_id,token_hash,label,created_at,last_used_at,last_ip,revoked_at FROM tokens WHERE peer_id=? ORDER BY id`, peerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TokenRow
	for rows.Next() {
		var t TokenRow
		if err := rows.Scan(&t.ID, &t.PeerID, &t.TokenHash, &t.Label, &t.CreatedAt, &t.LastUsedAt, &t.LastIP, &t.RevokedAt); err != nil {
			return nil, err
		}
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (s *sqliteStore) TouchToken(id int64, ts int64, ip string) error {
	_, err := s.db.Exec(`UPDATE tokens SET last_used_at=?, last_ip=? WHERE id=?`, ts, ip, id)
	return err
}

func (s *sqliteStore) RevokeToken(id int64, ts int64) error {
	_, err := s.db.Exec(`UPDATE tokens SET revoked_at=? WHERE id=?`, ts, id)
	return err
}

// ---- invites ----

func (s *sqliteStore) CreateInvite(inv *Invite) error {
	_, err := s.db.Exec(`INSERT INTO invites(code_hash,intended_id,agent_type,created_by,created_at,expires_at,used_by,used_at)
		VALUES(?,?,?,?,?,?,?,?)`, inv.CodeHash, inv.IntendedID, inv.AgentType, inv.CreatedBy, inv.CreatedAt, inv.ExpiresAt, inv.UsedBy, inv.UsedAt)
	return err
}

func (s *sqliteStore) GetInvite(codeHash string) (*Invite, error) {
	var inv Invite
	err := s.db.QueryRow(`SELECT code_hash,intended_id,agent_type,created_by,created_at,expires_at,used_by,used_at FROM invites WHERE code_hash=?`, codeHash).
		Scan(&inv.CodeHash, &inv.IntendedID, &inv.AgentType, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedBy, &inv.UsedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &inv, nil
}

func (s *sqliteStore) ListInvites() ([]*Invite, error) {
	rows, err := s.db.Query(`SELECT code_hash,intended_id,agent_type,created_by,created_at,expires_at,used_by,used_at FROM invites ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Invite
	for rows.Next() {
		var inv Invite
		if err := rows.Scan(&inv.CodeHash, &inv.IntendedID, &inv.AgentType, &inv.CreatedBy, &inv.CreatedAt, &inv.ExpiresAt, &inv.UsedBy, &inv.UsedAt); err != nil {
			return nil, err
		}
		out = append(out, &inv)
	}
	return out, rows.Err()
}

func (s *sqliteStore) MarkInviteUsed(codeHash, usedBy string, ts int64) error {
	_, err := s.db.Exec(`UPDATE invites SET used_by=?, used_at=? WHERE code_hash=?`, usedBy, ts, codeHash)
	return err
}

func (s *sqliteStore) DeleteInvite(codeHash string) error {
	_, err := s.db.Exec(`DELETE FROM invites WHERE code_hash=?`, codeHash)
	return err
}

// ---- messages ----

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func (s *sqliteStore) InsertMessage(m *Message) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	res, err := s.db.Exec(`INSERT INTO messages(id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,status,op,target,detail,decision,expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, m.ID, m.Sender, m.Recipient, m.Kind, m.InReplyTo, m.RootID, boolToInt(m.RequiresApproval), m.ApprovalState, m.Payload, m.CreatedAt,
		m.Status, m.Op, m.Target, m.Detail, m.Decision, m.ExpiresAt)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func scanMessage(rows *sql.Rows) (*Message, error) {
	var m Message
	var req int
	err := rows.Scan(&m.Seq, &m.ID, &m.Sender, &m.Recipient, &m.Kind, &m.InReplyTo, &m.RootID, &req, &m.ApprovalState, &m.Payload, &m.CreatedAt,
		&m.Status, &m.Op, &m.Target, &m.Detail, &m.Decision, &m.ExpiresAt)
	if err != nil {
		return nil, err
	}
	m.RequiresApproval = req != 0
	return &m, nil
}

func (s *sqliteStore) GetBySenderID(sender, id string) (*Message, error) {
	var m Message
	var req int
	err := s.db.QueryRow(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE sender=? AND id=?`, sender, id).
		Scan(&m.Seq, &m.ID, &m.Sender, &m.Recipient, &m.Kind, &m.InReplyTo, &m.RootID, &req, &m.ApprovalState, &m.Payload, &m.CreatedAt,
			&m.Status, &m.Op, &m.Target, &m.Detail, &m.Decision, &m.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.RequiresApproval = req != 0
	return &m, nil
}

func (s *sqliteStore) GetBySeq(seq int64) (*Message, error) {
	var m Message
	var req int
	err := s.db.QueryRow(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE seq=?`, seq).
		Scan(&m.Seq, &m.ID, &m.Sender, &m.Recipient, &m.Kind, &m.InReplyTo, &m.RootID, &req, &m.ApprovalState, &m.Payload, &m.CreatedAt,
			&m.Status, &m.Op, &m.Target, &m.Detail, &m.Decision, &m.ExpiresAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	m.RequiresApproval = req != 0
	return &m, nil
}

func (s *sqliteStore) GetByIDAnySender(id string) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE id=? ORDER BY seq`, id)
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

// visibilityExpr is the single source of truth for "which messages can a peer
// see" (DESIGN §4.4): direct, broadcast, or a room the peer belongs to. It
// takes four peer-id parameters in order (direct, broadcast sender, room
// sender, room member). VisibleTo and MessageVisibleTo share it verbatim so two
// implementations of the delivery rule cannot drift; acking (AckIfVisible)
// reuses MessageVisibleTo for the same reason.
const visibilityExpr = `((m.recipient = ?)
		  OR (m.recipient = '*' AND m.sender != ?)
		  OR (m.recipient LIKE 'grp_%' AND m.sender != ? AND EXISTS (
		       SELECT 1 FROM room_members rm
		       WHERE rm.room_id = m.recipient AND rm.peer_id = ? AND rm.start_seq < m.seq)))`

// visibleSelect is the column list every message read shares.
const visibleSelect = `SELECT m.seq,m.id,m.sender,m.recipient,m.kind,m.in_reply_to,m.root_id,
		m.requires_approval,m.approval_state,m.payload,m.created_at,
		m.status,m.op,m.target,m.detail,m.decision,m.expires_at
		FROM messages m`

// VisibleTo implements the per-identity delivery view (DESIGN §4.4):
// direct + broadcast + room membership, seq > since, not yet acked by the
// peer, not held in pending approval.
func (s *sqliteStore) VisibleTo(peerID string, since int64, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	rows, err := s.db.Query(visibleSelect+`
		WHERE m.seq > ?
		  AND m.approval_state NOT IN ('pending','rejected')
		  AND NOT EXISTS (SELECT 1 FROM acks a WHERE a.message_seq = m.seq AND a.peer_id = ?)
		  AND `+visibilityExpr+`
		ORDER BY m.seq ASC LIMIT ?`, since, peerID, peerID, peerID, peerID, peerID, limit)
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

// MessageVisibleTo reports whether one message is visible to peerID under the
// same rule VisibleTo uses. Acking goes through this, so a peer can never ack
// (and thereby silently swallow) a message it was not entitled to see.
func (s *sqliteStore) MessageVisibleTo(seq int64, peerID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages m
		WHERE m.seq = ?
		  AND m.approval_state NOT IN ('pending','rejected')
		  AND `+visibilityExpr, seq, peerID, peerID, peerID, peerID).Scan(&n)
	return n > 0, err
}

// AckIfVisible acks one message when it is visible to that peer, reporting
// whether the ack was applied.
func (s *sqliteStore) AckIfVisible(messageSeq int64, peerID string, ts int64) (bool, error) {
	ok, err := s.MessageVisibleTo(messageSeq, peerID)
	if err != nil || !ok {
		return false, err
	}
	if err := s.Ack(messageSeq, peerID, ts); err != nil {
		return false, err
	}
	return true, nil
}

func (s *sqliteStore) ThreadMessages(rootID string, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE root_id=? ORDER BY seq ASC LIMIT ?`, rootID, limit)
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

func (s *sqliteStore) LastNInThread(rootID string, n int) ([]*Message, error) {
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE root_id=? ORDER BY seq DESC LIMIT ?`, rootID, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rev []*Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		rev = append(rev, m)
	}
	// reverse to chronological
	out := make([]*Message, len(rev))
	for i, m := range rev {
		out[len(rev)-1-i] = m
	}
	return out, rows.Err()
}

func (s *sqliteStore) CountThreadSince(rootID string, sinceSeq int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM messages WHERE root_id=? AND seq>?`, rootID, sinceSeq).Scan(&n)
	return n, err
}

func (s *sqliteStore) ThreadOldestTs(rootID string) (int64, error) {
	var ts sql.NullInt64
	err := s.db.QueryRow(`SELECT MIN(created_at) FROM messages WHERE root_id=?`, rootID).Scan(&ts)
	if err != nil {
		return 0, err
	}
	if !ts.Valid {
		return 0, nil
	}
	return ts.Int64, nil
}

func (s *sqliteStore) MaxSeq() (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(seq) FROM messages`).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

func (s *sqliteStore) ThreadRoots(limit int) ([]string, error) {
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT root_id, MAX(seq) AS mx FROM messages GROUP BY root_id ORDER BY mx DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var r string
		var mx int64
		if err := rows.Scan(&r, &mx); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *sqliteStore) SearchMessages(query string, limit int) ([]*Message, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(query)
	rows, err := s.db.Query(`SELECT seq,id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,
		status,op,target,detail,decision,expires_at
		FROM messages WHERE payload LIKE ? ESCAPE '\' ORDER BY seq DESC LIMIT ?`, "%"+esc+"%", limit)
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

func (s *sqliteStore) ApproveMessage(seq int64, approved bool) error {
	state := "approved"
	if !approved {
		state = "rejected"
	}
	_, err := s.db.Exec(`UPDATE messages SET approval_state=? WHERE seq=?`, state, seq)
	return err
}

func (s *sqliteStore) DeleteMessages(seqs []int64) error {
	if len(seqs) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, seq := range seqs {
		if _, err := tx.Exec(`DELETE FROM acks WHERE message_seq=?`, seq); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM messages WHERE seq=?`, seq); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *sqliteStore) LastHandshakeTs(rootID, sender, status string) (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT MAX(created_at) FROM messages
		WHERE root_id=? AND sender=? AND kind='status' AND status=?`, rootID, sender, status).Scan(&v)
	if err != nil {
		return 0, err
	}
	if !v.Valid {
		return 0, nil
	}
	return v.Int64, nil
}

// ---- permission handshake (§6.5) ----

func (s *sqliteStore) CreatePermissionRequest(pr *PermissionRequest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO permission_requests(request_id,thread,requester,approver,status,op,target,detail,created_at,expires_at,decided_at,decision_id)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, pr.RequestID, pr.Thread, pr.Requester, pr.Approver, pr.Status,
		pr.Op, pr.Target, pr.Detail, pr.CreatedAt, pr.ExpiresAt, pr.DecidedAt, pr.DecisionID)
	return err
}

func (s *sqliteStore) GetPermissionRequest(requestID string) (*PermissionRequest, error) {
	var pr PermissionRequest
	err := s.db.QueryRow(`SELECT request_id,thread,requester,approver,status,op,target,detail,created_at,expires_at,decided_at,decision_id
		FROM permission_requests WHERE request_id=?`, requestID).
		Scan(&pr.RequestID, &pr.Thread, &pr.Requester, &pr.Approver, &pr.Status,
			&pr.Op, &pr.Target, &pr.Detail, &pr.CreatedAt, &pr.ExpiresAt, &pr.DecidedAt, &pr.DecisionID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &pr, nil
}

func (s *sqliteStore) CountOpenPermissions(thread string, now int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM permission_requests
		WHERE thread=? AND status='pending' AND expires_at>?`, thread, now).Scan(&n)
	return n, err
}

// DecidePermission inserts the decision message and conditionally flips a
// pending, unexpired row in ONE transaction. won=false must be followed by
// a re-read so the caller can tell same-value idempotency from conflicts.
func (s *sqliteStore) DecidePermission(msg *Message, requestID, decision string, decidedAt int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO messages(id,sender,recipient,kind,in_reply_to,root_id,requires_approval,approval_state,payload,created_at,status,op,target,detail,decision,expires_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, msg.ID, msg.Sender, msg.Recipient, msg.Kind, msg.InReplyTo, msg.RootID,
		boolToInt(msg.RequiresApproval), msg.ApprovalState, msg.Payload, msg.CreatedAt,
		msg.Status, msg.Op, msg.Target, msg.Detail, msg.Decision, msg.ExpiresAt); err != nil {
		return false, err
	}
	newStatus := "allowed"
	if decision == "deny" {
		newStatus = "denied"
	}
	res, err := tx.Exec(`UPDATE permission_requests
		SET status=?, decided_at=?, decision_id=?
		WHERE request_id=? AND status='pending' AND expires_at>?`,
		newStatus, decidedAt, msg.ID, requestID, decidedAt)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, tx.Commit()
	}
	return true, tx.Commit()
}

func (s *sqliteStore) MarkPermissionExpired(requestID string, now int64) error {
	_, err := s.db.Exec(`UPDATE permission_requests SET status='expired'
		WHERE request_id=? AND status='pending' AND expires_at<=?`, requestID, now)
	return err
}

func (s *sqliteStore) PrunePermissions(olderThan int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM permission_requests WHERE expires_at<? AND status!='pending'`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- acks ----

func (s *sqliteStore) Ack(seq int64, peerID string, ts int64) error {
	_, err := s.db.Exec(`INSERT OR IGNORE INTO acks(message_seq,peer_id,acked_at) VALUES(?,?,?)`, seq, peerID, ts)
	return err
}

func (s *sqliteStore) IsAcked(seq int64, peerID string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM acks WHERE message_seq=? AND peer_id=?`, seq, peerID).Scan(&n)
	return n > 0, err
}

func (s *sqliteStore) AckedCount(seq int64) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM acks WHERE message_seq=?`, seq).Scan(&n)
	return n, err
}

// ---- fuses ----

func (s *sqliteStore) FuseWatermark(rootID string) (int64, error) {
	var seq sql.NullInt64
	err := s.db.QueryRow(`SELECT cleared_seq FROM thread_fuses WHERE root_id=?`, rootID).Scan(&seq)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, err
	}
	if !seq.Valid {
		return 0, nil
	}
	return seq.Int64, nil
}

func (s *sqliteStore) SetFuseWatermark(rootID string, seq int64) error {
	_, err := s.db.Exec(`INSERT INTO thread_fuses(root_id,cleared_seq,updated_at) VALUES(?,?,?)
		ON CONFLICT(root_id) DO UPDATE SET cleared_seq=excluded.cleared_seq, updated_at=excluded.updated_at`,
		rootID, seq, time.Now().Unix())
	return err
}

func (s *sqliteStore) ClearFuseWatermark(rootID string) error {
	_, err := s.db.Exec(`DELETE FROM thread_fuses WHERE root_id=?`, rootID)
	return err
}

// ---- verify ----

func (s *sqliteStore) SetSmoke(peerID, smokeID string, ts int64) error {
	_, err := s.db.Exec(`INSERT INTO verify_state(peer_id,smoke_id,updated_at) VALUES(?,?,?)
		ON CONFLICT(peer_id) DO UPDATE SET smoke_id=excluded.smoke_id, updated_at=excluded.updated_at`, peerID, smokeID, ts)
	return err
}

func (s *sqliteStore) GetSmoke(peerID string) (string, int64, error) {
	var id string
	var ts int64
	err := s.db.QueryRow(`SELECT smoke_id,updated_at FROM verify_state WHERE peer_id=?`, peerID).Scan(&id, &ts)
	if err == sql.ErrNoRows {
		return "", 0, nil
	}
	if err != nil {
		return "", 0, err
	}
	return id, ts, nil
}

func (s *sqliteStore) ClearSmoke(peerID string) error {
	_, err := s.db.Exec(`DELETE FROM verify_state WHERE peer_id=?`, peerID)
	return err
}

// ---- audit ----

func (s *sqliteStore) AppendAudit(actor, action, detail string, ts int64) error {
	_, err := s.db.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`, ts, actor, action, detail)
	return err
}

func (s *sqliteStore) ListAudit(actor, action string, since, limit int) ([]*AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	q := `SELECT seq,ts,actor,action,detail FROM audit_log WHERE seq>?`
	args := []any{since}
	if actor != "" {
		q += ` AND actor=?`
		args = append(args, actor)
	}
	if action != "" {
		q += ` AND action=?`
		args = append(args, action)
	}
	q += ` ORDER BY seq ASC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.Seq, &e.Ts, &e.Actor, &e.Action, &e.Detail); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (s *sqliteStore) PruneAudit(olderThan int64) (int64, error) {
	res, err := s.db.Exec(`DELETE FROM audit_log WHERE ts<?`, olderThan)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ---- ops ----

func (s *sqliteStore) DBSize() (int64, error) {
	var pageCount, pageSize int64
	if err := s.db.QueryRow(`PRAGMA page_count`).Scan(&pageCount); err != nil {
		return 0, err
	}
	if err := s.db.QueryRow(`PRAGMA page_size`).Scan(&pageSize); err != nil {
		return 0, err
	}
	return pageCount * pageSize, nil
}

func (s *sqliteStore) Vacuum() error {
	_, err := s.db.Exec(`VACUUM`)
	return err
}

func (s *sqliteStore) Close() error { return s.db.Close() }
