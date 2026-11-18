package store

import "testing"

func TestMuseAccountsCRUDAndCascades(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "alice")
	mustPeer(t, st, "bob")

	a := &MuseAccount{
		ID: "ma_1", PeerID: "alice", Label: "a@example.com",
		CookiesEnc: "ciphertext", CookiesExp: `{"hatch_vml":1900000000}`,
		SessionStatus: "assigned", VMID: "vm-1", VMState: "RUNNING", VMStateAt: 99,
		AuthState: "ok", LastError: "",
		LastKeepaliveAt: 100, LastAttemptAt: 101, LastAttemptOK: true,
		LastWakeAt: 102, LastWakeOK: true,
		LastWakeError: "", LastWakeState: "running",
		AutoWake: true, Enabled: true,
		ChatEnabled: true, ChatConsentAt: 95, ChatConsentBy: "admin",
		ChatConsentVersion: 1, ChatConsentEvidence: "msg-12345",
		LastChatAt: 99, LastChatStatus: "replied",
		AuthorizedAt: 90, UpdatedAt: 102,
	}
	if err := st.CreateMuseAccount(a); err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := st.GetMuseAccount("ma_1")
	if err != nil || got == nil {
		t.Fatalf("get: %+v (%v)", got, err)
	}
	if got.PeerID != "alice" || got.CookiesEnc != "ciphertext" || got.VMStateAt != 99 ||
		!got.LastAttemptOK || !got.LastWakeOK || got.LastWakeState != "running" ||
		got.LastWakeError != "" || !got.AutoWake || !got.Enabled ||
		!got.ChatEnabled || got.ChatConsentAt != 95 || got.ChatConsentVersion != 1 ||
		got.ChatConsentEvidence != "msg-12345" || got.LastChatAt != 99 || got.LastChatStatus != "replied" {
		t.Fatalf("round-trip: %+v", got)
	}
	byPeer, err := st.GetMuseAccountByPeer("alice")
	if err != nil || byPeer == nil || byPeer.ID != "ma_1" {
		t.Fatalf("by peer: %+v (%v)", byPeer, err)
	}
	if none, err := st.GetMuseAccountByPeer("nobody"); err != nil || none != nil {
		t.Fatalf("missing peer lookup: %+v (%v)", none, err)
	}

	// One account per peer: a second row with the same peer must fail even
	// with a different primary key.
	dup := *a
	dup.ID = "ma_2"
	if err := st.CreateMuseAccount(&dup); err == nil {
		t.Fatal("expected unique peer_id violation")
	}

	// Save updates every mutable field and keeps authorized_at.
	got.Label = "renamed"
	got.Enabled = false
	got.AutoWake = false
	got.LastAttemptOK = false
	got.LastWakeOK = false
	got.LastWakeState = "unconfirmed"
	got.LastWakeError = "wake not confirmed"
	got.VMState = "UNKNOWN"
	got.VMStateAt = 222
	got.UpdatedAt = 200
	if err := st.SaveMuseAccount(got); err != nil {
		t.Fatalf("save: %v", err)
	}
	got2, _ := st.GetMuseAccount("ma_1")
	if got2.Label != "renamed" || got2.Enabled || got2.AutoWake || got2.LastAttemptOK ||
		got2.LastWakeOK || got2.LastWakeState != "unconfirmed" ||
		got2.LastWakeError != "wake not confirmed" || got2.VMState != "UNKNOWN" ||
		got2.VMStateAt != 222 || got2.UpdatedAt != 200 {
		t.Fatalf("save round-trip: %+v", got2)
	}
	if got2.AuthorizedAt != 90 {
		t.Fatalf("authorized_at changed: %+v", got2)
	}

	list, err := st.ListMuseAccounts()
	if err != nil || len(list) != 1 || list[0].ID != "ma_1" {
		t.Fatalf("list: %+v (%v)", list, err)
	}

	// Deleting the peer revokes its Muse authorization in the same tx.
	if _, err := st.DeletePeer("alice", 300); err != nil {
		t.Fatalf("delete peer: %v", err)
	}
	if got, _ := st.GetMuseAccount("ma_1"); got != nil {
		t.Fatalf("binding survived DeletePeer: %+v", got)
	}
}

