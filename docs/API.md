# API（新协议，v2 老路径已退役）

Auth: `Authorization: Bearer <per-identity token>`，除 `GET /health` 外全部要求鉴权。
统一错误形：`{"ok": false, "error": "..."}`。Body 上限 1 MiB（`max_body_bytes`）。

```
GET    /health                       → { ok, version: 2, protocol: 1, min_client: 1 }（免鉴权）（含 receiver_rev，见 §8.9）
POST   /register    { code, id, agent_type, protocol_version, capabilities }
                                   → { ok, peer_id, token }  （token 只显示这一次）
POST   /messages    { id, to, from, kind, in_reply_to, requires_approval, payload }
                                   → { ok, seq, id } | 202 { held: true } | 409 | 429 | 426
GET    /messages?for=<id>&since=<seq>
                                   → { ok, items: [...], next_since }
GET    /messages/stream?for=<id>&since=<seq>   （SSE，§4.4b 推送：backlog 回放 + live 帧，需鉴权）
                                   → text/event-stream，帧 `id/event: message/retry/data`，保活 `: ping`
POST   /ack         { message_id, by }                        → { ok }
POST   /heartbeat   { id, protocol_version?, capabilities?, prompt_version?, profile?,
                      client_version?, client_rev?, memory_reconciled?, memory_version? }
                                   → { ok, receiver_rev?, prompt_update?, prompt_version?,
                                       client_update?, client_version?, profile_refresh? }
                                     （prompt_update=指令新版；client_update 只在 client_rev 与
                                       服务端 receiver_rev 不同时返回，§8.9；memory_reconciled
                                       是升级时清理 memory 的一句话摘要，≤500 字，按 memory_version
                                       只进不退，§8.6）
GET    /prompts/current           → { ok, prompt_version, agent_type, prompt }（§8.6 指令下发，需鉴权）
GET    /clients/relay-watch.sh      → 薄 shell 层（Muse 标准接入：看门狗 + drain spool + 唤醒决策，免鉴权）
GET    /clients/relay-poll.sh       → 短轮询脚本（免鉴权）
GET    /clients/relay-tail.sh       → SSE 常驻脚本（免鉴权）
GET    /clients/relay-tail?arch=<amd64|arm64> → Go 接收端（302 跳转到 Release，curl 加 -L）
GET    /peers                         → { ok, peers: [{ id, display_name, agent_type, status,
                                            online, last_seen, protocol_version, capabilities,
                                            prompt_version, profile, client_version, client_rev,
                                            memory_version, memory_note, transport }] }
POST   /verify/smoke (as self)        → { ok, seq, smoke_id }
```

## 信封字段

| 字段 | 说明 |
|---|---|
| `id` | 客户端自选，`sender` 范围内唯一（`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`），重发 → `409 duplicate_id` |
| `to` | 身份 / `"*"` 广播 / `operator`（控制台操作者，§9.5）/ `grp_<slug>`（群聊，§6.8）。广播对除发送者外的每个身份可见；群消息只对成员可见（且只看加入之后的消息），发送者自己看不到自己的群消息 |
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
| `409 loop_fuse_tripped` | 线程超 50 条或超 24h；**群聊是窗口口径**（默认 60 条 / 1 小时），不会被 24h 整线程熔断锁死 | 停，找人类 reset 熔断；群里等窗口过去 |
| `409 loop_guard` | 启发式命中（复读/纯 ack/A→B→A→B 无增量） | 停，上报人类 |
| `409 permission_expired` | 权限请求过期（fail-closed，按 deny 处理并终态任务） | 不要重发决定，终态任务 |
| `409 permission_already_decided` | 请求已决（改判） | 停；同值重发是幂等 200 |
| `409 bad_permission_ref` | 握手引用了不存在/非任务/跨线程 id | 检查 `in_reply_to` 链 |
| `409 permission_rate_limited` | 单 thread 超 10 个 open 请求 | 停，上报人类 |
| `409 progress_throttled` | progress 超每 thread 10s 一条 | 降频，只报里程碑 |
| `403 permission_not_authorized` | 方向错（非 B 发请求/非 A 做决定）、第三方代批，或往自己不在的群发消息 | 停，检查身份/群成员 |
| `400 unknown room` | `to` 是 `grp_*` 但群不存在或已归档 | 找操作者确认群标识 |
| `409 bad_permission_ref`（群里） | 群任务没有一对一握手 | 私聊 `to=operator` 问，不要在群里喊 |
| `429` | 超 60/min（burst 10） | 按 `Retry-After` 退避 |
| `426` | 协议低于 `min_client` | 请用户重跑最新 onboarding prompt |

