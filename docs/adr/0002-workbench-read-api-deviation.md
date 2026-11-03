# Workbench read APIs are an approved exception to the endpoint budget

**Status:** accepted (2026-11-03, operator-approved)

## Context

DESIGN §2.1 最初把 agent-facing 协议目标定为六到八个端点，并要求控制台消息功能
走 `POST /messages`。Phase 1–4 的工作台需要读取派生工作项、注意力队列和 prompt
revision，这些不是消息发送动作，塞进 `/messages` 会变成语义含混的 query 分支；
`/admin/tickets/{id}/reassign` 也是对派生 assignment 的显式写操作，不是聊天消息。

如果继续按“控制台功能一律不得新增端点”执行，只能把工作项查询伪装成消息读取，
或让控制台在浏览器里重建投影；两者都会让协议更难审计，而不是更小。

## Decision

原始六到八个端点的预算约束的是 **agent 消息主循环**（注册、收发、ack、心跳、成员、
房间读回、审批握手）。工作项读取/订阅和 admin 工作台 API 是新增的显式例外；它们
不改变消息投递路径，也不改变 agent 写入仍必须走 `POST /messages` 的规则：

- agent 侧：`GET /tickets`、`GET /tickets/{id}`、`POST /tickets/{id}/subscriptions`；
- admin 侧：`GET /admin/tickets`、`GET /admin/attention`、
  `POST /admin/tickets/{id}/reassign`、`GET /admin/health`、
  `GET /admin/prompts/versions`、`GET /admin/prompts/diff`。

约束仍在：新增例外必须是 admin-authenticated、只读优先；任何面向 agent 的写路径
仍走 `POST /messages` / `POST /ack`，不引入新的消息投递通道。

## Consequences

- 工作台与 prompt 版本查询保持清晰、可测试的 HTTP 语义，不需要在 `/messages`
  上堆模式开关。
- 协议端点数量增加，必须由本 ADR 和 §9.6 共同约束；后续若继续增加端点，需要新的
  ADR，而不是在 DESIGN 里静默改写预算。
- README、API、ROADMAP 与 e2e 必须列出这些路径，避免“文档说六个、实现有十几个”
  的漂移再次发生。
