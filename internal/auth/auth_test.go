package auth

import (
	"testing"
	"time"

	"github.com/AlixWang/agent-relay/internal/store"
)

func openTestStore(t *testing.T) store.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestRegisterHappyPath(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 3600)
	now := time.Now().Unix()
	code, _, err := svc.CreateInvite("muse-c", "muse", "admin", now)
	if err != nil {
		t.Fatalf("invite: %v", err)
	}
	peer, plaintext, err := svc.Register(code, "muse-c", "muse", 1, `{"shell":true}`, now)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if peer.Status != "pending" || plaintext == "" {
		t.Fatalf("bad register result: %+v", peer)
	}
	// Token authenticates.
	got, _, err := svc.Authenticate(plaintext)
	if err != nil || got == nil || got.ID != "muse-c" {
		t.Fatalf("authenticate: %v %+v", err, got)
	}
	// Invite is single-use.
	if _, _, err := svc.Register(code, "muse-d", "muse", 1, "{}", now); err == nil {
		t.Fatal("expected used_code rejection")
	}
}

func TestRegisterBoundID(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 3600)
	now := time.Now().Unix()
	code, _, _ := svc.CreateInvite("muse-c", "muse", "admin", now)
	if _, _, err := svc.Register(code, "muse-x", "muse", 1, "{}", now); err == nil {
		t.Fatal("expected id_mismatch")
	} else if re, ok := err.(*RegisterError); !ok || re.Code != "id_mismatch" {
		t.Fatalf("wrong error: %v", err)
	}
}

func TestRegisterExpired(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 1) // 1s TTL
	now := time.Now().Unix()
	code, _, _ := svc.CreateInvite("", "generic", "admin", now)
	if _, _, err := svc.Register(code, "g1", "generic", 1, "{}", now+5); err == nil {
		t.Fatal("expected expired_code")
	}
}

func TestPrunedPeerReregistersOnTokenUse(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 3600)
	now := time.Now().Unix()
	code, _, _ := svc.CreateInvite("", "muse", "admin", now)
	_, plaintext, err := svc.Register(code, "returner", "muse", 1, "{}", now)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	// Retention prunes the peer row; the token row survives (no FK).
	// Touch the peer stale first: PrunePeers only removes last_seen>0 rows.
	if err := st.TouchPeer("returner", now-8*86400); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PrunePeers(now - 7*86400); err != nil {
		t.Fatalf("prune with live token must not fail: %v", err)
	}
	if p, _ := st.GetPeer("returner"); p != nil {
		t.Fatal("peer should be pruned")
	}
	// Old token still authenticates and recreates the peer as pending.
	got, _, err := svc.Authenticate(plaintext)
	if err != nil || got == nil || got.ID != "returner" {
		t.Fatalf("authenticate: %v %+v", err, got)
	}
	if got.Status != "pending" {
		t.Fatalf("recreated status: %s", got.Status)
	}
	entries, _ := st.ListAudit("returner", "peer.reregistered", 0, 10)
	if len(entries) != 1 {
		t.Fatal("missing peer.reregistered audit")
	}
}

func TestRevokeIsImmediate(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 3600)
	now := time.Now().Unix()
	code, _, _ := svc.CreateInvite("", "muse", "admin", now)
	_, plaintext, err := svc.Register(code, "muse-a", "muse", 1, "{}", now)
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	toks, _ := st.ListTokensByPeer("muse-a")
	if len(toks) != 1 {
		t.Fatalf("tokens: %d", len(toks))
	}
	if err := st.RevokeToken(toks[0].ID, now); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	got, _, _ := svc.Authenticate(plaintext)
	if got != nil {
		t.Fatal("revoked token still authenticates")
	}
}
