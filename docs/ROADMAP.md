# ROADMAP — 未决问题与 P2 后续（DESIGN §13）

本文收敛设计文档 §13 的四个 open questions，给出本仓库当前结论与后续触发条件。

## 1. Worker pool vs. personal inboxes

**现状：** per-assistant inboxes + broadcast（`VisibleTo` 按身份过滤，广播靠 ack fan-out）。
**结论：** P2 之前不做 worker-pool。如需“空闲者领任务”，需原子 claim + lease，
这是唯一值得引入真 MQ（NATS / Redis Streams `XREADGROUP/XACK`）的特性。
触发条件：出现“多个同质 assistant 抢单”真实需求。
`docs/SUPERAGENT.md` 给出了演进形状：Ticket 跨 Assignment 存活，lease 是 Assignment
而不是新的消息 kind。Phase 1 先做单 assignee 只读投影；Phase 2 加
`ticket_assignments`（pending/running/ended + lease）与 reassign，Ticket id 不变。
届时不必给 `messages` 加 `claimed_by/lease_expires`；store 接口已隔离。

## 2. Payload size / 附件

**现状：** 纯文本 `payload`（tasks ≤200KB 由 guard 隐式约束，body 上限 1MiB）。
图片/音频等走共享存储链接（Tailscale 共享目录或对象存储预签名 URL）。
**结论：** 协议保持 text-only + links，不做二进制附件。
触发条件：出现“必须经中继审计的多媒体任务”且链接方案被证明不够用。
届时优先做“对象引用”字段（`attachment_ref`），而非内联二进制。

## 3. Hermes/Claw capability 探测定稿

**现状：** `internal/prompts` 内置四套模板 + `agentCaps` 矩阵（muse/claw 全能力，
hermes/generic 无 shell/定时/后台/文件）；注册时回传 `capabilities` JSON 存库，
UI 以 badge 展示；职务 hint 靠人类看 badge（不派 shell 任务给 shell-less 成员）。
**待办（经验性）：** 等第一个非 Muse 助手真实 onboard 后，按实测修正
`agentCaps` 与模板条件段，把结论写回本节。

## 4. Human approval UX 分工

**现状：** 双轨并行（DESIGN §6.4）——服务端 `requires_approval` hold（已实现，
UI 批准/拒绝）+ 客户端 prompt 级 sensitive 自查（四套模板 §7 已含 hygiene 原文）。
**待校准：** 等一次真实敏感任务 incident 后，复盘 hold 粒度（是按线程还是按消息）
与客户端确认提示词强度，届时更新 guard 阈值与模板。

## 5. 工作项层（superagent-inspired）

**状态：** Phase 1–4 均已实现（`docs/SUPERAGENT.md`、ADR-0001）：
`tickets`/`message_tickets` 投影 + 控制台工作台（Phase 1）；
`ticket_assignments`/`ticket_subscriptions` + agent `GET /tickets` + admin
reassign + lease 到期回收 + `waiting|ready|failed|dropped`（dropped 终态）（Phase 2）；
`policy_versions`/`mandates` + 确定性 task gate + `effects` outbox + per-kind `/admin/health`（Phase 3）；
`prompt_revisions`（含 `base_revision`）+ `template_revision` + `/admin/prompts/versions` +
`/admin/prompts/diff`（Phase 4）。
消息表仍是唯一消息写模型；Ticket/Assignment 是运维投影。

**后续增强（已完成基础上）：**

- **Worker pool / claim**：`ticket_assignments` 已有 pending/running/ended、`lease_expires_at`
  与过期回收（到期后等人工改派）；下一步是原子 claim + lease 续约。Ticket id 不变。
- **订阅范围**：`ticket_subscriptions` 已有 own/context，下一步是“结束 Ticket 后
  人工恢复”和面板上的 context 订阅操作；隐私约束不变。
- **授权生命周期**：`mandates` 已记录 policy version/scope/requester，下一步是
  resume 时的 revalidation 与 Policy 变更通知。
- **Effects 扩展**：当前覆盖 offline webhook，并按 kind 暴露 pending/due/failed/last_error；
  下一步接入通知类副作用与更完整的重试操作面板。
- **Prompt**：已有模板 revision/hash、`base_revision` 与 diff 查询接口；编辑器级差异 UI 仍是后续。

