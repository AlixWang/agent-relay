package muse

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestKeyFileCreateAndLoad(t *testing.T) {
	dir := t.TempDir()
	key, created, err := loadOrCreateKey(dir)
	if err != nil || !created || len(key) != keyBytes {
		t.Fatalf("create: %d %v %v", len(key), created, err)
	}
	path := filepath.Join(dir, keyFileName)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("key mode: %o", info.Mode().Perm())
	}
	again, created, err := loadOrCreateKey(dir)
	if err != nil || created || string(again) != string(key) {
		t.Fatalf("reload: created=%v err=%v same=%v", created, err, string(again) == string(key))
	}
}

func TestInvalidKeyFileIsNeverOverwritten(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, keyFileName)
	if err := os.WriteFile(path, []byte("not-a-key\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := loadOrCreateKey(dir); err == nil {
		t.Fatal("invalid key must fail")
	}
	raw, _ := os.ReadFile(path)
	if strings.TrimSpace(string(raw)) != "not-a-key" {
		t.Fatalf("invalid key was overwritten: %q", raw)
	}
}

func TestEncryptDecryptRoundTrip(t *testing.T) {
	key := make([]byte, keyBytes)
	for i := range key {
		key[i] = byte(i)
	}
	blob, err := encryptCookies(key, []byte(`{"hatch_sess":"s"}`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := decryptCookies(key, blob)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"hatch_sess":"s"}` {
		t.Fatalf("plaintext: %q", raw)
	}
	if strings.Contains(blob, "hatch_sess") {
		t.Fatal("ciphertext contains plaintext")
	}

	wrong := make([]byte, keyBytes)
	if _, err := decryptCookies(wrong, blob); err == nil {
		t.Fatal("wrong key must fail")
	}
	if _, err := decryptCookies(key, blob[:len(blob)-4]); err == nil {
		t.Fatal("truncated ciphertext must fail")
	}
}

func TestDecodeKeyLength(t *testing.T) {
	if _, err := decodeKey("AAAA"); err == nil {
		t.Fatal("short key must fail")
	}
}
