package muse

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	keyFileName = "muse-cookie.key"
	keyBytes    = 32
)

// loadOrCreateKey loads data_dir/muse-cookie.key, creating it on first use.
// An existing file is never overwritten: a damaged/rotated key would make
// every stored cookie undecryptable, so the operator must fix or remove it
// deliberately. created reports a first-run key (callers warn when accounts
// already exist).
func loadOrCreateKey(dataDir string) (key []byte, created bool, err error) {
	if strings.TrimSpace(dataDir) == "" {
		return nil, false, errors.New("muse: data_dir required for cookie key")
	}
	path := filepath.Join(dataDir, keyFileName)
	raw, err := os.ReadFile(path)
	if err == nil {
		key, err := decodeKey(string(raw))
		if err != nil {
			return nil, false, fmt.Errorf("muse: invalid %s: %w", path, err)
		}
		// Repair a key file copied with loose permissions.
		_ = os.Chmod(path, 0o600)
		return key, false, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, false, fmt.Errorf("muse: read %s: %w", path, err)
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("muse: create data dir: %w", err)
	}
	key = make([]byte, keyBytes)
	if _, err := rand.Read(key); err != nil {
		return nil, false, fmt.Errorf("muse: generate cookie key: %w", err)
	}
	// O_EXCL: a concurrent creator wins and we read its key instead.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		if errors.Is(err, os.ErrExist) {
			raw, rerr := os.ReadFile(path)
			if rerr != nil {
				return nil, false, fmt.Errorf("muse: read %s: %w", path, rerr)
			}
			k, derr := decodeKey(string(raw))
			if derr != nil {
				return nil, false, fmt.Errorf("muse: invalid %s: %w", path, derr)
			}
			return k, false, nil
		}
		return nil, false, fmt.Errorf("muse: create %s: %w", path, err)
	}
	if _, err := f.WriteString(encodeKey(key) + "\n"); err != nil {
		_ = f.Close()
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("muse: write %s: %w", path, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(path)
		return nil, false, fmt.Errorf("muse: write %s: %w", path, err)
	}
	return key, true, nil
}

func encodeKey(key []byte) string { return base64.RawURLEncoding.EncodeToString(key) }

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

// encryptCookies returns base64(nonce||ciphertext) for the JSON cookie jar.
func encryptCookies(key, plaintext []byte) (string, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := gcm.Seal(nonce, nonce, plaintext, nil)
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}

func decryptCookies(key []byte, blob string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(blob))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(raw) < gcm.NonceSize() {
		return nil, errors.New("ciphertext too short")
	}
	nonce, ciphertext := raw[:gcm.NonceSize()], raw[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}