## 6. 资源中心（密钥 / MCP / Skill / Prompt）

**设计：** DESIGN §4.12、ADR-0004（清单）、ADR-0005（拉取）。资源中心本身只登记、授权、下发，
不执行 skill；内置 MCP 网关见下节 §7 与 ADR-0007（默认关闭的独立受控通道）。

- [x] **P1** 表结构、`secrets.key` 加密、secret 资源、按 peer / `*` 授权、
  `GET /resources/current`、`GET /resources/secrets/{name}`、heartbeat `resources_update` /
  `resources_rev` 回执、控制台「资源中心」密钥页
- [x] **P2** `mcp` / `prompt` 资源；MCP 中立格式校验；`GET /resources/mcp/{name}`
  解析 `${secret:}`，缺授权 409；授权 MCP 时提示缺少的依赖密钥
- [x] **P3** 接收端（relay-tail、relay-poll.sh、relay-tail.sh、两份 relay-watch.sh、Hermes supervisor）
  转发 `resources_update`、heartbeat 带 `.resources_rev`、`.resources_rev.staged` 去重（`receiver_rev` 会变）；
  四个模板加「资源同步」一节，`PromptVersion` 18；成员页显示同步状态
- [x] **P3b** `skill` 上传/校验/下载（tar.gz，`SKILL.md` frontmatter，≤1 MiB 解压，防路径穿越/链接/gzip 炸弹）
- [x] **P4** Guard / `POST /logs` 入站密钥明文打码为 `[secret:NAME]`，审计 `secret.redacted`
- [x] **P5** 助手自助注册（ADR 0006）：active peer 可提交 secret / mcp / skill / prompt
  到 `resource_submissions` 待审核区；operator 在 `/admin/resource-submissions` 批准/拒绝/改名
  （skill 逐个预览），批准后进入正式目录；owner 归属、per-peer 配额与开关、pending 过期清理、
  来源助手下架申请，以及 prompt v19「资源上报」章节

## 7. 内置 MCP 网关（ADR-0007）

**状态：** 已实现（默认关闭，需显式开启 `[mcp_gateway].enabled` 与 `[mcp_gateway.write].enabled`）：

- [x] **网关内核（`internal/mcpgateway`）**：Streamable HTTP（`2025-06-18` / `2025-03-26`，无状态 JSON-only）、聚合端点 `/mcp` 与分服务端点 `/mcp/{service}`、`Origin` 校验、双认证（relay token peer 授权 + 一次性明文 `rmk_` 外部 Key 与宽限轮换）、`mcp:<service>:<read|write>:<tool|*>` 权限模型、分服务速率限制与未鉴权审计防洪、四层写开关与高风险二次审批状态机（24h 待批 / 10m 一次性执行窗口 / 严格冻结参数）、共享 Google OAuth2 ADC Token 提供器与 `mcp_audit` + `audit_log` 镜像保留（读 90d / 写 180d / 上限 200 万行）。
- [x] **首批服务**：AdSense（9 个只读工具）、Bing Webmaster（16 个只读 + 6 个写工具，其中 4 个低风险直连，`add_blocked_url` / `remove_blocked_url` 高风险走二次批准）、Google Analytics（9 个只读工具，复用同一 ADC secret）。
- [x] **控制台**：MCP 网关页签（服务状态、外部 Key 签发/轮换/吊销、Peer 授权、高风险审批、审计筛选与 CSV 导出、Dry-run 参数校验）。
- [ ] **后续扩展**：AdSense 写工具占位（5 个）待项目取得 AFP 资格与全权限 ADC 后开放。

## P2 polish 状态（本版已含）

- [x] Store 接口隔离（`internal/store.Store`），Postgres 可 drop-in（见 POSTGRES.md）
- [x] 消息全文搜索（`GET /admin/messages?q=`，payload LIKE + 转义）
- [x] 按 capability badge 的职务 hint（UI 展示，人工分派）
- [x] 公网 `--public` TLS 模式 + admin Secure cookie
- [x] 线程导出（UI 审计导出 JSON；归档 JSONL 在 `data_dir/archive/`）
- [ ] Postgres driver 实现（接口已留，需真实需求再做）
- [ ] NATS/Redis 评估（见上 §1，仅 worker-pool 触发）
