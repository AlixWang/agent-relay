package mcpgateway

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// KeyPrefix is the recognizable external-client key prefix (design v5 §4.3).
const KeyPrefix = "rmk_"

// GenerateKey mints rmk_<32 random bytes base64url>. The plaintext is returned
// exactly once; only HashKey(plaintext) is stored.
func GenerateKey() (plaintext, prefix, hash string, err error) {
	var b [32]byte
	if _, err = rand.Read(b[:]); err != nil {
		return "", "", "", fmt.Errorf("rand: %w", err)
	}
	plaintext = KeyPrefix + base64.RawURLEncoding.EncodeToString(b[:])
	prefix = plaintext
	if len(prefix) > 12 {
		prefix = prefix[:12]
	}
	return plaintext, prefix, HashKey(plaintext), nil
}

// HashKey is SHA-256 hex, no pepper (design v5 §4.3).
func HashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// KeyExpired reports the expiry rule (0 means no expiry).
func KeyExpired(expiresAt, now int64) bool {
	return expiresAt != 0 && expiresAt <= now
}

// keyTTLDays picks the documented default TTL: read-only 90d, any write 30d,
// a single specific tool 7d. Explicit per-request overrides are handled by the
// admin handler.
func keyTTLDays(scopes []string, cfg KeysConfig) int {
	cfg = cfg.normalized()
	writes, specificWrites := 0, 0
	for _, raw := range scopes {
		sc, err := ParseScope(raw)
		if err != nil {
			continue
		}
		if sc.Class == "write" {
			writes++
			if sc.Tool != "*" {
				specificWrites++
			}
		}
	}
	switch {
	case writes == 1 && specificWrites == 1:
		return cfg.PerToolKeyTTLDays
	case writes > 0:
		return cfg.WriteKeyTTLDays
	default:
		return cfg.ReadKeyTTLDays
	}
}

// NewID returns a short opaque id with a stable prefix, used for key and
// approval rows. Not a credential.
func NewID(prefix string) (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("rand: %w", err)
	}
	return prefix + hex.EncodeToString(b[:]), nil
}
