# Muse 直接对话（v1）

后台以成员已授权的 muse.ai 登录态，通过原生 Noise 网关的 `chat.stream` / `chat.history` 发一句纯文本并读一条回复。默认关闭，admin-only，逐成员 opt-in；消息进入成员自己的 Muse 主聊天（或指定且已存在的 side chat），成员可见。**不是**任务下发、不是回执、不是审批通道。

## 三层开关

1. **部署门** `muse_direct_chat_enabled`（默认 `false`，改动需重启）。
2. **运行时 kill switch**：`POST /admin/muse/chat/settings {enabled}`，即时生效；关闭时停止在途 job 的本地轮询并关连接，但**不**逐条发送 `chat.cancel`，避免关闭动作放大成上游 RPC 洪峰。
3. **成员 opt-in**：`muse_accounts.chat_enabled` 默认 0。只能由 admin 在控制台开启，记录 `chat_consent_at/by/version/evidence` 并在同一事务写 `muse.chat.opt_in` 审计。重新授权（换 cookie）或更换 `peer_id` 会把 consent 重置为 0，必须重新同意。

没有“全部开启”按钮；没有成员自助端点；agent / peer token / webhook / cron 都不能调用。

## 端点

见 `docs/API.md` 的“Muse 直接对话”一节。POST 成功固定 202；job 的 `status/error` 才表达运行结果。`GET` 对存在的 job 一律 200，终态保留 1 小时（内存，不落库，重启即 404）。审计不受 job GC 影响。

## 状态机

```
preparing → sending → confirming → waiting → replied
                                             ├ timeout
                                             ├ delivery_unknown
                                             ├ reply_unknown
                                             ├ failed
                                             └ cancelled
```

- `preparing`：worker 在 `opMu` 内做 session/token/必要时 wake；失败未发消息。
- `sending`：读 baseline（`chat.history limit=1`）+ 最近 5 条 busy check，然后 `muse.chat.sending` 审计；审计写不进去就不发。之后写 `chat.stream`。
- `confirming`：等 echo（≤10s）并在 history 找自己的 `message.user`（≤5s）。找不到 → `delivery_unknown`，**绝不自动重发**。
- `waiting`：从确认投递起算预算（默认 180s，最大 300s），每 2s ±10% 轮询 `chat.history limit=20`；连续 3 次瞬态失败才重连一次只读继续，再失败 → `reply_unknown`。
- 回复判定：`message.assistant` + `seq>baseline` + 两处 `reply_to_message_id` 都为空 + 有文本/agent 附件/卡片 + `status` 非空 + scope 匹配；`reply_to_message_id` 非空是主动推送，不算回复。同一窗口的后续候选放 `additional_replies`；期间有其他 user → `confidence=ambiguous` + warning，不阻断返回。
- 超时**不**自动 cancel；显式 `cancel` 才 best-effort 发 `chat.cancel`（4s 超时，4xx 视为 unsupported），无论 RPC 成败本地都 `cancelled`。不调用 `delete_turn`，不删除已发消息。
- 媒体：只返回回复事件自带附件元数据；不等待/不下载后到的 media，不追 presentation。

## 并发、lease 与限流

- 每成员 1 个活跃对话；第二个请求 409 `muse_chat_in_progress` 并带原 `chat_id`。
- 全局 4 条活跃连接；超出 429 `muse_chat_busy` + `Retry-After:5`，不排队。
- 每成员 6 条/分、30 条/时；全局 30 条/分；超出 429 `muse_chat_rate_limited`。
- job map 上限 256；终态 1h GC，活跃 job 不淘汰。
- 对话持有账号 lease；`Keepalive`/`Wake`/`KeepaliveAll`/`MaybeWakeOffline` 在 lease 存在时让路（409/跳过），避免同一账号的 Noise 连接互相抢读。对话准备阶段仍复用 `opMu` 保护 cookie/账号行，但 stream/轮询全程不持 `opMu`。

## 审计

只写 `audit_log` 元数据，不写全文/回复全文/其他消息/凭证：

