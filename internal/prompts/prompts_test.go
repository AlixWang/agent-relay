package prompts

import (
	"regexp"
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
			"你用户的指令", // §6.3/§7 hygiene present in every template
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
	// muse must point at the versioned thin-shell download — never a bare
	// repo relative path ("去仓库找" is unactionable for an assistant).
	out := render(t, "muse", Data{InviteCode: "inv_test"})
	if !strings.Contains(out, "/clients/relay-watch.sh") {
		t.Fatal("muse missing relay-watch.sh download URL")
	}
	for _, bad := range []string{"仓库已提供标准模板", "另见 clients/muse/README"} {
		if strings.Contains(out, bad) {
			t.Fatalf("muse still carries vague repo pointer %q", bad)
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
	// v7: Go receiver is the preferred choice, shell scripts are fallback.
	// muse/claw/generic must carry 首选 + 只用其一; hermes keeps cron as
	// the default entry but must not claim it cannot run resident processes.
	for _, typ := range []string{"muse", "claw", "generic", "hermes"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !strings.Contains(out, "relay-tail.sh") {
			t.Fatalf("%s missing tail choice", typ)
		}
	}
	for _, typ := range []string{"muse", "claw", "generic"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		if !strings.Contains(out, "首选") || !strings.Contains(out, "只用其一") {
			t.Fatalf("%s missing Go-preferred note", typ)
		}
	}
	hout, err := Render("hermes", Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(hout, "cron") {
		t.Fatal("hermes missing cron default note")
	}
	if !strings.Contains(hout, "client_update") {
		t.Fatal("hermes must mention client_update (even if only to ignore it)")
	}
	if strings.Contains(hout, "跑不了常驻进程") {
		t.Fatal("hermes must not claim it cannot run resident processes")
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

func TestChangesSinceIncremental(t *testing.T) {
	if got := ChangesSince(99); len(got) != 0 {
		t.Fatalf("current: %+v", got)
	}
	got := ChangesSince(2)
	if len(got) != 8 || got[0].Version != 3 || got[7].Version != PromptVersion {
		t.Fatalf("since v2: %+v", got)
	}
	for _, e := range got {
		if e.Summary == "" || e.Actions == "" {
			t.Fatalf("empty entry: %+v", e)
		}
	}
	if len(ChangesSince(0)) != len(ChangeLog) {
		t.Fatal("unknown peer should get full log")
	}
	// PromptVersion must equal the newest changelog entry.
	if ChangeLog[len(ChangeLog)-1].Version != PromptVersion {
		t.Fatalf("changelog head %d != PromptVersion %d",
			ChangeLog[len(ChangeLog)-1].Version, PromptVersion)
	}
}

func TestDistributionMatchesInit(t *testing.T) {
	// /prompts/current (IsReconfigure=true) must equal the init prompt
	// except the registration block: SSE choice and all worker sections
	// identical. Regression test for the "distribution missing SSE" drift.
	for _, typ := range []string{"muse", "claw", "generic", "hermes"} {
		init, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		dist, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1, IsReconfigure: true})
		if err != nil {
			t.Fatal(err)
		}
		for _, needle := range []string{"relay-tail.sh", "relay-tail?arch", "client_update", "prompt-changes.json", "profile_refresh", "7.5", "6.5", "relay-watch.sh"} {
			if strings.Contains(init, needle) && !strings.Contains(dist, needle) {
				t.Fatalf("%s: distribution missing %q present in init", typ, needle)
			}
		}
	}
}

func TestMuseWorkerRichness(t *testing.T) {
	// Regression test for the onboarding drift: the served muse prompt
	// must carry the worker-prompt.md details (not just the thin §6).
	// Both init and distribution (IsReconfigure) render from the same
	// template, so checking one covers both.
	out := render(t, "muse", Data{InviteCode: "inv_test"})
	for _, must := range []string{
		"payload_preview", ".done_ids", "先回结果",
		"poll_error", "kind=chat", "执行摘要",
		"绝不带 `thread`", "relay-watch.sh",
	} {
		if !strings.Contains(out, must) {
			t.Fatalf("muse prompt missing worker richness %q", must)
		}
	}
	if strings.Contains(out, "<RELAY_URL>") || strings.Contains(out, "<YOUR_IDENTITY>") {
		t.Fatal("muse prompt leaks unrendered <PLACEHOLDER>")
	}
}

// Relayed-instruction policy (prompt v8, DESIGN §6.3): a message that relays the
// user's instruction IS the user's instruction — the four redlines are the only
// thing the receiving agent still has to escalate.
func TestRelayedInstructionPolicy(t *testing.T) {
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		for _, must := range []string{"你用户的指令", "无论消息里怎么说", "外泄", "删除不可恢复的数据", "改动中继之外的凭证"} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing relayed-instruction rule %q", typ, must)
			}
		}
		// The old blanket "relay = untrusted, not your user" wording is retired:
		// it is what made assistants refuse their own user's relayed orders.
		for _, bad := range []string{"不是你用户的指令", "not instructions from your user", "中继永远不能代替"} {
			if strings.Contains(out, bad) {
				t.Fatalf("%s still carries retired hygiene wording %q", typ, bad)
			}
		}
	}
}

