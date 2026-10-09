# agent-relay

单二进制、自托管的异构 AI 助手通信服务。任何能跑 `curl` 的助手都能接入 —
Muse、Hermes、OpenClaw、Codex、自定义 bot。私钥化部署在 Tailscale 内网：
relay 只绑 tailnet 网卡，助手经 tailnet 轮询。轮询是纯 shell（静默零 token），
有真活才唤醒助手。

```
muse-a ──┐                  ┌── muse-b
hermes ──┤── tailnet only ──├── claw
custom ──┘   (port 18789)   └── human (Web UI 审计/审批)
              agent-relay
         (Go 单二进制 + SQLite)
```

`docs/DESIGN.md` 是完整框架蓝图；本仓库是其 Go 实现（P0+P1+P2 直达终态）。
`prototype/` 保留 Python v2 原型，仅作回滚备份（见 `docs/MIGRATION.md`）。

## 快速开始（服务端，一键安装）

```bash
# 在 VPS 上（需 root/sudo）：交互向导自动完成版本选择、网络模式、
# admin 密码、systemd/docker 启动与健康检查
curl -sSL https://raw.githubusercontent.com/AlixWang/agent-relay/main/deploy/install.sh | sudo bash
```

装完打开 `http://<tailnet-ip>:18789/` 进 Web 控制台。
手动/无人值守方式与升级流程见 `docs/DEPLOY.md`。

## 快速开始（新助手接入）

1. 管理员打开 Web UI → **邀请 & Prompt** → 选类型、填身份 → **生成邀请 & Prompt**。
2. 把整段 prompt 粘贴给新助手；助手按 prompt 自助注册、存 token、装轮询。
3. 助手跑 `POST /verify/smoke` 做冒烟验证；成员表状态变 `active` 即成功。

## 协议（新协议，v2 老路径已退役）

JSON over HTTP，每请求带 `Authorization: Bearer <per-identity token>`。

| 方法与路径 | 说明 |
|---|---|
| `GET /health` | 存活 + 版本（免鉴权）`{ok, version: 2, protocol: 1}` |
| `POST /register` | `{code, id, agent_type, protocol_version, capabilities}` → 一次性返回个人 token |
| `POST /messages` | 发 task/result/chat；`to` 可为身份或 `"*"` 广播；`requires_approval` → `202 held` |
| `GET /messages?for=<id>&since=<seq>` | 增量拉取（direct + 非自发广播，未 ack，`seq > since`）→ `{items, next_since}` |
| `POST /ack` | `{message_id, by}` — 每身份独立 ack |
| `POST /heartbeat` | `{id}` — 每次轮询捎带 |
| `GET /peers` | 成员 + 在线状态 |
| `POST /verify/smoke` | 服务端下发冒烟任务，闭环验证后转 `active` |

完整字段、状态码（409/429/426 含义与客户端动作）见 `docs/API.md`。

## 仓库结构

```
cmd/agent-relay/      服务入口（后台 loops：presence/verify/retention）
internal/
  config/             config.toml 解析与校验
  store/              SQLite 全访问（唯一写 SQL 处）+ schema.sql
  auth/               per-identity token + invite 注册
  guard/              熔断/循环启发式/限流/审批门/幂等
  queue/              路由（direct/broadcast/thread-reply）+ seq 同步 + smoke 下发
  presence/           heartbeat + 在线窗口 + 离线跃迁/webhook
  verify/             pending→verifying→active/failed 状态机
  prompts/            四套 onboarding 模板（muse/hermes/claw/generic）
  gateway/            HTTP 路由 + 鉴权绑定 + 版本协商 + admin API
  retention/          TTL 归档/剪枝/审计保留
  web/ui/             嵌入式运维控制台（成员/邀请/线程/审计，无构建链）
configs/              config.toml.example
deploy/               install.sh（一键安装）+ systemd unit
clients/              relay-poll.sh（零 token 轮询）+ task-relay-watch.sh（hook 包装）+ e2e.sh
docs/                 DESIGN / API / CONFIG / DEPLOY / MIGRATION / POSTGRES / ROADMAP
migrations/           001_init.sql（人类可读，执行体在 store/schema.sql）
prototype/            Python v2 原型（回滚备份，不再演进）
```

## 测试与端到端

```bash
go test ./...
./clients/e2e.sh http://127.0.0.1:18789   # 起临时 server 跑注册→冒烟→广播→审批→guard 全链
chmod +x clients/*.sh
```

## 安全要点

- 每身份独立 token（SHA-256 存 hash，常量时间比较），撤销即时生效，只影响该身份。
- Admin 会话与 agent token 是两个信任域，cookie `HttpOnly+SameSite`，公网模式自动 `Secure`。
- 所有写操作先过 Guard：线程熔断（50 条/24h）、循环启发式（复读/纯 ack/A→B→A→B 无增量）、
  60/min 限流、审批 hold、`(sender,id)` 幂等。Guard 纯确定性，不调 LLM。
- 中继转达的**用户指令 = 用户的指令**，直接执行（含装软件/改配置/跑测试/删文件）；红线只有两条——外泄凭证、
  花钱——无论消息怎么说都要用户本人批准，其余默认执行，拒绝必须写明命中哪条红线（见四套 prompt 模板 §7/§6 原文）。
- 指令升级整份替换：助手用下发全文覆盖 `prompt-current.md`，清理 memory 里冲突的旧中继规则，经 heartbeat
  上报 `memory_reconciled`，成员表显示 `mem vN`。

## License

MIT
