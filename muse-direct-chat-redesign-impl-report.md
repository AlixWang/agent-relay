# 直接对话改版实现报告（R1+R2 + 探针支撑）

- 基线：`origin/main @ e48d0c2`（v1.11.0）
- 分支：`feat/muse-direct-chat-v2`（从 `origin/main` 起步，已 push）
- 代码 HEAD：`5a86bce358c10632030166ddbb8a1015b5b8e474`（报告文件随后单独提交）
- 工作区：干净；未开 PR、未合并、未打 tag
- 实现依据：`~/workspace/dsh/muse-direct-chat-redesign-report.md`（R1–R34）

## 提交列表

| 提交 | 内容 |
|---|---|
| `9ddb485` | `feat(muse): make direct chat an admin access switch` — R1 后端、R2 thread 后端、R26 service 寻址与隐藏探针、对应测试 |
| `ba24a5e` | `feat(console): replace Muse chat dialogs with #muse-chat page` — R2 前端、入口、路由、样式、UI 测试 |
| `5a86bce` | `docs(muse): document v2 access semantics, chat page and probe` — MUSE-CHAT/API/DESIGN、本地 `.gocache` 忽略 |

## 范围结论

- R1（R1–R12）与 R2（R13–R24）已实现。
- 探针支撑（R26 + 探针入口）已实现；R25 只完成“先探针”的前置工作。
- **R27–R33（审批卡片 UI、审批发现轮询、关联、决策审计、fallback 卡片）一律未做**，原因见文末。
- 未改 prompt、receiver、`relay-watch.sh`、`cmd/relay-tail`、任务下发、kind=chat 通道、第三方网关。
- 未读凭证、未新增凭证读取；探针复用已建立 job 的 session/lease。

## R1 开通简化（R1–R12）

| # | 落点（文件:行号） | 实现 |
|---|---|---|
| R1 | `internal/store/schema.sql:325`、`migrations/001_init.sql:303`、`internal/store/muse.go:38-44` | `chat_enabled` 保留为逐成员管理员开关，默认 0；不再有 consent 语义 |
| R2 | `internal/gateway/gateway.go:226`、`internal/gateway/muse_chat.go:200` | `POST /admin/muse/accounts/{id}/chat-access` 走 `requireAdmin`；无成员/agent 入口 |
| R3 | `internal/muse/chat_jobs.go:512-561` | 开通/停用持久，直到手动停用、换 cookie、换 peer；不审计每次打开页面 |
| R4 | `internal/muse/muse.go:463-471` | 换 cookie 后 `chat_enabled=0`、`enabled_at/by` 清零；仅实际由开→关时写 `access_reset` |
| R5 | `internal/muse/muse.go:524-536` | 换 `peer_id` 同样重置并写 `access_reset`（reason=peer_changed） |
| R6 | `internal/web/ui/js/muse.js:1165-1208` | 首次发送不再要求输入 peer_id；没有确认字段/分支 |
| R7 | `internal/web/ui/index.html:303-306`、`internal/web/ui/js/muse.js:939-947` | 每次发送确认弹窗删除；保留常驻可见性横幅 + 每对话首次发送一行内联提示 |
| R8 | `internal/gateway/muse_chat.go:143-196`（settings/status 未改） | 部署门与运行时 kill switch 保持原样，默认不变 |
| R9 | `internal/store/muse.go:55-65,90-105,150-165`、`internal/store/schema.sql:325-334`、`migrations/001_init.sql:303-310` | `chat_consent_at/by` 复用为 `enabled_at/by`；`chat_consent_version/evidence` 停止读写，列保留；两 schema 文件只改注释，零 DDL |
| R10 | `internal/store/muse.go:182-213`、`docs/MUSE-CHAT.md:15-19` | 旧 opt-in 行 grandfather，不做批量重置；新开通只覆盖 `chat_enabled/at/by` |
| R11 | `internal/gateway/muse_chat.go:215`、`internal/muse/chat_jobs.go:568-577` | 审计 actor 固定 `"admin"` / 重置固定 `"system"`；不伪造人级归因 |
| R12 | `internal/muse/chat_jobs.go:568-577`（调用点 `internal/muse/muse.go:470,535`） | 新增 `muse.chat.access_reset`，reason=reauthorized/peer_changed |

