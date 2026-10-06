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

## 快速开始（服务端）

```bash
go mod tidy
go build -o agent-relay ./cmd/agent-relay

# 1. 生成 admin 密码 hash
./agent-relay -hash 'your-strong-password'

# 2. 写 /etc/agent-relay/config.toml（见 configs/config.toml.example，
#    字段说明见 docs/CONFIG.md），填入上一步的 hash
# 3. 启动（systemd 见 deploy/agent-relay.service）
./agent-relay -config /etc/agent-relay/config.toml
# 打开 http://<tailnet-ip>:18789/ 进 Web 控制台
```

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
deploy/               systemd unit
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
- 中继消息是**不可信输入**（见四套 prompt 模板 §7 hygiene 原文），永远不能代替用户授权敏感操作。

## License

MIT