func TestPrunePeersRemovesMuseAccounts(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "stale")
	mustPeer(t, st, "fresh")
	// last_seen is what retention uses; set it explicitly after create.
	if err := st.TouchPeer("stale", 100); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchPeer("fresh", 900); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"stale", "fresh"} {
		if err := st.CreateMuseAccount(&MuseAccount{
			ID: "ma_" + p, PeerID: p, AuthState: "ok", Enabled: true, AutoWake: true,
			AuthorizedAt: 1, UpdatedAt: 1,
		}); err != nil {
			t.Fatalf("create %s: %v", p, err)
		}
	}
	n, err := st.PrunePeers(500)
	if err != nil || n != 1 {
		t.Fatalf("prune: %d (%v)", n, err)
	}
	if got, _ := st.GetMuseAccount("ma_stale"); got != nil {
		t.Fatalf("stale binding survived prune: %+v", got)
	}
	if got, _ := st.GetMuseAccount("ma_fresh"); got == nil {
		t.Fatal("fresh binding was pruned")
	}
}

func TestMuseChatConsentIsTransactionalAndNarrow(t *testing.T) {
	st := openTest(t)
	mustPeer(t, st, "alice")
	a := &MuseAccount{ID: "ma_chat", PeerID: "alice", AuthState: "ok", Enabled: true,
		AutoWake: true, AuthorizedAt: 1, UpdatedAt: 1}
	if err := st.CreateMuseAccount(a); err != nil {
		t.Fatal(err)
	}
	if err := st.SetMuseChatConsent(a.ID, true, 100, "admin", "member said yes", 1,
		"admin", "muse.chat.opt_in", "account=ma_chat peer=alice consent=1", 100); err != nil {
		t.Fatalf("enable: %v", err)
	}
	got, _ := st.GetMuseAccount(a.ID)
	if !got.ChatEnabled || got.ChatConsentAt != 100 || got.ChatConsentBy != "admin" ||
		got.ChatConsentVersion != 1 || got.ChatConsentEvidence != "member said yes" {
		t.Fatalf("consent round-trip: %+v", got)
	}
	rows, err := st.ListAudit("", "muse.chat.opt_in", 0, 10)
	if err != nil || len(rows) != 1 {
		t.Fatalf("audit with consent: %v %+v", err, rows)
	}
	// Disabling keeps the historical consent metadata.
	if err := st.SetMuseChatConsent(a.ID, false, 200, "", "", 0,
		"admin", "muse.chat.opt_out", "account=ma_chat peer=alice", 200); err != nil {
		t.Fatalf("disable: %v", err)
	}
	got, _ = st.GetMuseAccount(a.ID)
	if got.ChatEnabled || got.ChatConsentAt != 100 || got.ChatConsentEvidence != "member said yes" {
		t.Fatalf("opt-out erased consent history: %+v", got)
	}
	// A missing account must not leave an audit row behind.
	if err := st.SetMuseChatConsent("ma_missing", true, 300, "admin", "evidence", 1,
		"admin", "muse.chat.opt_in", "account=missing", 300); err == nil {
		t.Fatal("expected unknown account error")
	}
	rows, _ = st.ListAudit("", "muse.chat.opt_in", 0, 10)
	if len(rows) != 1 {
		t.Fatalf("failed transaction wrote audit: %+v", rows)
	}

	if err := st.SetMuseChatStatus(a.ID, 400, "timeout"); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetMuseAccount(a.ID)
	if got.LastChatAt != 400 || got.LastChatStatus != "timeout" || got.UpdatedAt != 1 {
		t.Fatalf("narrow status update: %+v", got)
	}
}
