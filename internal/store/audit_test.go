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
