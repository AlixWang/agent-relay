# Muse 直接对话（v2）

后台以成员已授权的 muse.ai 登录态，通过原生 Noise 网关的 `chat.stream` / `chat.history` 发纯文本并读回复。默认关闭，admin-only，逐成员由管理员一键开通；消息进入成员自己的 Muse 主聊天（或指定且已存在的 side chat），成员可见。**不是**任务下发、不是回执、不是审批通道。

v2 相对 v1 的变化：删除开通同意弹窗与两次发送确认，`chat_enabled` 从“成员同意记录”改为“管理员操作开关”；对话从 modal 抽屉改为独立页面 `#muse-chat`，线程来自内存 job（1 小时）；审批仍只做隐藏探针，**没有**审批卡片 UI。

## 三层开关

1. **部署门** `muse_direct_chat_enabled`（默认 `false`，改动需重启）。
2. **运行时 kill switch**：`POST /admin/muse/chat/settings {enabled}`，即时生效；关闭时停止在途 job 的本地轮询并关连接，但**不**逐条发送 `chat.cancel`，避免关闭动作放大成上游 RPC 洪峰。
3. **成员开关** `muse_accounts.chat_enabled` 默认 0，语义是管理员操作开关，不再是成员同意记录。成员行“对话”按钮：未开通时点一次即开通并进入；已开通直接进入。开通只写一条 `muse.chat.access_grant` 审计，不审计每次打开页面。

没有“全部开启”按钮；没有成员自助端点；agent / peer token / webhook / cron 都不能调用。所有 admin 等效，审计 actor 固定 `"admin"`，不伪造人级归因。

### 复用列与旧数据

- `chat_consent_at/by` 复用为 `enabled_at/by`，**零 DDL**；`chat_consent_version/evidence` 停止读写，列保留作废弃（不删除）。
- 旧 opt-in 行 grandfather：升级后仍为开通，语义自动变成管理员开关，不批量重置。
- 旧审计行保留。v1 时期的 `muse.chat.opt_in/opt_out` 含义是“成员同意”，v2 之后不再写入；新行一律用 `access_*`。

### 重置规则

换 cookie（重新授权）或换 `peer_id` 时 `chat_enabled` 仍重置为 0（安全不变量不变：开通跟的是已核验身份绑定），并写 `muse.chat.access_reset`（actor `system`，reason=`reauthorized` / `peer_changed`）。重新开通只需在成员行再点一次“对话”。

## 对话页

- 子路由 `#muse-chat?peer=<peer_id>&session=<session_id>`，属于 Muse 信息架构，无侧栏项；唯一入口是 Muse 成员行的“对话”按钮。不复用 `#page-threads`。
- 桌面双栏：左侧已开通成员，右侧线程 + 常驻输入框；移动端左栏变抽屉（`.cmd-layout.sessions-open`）。复用 `.cmd-*` 视觉，但是独立页面。
- 线程来源只有本进程内存 job：`GET /admin/muse/chat/thread?peer_id=...&session=...` 返回最近 1 小时的 jobs（操作者已发文本、回复、状态）。刷新后同进程 1 小时内可恢复；进程重启或超过 1 小时显示边界提示 + “去 Muse 查看”。**不落库、不读 `chat.history` 回填**。
- 历史文本只在 thread 端点返回（admin-only、不写日志/审计）；job 轮询端点 `GET /admin/muse/chat/{id}` 仍不返回文本。
- 一次一个在途 job：发送按钮在 active 时禁用（仍可继续打字）。收到 409 `muse_chat_in_progress` 时自动附着现有 `chat_id` 读取进展；不排队。
- 可通过页眉下拉切换线程，也可在「对话设置」输入已有线程 ID。`session` 参数标识线程，未知 ID 由服务端返回 `unknown_session`。
- 「对话设置」可调整等待回复的时长，默认 180 秒，范围 30–300 秒，受服务端 `max_budget_secs` 上限约束。倒计时从确认投递开始。
- 对话进行中，输入区显示「停止」。确认后调用 `chat.cancel`。「对话设置」内可查看运行时状态、返回 Muse 页或停用成员。停用仍需确认，记录 `access_revoke` 审计并停止本地等待。
- 输入框上方常驻提示，说明成员可见、消息不可撤回、可能触发 agent 操作，以及原生审批入口。
- 异常按状态/错误码呈现，**绝不自动重发、绝不伪造成功**。`delivery_unknown` 只能新发一条（新 `client_request_id`）并重新确认；`timeout` 不自动 cancel；`ambiguous` 照旧标注。
- 任务隔离：独立路由/视觉/输入；没有 task/result、task/chat kind、@、ack。

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
- `sending`：读 baseline（`chat.history`）+ 最近 5 条 busy check，然后 `muse.chat.sending` 审计；审计写不进去就不发。之后写 `chat.stream`。
- `confirming`：写前失败或 `chat.stream` 明确拒绝（401/403/404/429）直接报错。请求可能已写入时，即使流 reset、断连或 echo 超时（≤10s），也只读 history 确认（≤5s）。仅匹配到 baseline 之后自己的 `message.user` 才算已投递；未匹配或无法读取 → `delivery_unknown`，**绝不自动重发**。
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

