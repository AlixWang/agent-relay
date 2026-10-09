package auth

import (
	"testing"
	"time"
)

// The relay's own identities must not be claimable. v0.14.0 shipped without
// this: an invited assistant could register the id "user" (which its guard
// treated as the console operator) and then post into conversations it did not
// belong to while the members saw the message as the human's.
func TestReservedIdentitiesAreNotClaimable(t *testing.T) {
	st := openTestStore(t)
	svc := New(st, 3600)
	now := time.Now().Unix()

	for _, id := range []string{"operator", "system", "grp_ops", "grp_x"} {
		if !ReservedID(id) {
			t.Fatalf("%s should be reserved", id)
		}
		// Fails fast when minting the invite in the console...
		if _, _, err := svc.CreateInvite(id, "muse", "admin", now); err == nil {
			t.Fatalf("CreateInvite accepted reserved id %s", id)
		}
		// ...and again at the door if a code was minted unbound.
		code, _, err := svc.CreateInvite("", "muse", "admin", now)
		if err != nil {
			t.Fatalf("unbound invite: %v", err)
		}
		if _, _, err := svc.Register(code, id, "muse", 1, "{}", now); err == nil {
			t.Fatalf("Register accepted reserved id %s", id)
		}
	}

	// Lookalikes are fine: only exact ids and the grp_ prefix are reserved.
	for _, id := range []string{"alice", "grp", "operators", "system2", "grp-ops"} {
		if ReservedID(id) {
			t.Fatalf("%s must remain registerable", id)
		}
	}
}

func TestRoomIDValidation(t *testing.T) {
	valid := []string{"grp_ops", "grp_a", "grp_team-1_x"}
	invalid := []string{"grp_", "grp_OPS", "grp_ops!", "ops", "grp_ops ops", "grp_" + string(make([]byte, 60))}
	for _, id := range valid {
		if !ValidRoomID(id) {
			t.Fatalf("%q should be a valid room id", id)
		}
	}
	for _, id := range invalid {
		if ValidRoomID(id) {
			t.Fatalf("%q should be rejected", id)
		}
	}
	if !ValidTarget("grp_ops") || !ValidTarget("operator") || !ValidTarget("*") || ValidTarget("grp_") {
		t.Fatal("ValidTarget does not accept the room/operator/broadcast forms")
	}
}

func TestNewMessageIDIsValidAndUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		id := NewMessageID("op")
		if !ValidID(id) {
			t.Fatalf("%q is not a valid message id", id)
		}
		if seen[id] {
			t.Fatalf("duplicate id %q", id)
		}
		seen[id] = true
	}
}
