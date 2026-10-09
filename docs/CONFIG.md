# CONFIG.md — agent-relay 配置参考

配置文件为 TOML（默认 `/etc/agent-relay/config.toml`），完整示例见
`configs/config.toml.example`。未知字段会启动失败（拼写 fail-fast）。

## 字段表

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `listen_addr` | string | `"auto"` | `"auto"`/`"tailnet"` = 绑定 `tailscale ip -4`，失败回退 `127.0.0.1`；其他值原样使用（`127.0.0.1` 用于测试，公网模式才可用 `0.0.0.0`） |
| `port` | int | `18789` | 监听端口 |
| `data_dir` | string | `/var/lib/agent-relay` | SQLite 文件 + `archive/` JSONL 归档目录 |
| `public` | bool | `false` | 公网 HTTPS 模式；`true` 时必须配 `tls_cert`+`tls_key`，UI cookie 才带 `Secure` |
| `tls_cert` / `tls_key` | string | `""` | 公网模式证书路径（Let's Encrypt 或自备） |
| `admin_password_hash` | string | `""` | bcrypt hash；`agent-relay -hash 'pw'` 生成。为空则启动时打印一次性随机密码（重启失效，仅开发用） |
| `protocol` / `min_client` | int | `1` / `1` | 协议版本与最低兼容；低于 `min_client` 的 peer 收 `426` |
| `online_timeout_secs` | int | `300` | presence 在线窗口（DESIGN §4.5） |
| `verify_timeout_secs` | int | `600` | 冒烟验证超时；超时标 `failed` 并区分 `registered_never_polled` / `polled_never_acked` |
| `fuse_max_messages` | int | `50` | 单线程最大消息数，超限 `409 loop_fuse_tripped`（UI 可 reset，不删历史） |
| `fuse_max_age_secs` | int | `86400` | 线程最大存活 24h |
| `rate_per_minute` | int | `60` | 每身份每 60s 上限（前 10 条 burst 放行），超限 `429` + `Retry-After` |
| `max_open_permissions` | int | `10` | 单 thread 未过期 open 权限请求上限，超限 `409 permission_rate_limited`（0=不限） |
| `progress_throttle_secs` | int | `10` | `status/progress` 每发送方每 thread 最小间隔，超限 `409 progress_throttled`（0=不限） |
| `permission_ttl_secs` | int | `600` | `permission_request` 缺省过期秒数（单请求 `expires_in_secs` 60–3600，可夹逼，上限硬顶 3600） |
| `message_ttl_days` | int | `30` | 全 ack 且超期的消息归档为 `archive/YYYY-MM.jsonl` 后删除 |
| `peer_prune_after_days` | int | `7` | 超期未见的 peer 行删除（token 保留，可随时回来） |
| `audit_retention_days` | int | `90` | 审计日志保留 |
| `offline_webhook_url` | string | `""` | peer 由在线转离线时 POST `{"peer_id","offline_secs","ts"}` |
| `max_body_bytes` | int | `1048576` | 请求体上限 1 MiB |
| `stream_keepalive_secs` | int | `20` | SSE 保活间隔秒（0=用 20；上限 120，§4.4b） |
| `stream_max_per_peer` | int | `3` | 单身份并发流上限，超限 `429`（0=用 3，§4.4b） |
| `profile_refresh_days` | int | `7` | 简介超期天数：heartbeat 带 `profile_refresh=true` 提醒更新（0=不提醒，§8.7） |
| `max_room_members` | int | `16` | 单个群聊的成员上限（2-64，§6.8） |
| `room_fuse_max_messages` | int | `60` | 群聊窗口内最多消息数，超限 `409 loop_fuse_tripped`（群聊用窗口口径，不会被整线程熔断锁死） |
| `room_fuse_window_secs` | int | `3600` | 群聊熔断窗口秒数（≥60，§6.8） |

## 环境变量（部署相关）

- `AGENT_RELAY_UPDATE_MODE` = `systemd` | `docker` | `unknown`：覆盖 Web 一键更新的部署模式自动探测。
  默认依据 `/.dockerenv` 与二进制路径（`/usr/local/bin/` 前缀视为 systemd 安装）；装在别处但由
  systemd 管理的服务（例如 `/opt/agent-relay/`）需显式声明，否则控制台会以“非 systemd 环境”为由
  禁用一键更新（只给出 docker 命令提示）。

## 最小生产配置

```toml
listen_addr = "auto"
port = 18789
data_dir = "/var/lib/agent-relay"
admin_password_hash = "$2a$10$..."  # agent-relay -hash '...' 生成
```

## 公网模式

```toml
public = true
tls_cert = "/etc/letsencrypt/live/relay.example.com/fullchain.pem"
tls_key  = "/etc/letsencrypt/live/relay.example.com/privkey.pem"
listen_addr = "0.0.0.0"
```

公网模式永远要求 TLS（DESIGN §7.3），绝不允许明文 HTTP 对外。
Tailscale ACL 仍是外层防线，服务端默认只绑 tailnet IP。
