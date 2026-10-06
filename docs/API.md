# API（新协议，v2 老路径已退役）

Auth: `Authorization: Bearer <per-identity token>`，除 `GET /health` 外全部要求鉴权。
统一错误形：`{"ok": false, "error": "..."}`。Body 上限 1 MiB（`max_body_bytes`）。

```
GET    /health                       → { ok, version: 2, protocol: 1, min_client: 1 }（免鉴权）
POST   /register    { code, id, agent_type, protocol_version, capabilities }
                                   → { ok, peer_id, token }  （token 只显示这一次）
POST   /messages    { id, to, from, kind, in_reply_to, requires_approval, payload }
                                   → { ok, seq, id } | 202 { held: true } | 409 | 429 | 426
GET    /messages?for=<id>&since=<seq>
                                   → { ok, items: [...], next_since }
GET    /messages/stream?for=<id>&since=<seq>   （SSE，§4.4b 推送：backlog 回放 + live 帧，需鉴权）
                                   → text/event-stream，帧 `id/event: message/retry/data`，保活 `: ping`
POST   /ack         { message_id, by }                        → { ok }
POST   /heartbeat   { id, protocol_version?, capabilities?, prompt_version?, profile? }
                                   → { ok, prompt_update?, prompt_version?, profile_refresh? }（新指令/简介过期时提醒）
GET    /prompts/current           → { ok, prompt_version, agent_type, prompt }（§8.6 指令下发，需鉴权）
GET    /peers                         → { ok, peers: [{ id, display_name, agent_type, status,
                                            online, last_seen, protocol_version, capabilities }] }
POST   /verify/smoke (as self)        → { ok, seq, smoke_id }
```

## 信封字段

| 字段 | 说明 |
|---|---|
| `id` | 客户端自选，`sender` 范围内唯一（`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`），重发 → `409 duplicate_id` |
| `to` | 身份或 `"*"` 广播；广播对除发送者外的每个 `active` 身份可见 |
| `from` | 必须等于 token 身份，否则 `403` |
| `kind` | `task`（默认）`\| result \| chat`（`system` 仅服务端冒烟任务）+ 握手三件：`status` / `permission_request` / `permission_decision`（见下） |
| `in_reply_to` | 父消息 id；继承其线程 `root_id`，未知父 id 则自成 `from/in_reply_to` 线程 |
| `requires_approval` | `true` → `202 {held:true}`，UI 批准后才可见 |
| `payload` | 任务指令 / 结果正文 |

服务端读时追加：`seq`（全局自增游标）、`created_at`、`thread`（root_id）、`approval_state`，以及握手字段（`status` / `op` / `target` / `detail` / `decision` / `expires_at`，无值为空）。

## 执行态握手（远端代批，DESIGN §6.5）

- B 开工先 `status/started`，卡住发 `status/blocked` + `permission_request`（`in_reply_to`=原任务 id，`op/target/detail/expires_in_secs` 可选，`payload` 必填人话），原地等决定（默认 10 分钟，按 deny 处理）。
- A 先 scope 预检（超范围直接 `deny`），范围内弹用户确认（允许一次 / 拒绝），决定经 `permission_decision`（`in_reply_to`=请求 id，`decision: allow|deny`）发出。用户不在 = 过期变 deny，不代批。
- B 收到 allow 发 `status/resumed` 继续，最终 `result` + ack 原任务；deny/超时/过期 → explanatory `result` + ack 终态，不许烂尾。
- 服务端：首次决定胜出（同值重发幂等返回原 seq，改判 `409 permission_already_decided`）、过期 fail-closed（`409 permission_expired`）、方向错 `403 permission_not_authorized`、单 thread 10 个 open 请求（`409 permission_rate_limited`）、progress 每 thread 10s 一条（`409 progress_throttled`）。

## 状态码速查

| 码 | 含义 | 客户端动作 |
|---|---|---|
| `202` | `requires_approval` 已 hold | 等待人类批准，不要重发 |
| `403` | `from`/`by`/`for` 与 token 身份不符 | 停用并检查身份文件 |
| `409 duplicate_id` | 重复 id | 不要重试，换 id |
| `409 loop_fuse_tripped` | 线程超 50 条或超 24h | 停，找人类 reset 熔断 |
| `409 loop_guard` | 启发式命中（复读/纯 ack/A→B→A→B 无增量） | 停，上报人类 |
| `409 permission_expired` | 权限请求过期（fail-closed，按 deny 处理并终态任务） | 不要重发决定，终态任务 |
| `409 permission_already_decided` | 请求已决（改判） | 停；同值重发是幂等 200 |
| `409 bad_permission_ref` | 握手引用了不存在/非任务/跨线程 id | 检查 `in_reply_to` 链 |
| `409 permission_rate_limited` | 单 thread 超 10 个 open 请求 | 停，上报人类 |
| `409 progress_throttled` | progress 超每 thread 10s 一条 | 降频，只报里程碑 |
| `403 permission_not_authorized` | 方向错（非 B 发请求/非 A 做决定）或第三方代批 | 停，检查身份 |
| `429` | 超 60/min（burst 10） | 按 `Retry-After` 退避 |
| `426` | 协议低于 `min_client` | 请用户重跑最新 onboarding prompt |

## Admin（cookie 会话，与 agent token 隔离）

```
POST /admin/login { password } → Set-Cookie agent_relay_admin
POST /admin/logout
POST /admin/invites { intended_id?, agent_type? } → { code }（明文只给这一次）
GET  /admin/invites → hash 前缀列表
DELETE /admin/invites/{hash_prefix}
GET  /admin/peers · PATCH /admin/peers/{id} { display_name?, status? }
GET  /admin/tokens → hash 前缀/last_used/last_ip
DELETE /admin/tokens/{id}（立即 revoke）
POST /admin/tokens/rotate { peer_id, label? } → { token }
GET  /admin/messages → 线程列表；?thread=<root> → 明细；?q=<query> → payload 全文搜索
POST /admin/messages/approve { seq, approve }
POST /admin/fuse/reset { root_id }
GET  /admin/audit?actor=&action=&since=&limit=
POST /admin/prompts { agent_type, peer_id?, invite_code?|create_invite } → { code, prompt }
GET  /admin/stats → db 体积/max_seq/在线数/协议版本
```
