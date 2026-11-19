# 直接对话会话历史失败：底层原因可观测性报告

- 分支：`feat/muse-chat-diagnose`（起点 `origin/main` 的 merge commit `6060092`）
- 范围：只改 `internal/muse` 直接对话失败路径的审计详情与服务端日志，**不改寻址逻辑、不改产品行为**
- 提交：本报告与实现同一提交（见分支 HEAD）

## 1. 改了什么

### 1.1 `internal/muse/chat_jobs.go`

baseline `History` 失败时：

1. 先用 `chatFailureAs(berr)` 取出底层 `*chatFailure`，不再只看被覆盖后的 `muse_history_unavailable`；
2. 新增一行服务端日志，带 chat / peer / account、底层 `reason`、`failure_stage`、HTTP `status`（若有）；
3. 在 `chatOutcome` 上新增 `FailureReason` / `FailureStatus`，只用于审计；
4. `auditOutcome` 写 `muse.chat.failed` 时追加 `reason=` 和 `status=`；
5. 用户-facing 的 `ChatError.Code` 仍强制为 `muse_history_unavailable`，界面文案和“写前失败、不发送”语义不变。

关键位置：

- `chat_jobs.go:743-764`：baseline 失败捕获、日志、`chatOutcome` 字段；
- `chat_jobs.go:1078-1086`：审计详情追加字段；
- `chat_jobs.go:1286-1294`：`chatOutcome.FailureReason` / `FailureStatus` 定义。

日志行格式（测试抓到的实际格式）：

```text
muse chat: baseline history failed chat=mc_... peer=alice account=ma_... stage=sending error_class=muse_history_unavailable reason=muse_main_chat_unresolved failure_stage=session_list
```

有 HTTP 状态时追加：

```text
muse chat: baseline history failed chat=mc_... peer=alice account=ma_... stage=sending error_class=muse_history_unavailable reason=muse_protocol_drift failure_stage=session_list status=404
```

### 1.2 `internal/muse/chat_live.go`

`resolveSession` 的每个失败返回点都先记一行 `muse chat: session list failed ...`，再返回原来的 `*chatFailure`（返回值、错误码、寻址行为都不变）：

- `l == nil || l.cc == nil` → `muse_upstream_error`，`shape=unavailable`；
- Noise 调用失败（超时、断连、reset、解密失败等）→ `chatFailureFromNoise` 映射后的码，`shape=unavailable`；
- `sessions.list` 非 2xx → `chatStatusFailure` 映射后的码，`shape=unparsed body_bytes=N`；
- 2xx 但 JSON 解析失败 → `muse_protocol_drift`，`shape=invalid_json body_bytes=N`；
- 列表解析成功但认不出主聊天 → `muse_main_chat_unresolved`，`shape=sessions=N primary_field=... thread_field=... entries=... resolved=no`。

为了在 `resolveSession` 的日志里带上 chat/peer，`liveChatSession` 增加了诊断用的 `chatID` / `peerID` 字段，由 job 在第一次 `History` 前写入；这些字段不参与寻址、不写审计数据库，只用于日志。account id 仍取 session 自己的 `accountID`。

`chatSessionsTimeout` 由 `const` 改为 `var`（默认仍是 **4s**），只为让测试能把超时缩短到 80ms；生产行为不变。这是本次唯一为测试引入的可调开关。

### 1.3 日志里有什么、没有什么

会记录：

- chat id（`mc_...`）、peer id、account id；
- 底层码（`reason=`）、stage（`stage=` / `failure_stage=`）、HTTP 状态（`status=`）；
- 会话列表形状摘要：`sessions` 总数、带 `is_primary` 字段的条数、带 `is_thread` 字段的条数、每条的两个布尔值（`P/p`、`T/t`、`-` 表示字段缺失）、是否解析出主聊天（`resolved=yes/no`）。

**不记录**：

