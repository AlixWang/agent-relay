# 直接对话“确认回读”缺陷修复报告

- 分支：`feat/muse-chat-confirm-fix`（从 `origin/main` `d4b0144` 起步）
- 范围：只修 `internal/muse` 的直接对话确认寻址及其测试；未改 prompt、receiver、`relay-watch.sh`、guard、探针端点权限、R3 权限卡片 UI、虚拟列表。
- 结果：修复后空 `session_id` 的主聊天发送与历史回读使用同一个显式会话标识，确认循环能看到刚写入的 `message.user`；无法解析主聊天时在写入前失败，不会把“不知道发没发出”报成“已发出”。

## 1. 根因

### 1.1 生产证据

据本次任务提供的 2026-11-19 生产审计（v1.12.0）：

- 两条发给 erdan 的只读 `pwd` 直接对话（chat `mc_3e2fecd5e3f68c51` 16:58、`mc_fb6a22e77fe40d4a` 17:02，预算 300s）都以 `delivery_unknown` 收尾。
- `muse.chat.sending` 均为 `session=main baseline=0`。
- `muse.chat.delivery_unknown` 均为 `reason=no_user_event last_seq=0`。
- 发送与未知相隔正好 5 秒，等于确认预算。
- 两条消息实际都送达：以用户提问形式出现在 erdan 的主聊天，并得到 `/home/hatch` 回复。
- Lisa 的直接对话列表同样显示“投递未知”，说明不是 erdan 账号个例。

### 1.2 代码路径

| 环节 | 位置 | 行为 |
|---|---|---|
| 建 job | `internal/muse/chat_jobs.go:203-204, 324` | 请求未带 `session_id` 时 `job.SessionID == ""`，`Session=="main"`、`Visibility=="member_main_chat"` |
| 读 baseline | `internal/muse/chat_jobs.go:738` | `sess.History(ctx, job.SessionID, 5, 0)`，即 `History("", ...)` |
| 发送 | `internal/muse/chat_jobs.go:764` | `sess.Send(ctx, job.SessionID, ...)`，即 `Send("", ...)` |
| 确认 | `internal/muse/chat_jobs.go:832` | 5s 预算、每 250ms 调一次 `History("", ...)`；`chatConfirmBudget`/`chatConfirmEvery` 在 `chat_live.go:39-40` |
| 匹配用户事件 | `internal/muse/chat_events.go:273` | `findChatUser` 按 `message.user` + 文本哈希 + `seq > baseline` 匹配 |
| 失败收尾 | `internal/muse/chat_jobs.go:782, 798` | 找不到即 `delivery_unknown`，审计 `reason=no_user_event last_seq=...`（`chat_jobs.go:1043-1045`） |

修复前的 `internal/muse/chat_live.go` 中：

- `Send`（原约 :368）仅在 `sessionID != ""` 时写入 `payload["session_id"]`；为空时省略，上游默认写主聊天。
- `History`（原约 :313）仅在 `sessionID != ""` 时给 `/chat/history` 加 `session_id`；为空时省略，上游返回空历史。
- `chatHistoryPath`（原约 :334）同样按“空即省略”拼 URL。

写路径和读路径对“空 `session_id`”的解释不一致：发送落到主聊天，回读却读到空会话。于是 `baseline=0`、确认窗口内 `last_seq=0`，永远找不到刚发的用户事件。

## 2. 上游如何定位主聊天会话

### 2.1 上游有列会话接口

`~/workspace/dsh/muse-cli-ref/src/muse_cli/routes.json:108-111`：

```json
{
  "method": "sessions.list",
  "http": "GET",
  "path": "/api/session/list"
}
```

第三方逆向 SDK `~/workspace/dsh/.scratch/muse-protocol-go/muse/proto.go:1365` 也在同一条 Noise 连接（service 0）上直接请求 `/api/session/list`，说明它不是只能走网页 cookie 的接口，而是 daemon 可用接口。

### 2.2 主聊天在会话列表里有真实 ID

