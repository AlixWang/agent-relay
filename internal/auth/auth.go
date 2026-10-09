// Package auth owns per-identity tokens and invite codes (DESIGN §4.2, §7).
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// roomRe matches room routing aliases (§6.8): grp_<slug>, lowercase so a room
// can never be confused with a peer id by case.
var roomRe = regexp.MustCompile(`^grp_[a-z0-9][a-z0-9_-]{0,58}$`)

// Reserved pseudo-identities (§6.8, §9.5). They are senders/recipients on the
// message path but never authenticate and never get a peer row:
//
//	operator — the human at the console
//	system   — the server itself (smoke tests, notices)
//	grp_*    — room routing aliases
const (
	OperatorID = "operator"
	SystemID   = "system"
)

// ValidID reports whether s is a legal peer identity or message id.
func ValidID(s string) bool { return idRe.MatchString(s) }

// ValidRoomID reports whether s is a legal room alias.
func ValidRoomID(s string) bool { return roomRe.MatchString(s) }

// ReservedID reports whether s belongs to the relay itself and therefore must
// not be claimable by a registering assistant. Without this an invited
// assistant can take the id "operator" (or a room alias) and speak as the
// human — the v0.14.0 conversations design shipped exactly that hole.
func ReservedID(s string) bool {
	return s == OperatorID || s == SystemID || strings.HasPrefix(s, "grp_")
}

// ValidTarget allows "*" (broadcast), the relay's own identities, a room alias
// (grp_<slug> — not the bare prefix), or a normal peer identity.
func ValidTarget(s string) bool {
	switch {
	case s == "*", s == OperatorID, s == SystemID:
		return true
	case strings.HasPrefix(s, "grp_"):
		return ValidRoomID(s)
	default:
		return ValidID(s)
	}
}

// NewMessageID returns a fresh id for server-originated messages (console
// sends, §9.5). Client sends supply their own ids for idempotency; the console
// has no retry identity to preserve, so the server mints one.
func NewMessageID(prefix string) string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	return prefix + "-" + hex.EncodeToString(b[:])
}

// HashToken returns the SHA-256 hex of a plaintext token.
func HashToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// GenerateToken creates a 32-byte base64url token (DESIGN §7.1).
func GenerateToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// GenerateInviteCode creates a one-time invite code, e.g. "inv_7f3...".
func GenerateInviteCode() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return "inv_" + base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// HashInvite returns the SHA-256 hex of an invite code.
func HashInvite(code string) string { return HashToken(code) }

// Service bundles token/invite operations over the store.
type Service struct {
	store.Store
	InviteTTLSecs int64
}

// New returns an auth Service. inviteTTL <= 0 defaults to 1 hour.
func New(st store.Store, inviteTTL int64) *Service {
	if inviteTTL <= 0 {
		inviteTTL = 3600
	}
	return &Service{Store: st, InviteTTLSecs: inviteTTL}
}

// Authenticate resolves a bearer token to its peer. Returns (nil, nil) when
// the token is unknown, revoked, or belongs to a suspended peer.
func (s *Service) Authenticate(plaintext string) (*store.Peer, *store.TokenRow, error) {
	if plaintext == "" {
		return nil, nil, nil
	}
	p, tok, err := s.FindPeerByTokenHash(HashToken(plaintext))
	if err != nil || p != nil || tok == nil {
		return p, tok, err
	}
	// Token is valid but the peer row is gone (pruned by retention while
	// the token survived, DESIGN §10.2). Recreate as pending so the
	// returning assistant can authenticate, heartbeat, and re-verify.
	// NOTE: suspended peers keep their row, so an admin suspension is
	// never resurrected by this path.
	now := time.Now().Unix()
	recreated := &store.Peer{ID: tok.PeerID, Status: "pending", CreatedAt: now}
	if err := s.CreatePeer(recreated); err != nil {
		return nil, nil, err
	}
	_ = s.AppendAudit(tok.PeerID, "peer.reregistered",
		"peer row pruned earlier, recreated on token use", now)
	return recreated, tok, nil
}