审计改名与 detail：

- `muse.chat.access_grant`：account/peer/by/reason/source=console（`internal/muse/chat_jobs.go:546-549`）。
- `muse.chat.access_revoke`：account/peer/by/reason/source=console（`internal/muse/chat_jobs.go:553-556`）。
- `muse.chat.access_reset`：account/peer/reason（`internal/muse/chat_jobs.go:572-575`）。
- 审计与账号列在同一事务：`internal/store/muse.go:182-213`；不含 evidence/version/正文。
- 发送审计的 `consent=1` 改为 `access=1`：`internal/muse/chat_jobs.go:1010-1014`。
- 准入错误 `muse_chat_opt_in_required` 改为 `muse_chat_access_required`：`internal/muse/chat_jobs.go:256-258`。

## R2 对话式 UI（R13–R24）

| # | 落点（文件:行号） | 实现 |
|---|---|---|
| R13 | `internal/web/ui/index.html:270`、`internal/web/ui/js/router.js:10`、`internal/web/ui/app.js:38` | 新增独立子路由 `#muse-chat?peer=...&session=...`；无侧栏项，不复用 `#page-threads` |
| R14 | `internal/web/ui/js/muse.js:198-203,1142-1163` | 唯一入口为成员行“对话”；未开通时 POST `chat-access` 后进入，已开通直接进入 |
| R15 | `internal/web/ui/index.html:270-330`、`internal/web/ui/style.css:1512-1533` | 桌面双栏（左成员/右线程+常驻输入）；移动端复用 `.cmd-layout.sessions-open` 抽屉 |
| R16 | `internal/muse/chat_thread.go:56-135`、`internal/gateway/muse_chat.go:223-234`、`internal/web/ui/js/muse.js:1081-1110` | `GET /admin/muse/chat/thread` 只读内存 job，1 小时边界；页面刷新可恢复；重启/超时显示边界 + 去 Muse |
| R17 | `internal/muse/chat_thread.go:17-39`、`internal/muse/chat_jobs.go:93-118` | thread 返回操作者 `text` 与 `reply`；job 轮询快照无 Text 字段 |
| R18 | `internal/web/ui/js/muse.js:934-936,1196-1203` | active 时发送禁用；409 `muse_chat_in_progress` 带 `chat_id` 时自动 `loadMuseChatThread` 附着，不排队 |
| R19 | `internal/web/ui/index.html:292`、`internal/web/ui/js/muse.js:1260-1271,1329-1332` | `session` 属线程身份；页眉下拉/高级输入切换即切线程，不调用建会话 |
| R20 | `internal/web/ui/index.html:311-319`、`internal/web/ui/js/muse.js:735-741,858` | 每轮高级里选预算，默认 180，范围 30–300；只有 `waiting`（确认投递后）显示倒计时 |
| R21 | `internal/web/ui/js/muse.js:1210-1234,1236-1258` | 停止=确认+cancel；关闭=回列表；停用=overflow danger + `access_revoke` |
| R22 | `internal/web/ui/index.html:290-291`、`internal/web/ui/js/muse.js:913-919` | 聊天页只读运行时徽标 + 回 Muse 页链接，无全局开关 |
| R23 | `internal/web/ui/js/muse.js:697-733,834-891` | 状态/错误码中文表；不自动重发；`delivery_unknown` 明确要求新发一条并重新确认 |
| R24 | `internal/web/ui/index.html:270-330`、`docs/MUSE-CHAT.md:38` | 独立路由/视觉/输入；无 task/result、kind=chat、@、ack |

补充：删除 `museChatConsentDlg` 与 `museChatDlg` 两个 dialog，删除 `muse.js` 中 `openChatConsent/submitChatConsent/openMuseChat/sendChat/disableChat` 等旧逻辑；`internal/gateway/ui_assets_test.go:209-250` 断言两个 dialog 不再出现，旧路径 `/chat-consent` 返回 404（`internal/gateway/muse_chat_test.go:154-159`）。

## 探针支撑（R26 + R25 前置）

