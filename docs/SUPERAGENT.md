# dsh-superagent 对照评估：agent-relay 的下一步

> 参考对象：[deepseek-ai/dsh-superagent-prototype](https://github.com/deepseek-ai/dsh-superagent-prototype)
>（DeepSeek Harness 团队的 research prototype，官方明确“not production-ready”）。
> 本文记录我们能从它身上学什么、不学什么，以及为什么先做“只读工作项投影”。

## 1. 它是什么，不是什么

dsh-superagent 是**面向单个 GitHub 仓库的主动型 coding agent**，以 DeepSeek Harness bundle 的形式运行：

- 它监听 GitHub（可选飞书），把消息、Issue、PR、CI 事件变成 `Trigger`；
- 只有被允许的人或团队预先写下的规则能开启工作；
- 接受后创建 `Ticket`，记录 `Mandate`（授权依据、范围、Policy 版本）；
- 通过 `Assignment` 把 Ticket 交给一个 Executor Session（`worker-<ticket id>`）；
- 结果通过 `Channel` 回到原对话；
- Web 面板只展示“需要人”的工作、系统健康和单件工作的历史。

它不是通用消息总线，也不是多租户基础设施。它的领域模型（`Source / Trigger / Thread / Eval / Ticket / Mandate / Assignment / Subscription / Channel`）是为“一个团队 + 一个仓库 + 同构 DSH 执行器”设计的。

agent-relay 是另一层：

- 多身份、异构运行时（Muse、Hermes、OpenClaw、自定义 bot）；
- 传输与治理优先：per-identity token、seq 同步、ack、熔断、限流、审批、审计；
- 客户端可以只会 `curl`，服务端不能假设对方有 DSH Session；
- 单二进制 + SQLite + Tailscale-first。

因此结论是：**借它的“工作项/授权/订阅/持久化副作用/注意力面板”模式，不借它的完整应用领域。**

## 2. 核心对照

| 维度 | agent-relay 现状 | dsh-superagent | 结论 |
|---|---|---|---|
| 定位 | 多助手通信与治理基础设施 | 单仓库主动型 Agent 应用 | 不同层，不能照搬 |
| 工作单元 | 单条 `messages`；`root_id` 只是消息链 | `Ticket` 是授权的工作聚合根；消息只是 Trigger | **最大启发：把工作从消息中拆出来** |
| 授权 | 每条消息的 `requires_approval`；两红线在 prompt | `Mandate` 记录 policy version、scope、来源；宿主 gate 强制执行 | 先做只读 Ticket；后续把授权挂到 Ticket |
| 后续讨论 | ack 后从投递视图消失；需重新发消息 | `Subscription` 把 Thread 后续消息扇出到 Ticket；context 不授予权限 | 后续做 Ticket 订阅 |
| 执行者 | 外部 peer（异构） | DSH Executor Session + 每 Ticket 目录 | 保持外部；只记录 Assignment 元数据 |
| 调度 | 无 dispatcher；人/peer 直接发 | Main Session 路由，worker 执行 | dispatcher 应是可选 peer，不内嵌 LLM |
| 持久化副作用 | 部分同步写，部分 best-effort goroutine | `Effects/Outbox`：先存后做、退避、health | 后续做 effects 表 |
| 控制台 | 线程/成员/审批，按时间排 | 按“需要人”的 stage 排，健康状态不占空间 | **本次先做工作台/注意力队列** |
| Prompt | 全局 `PromptVersion` + 整包替换 | 不可变 revision、base、默认差异、模板分 part | 后续按模板分版本 |
| 概念数量 | 少 | 多但克制；每个概念有明确规则 | 只加 Ticket/Stage，保持少 |

## 3. 六个可迁移模式

### 3.1 消息是事件，Ticket 是工作

superagent 的 `Trigger` 不可变、可审计、可幂等；`Ticket` 承载状态、归属、授权和生命周期。agent-relay 把两者压在一张 `messages` 表里，导致：

- 无法回答“这件工作是谁的、走到哪了、下一步等谁”；
- 一条 `result` 只是消息，不会关闭任何“工作”；
- 人只能逐条读线程，无法按工作巡检；
- 重启/换执行者后没有可恢复的工作身份。

**决定**：先做只读 Ticket 投影（`tickets` + `message_tickets`），消息仍是唯一写模型。等真实使用证明需要，再升级为写模型。

### 3.2 授权是工作的属性，不是消息的 flag

superagent 的 `Mandate` 记录：Policy 版本、范围、强度、请求者身份、来源如何收到 Trigger。Policy 变化不会立即吊销 live Ticket，而是在恢复时重新校验；只有人的 stop 或 revalidation 失败才撤销。

agent-relay 的 `requires_approval` 是发送方在单条消息上打的 flag；两红线只在 prompt 里。服务端无法回答“这个 assistant 对这件工作被授权做什么、授权何时失效”。

**决定（后续）**：Ticket 增加 `policy_version`、`scope`、`requester`；服务端做确定性 admission gate；不把 LLM/Eval 放进 relay。两红线仍由客户端 prompt 执行，但结构化审批字段优先于 prose。

### 3.3 Subscription：同一件事持续，而不是反复 @

superagent 的 Ticket 订阅 Thread：后续消息自动到达执行者；`context` 订阅只提供上下文，不授予新权限，也不恢复已结束的 Ticket。

agent-relay 的 ack 模型是“投递队列”：消息被 ack 后从 `VisibleTo` 消失；后续讨论必须重新发一条 direct message。长任务因此容易丢上下文。

**决定（后续）**：增加 `ticket_subscriptions(own|context)`；投递视图把订阅消息纳入 assignee 的可见范围。**隐私红线**：只有 assignee 本来就是该 thread 参与者时才允许订阅，避免把私聊内容扇出给第三方。

### 3.4 Stored work first

superagent 的 `Effects` 是持久化副作用队列：命令在事务内 `enqueue`，提交后才执行；失败按 2s→60s / 5s→300s 退避；每种 effect 有 health；重启保留 pending 行。

agent-relay 的 `Send` 先插消息、再单独 `CreatePermissionRequest`（半写风险）；离线 webhook 是 goroutine；room roster notice 同步插入。没有统一的“意图先落库”层。

**决定（后续）**：`effects` 表 + worker + health；先覆盖 webhook、通知类副作用，再迁移关键写路径。

### 3.5 注意力是产品指标

superagent 面板回答三个问题：

1. 现在有需要我处理的吗？
2. 系统健康吗、在做正确的事吗？
3. 某件工作的历史是什么？

原则：健康状态不占空间；每个数字都要改变人的动作；stage 定义可检查、互斥、覆盖所有情况。

agent-relay 控制台目前按线程时间排，人需要自己巡。**决定（本次）**：工作台 + attention queue，按 `person/*` 优先；空状态只留一句话。

### 3.6 Router/Worker 是拓扑，不是模型

superagent 把“接话/路由”和“执行”拆成不同 Session，不是让一个模型在提示词里决定要不要分工。这样做的好处是：路由不被长任务阻塞、执行者有独立上下文、每件工作有自己的目录和状态。

agent-relay 不需要内嵌这个 Router。它本身就是 router 基础设施；如果将来需要主动分派，可以增加一个可选的 **dispatcher peer**（普通 assistant 身份，使用现有协议），由它调用 `/messages` 创建 Ticket，而不是把 LLM 放进 relay 进程。

## 4. 采纳 / 适配 / 拒绝

| 机制 | 决定 | 理由 |
|---|---|---|
| Ticket / Stage | **采纳**（先只读投影） | 解决“消息不是工作”的结构缺口；零协议风险 |
| Assignment（派工） | **适配**（后续） | Ticket 与执行者解耦，支持重启、换人、worker pool |
| Mandate / Policy version | **适配**（后续） | 把授权从 prompt 移到服务端可审计对象 |
| Subscription | **适配**（后续） | 长任务续接；必须加隐私约束 |
| Effects / Outbox | **采纳**（后续） | 崩溃恢复、重试、health；与单二进制/SQLite 兼容 |
| Attention UI | **采纳**（本次） | 直接减少人的巡检成本 |
| Prompt 分版本 | **适配**（后续） | 四套模板全局版本会互相 nudge |
| Eval（LLM 判定接活） | **拒绝** | relay 的 guard 必须确定性；主动判断应由外部 dispatcher 承担 |
| Source / Channel 插件 | **拒绝** | relay 的 source 就是 peer；不要变成 GitHub/Feishu 集成中心 |
| 每 Ticket 一个 DSH Session | **拒绝** | 执行环境属于各 assistant runtime，relay 不管理目录/进程 |
| PR / auto-merge 领域 | **拒绝** | 单仓库应用领域，不是通信基础设施 |

## 5. 分阶段路线

| 阶段 | 内容 | 状态 |
|---|---|---|
| Phase 0 | 本文 + ADR + 术语/ROADMAP 更新 | 本次 |
| Phase 1 | `tickets`/`message_tickets`、`internal/work` 投影、工作台、attention queue | 本次 |
| Phase 2 | `ticket_assignments`、reassign/lease（到期结束并要求改派）、`GET /tickets`、`ticket_subscriptions`、status 扩展 `waiting/ready/failed/dropped`（dropped 终态） | 已实现 |
| Phase 3 | `policy_versions` + `mandates`、确定性 gate、`effects` outbox、health | 已实现 |
| Phase 4 | Prompt 按模板分版本、不可变 revision、base/差异 | 已实现（revision/hash + base_revision + versions + diff 接口；编辑器 UI 仍可后续增强） |

**Phase 1 的明确非目标**：不改 `messages` 表、不增加 agent 端点、不 bump `PromptVersion` / `receiver_rev`、不引入新配置项。Phase 2/3 的协议增量均为向后兼容的 additive 字段/端点；Phase 4 只增加 revision 元数据，不改变现有 `prompt_version` 升级路径。

## 6. 边界与失败模式（Phase 1）

- `to=*` / `to=grp_*` / `to=operator` 的 task 不是 Ticket：没有单一 assignee。
- `sender=system`、`root_id=verify/*` 的冒烟任务排除。
- 同一 thread 可有多件工作：每条 `kind=task` 独立 Ticket，后续消息归最近祖先 task。
- task 发给未注册 peer：仍建 Ticket，显示 `person/offline`；对方注册后恢复。
- 审批拒绝：Ticket `cancelled`，保留证据。
- permission 过期：不再计 `person/answer`，30 秒刷新回落到 working/blocked。
- open Ticket 的消息不被 retention 归档；ended Ticket 可归档并清理。
- 投影失败只影响工作台，不影响 `POST /messages`、SSE、审批、房间。
- Phase 2 订阅只对原本就是该 thread 参与者的 assignee 投递，不能把私聊扇出给第三方。
- Phase 2 `status=waiting|ready|failed|dropped` 只改变工作项阶段；`dropped` 将 Ticket 结束为 done。
- Phase 3 task gate 只做确定性判断（peer 是否允许发 task、是否自动 hold）；不做 LLM 判断。
- Phase 3 effects 是 at-least-once：重复 POST 可能发生，接收方应按 `kind+key` 幂等。
- Phase 4 prompt revision 是内容 hash；旧 revision 行只增不改。

## 7. 参考阅读

按 superagent 官方文档顺序：

- [000 Philosophy](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/000-philosophy.md)
- [001 Domain model](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/001-domain.md)
- [002 Authorization](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/002-authorization.md)
- [003 Execution](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/003-execution.md)
- [004 Prompts](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/004-prompts.md)
- [005 Panel](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/005-panel.md)
- [007 Architecture](https://github.com/deepseek-ai/dsh-superagent-prototype/blob/main/docs/007-architecture.md)

## 8. Phase 1 实现索引

- 数据表：`internal/store/schema.sql`、`migrations/001_init.sql`（`tickets`、`message_tickets`）
- Store：`internal/store/tickets.go`（Ticket 类型、分页、retention pin、stage counts）
- 投影：`internal/work/stage.go`（stage 表）、`internal/work/projector.go`（2 秒 pass、Rebuild、attention）
- Admin API：`internal/gateway/tickets.go`（`GET /admin/tickets`、`GET /admin/attention`）
- 控制台：`internal/web/ui/js/work.js`、`internal/web/ui/index.html`（`#work`）
- 测试：`internal/work/*_test.go`、`internal/store/tickets_test.go`、
  `internal/gateway/tickets_test.go`、`internal/web/ui/_work-state.test.mjs`、
  `internal/retention/retention_test.go`
- 决策记录：`docs/adr/0001-work-items-as-projection.md`

## 9. Phase 2–4 实现索引

- **Phase 2**：`ticket_assignments`/`ticket_subscriptions`（`internal/store/assignments.go`、
  `internal/store/subscriptions.go`）、`internal/work/projector.go` 的 assignment/订阅/lease
  联动、agent `GET /tickets` 与 `POST /admin/tickets/{id}/reassign`
  （`internal/gateway/tickets.go`）、`status=waiting|ready|failed|dropped`
  （`internal/guard/guard.go`、`internal/work/stage.go`）。
- **Phase 3**：`policy_versions`/`mandates`（`internal/store/policy.go`）、
  `allow_peer_tasks`/`require_task_approval_from_peers`（`internal/config`、
  `internal/guard`）、`internal/effects`（持久化 webhook 重试 + per-kind health）、
  `GET /admin/health`。
- **Phase 4**：`prompt_revisions` + `base_revision`（`internal/store/prompt_revisions.go`）、
  `prompts.TemplateRevision` / `TemplateSource`（`internal/prompts/prompts.go`）、
  `GET /prompts/current` 的 `template_revision`、`GET /admin/prompts/versions` 与
  `GET /admin/prompts/diff`（base 链接解析在 `internal/prompts/revisions.go`）。