// CreateInvite generates a one-time code. The plaintext is returned once;
// only the hash is stored. intendedID may be "" (assistant chooses).
func (s *Service) CreateInvite(intendedID, agentType, createdBy string, now int64) (string, *store.Invite, error) {
	if intendedID != "" && ReservedID(intendedID) {
		return "", nil, &RegisterError{Code: "bad_id", Detail: "id " + intendedID + " is reserved for the relay"}
	}
	code, err := GenerateInviteCode()
	if err != nil {
		return "", nil, err
	}
	if agentType == "" {
		agentType = "generic"
	}
	inv := &store.Invite{
		CodeHash:   HashInvite(code),
		IntendedID: intendedID,
		AgentType:  agentType,
		CreatedBy:  createdBy,
		CreatedAt:  now,
		ExpiresAt:  now + s.InviteTTLSecs,
	}
	if err := s.Store.CreateInvite(inv); err != nil {
		return "", nil, err
	}
	return code, inv, nil
}

// RegisterError is a machine-readable registration failure.
type RegisterError struct {
	Code   string // invalid_code | expired_code | used_code | bad_id | id_taken | id_mismatch
	Detail string
}

func (e *RegisterError) Error() string { return e.Code + ": " + e.Detail }

// Register redeems an invite code for a new peer + personal token.
// The token plaintext is returned once and never recoverable afterwards.
func (s *Service) Register(code, id, agentType string, protocolVer int, capabilities string, now int64) (*store.Peer, string, error) {
	inv, err := s.Store.GetInvite(HashInvite(code))
	if err != nil {
		return nil, "", err
	}
	if inv == nil {
		return nil, "", &RegisterError{Code: "invalid_code", Detail: "unknown invite code"}
	}
	if inv.UsedAt != 0 {
		return nil, "", &RegisterError{Code: "used_code", Detail: "invite already redeemed by " + inv.UsedBy}
	}
	if now > inv.ExpiresAt {
		return nil, "", &RegisterError{Code: "expired_code", Detail: "invite expired"}
	}
	if !ValidID(id) {
		return nil, "", &RegisterError{Code: "bad_id", Detail: "id must match ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$"}
	}
	if ReservedID(id) {
		return nil, "", &RegisterError{Code: "bad_id", Detail: "id " + id + " is reserved for the relay"}
	}
	if inv.IntendedID != "" && inv.IntendedID != id {
		return nil, "", &RegisterError{Code: "id_mismatch", Detail: "invite is bound to identity " + inv.IntendedID}
	}
	if existing, err := s.Store.GetPeer(id); err != nil {
		return nil, "", err
	} else if existing != nil {
		return nil, "", &RegisterError{Code: "id_taken", Detail: "identity already registered"}
	}
	if agentType == "" {
		agentType = inv.AgentType
	}
	if capabilities == "" {
		capabilities = "{}"
	}
	peer := &store.Peer{
		ID:           id,
		AgentType:    agentType,
		ProtocolVer:  protocolVer,
		Capabilities: capabilities,
		Status:       "pending",
		CreatedAt:    now,
	}
	if err := s.Store.CreatePeer(peer); err != nil {
		return nil, "", err
	}
	plaintext, err := GenerateToken()
	if err != nil {
		return nil, "", err
	}
	if _, err := s.Store.CreateToken(id, HashToken(plaintext), "initial", now); err != nil {
		return nil, "", err
	}
	if err := s.Store.MarkInviteUsed(inv.CodeHash, id, now); err != nil {
		return nil, "", err
	}
	_ = s.Store.AppendAudit(id, "peer.registered",
		fmt.Sprintf("agent_type=%s protocol=%d", agentType, protocolVer), now)
	_ = s.Store.UpdatePeerCapabilities(id, protocolVer, capabilities)
	return peer, plaintext, nil
}

// Rotate issues a fresh token for peerID; the old tokens must be revoked
// separately by the caller so rotation can be staged.
func (s *Service) Rotate(peerID, label string, now int64) (string, error) {
	plaintext, err := GenerateToken()
	if err != nil {
		return "", err
	}
	if _, err := s.Store.CreateToken(peerID, HashToken(plaintext), label, now); err != nil {
		return "", err
	}
	_ = s.Store.AppendAudit("admin", "token.rotated", "peer="+peerID, now)
	return plaintext, nil
}

// Touch records token usage for the UI (last_used_at, last_ip).
func (s *Service) Touch(tokID int64, ip string) {
	_ = s.Store.TouchToken(tokID, time.Now().Unix(), ip)
}