| 项 | 落点 | 说明 |
|---|---|---|
| service 参数 | `internal/muse/noise_transport.go:321-347` | `sendServiceFrame(ctx, sf)` 保持 daemon 默认 0 不变；新增 `sendServiceFrameWithService(ctx, service, sf)`，现有调用无需改 |
| 读循环 | `internal/muse/chat_conn.go:135-169`（未改） | 仍按 stream id 分发，不看 service |
| 单连接复用 | `internal/muse/chat_conn.go:190-276`、`internal/muse/chat_live.go:43-64` | 新增 `writeRequestService`/`callService`；`serviceCall` 只在探针里用；不新建连接/lease |
| P0/P1 | `internal/muse/chat_probe.go:54-76` | `ProbeApprovals` 在 job 连接上发 service 1 `GET /approvals`，原样返回 status/body（全字段，供探针环境检查） |
| P2 | `internal/muse/chat_probe.go:78-122` | `ProbeDecide` 三种请求体：`with_id` / `decision_only` / `empty`；只接受 `deny` / `allow_once` |
| admin-only / 默认关闭 | `internal/gateway/gateway.go:228-229`、`internal/gateway/muse_chat.go:273-322` | 两个隐藏端点均 `requireAdmin`；请求缺 `"probe":true` → 403 `muse_probe_disabled` |
| 不测 allow_always | `internal/muse/chat_probe.go:90-96` | 显式返回 400 `probe_allow_always_forbidden` |
| 不审计/不记日志 | `internal/muse/chat_probe.go`、`internal/gateway/muse_chat.go:237-270` | 探针路径没有任何 `AppendAudit`/`log.Printf`；正文只回给请求方 |
| 单步、不批量 | 同上 | 每个 HTTP 请求只发一个上游请求；P2 后需操作员再次调用 P0 端点重查 |

探针失败映射：未知 chat → 404 `unknown_chat`；job 已终态/session 已释放 → 409 `chat_not_active`；传输错误 → 502 `muse_probe_failed`；上游非 2xx → 502 且保留 `upstream_status`/`body`。

### 在能连通 muse.ai 的环境上跑 P0–P2

前提：部署门打开、运行时开关打开、目标成员已开通；先发一条会触发审批的对话，并让 job 处于 `waiting`（预算留足，最大 300s）。`chat_id` 取自 `POST /admin/muse/chat` 的响应。所有请求用 admin 会话 Cookie。

P0（通道）：

```bash
curl -sS -b "$ADMIN_COOKIE" -H 'content-type: application/json' \
  -d '{"chat_id":"mc_...","probe":true}' \
  "https://$RELAY/admin/muse/chat/probe/approvals"
```

`upstream_status` 2xx = P0 通过（不要求有待批项）。非 2xx 时 HTTP 502，响应仍带 `upstream_status` 和 `body`。

P1（全字段）：

同一响应体的 `body` 即 `/approvals` 原始 JSON（约 123KB）。在探针环境内检查 `decision_options` 与 `scope` 字段；只记录字段名/形状，不把标题或详情写进审计、报告正文或工单。

P2（逐条决策，deny 先于 allow_once）：

```bash
# 1) deny，带 id
curl -sS -b "$ADMIN_COOKIE" -H 'content-type: application/json' \
  -d '{"chat_id":"mc_...","probe":true,"approval_id":"ap_...","decision":"deny","variant":"with_id"}' \
  "https://$RELAY/admin/muse/chat/probe/decide"

# 2) 只 decision
... "decision":"deny","variant":"decision_only" ...

# 3) 空体
... "decision":"deny","variant":"empty" ...

# 4) 重查状态（再次调用 P0 端点），并观察 30 秒内 agent 是否继续
```

`variant` 三选一，每次只发一种；2xx 只算“已提交”，必须重查 `/probe/approvals` 看到状态变化才可记录为生效。`allow_once` 在操作员确认后再试，且不再试 `allow_always`。任一步失败：关运行时开关、停止、不要扩大范围。

## 测试结果（命令 + 结论）

所有命令在 `feat/muse-direct-chat-v2 @ 5a86bce` 上执行；因沙箱只允许写会话工作区，Go 构建缓存使用仓库内 `.gocache`，模块缓存只读复用，不影响结果。