- `muse-cli` 的 bridge 用 `is_primary`（缺省时用 `!is_thread`）判断主聊天，再把它在 UI 层规范化为 `session_id: null`：`muse-cli-ref/src/muse_cli/bridge.py:199-211`。
- 其测试直接给出的主聊天原始形状是 `{"session_id":"m","is_primary":true,"is_thread":false,...}`：`muse-cli-ref/tests/test_bridge.py:210-217`。
- demo 服务器的主聊天同样有真实 ID：`muse-cli-ref/src/muse_cli/demo.py:214`（`MAIN = "demo-main"`）。
- `docs/TUI.md:81-85` 里的 `session_id: null` 是 bridge/UI 规范化后的形状，不是原始上游响应。

结论：**主聊天的真实 `session_id` 可以从 `GET /api/session/list` 取得；不能把“UI 里显示为 null”当成“上游没有 ID”。**

### 2.3 发送回显带什么

- `muse-cli-ref/docs/PROTOCOL.md:106` 与 SDK `muse/proto.go:360-380` 表明：`/chat/stream` 的首个流记录可能带 `message_id`；SDK 会解析它作为本轮锚点。
- 现有 `Send` 只统计回显的 NDJSON 行数/合法性（`chat_live.go` 的 `countFullLines`），没有解析 `message_id`；回显本身也不带可用的 `session_id`。
- 即使解析出 `message_id`，它只证明上游接受了请求，不能替代历史回读证明 `message.user` 已落库；因此本次没有把回显降级为投递证据。

### 2.4 不带 `session_id` 的 history 实际返回什么

- 生产审计已给出直接答案：`chat_events` 为空，`baseline=0`、`last_seq=0`。
- `muse-cli-ref/src/muse_cli/cli.py:578` 的注释“`chat.history` without session_id reads the main chat”与生产观察相矛盾；该注释所依赖的是 demo/CLI 的简化行为，不能作为 native Noise 路径的真相。
- 第三方 SDK 从来不给主聊天省略 `session_id`，它在 `muse/proto.go:513` 始终用显式 `session_id` 读 `/chat/history`。

## 3. 选定修复方案

### 3.1 方案：显式寻址主聊天

在 `internal/muse/chat_live.go` 增加每连接一次的主聊天解析：

1. `resolveSession(ctx, sessionID)`：
   - `sessionID != ""`：原样返回（side chat 行为不变，不额外查列表）。
   - `sessionID == ""`：调 `GET /api/session/list`，从 `sessions` 中选主聊天，缓存到 `liveChatSession.mainSessionID`。
2. 选择规则（`mainChatSessionID`）：
   - 优先 `is_primary == true` 的项；
   - 否则若恰有一个 `is_thread == false`，取它；
   - 若响应完全没有 `is_primary`/`is_thread` 字段（旧形状），按文档顺序取第一项（主聊天在前）；
   - 其他歧义/缺失情况返回空，不做猜测。
3. `History`、`Send`、`Cancel` 都先调用 `resolveSession`，统一使用显式 `sid`：
   - `/chat/stream` body 始终带 `"session_id": sid`；
   - `/chat/history?...&session_id=sid`；
   - `/chat/cancel` body 带 `session_id`。
4. 解析失败发生在任何写之前：`Send` 返回 `Wrote=false` 的错误，job 在 baseline 阶段即 `failed`（客户端看到 `muse_history_unavailable`，本次未发送），不会出现“先发出去、再报不知道”。

关键代码：

- `chat_live.go:34`：`chatSessionsPath = "/api/session/list"`、`chatSessionsTimeout = 4s`
- `chat_live.go:340`：`mainChatSessionID`
- `chat_live.go:379`：`resolveSession`
- `chat_live.go:413`：`History` 显式寻址
- `chat_live.go:472`：`Send` 显式寻址
- `chat_live.go:618`：`Cancel` 显式寻址

### 3.2 为什么不是其他方案