只写 `audit_log` 元数据，不写全文/回复全文/审批标题/详情/凭证：

| action | actor | 关键字段 |
|---|---|---|
| `muse.chat.access_grant` | `admin` | account, peer, by, reason, source=console |
| `muse.chat.access_revoke` | `admin` | account, peer, by, reason, source=console |
| `muse.chat.access_reset` | `system` | account, peer, reason=reauthorized/peer_changed |
| `muse.chat.runtime_flag` | `admin` | enabled, by |
| `muse.chat.disabled` | `admin` | active_jobs, reason=kill_switch |
| `muse.chat.sending` | `admin` | chat, peer, account, session, baseline, text_len, text_sha256, request_id, access=1 |
| `muse.chat.sent` | `system` | chat, echo, user_seq, delivery=confirmed, elapsed_ms |
| `muse.chat.replied` | `system` | chat, reply_seq, reply_id, reply_status, elapsed_ms, polls, confidence |
| `muse.chat.timeout` | `system` | chat, partial, last_seq, elapsed_ms, polls, history_reads, history_errors, new_event_polls, last_seq_advanced, assistant_events, draft_events |
| `muse.chat.cancelled` | `system` | chat, cancel=sent/unsupported/skipped |
| `muse.chat.failed` | `system` | chat, stage, error_class |
| `muse.chat.delivery_unknown` | `system` | chat, reason, last_seq |

`sending` 审计失败会阻断发送；终态审计失败只记服务端日志（消息可能已发出，不能回滚）。保留期沿用 `audit_retention_days`（默认 30 天）。旧 `muse.chat.opt_in/opt_out` 行保留但不改写。

## 审批探针（隐藏，非卡片 UI）

本轮只为设计报告 §4.5 的 P0–P2 提供入口，**没有**审批发现轮询、卡片、关联或决策审计（R27–R33 未做）。探针是 admin-only、默认关闭、明确标注的实验入口：

- 复用某个在途 chat job 的既有 Noise 连接与 lease，不新建连接；service 1 = sentinel。
- 请求必须带 `"probe": true`，否则 403 `muse_probe_disabled`。
- 输出只回到请求方（探针环境），不写审计、不写服务端日志；不要把返回正文贴进报告或工单。
- 只测 `deny` / `allow_once`；`allow_always` 在代码里直接拒绝（400 `probe_allow_always_forbidden`）。
- 每个请求只做一步，不批量、不自动、不重试。

### P0：service 1 可达

先开一个对话 job（保持 `waiting`，预算留足），然后：

```bash
curl -sS -b "<admin cookie>" -H 'content-type: application/json' \
  -d '{"chat_id":"mc_...","probe":true}' \
  https://<relay>/admin/muse/chat/probe/approvals
```

