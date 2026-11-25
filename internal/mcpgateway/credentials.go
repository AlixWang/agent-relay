package mcpgateway

import (
	"errors"
	"strings"
	"sync"
	"time"
)

// SecretResolver is the server-side secret store (internal/resources). The
// gateway resolves secret:<name> references through it; services never read
// files or environment variables.
type SecretResolver interface {
	RevealServerSecret(name string) (string, error)
}

// ErrCredentialMissing is deliberately generic: it must not reveal whether a
// secret exists, is misnamed, or the resolver is unavailable.
var ErrCredentialMissing = errors.New("credential not available")

// Credentials resolves validated secret: references. Values are cached in
// memory for 30s (design v5 §3.1) and are never written to disk or logged.
// The ADC bearer token has its own longer-lived cache in the AdSense service;
// this cache only avoids repeated decrypt/lookup of the raw secret.
type Credentials struct {
	resolver SecretResolver
	ttl      time.Duration

	mu    sync.Mutex
	now   func() time.Time
	cache map[string]cachedSecret
	gen   map[string]uint64
}

type cachedSecret struct {
	value   string
	expires time.Time
}

func NewCredentials(resolver SecretResolver) *Credentials {
	return &Credentials{
		resolver: resolver, ttl: 30 * time.Second, now: time.Now,
		cache: map[string]cachedSecret{},
		gen:   map[string]uint64{},
	}
}

func (c *Credentials) SetClock(now func() time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if now != nil {
		c.now = now
	}
}

// Resolve returns the plaintext for a secret:<name> reference.
func (c *Credentials) Resolve(ref string) (string, error) {
	name, err := ParseSecretRef(ref)
	if err != nil {
		return "", ErrCredentialMissing
	}
	c.mu.Lock()
	now := c.now()
	if ent, ok := c.cache[name]; ok && now.Before(ent.expires) {
		c.mu.Unlock()
		return ent.value, nil
	}
	gen := c.gen[name]
	c.mu.Unlock()
	if c.resolver == nil {
		return "", ErrCredentialMissing
	}
	v, err := c.resolver.RevealServerSecret(name)
	if err != nil || strings.TrimSpace(v) == "" {
		return "", ErrCredentialMissing
	}
	c.mu.Lock()
	// Only populate the cache if Invalidate did not bump the generation while
	// RevealServerSecret was in flight.
	if c.gen[name] == gen {
		c.cache[name] = cachedSecret{value: v, expires: c.now().Add(c.ttl)}
	}
	c.mu.Unlock()
	return v, nil
}

// Invalidate drops a cached secret (used on rotation/tests).
func (c *Credentials) Invalidate(ref string) {
	name, err := ParseSecretRef(ref)
	if err != nil {
		return
	}
	c.mu.Lock()
	delete(c.cache, name)
	c.gen[name]++
	c.mu.Unlock()
}

// ParseSecretRef enforces the only allowed credential reference form. A bare
// value (a real key pasted into config) is rejected rather than treated as a
// secret name.
func ParseSecretRef(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if !strings.HasPrefix(ref, "secret:") {
		return "", errors.New("credential_ref must be secret:<name>")
	}
	name := strings.TrimPrefix(ref, "secret:")
	if !validSecretName(name) {
		return "", errors.New("invalid secret name")
	}
	return name, nil
}

func validSecretName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_' || r == '.' || r == '-':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