| action | 关键字段 |
|---|---|
| `muse.chat.opt_in` / `opt_out` | account, peer, consent, evidence, by / reason |
| `muse.chat.runtime_flag` | enabled, by |
| `muse.chat.disabled` | active_jobs, reason=kill_switch |
| `muse.chat.sending` | chat, peer, account, session, baseline, text_len, text_sha256, request_id, consent=1 |
| `muse.chat.sent` | chat, echo, user_seq, delivery=confirmed, elapsed_ms |
| `muse.chat.replied` | chat, reply_seq, reply_id, reply_status, elapsed_ms, polls, confidence |
| `muse.chat.timeout` | chat, partial, last_seq, elapsed_ms, polls |
| `muse.chat.cancelled` | chat, cancel=sent/unsupported/skipped |
| `muse.chat.failed` | chat, stage, error_class |
| `muse.chat.delivery_unknown` | chat, reason, last_seq |

`sending` 审计失败会阻断发送；终态审计失败只记服务端日志（消息可能已发出，不能回滚）。保留期沿用 `audit_retention_days`（默认 30 天）。

## D1–D26 落点

| # | 实现 |
|---|---|
| D1 | 只做问答；无 spool/task/result/ack |
| D2 | `session_id` 空 = 主聊天；非空只接受已存在 sid |
| D3 | `chat_enabled` 默认 0，config/runtime 默认关 |
| D4 | 控制台全文 v1 + evidence；`SetMuseChatConsent` 事务写账号列+审计；换 cookie/peer 重置 |
| D5 | 仅元数据 + `text_sha256`/`text_len`；发送前审计失败即不发 |
| D6 | 复用 `audit_retention_days`；consent 状态在账号列长期保留 |
| D7 | `chatDefaultBudgetHard=180s`，`chatMaxBudget=300s`，从确认投递起算 |
| D8 | `chatPollHard=2s`，±10% 抖动，首次立即，单次 4s 超时 |
| D9 | 每账号 lease + `chatSem` 容量 4 |
| D10 | 429 + Retry-After，不排队 |
| D11 | 每个 job 按需 dial，终态 `Close`，无池 |
| D12 | 写前最多一次（生产实现只在 dial 阶段重试一次）；写后只查 history，不自动重发 |
| D13 | 超时不 cancel；显式 cancel best-effort；不 delete_turn |
| D14 | 只取回复自带附件元数据，不下载/不等待 |
| D15 | 不调 `session.start`/`rename` |
| D16 | `chatNodeID` = HMAC(account.ID)[:16]，不落库 |
| D17 | 最近 5 条 busy check：草稿或 10 分钟内未完成 user → `muse_agent_busy`，不发 |
| D18 | 其他 user → `confidence=ambiguous` + `concurrent_user_activity` |
| D19 | 内存 job，终态 1h，map 上限 256 |
| D20 | POST 默认 202，`block_secs` 0–20，worker 不随 HTTP 取消 |
| D21 | 准入错误映射 400/404/409/429/503；运行期 job 状态+error.code |
| D22 | config + runtime + 成员 opt-in 三层 |
| D23 | 不硬编码成员；opt-in 机制允许 erdan 先开 |
| D24 | admin-only、纯 text、拒绝结构化字段、无审批 UI、不改 guard |
| D25 | 6/分、30/时、全局 30/分、4 并发 |
| D26 | 本文记录协议假设；漂移时按 90 天/触发式复核 |

## 已知限制

- job 和回复文本只在内存；进程重启后 GET 404，全文以成员 Muse 会话为准。
- `chat.cancel` 是 best-effort；未验证的 `delete_turn` 绝不调用，所以后台不能撤回已发消息。
- 超时后 Muse 侧生成可能继续；控制台只显示“已停止等待”。
- side chat 必须已存在；v1 不创建/不重命名会话。
- 回复归属在主聊天有人同时说话时只能标 `ambiguous`，无法做因果证明。
- 本实现未在真实 muse.ai 上跑探针（任务边界），协议形状来自设计报告与 `muse-cli` 证据；任何 `muse_protocol_drift` 都应停下复核。
- 控制台首次发送要求精确输入成员 `peer_id`（每浏览器会话一次）；这是操作者确认，不是加密签名。
