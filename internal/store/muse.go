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
	AuthState     string // unverified | ok | needs_reauth
	LastError     string

	LastKeepaliveAt int64
	LastAttemptAt   int64
	LastAttemptOK   bool
	LastWakeAt      int64
	LastWakeOK      bool
	AutoWake        bool
	Enabled         bool
	AuthorizedAt    int64
	UpdatedAt       int64
}

type rowScanner interface {
	Scan(dest ...any) error
}

const museAccountColumns = `id,peer_id,COALESCE(label,''),COALESCE(cookies_enc,''),COALESCE(cookies_exp,'{}'),
	COALESCE(session_status,''),COALESCE(vm_id,''),COALESCE(vm_state,''),
	COALESCE(auth_state,'unverified'),COALESCE(last_error,''),
	COALESCE(last_keepalive_at,0),COALESCE(last_attempt_at,0),COALESCE(last_attempt_ok,0),
	COALESCE(last_wake_at,0),COALESCE(last_wake_ok,0),
	COALESCE(auto_wake,1),COALESCE(enabled,1),authorized_at,updated_at`

func scanMuseAccount(sc rowScanner) (*MuseAccount, error) {
	var a MuseAccount
	var attemptOK, wakeOK, autoWake, enabled int
	err := sc.Scan(&a.ID, &a.PeerID, &a.Label, &a.CookiesEnc, &a.CookiesExp,
		&a.SessionStatus, &a.VMID, &a.VMState, &a.AuthState, &a.LastError,
		&a.LastKeepaliveAt, &a.LastAttemptAt, &attemptOK,
		&a.LastWakeAt, &wakeOK, &autoWake, &enabled, &a.AuthorizedAt, &a.UpdatedAt)
	if err != nil {
		return nil, err
	}
	a.LastAttemptOK = attemptOK != 0
	a.LastWakeOK = wakeOK != 0
	a.AutoWake = autoWake != 0
	a.Enabled = enabled != 0
	return &a, nil
}

func (s *sqliteStore) CreateMuseAccount(a *MuseAccount) error {
	_, err := s.db.Exec(`INSERT INTO muse_accounts(
		id,peer_id,label,cookies_enc,cookies_exp,session_status,vm_id,vm_state,
		auth_state,last_error,last_keepalive_at,last_attempt_at,last_attempt_ok,
		last_wake_at,last_wake_ok,auto_wake,enabled,authorized_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.PeerID, a.Label, a.CookiesEnc, a.CookiesExp, a.SessionStatus, a.VMID, a.VMState,
		a.AuthState, a.LastError, a.LastKeepaliveAt, a.LastAttemptAt, boolToInt(a.LastAttemptOK),
		a.LastWakeAt, boolToInt(a.LastWakeOK), boolToInt(a.AutoWake), boolToInt(a.Enabled),
		a.AuthorizedAt, a.UpdatedAt)
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
		peer_id=?,label=?,cookies_enc=?,cookies_exp=?,session_status=?,vm_id=?,vm_state=?,
		auth_state=?,last_error=?,last_keepalive_at=?,last_attempt_at=?,last_attempt_ok=?,
		last_wake_at=?,last_wake_ok=?,auto_wake=?,enabled=?,authorized_at=?,updated_at=?
		WHERE id=?`,
		a.PeerID, a.Label, a.CookiesEnc, a.CookiesExp, a.SessionStatus, a.VMID, a.VMState,
		a.AuthState, a.LastError, a.LastKeepaliveAt, a.LastAttemptAt, boolToInt(a.LastAttemptOK),
		a.LastWakeAt, boolToInt(a.LastWakeOK), boolToInt(a.AutoWake), boolToInt(a.Enabled),
		a.AuthorizedAt, a.UpdatedAt, a.ID)
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