`upstream_status` 为 2xx 即 P0 通过；失败返回 502 并带 `upstream_status` 与原响应正文。不要求有待批项。

### P1：待批项全字段

同一端点返回 `body`（`/approvals` 原始 JSON，约 123KB）。在探针环境内检查每条 pending 项的 `decision_options` 与 `scope` 字段。**只记录字段名与形状，不把标题/详情写进报告。**

### P2：逐条决策

对一条待批项，先 `deny`，每种请求体变体单独发一次（`with_id` / `decision_only` / `empty`），每次只发一条：

```bash
curl -sS -b "<admin cookie>" -H 'content-type: application/json' \
  -d '{"chat_id":"mc_...","probe":true,"approval_id":"ap_...","decision":"deny","variant":"with_id"}' \
  https://<relay>/admin/muse/chat/probe/decide
```

- 2xx 只表示“已提交”，不是“已生效”。随后再次调用 `/probe/approvals` 看状态变化，并在 30 秒内观察 agent 是否继续；没有继续就显示/记录“已提交，未见继续”。
- `deny` 之后再对另一条（或经操作员确认的同一条新请求）试 `allow_once`；不测 `allow_always`。
- 任一步失败：关运行时开关并停止；不要扩大范围。范围仅 erdan、先 staging、操作员在场。

## v2 落点（R1–R26）

| # | 实现 |
|---|---|
| R1–R3 | `chat_enabled` 管理员开关，默认 0；仅 admin；持久到停用/reauth/peer 变更 |
| R4–R5 | 换 cookie / 换 peer 重置 + `access_reset`（system） |
| R6–R7 | 删除 peer_id 输入确认与每次发送确认；保留可见性提示 + 首条内联提示 |
| R8 | 部署门/运行时开关不变 |
| R9–R10 | 列复用、零 DDL；旧 opt-in grandfather；version/evidence 停读写 |
| R11–R12 | actor 固定 `"admin"`；新增 access_reset |
| R13–R15 | `#muse-chat` 子路由；仅成员行入口；桌面双栏/移动单栏 |
| R16–R17 | thread 端点只读内存 1h；job GET 仍无文本 |
| R18 | 单在途；409 附着；不排队 |
| R19 | session 属线程身份，切换不新建 sid |
| R20 | 预算默认 180，30–300，确认投递后倒计时 |
| R21 | 停止确认 + cancel；关闭回列表；停用 danger + revoke |
| R22 | 聊天页只读徽标 + 回 Muse 链接 |
| R23 | 状态/错误码表；不自动重发、不伪造成功 |
| R24 | 无 task/result、kind、@、ack |
| R25 | 先探针；R27–R33 不做 |
| R26 | `sendServiceFrameService` 显式 service；读循环不变；复用 job 连接 |

## 已知限制

- job 和回复文本只在内存；进程重启后 404，页面只显示边界提示。全文以成员 Muse 会话为准。
- 不读 `chat.history` 回填，所以刷新后只能看到本进程 1 小时内的操作者已发消息；成员在 Muse 里发的其他消息不会出现在后台。
- `chat.cancel` 是 best-effort；未验证的 `delete_turn` 绝不调用，所以后台不能撤回已发消息。
- 超时后 Muse 侧生成可能继续；控制台只显示“已停止等待”。
- side chat 必须已存在；v2 不创建/不重命名会话。
- 回复归属在主聊天有人同时说话时只能标 `ambiguous`，无法做因果证明。
- 审计 actor 只能是 `"admin"`，无法区分具体管理员；本改版不伪造人级归因。
- 审批卡片 UI、审批轮询、审批关联、决策审计均未实现；探针尚未在真实 muse.ai 上跑过。
- 旧 v1 审计行（`opt_in/opt_out`）的语义是成员同意，不能与 v2 的 `access_*` 混读；以发布时间区分。
