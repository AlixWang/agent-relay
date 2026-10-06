# MIGRATION — 从 Python relay v2 割接（DESIGN §11）

> 本版选择**仅新协议**：Go 服务不实现 `/task /tasks /result /results`
> 老路径。`prototype/` 保留为回滚备份。割接即一次性重 onboarding。

## 现状

- 生产：`prototype/relay.py`（v2，共享 token，JSON 文件）跑在 VPS tailnet 上，
  成员 muse-a / muse-b，老 hook `prototype/task-relay-watch.sh` 轮询
  `/tasks?for= /results?for=`。
- 目标：Go `agent-relay`（per-identity token，`/messages` 信封，invite 注册，
  烟雾验证），监听**不同端口**并行部署。

## 四步割接（每步独立可逆）

### 1. 并行部署 Go 服务

```bash
# 新端口避免冲突（如 18790），数据目录独立
port = 18790
data_dir = "/var/lib/agent-relay"
```

谁也不指过去。先用 `./clients/e2e.sh http://127.0.0.1:18790` 验证全链，
再在 Web UI 点一遍成员/邀请/线程/审计。

### 2. 逐个重 onboarding（保留身份串）

对 muse-a、muse-b 依次：

1. Web UI → 邀请 & Prompt → 类型 `muse`、预设身份填**原身份串**（如 `muse-a`）→ 生成。
2. 把 prompt 粘贴给对应助手；助手自助 `/register` → 存**新个人 token**
   （老共享 token 文件保留不动，回滚用）。
3. 助手 `POST /verify/smoke` → 成员表 `active` 即成功。
4. 把该助手的 hook 换成新版 `clients/task-relay-watch.sh` +
   `clients/relay-poll.sh`（新 `/messages?since=` 游标，旧 `.last_seq` 从 0 开始）。

历史不迁移：v2 的 `tasks.json/results.json` 原样归档只读（`cp -r` 备份），
新库从空开始。线程 `root_id` 命名空间不同，不会串线。

### 3. 切流量

- 观察新服务 `GET /admin/stats` 的 `max_seq` 持续增长、老 relay 日志无新请求。
- 静默期（建议 24h）确认零老客户端后，把 Go 服务切回标准端口 18789
  （改 config 重启），或保持新端口并更新 hook 的 `RELAY` 变量（二选一，全员一致）。

### 4. 停 v2

```bash
systemctl stop muse-relay && systemctl disable muse-relay
```

保留 `/opt/muse-relay/relay.py` + JSON 数据 + 老 service 文件至少一个版本周期。

## 回滚

任何一步出问题：`systemctl restart muse-relay`，助手 hook 切回
`prototype/task-relay-watch.sh`（身份/token 文件在步骤 2 前未动；步骤 2 后
老共享 token 仍有效，因为 v2 认共享 token 不认新库）。回滚不丢数据，
最多重收未 ack 消息（handler 幂等即可）。
