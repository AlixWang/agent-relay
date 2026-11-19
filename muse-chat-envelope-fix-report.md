# 会话列表信封兼容修复报告

- 分支：`feat/muse-chat-envelope-fix`
- 起点：`origin/main` 的 merge commit `96b6ff0`（含 PR #28）
- 范围：只改 `internal/muse` 的 `GET /api/session/list` 解析与空列表诊断记录
- 未改：`mainChatSessionID` 选择规则、主聊天 id 缓存、用户-facing 错误码、写前失败语义、`delivery_unknown` 语义、side chat 行为、prompt、receiver、`relay-watch.sh`、guard、探针端点、R3 卡片、虚拟列表、数据库 schema

## 1. 改了什么

### 1.1 `internal/muse/chat_live.go`

`resolveSession` 现在同时接受两种有证据的 `sessions.list` 信封：

1. 裸信封：`{"sessions":[...]}`
2. 网关包装信封：`{"result":{"sessions":[...]}}`

解析方式：

- `chatSessionList` 增加可选的 `Result *chatSessionList`；
- 新增 `entries()`：`Result != nil` 时只解一层 `result.sessions`，否则使用顶层 `sessions`；
- `mainChatSessionID` 仍然原样接收 `[]chatSessionListEntry`，选择规则一行未改；
- `sessionListShape` 改用 `entries()` 计数，所以包装信封里的 side chat 也能得到正确的 `sessions=1` / flag 摘要；
- `chatSessionsTimeout` 仍为默认 **4s**，缓存与调用点未变。

### 1.2 空列表时的 `top_keys`

解析成功但有效列表为空（`sessions=0`）时，在原来那条
`muse chat: session list failed ... shape=...` 日志末尾追加：

```text
top_keys=[...]
```

规则：

- 只取响应**顶层 JSON object 的 key 名**；
- key 名按字典序排序，逐项经过 `chatAuditValue`（`=`、空白、控制字符替换为 `_`）；
- **绝不记录 key 对应的值**，也不记录 body、session id、token、cookie；
- `sessions>0` 时不输出该字段。

例如 `{"result":{"sessions":[]},"meta":{...}}` 会记：

```text
shape=sessions=0 primary_field=0 thread_field=0 entries=none resolved=no top_keys=[meta,result]
```

`{}` 会记：

```text
shape=sessions=0 primary_field=0 thread_field=0 entries=none resolved=no top_keys=[]
```

这能在下一次失败时直接看出上游到底包了哪一层，而不必再猜。

## 2. 接受的信封与依据

参考仓库里的证据：

- `~/workspace/dsh/muse-cli-ref/src/muse_cli/routes.json`：`sessions.list` 路由为 `GET /api/session/list`；
- `~/workspace/dsh/muse-cli-ref/src/muse_cli/gateway.py` 的 `call_json`：
  `return d.get("result", d) if isinstance(d, dict) else d`，即客户端同时接受裸对象和 `result` 包装；
- `~/workspace/dsh/.scratch/muse-protocol-go/muse/proto.go` 的 `historyEvents` 也有先解一层 `result` 的写法；
- 没有找到 `sessions.list` 使用其他包装（如 `data`、`payload`、多层 `result`）的证据。

因此本次只兼容 **裸 + `result` 一层**，没有凭空增加候选信封。

### 2.1 裸信封示例

```json
{"sessions":[
  {"session_id":"main-...","is_primary":true,"is_thread":false},
  {"session_id":"side-...","is_primary":false,"is_thread":true}
]}
```

解析结果：`sessions[0]` 被选为主聊天，`/chat/stream` 与 `/chat/history` 都显式使用该 id。

### 2.2 `result` 包装信封示例

```json
{"result":{"sessions":[
  {"session_id":"main-...","is_primary":true,"is_thread":false},
  {"session_id":"side-...","is_primary":false,"is_thread":true}
]},"meta":{"request_id":"..."}}
```

解析结果：取 `result.sessions`，同样选出主聊天；顶层 `meta` 只作为 key 名出现在空列表诊断里，值不会被记录。

## 3. 测试覆盖

新增/保留的 fake 上游测试在 `internal/muse/chat_live_test.go`：

