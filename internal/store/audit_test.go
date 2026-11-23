package store

import "testing"

func TestAutoWakeAuditSucceeded(t *testing.T) {
	cases := []struct {
		name   string
		detail string
		want   bool
	}{
		{
			"routine success",
			"peer=alice account=ma1 ok=true vm=UNKNOWN wake_ok=true wake_state=noise_confirmed error= wake_error=",
			true,
		},
		{
			"pre-error-field legacy success",
			"peer=alice account=ma1 ok=true vm=RUNNING wake_ok=true wake_state=noise_confirmed",
			true,
		},
		{
			"session failure",
			"peer=alice account=ma1 ok=false error=session failed",
			false,
		},
		{
			"ok true but wake error",
			"peer=alice account=ma1 ok=true vm=RUNNING wake_ok=false wake_state=unconfirmed error= wake_error=noise timeout after 3 tries",
			false,
		},
		{
			"ok false whose message contains ok=true",
			"peer=alice account=ma1 ok=false error=upstream said ok=true but failed",
			false,
		},
		{
			"ok true with session error containing spaces",
			"peer=alice account=ma1 ok=true vm=RUNNING wake_ok=false wake_state=unconfirmed error=noise handshake timeout wake_error=",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := autoWakeAuditSucceeded(tc.detail); got != tc.want {
				t.Fatalf("got %v want %v for %q", got, tc.want, tc.detail)
			}
		})
	}
}

func TestPurgeStatusSpamV1(t *testing.T) {
	st := openTest(t)
	now := int64(20000)

	// 1. Seed real task & result plus 10 repeated started + 8 repeated cancelled rows in op/Lisa.
	if _, err := st.InsertMessage(&Message{
		ID: "op-task-1", Sender: "operator", Recipient: "Lisa", Kind: "task",
		RootID: "op/Lisa", Payload: "upgrade script", CreatedAt: now - 500, ApprovalState: "n/a",
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		if _, err := st.InsertMessage(&Message{
			ID: "st-" + string(rune('a'+i)), Sender: "Lisa", Recipient: "operator", Kind: "status", Status: "started",
			InReplyTo: "op-task-1", RootID: "op/Lisa", Payload: "started", CreatedAt: now - 400 + int64(i), ApprovalState: "n/a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 8; i++ {
		if _, err := st.InsertMessage(&Message{
			ID: "ca-" + string(rune('a'+i)), Sender: "Lisa", Recipient: "operator", Kind: "status", Status: "cancelled",
			InReplyTo: "op-task-1", RootID: "op/Lisa", Payload: "timeout", CreatedAt: now - 300 + int64(i), ApprovalState: "n/a",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.InsertMessage(&Message{
		ID: "op-res-1", Sender: "Lisa", Recipient: "operator", Kind: "result",
		InReplyTo: "op-task-1", RootID: "op/Lisa", Payload: "done", CreatedAt: now - 100, ApprovalState: "n/a",
	}); err != nil {
		t.Fatal(err)
	}

	purge, err := st.PurgeStatusSpamV1(now)
	if err != nil {
		t.Fatal(err)
	}
	// 10 started -> keeps 2 (deleted 8); 8 cancelled -> keeps 2 (deleted 6); total deleted = 14.
	if purge.AlreadyDone || purge.Groups != 2 || purge.Deleted != 14 {
		t.Fatalf("unexpected purge result: %+v", purge)
	}
	msgs, err := st.ThreadMessages("op/Lisa", 100)
	if err != nil {
		t.Fatal(err)
	}
	// Kept: 1 task + 2 started + 2 cancelled + 1 result = 6 messages.
	if len(msgs) != 6 {
		t.Fatalf("remaining op/Lisa messages = %d, want 6", len(msgs))
	}
	// Audit row recorded.
	audits, err := st.ListAudit("system", "retention.purged", 0, 10)
	if err != nil || len(audits) != 1 {
		t.Fatalf("retention.purged audit = %+v err=%v", audits, err)
	}
	// Second run is a no-op.
	purge2, err := st.PurgeStatusSpamV1(now + 10)
	if err != nil || !purge2.AlreadyDone || purge2.Deleted != 0 {
		t.Fatalf("second purge should be no-op: %+v err=%v", purge2, err)
	}
}