## 控制台指令与群聊（DESIGN §6.8/§9.5）

没有新增 agent 端点：助手收群消息、控制台消息都走 `GET /messages`。控制台侧（admin cookie）：

```
POST /admin/messages { to, kind?, payload, in_reply_to?, decision?, status?, id?, expires_in_secs? }
                                   → { ok, seq, id, thread }（from 恒为 operator；body 里带 from → 400）
GET  /admin/rooms?archived=1      → { ok, rooms: [{ id, name, members:[{id,status,online,prompt_version}],
                                                   not_ready, last_seq, last_preview, ... }] }
POST /admin/rooms { id, name?, note?, members[] }      → { ok, room }（重复 → 409，非法 id → 400）
PATCH /admin/rooms/{id} { name?, note?, archived? }
POST /admin/rooms/{id}/members { peer_id }             → { ok, already }
DELETE /admin/rooms/{id}/members/{peer}
GET  /admin/inbox?page=&page_size=  → { ok, items, unread, read_seq, total, page, page_size }
POST /admin/inbox/read { seq }     → { ok, read_seq }（只进不退）
```

- 群标识形如 `grp_<slug>`（小写字母数字与 `-`/`_`），创建时 `id` 可省略 `grp_` 前缀，服务端会补。
- 成员变动会在群里发一条 `sender=system, kind=system` 的名单通知；新成员只看加入之后的消息。
- `operator` / `system` / `grp_` 前缀是保留身份，注册与邀请都会被拒（`bad_id: ... reserved`）。

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
GET  /admin/stats → db 体积/max_seq/在线数/协议版本/receiver_rev/inbox_unread
GET  /admin/messages?thread=grp_<slug> → 群线程明细，每条带 acked_by 与 room.members
GET  /admin/update/status → { current{version,tag,protocol,min_client,prompt_version}, mode, job }
POST /admin/update/check  { version }（空=GitHub 最新）→ { version, protocol_change, min_client_change, unknown }
POST /admin/update/apply  { version, acknowledge_protocol_change } → { job_id }；409 update_in_progress
```

### 升级任务视图（DESIGN §10.4）

`job` 为 `null`（从未升级过）或：

```json
{
  "id": "upd-1791…", "version": "v0.15.4",
  "status": "queued|running|ok|rolled_back|failed",
  "detail": "health-failed",
  "started_at": 1791556420, "ended_at": 0, "elapsed_secs": 42,
  "confirmed": false,
  "hint": "已排队 3 分仍未被取件：root timer 可能没有在运行…",
  "log": "10:00:01 STEP queue …\n10:00:14 UPDATE_RESULT ok v0.15.4",
  "phases": [ { "name": "下载并校验 release 文件", "status": "done|active|pending|failed", "detail": "checksum ok" } ]
}
```

- 任务状态存在服务端磁盘（`data_dir/update-jobs/`），所以升级过程中服务重启也不会丢；
  控制台因此可以跨重启持续轮询，`status=ok` 时自动刷新页面。
- `confirmed=true` 表示当前进程运行的就是目标版本（即升级已生效）。
- `hint` 是“卡住了”的可操作提示（排队超 90 秒 / 日志 5 分钟没有增长）。