- 任何 `session_id` 值；
- token / cookie / 请求头；
- 消息文本、请求 body、上游响应 body 原文；
- WSS URL。

非 2xx / 非 JSON 时只记 `body_bytes=N`（长度），不记内容。`chatAuditValue` 会把 `=`、空白和控制字符替换成 `_`，避免日志/审计字段注入。

## 2. 审计与日志示例

一条典型的“认不出主聊天”失败（以下 id/account 均为测试生成，不是生产数据；测试的 live session account 是 `acct-test`，生产两行会是同一个 `ma_...`）：

审计 `#audit`，action=`muse.chat.failed`：

```text
chat=mc_51205ce6d209f7bd stage=sending error_class=muse_history_unavailable reason=muse_main_chat_unresolved
```

服务端日志（同一次失败两行）：

```text
muse chat: session list failed chat=mc_51205ce6d209f7bd peer=alice account=acct-test stage=session_list reason=muse_main_chat_unresolved shape=sessions=1 primary_field=1 thread_field=1 entries=pT resolved=no
muse chat: baseline history failed chat=mc_51205ce6d209f7bd peer=alice account=ma_1ec714c97e944b71 stage=sending error_class=muse_history_unavailable reason=muse_main_chat_unresolved failure_stage=session_list
```

一条带 HTTP 状态的失败：

```text
# #audit
chat=mc_0f9c7b74f2ece3d1 stage=sending error_class=muse_history_unavailable reason=muse_protocol_drift status=404

# 服务端日志
muse chat: session list failed chat=mc_0f9c7b74f2ece3d1 peer=alice account=acct-test stage=session_list reason=muse_protocol_drift status=404 shape=unparsed body_bytes=20
muse chat: baseline history failed chat=mc_0f9c7b74f2ece3d1 peer=alice account=ma_c09cb9e507c0252b stage=sending error_class=muse_history_unavailable reason=muse_protocol_drift failure_stage=session_list status=404
```

形状摘要图例：

| 片段 | 含义 |
|---|---|
| `sessions=1` | 列表返回 1 条 |
| `primary_field=1` | 有 1 条带 `is_primary` 字段 |
| `thread_field=1` | 有 1 条带 `is_thread` 字段 |
| `entries=pT` | 第一条：`is_primary=false`（小写 p），`is_thread=true`（大写 T） |
| `entries=PT,pt` | 第一条两个字段都为 true，第二条都为 false |
| `entries=--` | 两条都没有这两个字段（旧形状） |
| `resolved=no` | 按现有选择规则没有解析出主聊天 |

## 3. 下次失败时怎么从 `#audit` 读出原因

过滤 action `muse.chat.failed`，看同一 chat 的 `detail`：

- `stage=sending error_class=muse_history_unavailable` 仍然是用户-facing 的“读历史失败、本次未发送”；
- 新增的 `reason=` 才是底层原因；
- 若出现 `status=`，就是上游 HTTP 状态。

`reason` 对照表（都是 `resolveSession` / `chatFailureFromNoise` / `chatStatusFailure` 现有的码，不新增用户错误码）：