| 备选 | 否决理由 |
|---|---|
| 主聊天发送时生成一个新的 UUID 作为 `session_id` | 会把消息写进一个新的 side chat，不再进入成员主聊天，改变产品语义。 |
| 保持发送省略 `session_id`，只修 history 的默认解析 | 上游 history 没有可靠的“空即主聊天”默认；生产已证明返回空。绕不开显式 ID。 |
| 把 `chat.stream` 回显当投递证据，history 降为尽力而为 | 回显只证明请求被接受，不能证明 `message.user` 落库；会弱化 `delivery_unknown`，可能把“不知道”报成“已发出”。本次寻址可修，不需要这种取舍。 |
| 请求体写 `"session_id": null` 期待上游按主聊天处理 | 未在本沙箱验证，且与“显式正确标识”目标不符；不同接口对 null 的解释仍可能不一致。 |
| 直接复用 `muse-cli` 的“空 scope 即主聊天” | CLI 注释与生产行为矛盾；CLI 的 demo 把空 session 默认到主聊天，不能代表 native daemon。 |

### 3.3 语义保持

- `delivery_unknown` 未删除、未弱化：真实“已写但 5s 内确认不到”的情况仍会报 `delivery_unknown`，且仍不自动重发。
- 新增的“主聊天无法解析”是**写前失败**，不是 `delivery_unknown`；不会误报“已发出”。
- side chat 的 `session_id` 原样传递，不查列表、不改写。
- `job.SessionID` 仍保持空（主聊天身份），因此 `ChatThread` 的主聊天过滤 `job.SessionID == ""`、线程页和审计 `session=main` 均不变；显式 ID 只存在于传输层。
- `chat.cancel` 也改用同一显式 ID，避免主聊天取消仍落到默认会话。

## 4. 改动文件

| 文件 | 改动 |
|---|---|
| `internal/muse/chat_live.go` | 新增 `chatSessionListEntry`/`chatSessionList`、`mainChatSessionID`、`resolveSession`；`History`/`Send`/`Cancel` 统一显式寻址；连接内缓存主聊天 ID；history/send/cancel 的错误 scope 判断改为显式会话。 |
| `internal/muse/chat_live_test.go`（新增，315 行） | 用 `musetest` 模拟“发送省略 session 写主聊天、history 省略 session 返回空、sessions.list 返回主聊天真实 ID”的上游；覆盖修复后 Send/History 一致、确认 `findChatUser` 命中、side chat 不改写、主聊天无法解析时零写入、主聊天选择规则、以及 job 到达 `waiting` 并写 `muse.chat.sent delivery=confirmed`。 |
| `internal/muse/chat_jobs_test.go` | 仅把测试 helper `start` 的参数从 `*fakeChatGateway` 放宽为 `chatGateway`，让 live session 也能注入；生产逻辑未改。 |

未改：prompt、receiver、`relay-watch.sh`、guard、探针端点及权限、R3 权限卡片 UI、虚拟列表、任何数据库 schema。

## 5. 验证结果

环境：Go `1.27.4` linux/amd64；本沙箱 `go` 不在默认 PATH，使用 `/home/hatch/sdk/go/bin/go`；构建缓存放仓库内被 `.gitignore` 忽略的 `.gocache/`，避免 `/tmp` tmpfs 满。

### 5.1 新增测试

```bash
go test ./internal/muse/ -run 'TestLiveChatSession|TestMainChatSessionID|TestChatEmptySession' -v
```

结果：全部通过，包括：

- `TestLiveChatSessionResolvesMainChatForSendAndHistory`
- `TestLiveChatSessionSideChatIsNotRemapped`
- `TestLiveChatSessionUnresolvedMainChatNeverWrites`
- `TestMainChatSessionIDSelection`（primary / unthreaded / 文档顺序 / 无主聊天 / 歧义 / null id）
- `TestChatEmptySessionReachesWaitingAndAuditsConfirmed`

### 5.2 缺陷复现（修复前）

在 `HEAD d4b0144` 的独立 worktree 中运行一个仅验证“空 session Send 落地、History 回读”的诊断测试（未提交，仅用于证明复现）：

```text
DEFECT REPRODUCED: empty-session history returned 0 events (message did land in main-1)
--- FAIL: TestPreFixReproEmptySessionHistory
```

