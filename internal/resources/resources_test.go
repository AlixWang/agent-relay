package resources

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/AlixWang/agent-relay/internal/store"
)

func newTestService(t *testing.T) (*Service, store.Store, string) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	dir := t.TempDir()
	svc, err := New(st, dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"alice", "bob"} {
		if err := st.CreatePeer(&store.Peer{ID: id, AgentType: "muse", Status: "active", CreatedAt: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return svc, st, dir
}

func TestKeyFileCreatedOnceWith0600(t *testing.T) {
	_, st, dir := newTestService(t)
	path := filepath.Join(dir, keyFileName)
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("key perms %v", fi.Mode().Perm())
	}
	before, _ := os.ReadFile(path)
	if _, err := New(st, dir); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if string(before) != string(after) {
		t.Fatal("existing key overwritten")
	}
}

func TestSecretStoredEncryptedAndRevealedOnlyWhenGranted(t *testing.T) {
	svc, st, _ := newTestService(t)
	if _, err := svc.PutSecret("OPENAI_API_KEY", "gpt", "sk-live-123"); err != nil {
		t.Fatal(err)
	}
	r, _ := st.GetResource(KindSecret, "OPENAI_API_KEY")
	if strings.Contains(string(r.Content), "sk-live-123") || strings.Contains(r.ContentHash, "sk-live") {
		t.Fatal("plaintext stored")
	}
	if !strings.HasPrefix(r.ContentHash, "hmac:") {
		t.Fatalf("secret hash should be keyed: %s", r.ContentHash)
	}
	if _, err := svc.RevealSecret("alice", "OPENAI_API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("ungranted reveal: %v", err)
	}
	if err := svc.Grant(KindSecret, "OPENAI_API_KEY", "alice"); err != nil {
		t.Fatal(err)
	}
	v, err := svc.RevealSecret("alice", "OPENAI_API_KEY")
	if err != nil || v != "sk-live-123" {
		t.Fatalf("reveal: %q %v", v, err)
	}
	if _, err := svc.RevealSecret("bob", "OPENAI_API_KEY"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bob must not see alice's grant: %v", err)
	}
	if _, err := svc.RevealSecret("alice", "NOPE"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

func TestWildcardGrantCoversFuturePeers(t *testing.T) {
	svc, st, _ := newTestService(t)
	_, _ = svc.PutSecret("K", "", "v")
	if err := svc.Grant(KindSecret, "K", AllPeers); err != nil {
		t.Fatal(err)
	}
	_ = st.CreatePeer(&store.Peer{ID: "carol", AgentType: "claw", Status: "active", CreatedAt: 2})
	if v, err := svc.RevealSecret("carol", "K"); err != nil || v != "v" {
		t.Fatalf("wildcard: %q %v", v, err)
	}
}

func TestGrantValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	if err := svc.Grant(KindSecret, "K", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("grant missing resource: %v", err)
	}
	_, _ = svc.PutSecret("K", "", "v")
	if err := svc.Grant(KindSecret, "K", "ghost"); !errors.Is(err, ErrUnknownPeer) {
		t.Fatalf("grant unknown peer: %v", err)
	}
	if err := svc.Grant(KindSecret, "K", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Grant(KindSecret, "K", "alice"); err != nil {
		t.Fatalf("grant must be idempotent: %v", err)
	}
	if err := svc.Revoke(KindSecret, "K", "alice"); err != nil {
		t.Fatal(err)
	}
	if err := svc.Revoke(KindSecret, "K", "alice"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("double revoke: %v", err)
	}
}

func TestPutSecretValidation(t *testing.T) {
	svc, _, _ := newTestService(t)
	for _, name := range []string{"", "../x", "a b", "${secret:x}", strings.Repeat("a", 65)} {
		if _, err := svc.PutSecret(name, "", "v"); !errors.Is(err, ErrInvalid) {
			t.Fatalf("name %q accepted: %v", name, err)
		}
	}
	if _, err := svc.PutSecret("K", "", ""); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty value on create: %v", err)
	}
	if _, err := svc.PutSecret("K", "", strings.Repeat("x", maxSecretBytes+1)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("oversized: %v", err)
	}
}

func TestDescriptionOnlyUpdateKeepsValueAndDigest(t *testing.T) {
	svc, _, _ := newTestService(t)
	_, _ = svc.PutSecret("K", "old", "v1")
	_ = svc.Grant(KindSecret, "K", "alice")
	m1, _ := svc.Manifest("alice")
	if _, err := svc.PutSecret("K", "new desc", ""); err != nil {
		t.Fatal(err)
	}
	m2, _ := svc.Manifest("alice")
	if m1.ManifestRev != m2.ManifestRev {
		t.Fatal("description-only edit moved the digest")
	}
	if v, _ := svc.RevealSecret("alice", "K"); v != "v1" {
		t.Fatalf("value lost: %q", v)
	}
	revs, _ := svc.Revisions(KindSecret, "K")
	if len(revs) != 2 || revs[0].Description != "new desc" {
		t.Fatalf("history: %+v", revs)
	}
}

func TestManifestDigest(t *testing.T) {
	svc, _, _ := newTestService(t)
	m, _ := svc.Manifest("alice")
	if m.ManifestRev != "" || len(m.Resources) != 0 {
		t.Fatalf("empty manifest: %+v", m)
	}
	_, _ = svc.PutSecret("A", "", "1")
	_, _ = svc.PutSecret("B", "", "2")
	_ = svc.Grant(KindSecret, "A", "alice")
	_ = svc.Grant(KindSecret, "B", AllPeers)
	m, _ = svc.Manifest("alice")
	if len(m.Resources) != 2 || m.ManifestRev == "" {
		t.Fatalf("manifest: %+v", m)
	}
	if m.Resources[0].FetchURL != "/resources/secrets/A" || m.Resources[0].Content != nil {
		t.Fatalf("secret entry must carry fetch_url only: %+v", m.Resources[0])
	}
	// Same grants via * vs direct → same digest.
	_ = svc.Grant(KindSecret, "A", "bob")
	mb, _ := svc.Manifest("bob")
	if mb.ManifestRev != m.ManifestRev {
		t.Fatal("direct and wildcard grants should digest the same")
	}
	// Re-saving the identical value keeps the digest (the HMAC is deterministic).
	_, _ = svc.PutSecret("A", "", "1")
	m2, _ := svc.Manifest("alice")
	if m2.ManifestRev != m.ManifestRev {
		t.Fatal("identical content moved the digest")
	}
	// Rotation moves it.
	_, _ = svc.PutSecret("A", "", "rotated")
	m3, _ := svc.Manifest("alice")
	if m3.ManifestRev == m.ManifestRev {
		t.Fatal("rotation did not move the digest")
	}
	// Revocation moves it.
	_ = svc.Revoke(KindSecret, "A", "alice")
	m4, _ := svc.Manifest("alice")
	if m4.ManifestRev == m3.ManifestRev || len(m4.Resources) != 1 {
		t.Fatalf("revoke: %+v", m4)
	}
	// Deleting drops it from every manifest.
	_ = svc.Delete(KindSecret, "B")
	m5, _ := svc.Manifest("alice")
	if m5.ManifestRev != "" || len(m5.Resources) != 0 {
		t.Fatalf("delete: %+v", m5)
	}
}

func TestNeedsUpdate(t *testing.T) {
	cases := []struct {
		applied, current string
		want             bool
	}{
		{"", "", false},         // never had any, still none
		{"", "abc", true},       // first grant
		{"abc", "abc", false},   // in sync
		{"abc", "def", true},    // changed
		{"abc", "", true},       // everything revoked
		{NoneMarker, "", false}, // confirmed empty
		{NoneMarker, "abc", true},
	}
	for _, c := range cases {
		if got := NeedsUpdate(c.applied, c.current); got != c.want {
			t.Fatalf("NeedsUpdate(%q,%q)=%v", c.applied, c.current, got)
		}
	}
}

func TestDeletePeerDropsGrants(t *testing.T) {
	svc, st, _ := newTestService(t)
	_, _ = svc.PutSecret("K", "", "v")
	_ = svc.Grant(KindSecret, "K", "alice")
	if _, err := st.DeletePeer("alice", 10); err != nil {
		t.Fatal(err)
	}
	_ = st.CreatePeer(&store.Peer{ID: "alice", AgentType: "muse", Status: "active", CreatedAt: 11})
	if _, err := svc.RevealSecret("alice", "K"); !errors.Is(err, ErrNotFound) {
		t.Fatal("re-registered peer inherited a deleted peer's grant")
	}
}

func TestKindsAllEnabled(t *testing.T) {
	for _, k := range []string{KindSecret, KindMCP, KindSkill, KindPrompt} {
		if err := CheckKind(k); err != nil {
			t.Fatalf("%s should be enabled: %v", k, err)
		}
	}
	if err := CheckKind("bogus"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bogus: %v", err)
	}
}

// The manifest JSON shape is a contract with the assistant prompt (ADR 0004).
// Renaming or dropping a field breaks every assistant's sync instructions.
func TestManifestSchemaStability(t *testing.T) {
	fields := func(v any) []string {
		var out []string
		ty := reflect.TypeOf(v)
		for i := 0; i < ty.NumField(); i++ {
			out = append(out, strings.Split(ty.Field(i).Tag.Get("json"), ",")[0])
		}
		return out
	}
	if got, want := fields(Manifest{}), []string{"manifest_rev", "resources"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Manifest fields %v, want %v", got, want)
	}
	want := []string{"kind", "name", "description", "revision_id", "hash", "updated_at", "fetch_url", "content", "requires_secrets"}
	if got := fields(ManifestEntry{}); !reflect.DeepEqual(got, want) {
		t.Fatalf("ManifestEntry fields %v, want %v", got, want)
	}
	b, _ := json.Marshal(&Manifest{Resources: []*ManifestEntry{}})
	if string(b) != `{"manifest_rev":"","resources":[]}` {
		t.Fatalf("empty manifest JSON: %s", b)
	}
}
