package resources

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyFileName = "secrets.key"
	keyBytes    = 32
)

// loadOrCreateKey loads data_dir/secrets.key, creating it (0600) on first use.
// Same contract as the Muse cookie key: an existing file is never overwritten,
// because a replaced key makes every stored secret undecryptable.
func loadOrCreateKey(dataDir string) ([]byte, error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, errors.New("resources: data_dir required for secrets key")
	}
	path := filepath.Join(dataDir, keyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := decodeKey(string(raw))
		if err != nil {
			return nil, fmt.Errorf("resources: invalid %s: %w", path, err)
		}
		_ = os.Chmod(path, 0o600) // repair a key file copied with loose permissions
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("resources: read %s: %w", path, err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, fmt.Errorf("resources: create data dir: %w", err)
	}
	key := make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("resources: generate key: %w", err)
	}
	// O_EXCL: a concurrent creator wins and we read its key instead.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, fmt.Errorf("resources: read %s: %w", path, rerr)
			}
			return decodeKey(string(raw))
		}
		return nil, fmt.Errorf("resources: create %s: %w", path, err)
	}
	if _, err := f.WriteString(base64.RawURLEncoding.EncodeToString(key) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, fmt.Errorf("resources: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, fmt.Errorf("resources: write %s: %w", path, err)
	}
	return key, nil
}

func decodeKey(s string) ([]byte, error) {
	key, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return nil, err
	}
	if len(key) != keyBytes {
		return nil, fmt.Errorf("key must be %d bytes, got %d", keyBytes, len(key))
	}
	return key, nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// seal returns nonce||ciphertext.
func seal(key, plaintext []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

func open(key, sealed []byte) ([]byte, error) {
	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	if len(sealed) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	return gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
}

// secretHash fingerprints a secret for the manifest digest. A keyed HMAC, not
// a bare SHA-256: API keys can be short or patterned, and the digest is stored
// and sent around, so it must not allow offline guessing of the plaintext.
func secretHash(key []byte, value string) string {
	m := hmac.New(sha256.New, key)
	m.Write([]byte("agent-relay/resource-secret\x00"))
	m.Write([]byte(value))
	return "hmac:" + hex.EncodeToString(m.Sum(nil))
}

func contentHash(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}