修复后同一路径的 `TestLiveChatSessionResolvesMainChatForSendAndHistory` 通过，`findChatUser` 命中 seq=1。说明测试确实覆盖了本缺陷，而不是只覆盖 happy path。

### 5.3 全量门禁

| 命令 | 结果 |
|---|---|
| `go vet ./...` | 通过（`VET_OK`） |
| `go build ./...` | 通过（`BUILD_OK`） |
| `go test ./...` | 全部包通过（`TEST_OK`；muse、gateway、web、store 等） |
| `go test -race ./internal/muse/...` | 通过（49.198s） |
| `go test -race ./internal/gateway/...` | 通过（64.292s） |

UI 未改，未运行 `make test-ui-js`，符合任务约定。

## 6. 部署后生产验证清单

部署/重启后（由用户按正常发布流程操作），按以下顺序核对：

1. **发一条只读测试消息**：在 `#muse-chat?peer=erdan` 发 `pwd`，预算 300s。
2. **状态应完整走到 waiting**：`preparing → sending → confirming → waiting`；job 快照的 `baseline_seq`、`user_seq`、`confirmed_at` 应非空，`user_seq > baseline_seq`，且 `error == null`。
3. **审计应出现确认成功**：
   - `muse.chat.sending`：`session=main baseline=<主聊天当前最大 seq>`（主聊天非空时应大于 0）。
   - `muse.chat.sent`：`delivery=confirmed user_seq=<刚发消息的 seq>`。
   - 不应出现新的 `muse.chat.delivery_unknown`。
4. **主聊天可见**：erdan 的 Muse 主聊天里应看到这条 `pwd`；随后应收到助手回复，job 进入 `replied`。
5. **待审批项**：在 `waiting` 期间按现有隐藏探针流程调用 `GET /admin/muse/chat/probe/approvals`（必须 admin、必须 `"probe":true`）确认 `/approvals` 可列出待批项；decide 仍只允许 `deny` / `allow_once`，绝不接受 `allow_always`。若消息内容本身不触发审批，换一条会请求权限的只读测试，不要放宽权限或改 guard。
6. **side chat 回归**：对一个已存在的 side chat `session_id` 再发一条，确认仍只落到该 side chat，且 `muse.chat.sent delivery=confirmed`。

### 若仍出现 `delivery_unknown` 如何取证

1. 查该 chat 的审计：
   - `muse.chat.sending` 的 `baseline`；
   - `muse.chat.delivery_unknown` 的 `last_seq`、`reason`。
2. 若 `baseline` 和 `last_seq` 仍都是 0：说明回读仍拿到空历史。先确认消息是否出现在 erdan 主聊天：
   - 已出现 → 写路径正常，问题在读取寻址；需要核对 `GET /api/session/list` 返回的 `sessions[]` 形状（主聊天是否带真实 `session_id`，`is_primary`/`is_thread` 是否符合预期）。不要在报告/日志里粘贴 token、cookie 或上游正文。
   - 未出现 → 不是本缺陷，按发送失败/上游错误另行排查。
3. 若 `baseline > 0` 但 `last_seq` 没增长到 `user_seq`：说明 history 读到了另一个会话或窗口；对比 `baseline`、`last_seq`、`user_seq` 和主聊天实际 seq。
4. 若 job 在 baseline 阶段直接 `failed`（`muse_history_unavailable`，本次未发送）：说明主聊天解析失败；核对 `sessions.list` 是否可访问、是否返回主聊天 ID，以及是否被 `muse_main_chat_unresolved`/`muse_protocol_drift` 归类。此时没有消息发出，不会产生新的 `delivery_unknown`。
5. 回滚判断：本改动只影响主聊天寻址；若生产 `sessions.list` 形状与预期不符，回滚分支即可恢复旧行为（旧行为会继续报 `delivery_unknown`，但不会新增错误发送）。

## 7. 交付边界

- 分支：`feat/muse-chat-confirm-fix`
- 未开 PR、未合并、未打 tag。
- 报告落盘：仓库根目录 `muse-chat-confirm-fix-report.md`。
- 生产验证需用户部署后执行；本沙箱不连接 muse.ai，未做直连验证。