| reason | 常见来源 | 下一步 |
|---|---|---|
| `muse_main_chat_unresolved` | `sessions.list` 是 2xx、JSON 也能解析，但按 `is_primary` / 唯一 `is_thread=false` / 旧形状首条都选不出主聊天 | 看同一时间的 `shape=`：`sessions=0` 优先怀疑信封不是裸 `{"sessions":[...]}`（例如 `{"result":{"sessions":[...]}}`）；`sessions=N` 但 flag 缺失/全 false 则是形状漂移 |
| `muse_protocol_drift` + `status=404` | `sessions.list` 在 daemon 上未路由，或路径/方法不匹配 | 核对第 5 节的 `sessions.list` 结论；不要先改寻址 |
| `muse_protocol_drift` 无 status | 2xx 但 body 不是 JSON，或 Noise reset / bad_decrypt / 不支持的版本 | 看 `shape=invalid_json body_bytes=N`；检查 daemon 协议版本 |
| `muse_upstream_error` + `status=5xx` | 上游 5xx | 上游故障，稍后重试；不是寻址问题 |
| `muse_upstream_error` 无 status | Noise 传输层非超时错误、连接断开、消费端停滞等 | 看日志 `shape=unavailable`；检查连接/网关 |
| `muse_upstream_timeout` | 4s `chatSessionsTimeout` 内没拿到 `sessions.list` 响应，或 Noise timeout | 看 `shape=unavailable`；上游慢或连接卡住 |
| `muse_needs_reauth` | 401 / Noise 401 | token/cookie 失效，走重新授权 |
| `muse_egress_forbidden` | 403 | 出口/权限问题 |
| `muse_upstream_rate_limited` | 429 | 上游限流，稍后重试 |

如果审计只有 `reason=muse_main_chat_unresolved`，直接去服务端日志抓同一 chat 的 `muse chat: session list failed ... shape=...`，形状摘要会告诉我们列表到底长什么样；不需要、也不应该把 `session_id` 打进日志。

## 4. 重新部署 + 重试验证清单

> 部署按仓库规则走 Release/console，不在本任务里手工改生产二进制；以下只列验证动作。

1. 部署包含本提交的构建，确认 gateway 已重启到新二进制。
2. 从控制台给 erdan 发一条 `pwd` 直接对话，记下 chat id 和时间（不要记 token/cookie）。
3. 若成功：
   - `#audit` 应出现 `muse.chat.sending` 和 `muse.chat.sent`，没有同 chat 的 `muse.chat.failed`；
   - 服务端日志不应出现 `muse chat: session list failed`。
4. 若再次失败：
   - 在 `#audit` 过滤 `muse.chat.failed`，读 `reason=` / `status=`；**原因即定**；
   - 再在服务端 `journalctl -u agent-relay`（或当前日志后端）按 `muse chat: session list failed` + chat id 抓形状摘要；
   - 按第 3 节对照表判断，不要先改寻址逻辑；
   - 若 `reason=muse_main_chat_unresolved` 且 `shape=sessions=0`，优先验证第 5 节的信封假设（裸 `sessions` 还是 `result.sessions` 包装）。
5. 把审计 detail 和日志行（保留 chat id 即可，不要带 session_id/token/cookie/正文）贴回排查记录。

## 5. `GET /api/session/list` 走 Noise 连接的假设：核对结论

**结论：分两层。**

### 5.1 已证实（我方代码路径）

- `resolveSession` 调的是 `l.cc.call(ctx, http.MethodGet, chatSessionsPath, nil, nil)`（`chat_live.go:495` 附近），`chatSessionsPath = "/api/session/list"`。
- `chatConn.call` 明确走 `callService(ctx, 0, ...)`；`writeRequestService` 最终调用 `noiseConn.sendServiceFrameWithService(ctx, 0, ...)`。也就是说，生产 chat 路径把 `/api/session/list` 作为 **service 0（daemon）的 ApplicationRequest** 发在已有的 Noise WebSocket 上，不是另开 HTTP/cookie 请求。
- 这条路径没有 HTTP fallback；`sendServiceFrame` 的注释也明确“service 0 = daemon”。

### 5.2 仍是假设（上游 daemon 的路由与信封）

- `muse-cli-ref/src/muse_cli/routes.json` 把 `sessions.list` 映射到 `GET /api/session/list`，且没有 `service` 覆盖，`gateway.py` 因此按 daemon（0）发送；
- 第三方逆向 SDK 的 `Diagnose` 也在同一条 Noise 连接（service 0）上探测 `/api/session/list`；
- 但这些都是**参考实现/候选证据**，不是生产 daemon 的抓包。我们无法从本仓库确认：
  1. 生产 daemon 一定在 Noise service 0 上路由这个 path；
  2. 返回的裸信封一定是 `{"sessions":[...]}`，而不是 `{"result":{"sessions":[...]}}` 或别的包装。
