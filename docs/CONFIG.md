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
| `message_ttl_days` | int | `30` | 全 ack 且超期的消息归档为 `archive/YYYY-MM.jsonl` 后删除 |
| `peer_prune_after_days` | int | `7` | 超期未见的 peer 行删除（token 保留，可随时回来） |
| `audit_retention_days` | int | `90` | 审计日志保留 |
| `offline_webhook_url` | string | `""` | peer 由在线转离线时 POST `{"peer_id","offline_secs","ts"}` |
| `max_body_bytes` | int | `1048576` | 请求体上限 1 MiB |

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
