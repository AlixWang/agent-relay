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

// PromptVersion is the current worker-instruction revision (§8.6).
// Bump on ANY template change (B-side or A-side contract wording counts).
// Peers report theirs in heartbeat; the server tells laggards to pull
// /prompts/current. Independent from protocol_version: instruction updates
// must not force re-registration.
// v3: SSE tail choice in muse/claw/generic, hermes cron-stays-default note.
// v4: assistant profile §8.7 (register/heartbeat dual-shape capabilities
// fix for the hermes 400 included).
//
// ChangeLog records what each revision changed and what the assistant must
// do about it. /prompts/current serves only the entries newer than the
// peer's reported version, so assistants upgrade incrementally instead of
// re-reading the full text. Keep entries short: section numbers + actions.
const PromptVersion = 8

// ChangeEntry is one revision's upgrade guide for assistants.
type ChangeEntry struct {
	Version int    `json:"version"`
	Summary string `json:"summary"` // one line: what changed
	Actions string `json:"actions"` // what the assistant must do (empty = read-only)
}

// ChangeLog is append-only: never edit a shipped entry, only append.
var ChangeLog = []ChangeEntry{
	{
		Version: 2,
		Summary: "执行态握手 §6.5（status/permission_request/permission_decision）+ 发送方 §6.6：B 执行中卡住要回 started/blocked，敏感操作先发 permission_request 等 decision，默认等 10 分钟按 deny。",
		Actions: "对照本地 §6 补上 §6.5/§6.6 两节；B 侧以后不再静默挂起。",
	},
	{
		Version: 3,
		Summary: "接收方式二选一：短轮询 relay-poll.sh（默认）或 SSE 常驻 relay-tail.sh（秒级推送，需常驻进程）；hermes 保持 cron 不动。",
		Actions: "能跑常驻进程的可换 relay-tail.sh（同唤醒格式）；换脚本后游标文件通用，无需重注册。",
	},
	{
		Version: 4,
		Summary: "自我简介 §8.7/§7.6/§6.6：注册带 profile（一句话能力+常干任务），heartbeat 可更新；服务端每周 profile_refresh 提醒。heartbeat/register 的 capabilities 改收对象或字符串（之前对象会 400）。",
		Actions: "注册时填 profile；被 profile_refresh 唤醒时从 memory 提炼重写简介，下次 heartbeat 带上。",
	},
	{
		Version: 5,
		Summary: "下发与初始化统一：/prompts/current 即完整全文（除注册块），SSE 二选一包含在内；新增本升级指引（changes 字段）。",
		Actions: "以后被 prompt_update 唤醒：先读 prompt-changes.json 看动了哪几节，对照本地改，再读 prompt-update.md 全文核对，最后 mv .prompt_version.staged 确认。",
	},
	{
		Version: 6,
		Summary: "主对话投递自查 §6.7（muse/claw/generic）/§5.7（hermes）：服务端没有主对话概念，消息交到哪是本地 runtime 的事；收不到先查唤醒链→游标→常驻三段，只动本地不重注册。",
		Actions: "被 prompt_update 唤醒后对照本地投递链补上自查节；下次“没收到”先按三步举证再上报。",
	},
	{
		Version: 7,
		Summary: "接收端首选 Go 二进制 §4（erdan 实测通过）+ 客户端更新 §7.6/§6.5：heartbeat 上报构建版本，服务端有新 Release 回 client_update 唤醒换二进制；跑不了二进制才回落 shell（poll/tail 二选一）。",
		Actions: "能跑二进制的按 §4 方式 A 切 Go 接收端（游标文件通用）；被 client_update 唤醒时下载新版替换重启。hermes 忽略 client_update。",
	},
	{
		Version: 8,
		Summary: "Muse 薄层服务端下发 §4（GET /clients/relay-watch.sh，不再指仓库相对路径）+ 常驻守则回填 §6/§7.6/§8（wake 元数据+按id取全文、先result后ack+.done_ids幂等、poll_error直报、chat/result处理、执行摘要、防假升级校验）。",
		Actions: "按 §4 从服务端下载 relay-watch.sh 并改 CONFIG 区；对照 §6/§6.4/§7/§7.6/§8 补上缺的守则节。",
	},
}

// ChangesSince returns the ChangeLog entries newer than v (ascending).
// Empty when the peer is current — the assistant has nothing to do.
func ChangesSince(v int) []ChangeEntry {
	var out []ChangeEntry
	for _, e := range ChangeLog {
		if e.Version > v {
			out = append(out, e)
		}
	}
	return out
}

// Types returns the supported agent types for the UI picker.
func Types() []string { return []string{"muse", "hermes", "claw", "generic"} }