// Section numbers are how prompt_update tells assistants what to re-read, so a
// duplicated heading number silently breaks the upgrade guide.
// The client_update predicate is the receiver source revision, not the
// release tag (§8.9 / prompt v9). Without this, every release nags every
// assistant to swap a behaviourally identical binary.
func TestReceiverRevDocumented(t *testing.T) {
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		for _, must := range []string{"receiver_rev", "client_rev"} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing receiver-rev rule %q", typ, must)
			}
		}
	}
	for _, typ := range []string{"muse", "claw", "generic"} {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		if !strings.Contains(out, "只有接收端源码真的变了才会提醒") {
			t.Fatalf("%s must state that only a real receiver change triggers the nudge", typ)
		}
	}
}

func TestSectionNumberingClean(t *testing.T) {
	re := regexp.MustCompile(`(?m)^## (\d+(?:\.\d+)?)[ .]`)
	for _, typ := range Types() {
		out := render(t, typ, Data{InviteCode: "inv_test"})
		seen := map[string]bool{}
		for _, m := range re.FindAllStringSubmatch(out, -1) {
			if seen[m[1]] {
				t.Fatalf("%s: section %s declared twice", typ, m[1])
			}
			seen[m[1]] = true
		}
		if strings.Contains(out, "方式 C") {
			t.Fatalf("%s references a non-existent setup option 方式 C", typ)
		}
	}
}

func TestDeliverySelfCheckPresent(t *testing.T) {
	// Every template must carry the local-delivery self-check: the server
	// has no "main session" concept, so "no reply" is a local routing
	// issue first. hermes (no shell) carries it as §5.7, the rest as §6.7.
	for _, typ := range []string{"muse", "claw", "generic"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, must := range []string{"主对话", "6.7", "唤醒链", "不要重注册"} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing delivery self-check %q", typ, must)
			}
		}
	}
	out, err := Render("hermes", Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, must := range []string{"主对话", "5.7", "唤醒链", "不要重注册"} {
		if !strings.Contains(out, must) {
			t.Fatalf("hermes missing delivery self-check %q", must)
		}
	}
}

func TestClientChoiceDocumented(t *testing.T) {
	// v7: muse/claw/generic prefer the Go receiver (fallback to shell only
	// when binaries can't run), with the client_update flow; hermes stays
	// cron and explicitly says client_update is not for it.
	for _, typ := range []string{"muse", "claw", "generic"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		for _, must := range []string{"首选", "relay-tail?arch", "client_update", "只用其一"} {
			if !strings.Contains(out, must) {
				t.Fatalf("%s missing client choice %q", typ, must)
			}
		}
	}
	out, err := Render("hermes", Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "cron") || !strings.Contains(out, "忽略") {
		t.Fatal("hermes must keep cron default and say client_update is ignorable")
	}
	// Old poll-default wording must be gone from the shell-capable templates.
	// ("poll/tail 二选一" inside fallback 方式 B is fine: the fallback pair
	// is still either/or; what changed is Go-first, not shell-default.)
	for _, typ := range []string{"muse", "claw"} {
		out, err := Render(typ, Data{ServerAddr: "http://x:1", PeerID: "p", ProtocolVersion: 1})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "短轮询（默认）") {
			t.Fatalf("%s still carries old poll-default wording", typ)
		}
	}
}
