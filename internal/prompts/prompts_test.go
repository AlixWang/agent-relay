package prompts

import (
	"strings"
	"testing"
)

func render(t *testing.T, agentType string, d Data) string {
	t.Helper()
	if d.ServerAddr == "" {
		d.ServerAddr = "http://100.1.2.3:18789"
	}
	if d.PeerID == "" {
		d.PeerID = "test-x"
	}
	if d.ProtocolVersion == 0 {
		d.ProtocolVersion = 1
	}
	out, err := Render(agentType, d)
	if err != nil {
		t.Fatalf("render %s: %v", agentType, err)
	}
	return out
}

func TestAllTypesRender(t *testing.T) {
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		for _, must := range []string{
			"http://100.1.2.3:18789", "inv_test", "test-x",
			"untrusted input", // §6.3 hygiene present in every template
			"/verify/smoke", "426",
		} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing %q", typ, must)
			}
		}
		if strings.Contains(out, "{{") || strings.Contains(out, "}}") {
			t.Fatalf("%s has unrendered template markers", typ)
		}
	}
}

func TestNetworkNoteModes(t *testing.T) {
	if got := NetworkNoteFor(false, "", false); !strings.Contains(got, "tailnet") {
		t.Fatalf("tailscale default: %q", got)
	}
	if got := NetworkNoteFor(false, "", true); !strings.Contains(got, "TLS") {
		t.Fatalf("public direct: %q", got)
	}
	if got := NetworkNoteFor(true, "https://relay.example.com", false); !strings.Contains(got, "反代") {
		t.Fatalf("behind proxy: %q", got)
	}
}

func TestNoHardcodedTailscaleAssumption(t *testing.T) {
	// Rendered prompts must not contradict a public/proxy deployment:
	// no bare "Tailscale" branding, no tailnet-only claims.
	for _, typ := range Types() {
		out := render(t, typ, Data{
			InviteCode:  "inv_test",
			ServerAddr:  "https://relay.example.com",
			NetworkNote: NetworkNoteFor(true, "https://relay.example.com", false),
		})
		for _, bad := range []string{"基于 Tailscale", "只监听 tailnet", "能上 tailnet"} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s contains stale tailscale copy %q", typ, bad)
			}
		}
		if !strings.Contains(out, "反代") {
			t.Fatalf("%s missing proxy note", typ)
		}
	}
}

func TestUnknownTypeFallsBack(t *testing.T) {
	out := render(t, "nope", Data{InviteCode: "inv_x"})
	if !strings.Contains(out, "agent_type") && !strings.Contains(out, "generic") {
		t.Fatalf("fallback broken: %.200s", out)
	}
}

func TestReconfigureSkipsRegistration(t *testing.T) {
	for _, typ := range Types() {
		out := render(t, typ, Data{IsReconfigure: true, PeerID: "old-peer"})
		// No executable registration path: the curl endpoint and the
		// invite-code flow must be gone (prose saying "don't re-register"
		// is expected and fine).
		if strings.Contains(out, "$RELAY/register") || strings.Contains(out, "一次性邀请码") {
			t.Fatalf("%s reconfigure still carries invite flow", typ)
		}
		if !strings.Contains(out, "沿用") {
			t.Fatalf("%s reconfigure missing token-reuse note", typ)
		}
		if strings.Contains(out, "{{.InviteCode}}") || strings.Contains(out, "{{") {
			t.Fatalf("%s reconfigure has raw markers", typ)
		}
	}
	// Fresh mode still carries the invite flow.
	out := render(t, "muse", Data{InviteCode: "inv_fresh"})
	if !strings.Contains(out, "inv_fresh") || !strings.Contains(out, "/register") {
		t.Fatal("fresh mode lost invite flow")
	}
}

func TestPollScriptDownloadDocumented(t *testing.T) {
	// muse/claw prompts must tell the assistant to download the script
	// from the server — never "ask the user for the full text" (dead loop:
	// the user doesn't have it either).
	for _, typ := range []string{"muse", "claw"} {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		if !strings.Contains(out, "/clients/relay-poll.sh") {
			t.Fatalf("%s missing script download URL", typ)
		}
		if strings.Contains(out, "向用户索取全文") {
			t.Fatalf("%s still says 'ask user for full text'", typ)
		}
	}
}

func TestAgentTypeEmbedded(t *testing.T) {
	// Registration curl in each template must report its own agent_type,
	// otherwise the UI capability badges lie.
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_x"})
		if !strings.Contains(out, `"agent_type":"`+typ+`"`) {
			t.Fatalf("%s template reports wrong agent_type", typ)
		}
	}
}

func TestHandshakeContractsPresent(t *testing.T) {
	// Both sides of the remote-approval handshake must be documented in
	// every template: B-side execution binding + wait/timeout rules, and
	// A-side scope pre-check + structured-fields-first rendering.
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		for _, must := range []string{
			"permission_request", "permission_decision",
			"执行绑定", "游标", // B-side: execution binding, cursor discipline
			"scope", "结构化字段", // A-side: pre-check, fields-first rendering
			"不代批", "explanatory result",
		} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing handshake contract %q", typ, must)
			}
		}
	}
}

func TestTailChoiceDocumented(t *testing.T) {
	for _, typ := range []string{"muse", "claw", "generic", "hermes"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !strings.Contains(out, "relay-tail.sh") {
			t.Fatalf("%s missing tail choice", typ)
		}
		// Both scripts must be presented as either/or, never both-at-once.
		if !strings.Contains(out, "二选一") && !strings.Contains(out, "只用其一") && typ != "hermes" {
			t.Fatalf("%s missing either/or note", typ)
		}
		if typ == "hermes" && !strings.Contains(out, "cron 默认不变") {
			t.Fatalf("hermes missing cron-stays-default note")
		}
	}
}

func TestProfileSectionsPresent(t *testing.T) {
	for _, typ := range []string{"muse", "claw", "generic", "hermes"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !strings.Contains(out, "profile") || !strings.Contains(out, "profile_refresh") {
			t.Fatalf("%s missing profile section", typ)
		}
		if !strings.Contains(out, `"profile":`) {
			t.Fatalf("%s register curl missing profile field", typ)
		}
	}
}
