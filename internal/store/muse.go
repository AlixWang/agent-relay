package store

import (
	"database/sql"
	"fmt"
)

// MuseAccount is one authorized muse.ai account bound to a relay peer
// (DESIGN §4.9). CookiesEnc is opaque AES-GCM ciphertext: the store never
// interprets it and no API may return it. CookiesExp is a JSON object of
// cookie name → unix expiry (not secret).
type MuseAccount struct {
	ID            string
	PeerID        string
	Label         string
	CookiesEnc    string
	CookiesExp    string
	SessionStatus string
	VMID          string
	VMState       string
	// VMStateAt is when VMState was last confirmed by a successful
	// /api/session response. Zero means the value predates this field.
	VMStateAt int64
	AuthState string // unverified | ok | needs_reauth
	LastError string

	LastKeepaliveAt int64
	LastAttemptAt   int64
	LastAttemptOK   bool
	LastWakeAt      int64
	LastWakeOK      bool
	// LastWakeError/LastWakeState are wake-scoped diagnostics; LastError stays
	// session/auth-scoped (P0: a failed wake must not mark the account broken).
	LastWakeError string
	LastWakeState string
	AutoWake      bool
	Enabled       bool
	// Direct-chat access is an administrator switch, separate from keepalive
	// authorization. It is reset to zero whenever the account's cookie or peer
	// binding changes. The chat_consent_* columns are reused as enabled_at/by;
	// chat_consent_version/evidence are deprecated and must not be read/written.
	ChatEnabled    bool
	ChatEnabledAt  int64
	ChatEnabledBy  string
	LastChatAt     int64
	LastChatStatus string
	AuthorizedAt   int64
	UpdatedAt      int64
}

type rowScanner interface {
	Scan(dest ...any) error
}

const museAccountColumns = `id,peer_id,COALESCE(label,''),COALESCE(cookies_enc,''),COALESCE(cookies_exp,'{}'),
	COALESCE(session_status,''),COALESCE(vm_id,''),COALESCE(vm_state,''),COALESCE(vm_state_at,0),
	COALESCE(auth_state,'unverified'),COALESCE(last_error,''),
	COALESCE(last_keepalive_at,0),COALESCE(last_attempt_at,0),COALESCE(last_attempt_ok,0),
	COALESCE(last_wake_at,0),COALESCE(last_wake_ok,0),
	COALESCE(last_wake_error,''),COALESCE(last_wake_state,''),
	COALESCE(auto_wake,1),COALESCE(enabled,1),
	COALESCE(chat_enabled,0),COALESCE(chat_consent_at,0),COALESCE(chat_consent_by,''),
	COALESCE(last_chat_at,0),COALESCE(last_chat_status,''),
	authorized_at,updated_at`