- `TestChatSessionListEnvelopeResolvesMainChatToWaiting`
  - 裸信封：完整 job 到 `waiting`，显式发送主聊天，`muse.chat.sent` 含 `delivery=confirmed`；
  - `result` 包装信封：同样完整走到 `waiting` 并写 `delivery=confirmed`；
  - 两种成功 case 都断言日志与全部审计不含 fake body 内容、主/侧 session_id 哨兵值。
- `TestChatSessionListEnvelopeUnresolvedShapes`
  - `result` 包装但只有 side chat：仍为 `muse_main_chat_unresolved`，`shape` 含 `sessions=1 primary_field=1 thread_field=1 entries=pT resolved=no`，且不写 `/chat/stream`；
  - `{}`：仍为 `muse_main_chat_unresolved`，`shape` 含 `sessions=0` 和 `top_keys=[]`；
  - 两种失败 case 都断言审计 / 日志不含 fake body 与 session_id 哨兵。
- 原有诊断测试 `TestChatBaselineFailureAuditsAndLogsUnderlyingReason` 保留并通过：`reason=`、`status=`、两行服务端日志、形状摘要均未回退。

## 4. 下次成功/失败怎么读

### 4.1 成功

审计 `muse.chat.sent`：

```text
chat=mc_... echo=yes user_seq=... delivery=confirmed elapsed_ms=...
```

job 状态应为 `waiting`，说明主聊天 id 已解析且消息确实写入。

### 4.2 失败

先看服务端两行日志：

```text
muse chat: session list failed chat=... peer=... account=... stage=session_list reason=muse_main_chat_unresolved shape=sessions=0 primary_field=0 thread_field=0 entries=none resolved=no top_keys=[...]
muse chat: baseline history failed chat=... peer=... account=... stage=sending error_class=muse_history_unavailable reason=muse_main_chat_unresolved failure_stage=session_list
```

再看审计 `muse.chat.failed`：

```text
chat=... stage=sending error_class=muse_history_unavailable reason=muse_main_chat_unresolved
```

2xx 且无 HTTP 错误时不要期待 `status=`；出现非 2xx 才会带 `status=`。

`top_keys` 的读法：

- `top_keys=[]`：响应就是 `{}`；
- `top_keys=[result]` 且 `sessions=0`：`result` 对象里没有 `sessions`，或 `sessions` 为空；
- `top_keys=[data]` / `[payload]` / 其他：出现了本次未兼容、也无证据的包装，先拿真实上游样本来确认，不要直接猜；
- `sessions=N` 且 `top_keys` 不出现：列表本身有内容，问题在 flag / 选择规则，而不是信封。

## 5. 部署与重试验证清单

1. 按仓库发布流程构建并部署该分支提交；不要手工替换生产二进制，部署动作仍走正常 release / 控制台 update。
2. 部署后确认新二进制版本 / commit 对应本次提交。
3. 让 erdan 在直接对话里发送 `pwd`。
4. 预期：job 进入 `waiting`（不是约 3 秒即失败），审计出现 `muse.chat.sent ... delivery=confirmed`。
5. 若仍是失败：读取同一时间的 `muse chat: session list failed` 行：
   - `reason` 是什么；
   - `shape=sessions=N` 的 N；
   - 是否出现 `top_keys=[...]` 及其 key 名。
6. 若 `reason=muse_main_chat_unresolved` 且 `sessions=0`：用 `top_keys` 判断上游信封是否仍未被覆盖；若为 `[result]` 但 `sessions` 仍为 0，检查上游 `result` 内是否还有一层未文档化结构，不要直接加猜测分支。
7. 若 `sessions>0` 但仍 unresolved：看 `primary_field` / `thread_field` / `entries`，这是主聊天 flag 形状问题，不应改动本次信封逻辑。
8. 若出现 `status=`：这是 HTTP 层失败，按 PR #28 的诊断路径处理，与信封解析无关。

## 6. 验证结果

在仓库内 `.gocache/` 构建缓存下执行，全部通过：

```text
gofmt -l internal/muse                         # 无输出
go vet ./...                                   # 通过
go build ./...                                 # 通过
go test ./... -count=1                         # 通过
go test -race ./internal/muse/... -count=1     # 通过
go test -race ./internal/gateway/... -count=1  # 通过
```

`go build` 期间仅出现模块缓存目录只读的 stat-cache 警告（`/home/hatch/go/pkg/mod/... permission denied`），退出码为 0，不影响构建与测试结果。