// Package prompts renders paste-ready onboarding prompts per agent type
// (DESIGN §8.1, §8.2, §9.3). Templates live in templates/*.tmpl and are
// versioned alongside the protocol version.
package prompts

import (
	"bytes"
	_ "embed"
	"fmt"
	"text/template"
)

//go:embed templates/muse.tmpl
var museTmpl string

//go:embed templates/hermes.tmpl
var hermesTmpl string

//go:embed templates/claw.tmpl
var clawTmpl string

//go:embed templates/generic.tmpl
var genericTmpl string

// Data is the rendering context for all prompt templates.
// Capability differences between assistant types are embodied by the four
// per-type templates (each written for its type's actual abilities), not by
// conditional flags: a hermes template never mentions background loops,
// a muse template never asks a human to proxy every curl.
type Data struct {
	ServerAddr      string // e.g. http://100.x.y.z:18789
	InviteCode      string // one-time code; placeholder text in reconfigure mode
	PeerID          string // pre-assigned identity, or "(you choose)"
	AgentType       string
	ProtocolVersion int
	// IsReconfigure renders the credential/registration sections for an
	// existing peer (protocol upgrade): reuse the saved token, skip /register.
	IsReconfigure bool
	// NetworkNote is a one-line parenthetical describing how the server is
	// reached, rendered after the address. Must match the deployment:
	// tailscale bind, public TLS, or reverse-proxy coexistence.
	// Empty falls back to the tailscale wording (backward compatible).
	NetworkNote string
}

// NetworkNoteFor derives the parenthetical from the effective serving mode.
// behindProxy/publicAddr mirror the server config (behind_proxy/public_addr);
// public mirrors direct-TLS mode. Anything else is the tailscale default.
func NetworkNoteFor(behindProxy bool, publicAddr string, public bool) string {
	if behindProxy && publicAddr != "" {
		return "公网地址（经反代接入用户常开 VPS，TLS 由反代终止）"
	}
	if public {
		return "公网地址（用户常开 VPS，TLS 直连）"
	}
	return "用户常开 VPS（只监听 tailnet）"
}

// Render returns the paste-ready prompt block for agentType.
// Unknown types fall back to generic.
func Render(agentType string, d Data) (string, error) {
	switch agentType {
	case "muse", "hermes", "claw", "generic":
	default:
		agentType = "generic"
	}
	d.AgentType = agentType
	if d.NetworkNote == "" {
		d.NetworkNote = "用户常开 VPS（只监听 tailnet）"
	}
	if d.IsReconfigure && d.InviteCode == "" {
		d.InviteCode = "(not needed — reuse your saved token)"
	}
	if d.PeerID == "" {
		d.PeerID = "(invite-bound identity, or a name you choose matching ^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$)"
	}

	var src string
	switch agentType {
	case "muse":
		src = museTmpl
	case "hermes":
		src = hermesTmpl
	case "claw":
		src = clawTmpl
	default:
		src = genericTmpl
	}
	t, err := template.New("prompt").Parse(src)
	if err != nil {
		return "", fmt.Errorf("parse prompt template: %w", err)
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, d); err != nil {
		return "", err
	}
	return buf.String(), nil
}

// Types returns the supported agent types for the UI picker.
func Types() []string { return []string{"muse", "hermes", "claw", "generic"} }