func scanMuseAccount(sc rowScanner) (*MuseAccount, error) {
	var a MuseAccount
	var attemptOK, wakeOK, autoWake, enabled, chatEnabled int
	err := sc.Scan(&a.ID, &a.PeerID, &a.Label, &a.CookiesEnc, &a.CookiesExp,
		&a.SessionStatus, &a.VMID, &a.VMState, &a.VMStateAt, &a.AuthState, &a.LastError,
		&a.LastKeepaliveAt, &a.LastAttemptAt, &attemptOK,
		&a.LastWakeAt, &wakeOK, &a.LastWakeError, &a.LastWakeState,
		&autoWake, &enabled,
		&chatEnabled, &a.ChatEnabledAt, &a.ChatEnabledBy,
		&a.LastChatAt, &a.LastChatStatus,
		&a.AuthorizedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.LastAttemptOK = attemptOK != 0
	a.LastWakeOK = wakeOK != 0
	a.AutoWake = autoWake != 0
	a.Enabled = enabled != 0
	a.ChatEnabled = chatEnabled != 0
	return &a, nil
}

func (s *sqliteStore) CreateMuseAccount(a *MuseAccount) error {
	_, err := s.db.Exec(`INSERT INTO muse_accounts(
		id,peer_id,label,cookies_enc,cookies_exp,session_status,vm_id,vm_state,vm_state_at,
		auth_state,last_error,last_keepalive_at,last_attempt_at,last_attempt_ok,
		last_wake_at,last_wake_ok,last_wake_error,last_wake_state,
		auto_wake,enabled,
		chat_enabled,chat_consent_at,chat_consent_by,
		last_chat_at,last_chat_status,authorized_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.PeerID, a.Label, a.CookiesEnc, a.CookiesExp, a.SessionStatus, a.VMID, a.VMState, a.VMStateAt,
		a.AuthState, a.LastError, a.LastKeepaliveAt, a.LastAttemptAt, boolToInt(a.LastAttemptOK),
		a.LastWakeAt, boolToInt(a.LastWakeOK), a.LastWakeError, a.LastWakeState,
		boolToInt(a.AutoWake), boolToInt(a.Enabled),
		boolToInt(a.ChatEnabled), a.ChatEnabledAt, a.ChatEnabledBy,
		a.LastChatAt, a.LastChatStatus, a.AuthorizedAt, a.UpdatedAt)
	return err
}

func (s *sqliteStore) GetMuseAccount(id string) (*MuseAccount, error) {
	a, err := scanMuseAccount(s.db.QueryRow(`SELECT `+museAccountColumns+` FROM muse_accounts WHERE id=?`, id))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *sqliteStore) GetMuseAccountByPeer(peerID string) (*MuseAccount, error) {
	a, err := scanMuseAccount(s.db.QueryRow(`SELECT `+museAccountColumns+` FROM muse_accounts WHERE peer_id=?`, peerID))
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return a, nil
}

func (s *sqliteStore) ListMuseAccounts() ([]*MuseAccount, error) {
	rows, err := s.db.Query(`SELECT ` + museAccountColumns + ` FROM muse_accounts ORDER BY peer_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*MuseAccount{}
	for rows.Next() {
		a, err := scanMuseAccount(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SaveMuseAccount updates every mutable column. Callers hold the muse
// service's operation lock so a keepalive write cannot clobber a concurrent
// settings change.
func (s *sqliteStore) SaveMuseAccount(a *MuseAccount) error {
	res, err := s.db.Exec(`UPDATE muse_accounts SET
		peer_id=?,label=?,cookies_enc=?,cookies_exp=?,session_status=?,vm_id=?,vm_state=?,vm_state_at=?,
		auth_state=?,last_error=?,last_keepalive_at=?,last_attempt_at=?,last_attempt_ok=?,
		last_wake_at=?,last_wake_ok=?,last_wake_error=?,last_wake_state=?,
		auto_wake=?,enabled=?,
		chat_enabled=?,chat_consent_at=?,chat_consent_by=?,
		last_chat_at=?,last_chat_status=?,authorized_at=?,updated_at=?
		WHERE id=?`,
		a.PeerID, a.Label, a.CookiesEnc, a.CookiesExp, a.SessionStatus, a.VMID, a.VMState, a.VMStateAt,
		a.AuthState, a.LastError, a.LastKeepaliveAt, a.LastAttemptAt, boolToInt(a.LastAttemptOK),
		a.LastWakeAt, boolToInt(a.LastWakeOK), a.LastWakeError, a.LastWakeState,
		boolToInt(a.AutoWake), boolToInt(a.Enabled),
		boolToInt(a.ChatEnabled), a.ChatEnabledAt, a.ChatEnabledBy,
		a.LastChatAt, a.LastChatStatus, a.AuthorizedAt, a.UpdatedAt, a.ID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown muse account %q", a.ID)
	}
	return nil
}

func (s *sqliteStore) DeleteMuseAccount(id string) error {
	_, err := s.db.Exec(`DELETE FROM muse_accounts WHERE id=?`, id)
	return err
}

// SetMuseChatAccess writes the per-member direct-chat switch and its audit row
// in one transaction. The account row and the audit trail can therefore never
// disagree about whether access exists. Disabling preserves enabled_at/by for
// history; enabling replaces them. The deprecated consent_version/evidence
// columns are never touched.
func (s *sqliteStore) SetMuseChatAccess(id string, enabled bool, at int64, by,
	actor, action, detail string, auditAt int64) error {

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var (
		res sql.Result
		q   string
	)
	if enabled {
		q = `UPDATE muse_accounts SET chat_enabled=1,chat_consent_at=?,chat_consent_by=? WHERE id=?`
		res, err = tx.Exec(q, at, by, id)
	} else {
		q = `UPDATE muse_accounts SET chat_enabled=0 WHERE id=?`
		res, err = tx.Exec(q, id)
	}
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown muse account %q", id)
	}
	if _, err := tx.Exec(`INSERT INTO audit_log(ts,actor,action,detail) VALUES(?,?,?,?)`,
		auditAt, actor, action, detail); err != nil {
		return err
	}
	return tx.Commit()
}

// SetMuseChatStatus records the latest direct-chat attempt for display. It is
// deliberately narrow: in-flight jobs must not rewrite the whole account row
// (and thus race a keepalive's cookie update).
func (s *sqliteStore) SetMuseChatStatus(id string, at int64, status string) error {
	_, err := s.db.Exec(`UPDATE muse_accounts SET last_chat_at=?,last_chat_status=? WHERE id=?`,
		at, status, id)
	return err
}