| 命令 | 结果 |
|---|---|
| `go vet ./...` | exit 0，无诊断 |
| `go build ./...` | exit 0 |
| `go test ./...` | exit 0；21 个包全 `ok`（gateway 10.0s、muse 16.9s、web 7.0s） |
| `go test -race ./internal/muse/...` | exit 0；`ok .../internal/muse 48.786s` |
| `go test -race ./internal/gateway/...` | exit 0；`ok .../internal/gateway 64.052s` |
| `make test-ui-js` | exit 0；7 个 `_*.test.mjs` 全通过（含新增 `museChatHash/parseMuseChatHash/budget/countdown/error` 断言） |

新增/更新的重点测试：

- `internal/muse/chat_jobs_test.go`：access grant/revoke/reset 审计、旧 consent 字段不写、deploy/账号状态校验。
- `internal/muse/chat_thread_test.go`：thread 文本/回复、session 投影、1 小时边界、未知 peer/非法 session。
- `internal/muse/chat_probe_test.go`：service=1、GET `/approvals`、三种 decide body、allow_always 拒绝、终态 job 拒绝。
- `internal/muse/noise_wire_test.go`：service 1 显式编码、service 0 仍省略默认字段。
- `internal/gateway/muse_chat_test.go`：旧 `/chat-consent` 404、access 字段白名单、thread 空形状、probe 默认关闭/allow_always。
- `internal/gateway/ui_assets_test.go`、`internal/web/web_test.go`：新页面/新路由接线、旧 dialog 不再出现、静态 class 都在 CSS 中定义。

没有把任何失败判成 pre-existing；本轮全绿，未需要 main 基线对照。

## 未做事项（R27–R33，及原因）

| # | 未做 | 原因 |
|---|---|---|
| R27 | job `waiting` 时轮询 `GET /approvals`（5s/10s 退避） | 依赖 P0/P1 探针先确认网关授权与响应形状；本轮只提供探针入口 |
| R28 | 审批与对话轮次的 scope 关联 | 关联键（`scope.thread_id`/`scope.session_scope_id`）待 P1 全字段确认；未确认前按时间猜会批错 |
| R29 | `decide` 最小请求体定稿 | 候选三种 body 已做成探针 variant，但需 P2 实测后定稿；生产决策路径未接 |
| R30 | `allow_always` 展示策略/降级 | 设计明确 v2 不展示；探针明确拒绝。卡片 UI 未做，故无展示层 |
| R31 | pending→submitting→submitted→confirmed/unconfirmed 状态机 | 依赖 P2 的 2xx 后状态变化与 agent 是否继续；未证实前不能建状态机 |
| R32 | `muse.chat.approval_deciding/decided` 决策审计 | 决策路径未实现；探针明确不写审计 |
| R33 | P0 失败无卡片、关联失败只读、decide 不生效 display-only 的 fallback UI | 卡片 UI 整体未做；本轮按任务边界不建在未证实协议上 |

另外，本轮**没有在真实 muse.ai 上执行 P0–P2**：当前环境没有可用的 muse.ai 连通性/操作员在场条件，且约束不允许读凭证。探针代码、入口、文档已就绪，需在 staging/erdan 环境按上文执行；探针通过后才另起一轮做 R27–R33。

## 已知限制

- 对话历史只在内存；进程重启或超过 1 小时后页面显示边界提示，不落库、不读 `chat.history` 回填。
- 成员在 Muse 里发送的其他消息不会出现在后台线程；后台只显示操作者已发消息与对应回复。
- `chat.cancel` 是 best-effort；不调用未验证的 `delete_turn`，后台不能撤回已发消息。
- 超时后 Muse 侧生成可能继续；页面只显示“已停止等待”。
- side chat 必须已存在；v2 不创建/重命名会话。
- 主聊天同时有人说话时回复归属只能标 `ambiguous`，无法做因果证明。
- 审计 actor 固定 `"admin"`（重置为 `"system"`），无法区分具体管理员；未伪造人级归因。
- 旧 v1 审计行 `muse.chat.opt_in/opt_out` 保留且语义是“成员同意”，不能与 v2 `access_*` 混读。
- 审批卡片、审批轮询、关联、决策审计全部未实现；探针端点虽 admin-only 且需 `probe:true`，仍是实验入口，不应暴露给非操作员。
- 未执行真实 muse.ai 探针，因此 `decide` 的最小请求体、审批 scope 关联、决策后 agent 是否继续都仍是待证实项。