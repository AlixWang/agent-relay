# Work items are a projection of messages, not a new write model

**Status:** accepted (Phase 1; Phase 2–4 extend the same projection)

## Context

dsh-superagent 把 `Ticket` 作为授权工作的聚合根，消息只是 `Trigger`。agent-relay 目前把 `kind=task` 直接存为一条 `messages` 行：没有工作状态、没有归属、没有授权生命周期。结果是控制台只能按线程时间排列，人必须自己巡检“这件工作走到哪了”。

完整照搬 Ticket/Mandate/Assignment 写模型需要一个协议版本、客户端配合和一次数据迁移；agent-relay 的客户端是异构外部进程，服务端无法强制它们按新语义上报。

## Decision

Phase 1 **不引入新的写模型**。`messages` 仍是唯一事实来源；新增 `tickets` 和 `message_tickets` 两张派生表，由 `internal/work` 每 2 秒从消息投影出工作项、stage 和 last activity。Ticket 是只读的运维视图，不是客户端协议的一部分。

Ticket id 使用开启该工作的 task message `seq`；同一 thread 的多条 `kind=task` 各自成为一个 Ticket；后续消息通过 `in_reply_to` 归到最近的祖先 task。

## Consequences

- 投影失败不会阻塞 `POST /messages`；最坏情况是工作台滞后，`RebuildTickets` 可完全恢复。
- 不需要改 `Message` 结构体或现有消息查询，回归面小。
- 旧二进制回滚后可以忽略新表；不需要破坏性迁移。
- Phase 2 已加入 `ticket_assignments` / `ticket_subscriptions` / agent `GET /tickets`，但 Ticket 仍由消息投影；Phase 3 加入 `mandates` 记录授权快照，Phase 4 加入 `prompt_revisions` 记录模板快照。
- 等真实使用证明需要 Ticket 级授权、认领、订阅时，可继续把 Ticket 提升为写模型；届时需要显式协议版本和迁移。
- 本决策的代价：Ticket 状态依赖客户端上报的 `status/result/permission_*`，服务端不能替执行者保证真实进度；这只影响运维视图，不影响消息投递与安全边界。