- 现有客户端只按裸 `{"sessions":[...]}` 解析；如果是 `result` 包装，`json.Unmarshal` 不会报错，但 `Sessions` 为空，最终会得到 `reason=muse_main_chat_unresolved`、`shape=sessions=0`。这正是新增日志要区分的场景。
- **因此：`/api/session/list` 走 Noise 连接这一点已证实；上游路由和裸信封形状仍是假设，等下一次失败（或成功）的 `reason` + `shape` 给出生产答案。本次按要求没有改寻址逻辑。**

## 6. 测试

新增 `internal/muse/chat_diagnose_test.go`，用 `musetest` 的 fake Noise 上游跑完整 job 路径，覆盖：

| case | 上游行为 | 期望审计/日志 `reason` | `status` |
|---|---|---|---|
| `sessions_list_404` | 非 2xx 404 | `muse_protocol_drift` | 404 |
| `sessions_list_502` | 非 2xx 502 | `muse_upstream_error` | 502 |
| `sessions_list_json_drift` | 2xx，`sessions` 不是数组 | `muse_protocol_drift` | 无 |
| `sessions_list_no_main_chat` | 2xx，只有 side chat，无主聊天 | `muse_main_chat_unresolved` | 无 |
| `sessions_list_noise_timeout` | handler 不响应，超过测试缩短的 deadline | `muse_upstream_timeout` | 无 |

每个 case 断言：

- 用户-facing `ChatError.Code` 仍是 `muse_history_unavailable`；
- `muse.chat.failed` 审计 detail 含 `stage=sending`、`error_class=muse_history_unavailable`、`reason=<底层码>`，有 HTTP 状态时含 `status=<码>`；
- 日志同时含 `session list failed` 与 `baseline history failed` 两条，含 chat id、peer、account、stage、底层码、`failure_stage=session_list`，有状态时含 `status=`；
- 没有发出 `/chat/stream` 请求（写前失败）；
- 审计和日志都不含 fake 上游的 body 内容或 `session_id`（测试里放了 `upstream-body-secret`、`body-secret`、`side-secret-id` 作为哨兵）；
- `sessions_list_no_main_chat` 还断言形状摘要含 `sessions=1 primary_field=1 thread_field=1 entries=pT resolved=no`。

## 7. 验证

全部在仓库内 `.gocache/` 下执行（Go 1.27.4）：

| 命令 | 结果 |
|---|---|
| `gofmt -l internal/muse/chat_jobs.go internal/muse/chat_live.go internal/muse/chat_diagnose_test.go` | 无输出（已格式化） |
| `go vet ./...` | 退出 0 |
| `go build ./...` | 退出 0 |
| `go test ./... -count=1` | 退出 0，所有包 ok |
| `go test -race ./internal/muse/... -count=1` | 退出 0（约 51s） |
| `go test -race ./internal/gateway/... -count=1` | 退出 0（约 65s） |
| 新增定向测试 `TestChatBaselineFailureAuditsAndLogsUnderlyingReason` | 5/5 子测试 PASS |

说明：`go build` 期间 Go 工具链对共享 module cache（`/home/hatch/go/pkg/mod/...`，root 所有）有一条写 `.info` 的权限提示；退出码仍为 0，编译/链接无错误，属于沙箱环境权限噪声，不是代码失败。

## 8. 明确没有改的东西

- 没有改 `resolveSession` / `mainChatSessionID` 的寻址规则和缓存逻辑；
- 没有改用户-facing 错误码、界面文案、写前失败语义、`delivery_unknown` 语义、side chat 行为；
- 没有改 prompt、receiver、`relay-watch.sh`、guard、探针端点、R3 卡片、虚拟列表、数据库 schema；
- 没有开 PR、没有合并、没有打 tag。
