# ROADMAP — 未决问题与 P2 后续（DESIGN §13）

本文收敛设计文档 §13 的四个 open questions，给出本仓库当前结论与后续触发条件。

## 1. Worker pool vs. personal inboxes

**现状：** per-assistant inboxes + broadcast（`VisibleTo` 按身份过滤，广播靠 ack fan-out）。
**结论：** P2 之前不做 worker-pool。如需“空闲者领任务”，需原子 claim + lease，
这是唯一值得引入真 MQ（NATS / Redis Streams `XREADGROUP/XACK`）的特性。
触发条件：出现“多个同质 assistant 抢单”真实需求。
届时协议需新增 `POST /claims {lease_secs}` + `POST /claims/ack`，`messages` 加
`claimed_by/lease_expires` 列；store 接口已隔离，改动收敛在 queue/guard 内。

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

## P2 polish 状态（本版已含）

- [x] Store 接口隔离（`internal/store.Store`），Postgres 可 drop-in（见 POSTGRES.md）
- [x] 消息全文搜索（`GET /admin/messages?q=`，payload LIKE + 转义）
- [x] 按 capability badge 的职务 hint（UI 展示，人工分派）
- [x] 公网 `--public` TLS 模式 + admin Secure cookie
- [x] 线程导出（UI 审计导出 JSON；归档 JSONL 在 `data_dir/archive/`）
- [ ] Postgres driver 实现（接口已留，需真实需求再做）
- [ ] NATS/Redis 评估（见上 §1，仅 worker-pool 触发）
