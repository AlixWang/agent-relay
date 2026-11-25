package mcpgateway

import (
	"crypto/subtle"
	"fmt"
	"strings"
	"time"

	"github.com/AlixWang/agent-relay/internal/auth"
	"github.com/AlixWang/agent-relay/internal/store"
)

// Scope is one mcp:<service>:<read|write>:<tool|*> grant (design v5 §4.2).
type Scope struct {
	Service string
	Class   string // read | write
	Tool    string // short tool name or *
}

func (s Scope) String() string {
	return "mcp:" + s.Service + ":" + s.Class + ":" + s.Tool
}

// ParseScope validates the shape only; registry existence is checked by
// ValidateScopes at issuance time.
func ParseScope(raw string) (Scope, error) {
	raw = strings.TrimSpace(raw)
	parts := strings.Split(raw, ":")
	if len(parts) != 4 || parts[0] != "mcp" {
		return Scope{}, fmt.Errorf("scope must be mcp:<service>:<read|write>:<tool|*>")
	}
	s := Scope{Service: parts[1], Class: parts[2], Tool: parts[3]}
	if !ValidServiceID(s.Service) {
		return Scope{}, fmt.Errorf("scope %q: invalid service id", raw)
	}
	if s.Class != "read" && s.Class != "write" {
		return Scope{}, fmt.Errorf("scope %q: class must be read or write", raw)
	}
	if s.Tool != "*" && !ValidToolName(s.Tool) {
		return Scope{}, fmt.Errorf("scope %q: invalid tool name", raw)
	}
	return s, nil
}

// ValidateScopes checks issuance input against the registry: every scope has
// a real service/tool and the read/write class matches the tool's risk.
// Wildcards are per-service only; mcp:*:... is rejected. The result is
// de-duplicated but order-preserving.
func ValidateScopes(reg *Registry, raw []string) ([]string, error) {
	if len(raw) == 0 {
		return nil, fmt.Errorf("at least one scope is required")
	}
	if len(raw) > 64 {
		return nil, fmt.Errorf("too many scopes (max 64)")
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		sc, err := ParseScope(item)
		if err != nil {
			return nil, err
		}
		if _, ok := reg.Get(sc.Service); !ok {
			return nil, fmt.Errorf("scope %q: unknown service", sc.String())
		}
		if sc.Tool != "*" {
			t, ok := reg.Tool(sc.Service, sc.Tool)
			if !ok {
				return nil, fmt.Errorf("scope %q: unknown tool", sc.String())
			}
			if RiskClass(t) != sc.Class {
				return nil, fmt.Errorf("scope %q: %s is a %s tool", sc.String(), sc.Tool, RiskClass(t))
			}
		}
		str := sc.String()
		if !seen[str] {
			seen[str] = true
			out = append(out, str)
		}
	}
	return out, nil
}

// ScopeMatch reports whether the granted scopes allow this tool. "*" matches
// every tool in the same service and class only; a read scope never grants a
// write and vice versa.
func ScopeMatch(scopes []string, serviceID, class, tool string) bool {
	for _, raw := range scopes {
		sc, err := ParseScope(raw)
		if err != nil {
			continue
		}
		if sc.Service != serviceID || sc.Class != class {
			continue
		}
		if sc.Tool == "*" || sc.Tool == tool {
			return true
		}
	}
	return false
}

// HasAnyScope reports whether at least one syntactically valid scope exists.
func HasAnyScope(scopes []string) bool {
	for _, raw := range scopes {
		if _, err := ParseScope(raw); err == nil {
			return true
		}
	}
	return false
}

// Authenticator resolves the two auth paths. Key failures are deliberately
// indistinguishable from peer failures (401 without detail).
type Authenticator struct {
	st          store.Store
	relay       *auth.Service
	minProtocol int
	now         func() time.Time
}

func NewAuthenticator(st store.Store, relay *auth.Service, minProtocol int) *Authenticator {
	return &Authenticator{st: st, relay: relay, minProtocol: minProtocol, now: time.Now}
}

func (a *Authenticator) SetClock(now func() time.Time) {
	if now != nil {
		a.now = now
	}
}

// Authenticate resolves a bearer token. rmk_ goes to the key path; anything
// else is a relay token (design v5 §4.1).
func (a *Authenticator) Authenticate(token, ip string) (Actor, *Error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	if strings.HasPrefix(token, KeyPrefix) {
		return a.authenticateKey(token, ip)
	}
	return a.authenticatePeer(token, ip)
}

func (a *Authenticator) authenticateKey(token, ip string) (Actor, *Error) {
	hash := HashKey(token)
	row, err := a.st.GetMCPKeyByHash(hash)
	if err != nil {
		return Actor{}, NewError(CodeInternal, "authentication failed", 500)
	}
	if row == nil || row.PrincipalType != "key" {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	if subtle.ConstantTimeCompare([]byte(row.KeyHash), []byte(hash)) != 1 {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	now := a.now().Unix()
	if row.Status != "active" || (row.RevokedAt != 0 && row.RevokedAt <= now) || KeyExpired(row.ExpiresAt, now) {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	if !HasAnyScope(row.Scopes) {
		return Actor{}, NewError(CodeInsufficientScope, "this key has no scopes", 403)
	}
	// Best effort: a usage-counter failure must not block calls.
	_ = a.st.UpdateMCPKeyUsage(row.ID, now, ip)
	return Actor{
		Kind: ActorKey, ID: row.ID, Name: row.Name, KeyID: row.ID,
		IP: ip, AuthKind: AuthMCPKey, Scopes: row.Scopes,
	}, nil
}

func (a *Authenticator) authenticatePeer(token, ip string) (Actor, *Error) {
	if a.relay == nil {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	peer, tok, err := a.relay.Authenticate(token)
	if err != nil {
		return Actor{}, NewError(CodeInternal, "authentication failed", 500)
	}
	if peer == nil {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	// MCP requires an active, verified assistant; pending/suspended peers
	// keep their message-path tokens but cannot reach upstream credentials.
	if peer.Status != "active" {
		return Actor{}, NewError(CodeUnauthorized, "unauthorized", 401)
	}
	if peer.ProtocolVer < a.minProtocol {
		return Actor{}, NewError(CodeUnauthorized, "client protocol too old; re-run onboarding", 403)
	}
	a.relay.Touch(tok.ID, ip)
	row, err := a.st.GetMCPPeerGrant(peer.ID)
	if err != nil {
		return Actor{}, NewError(CodeInternal, "authorization failed", 500)
	}
	if row == nil || row.Status != "active" || !HasAnyScope(row.Scopes) {
		return Actor{}, NewError(CodeInsufficientScope, "this peer has no MCP authorization", 403)
	}
	now := a.now().Unix()
	if row.ExpiresAt != 0 && row.ExpiresAt <= now {
		return Actor{}, NewError(CodeInsufficientScope, "this peer authorization has expired", 403)
	}
	_ = a.st.UpdateMCPKeyUsage(row.ID, now, ip)
	return Actor{
		Kind: ActorPeer, ID: peer.ID, Name: peer.ID, PeerID: peer.ID,
		IP: ip, AuthKind: AuthRelayToken, Scopes: row.Scopes,
	}, nil
}
