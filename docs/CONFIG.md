# CONFIG.md — agent-relay 配置参考

配置文件为 TOML（默认 `/etc/agent-relay/config.toml`），完整示例见
`configs/config.toml.example`。未知字段会启动失败（拼写 fail-fast）。

## 字段表

| 键 | 类型 | 默认 | 说明 |
|---|---|---|---|
| `listen_addr` | string | `"auto"` | `"auto"`/`"tailnet"` = 绑定 `tailscale ip -4`，失败回退 `127.0.0.1`；其他值原样使用（`127.0.0.1` 用于测试，公网模式才可用 `0.0.0.0`） |
| `port` | int | `18789` | 监听端口 |
| `data_dir` | string | `/var/lib/agent-relay` | SQLite 文件 + `archive/` JSONL 归档目录 + `muse-cookie.key`（Muse Cookie 加密密钥） |
| `public` | bool | `false` | 公网 HTTPS 模式；`true` 时必须配 `tls_cert`+`tls_key`，UI cookie 才带 `Secure` |
| `tls_cert` / `tls_key` | string | `""` | 公网模式证书路径（Let's Encrypt 或自备） |
| `admin_password_hash` | string | `""` | bcrypt hash；`agent-relay -hash 'pw'` 生成。为空则启动时打印一次性随机密码（重启失效，仅开发用） |
| `protocol` / `min_client` | int | `1` / `1` | 协议版本与最低兼容；低于 `min_client` 的 peer 收 `426` |
| `online_timeout_secs` | int | `300` | presence 在线窗口（DESIGN §4.5） |
| `verify_timeout_secs` | int | `600` | 冒烟验证超时；超时标 `failed` 并区分 `registered_never_polled` / `polled_never_acked` |
| `fuse_max_messages` | int | `500` | 普通 1:1 线程最大消息数，超限 `409 loop_fuse_tripped`（UI 可 reset，不删历史）。群聊与 operator 私聊不走这条 |
| `fuse_max_age_secs` | int | `86400` | 普通 1:1 线程最大存活 24h。群聊与 operator 私聊豁免，改走下面的滚动窗口 |
| `rate_per_minute` | int | `60` | 每身份每 60s 上限（前 10 条 burst 放行），超限 `429` + `Retry-After` |
| `allow_peer_tasks` | bool | `true` | 是否允许 peer 给其他 peer 发 `kind=task`；`false` 时 peer→peer 返回 `409 task_not_allowed`（peer→operator/system 与 operator/system 发起均不受限，Phase 3） |
| `require_task_approval_from_peers` | bool | `false` | `true` 时 peer→peer 的 task 自动 hold，等控制台审批后投递（Phase 3） |
| `max_open_permissions` | int | `10` | 单 thread 未过期 open 权限请求上限，超限 `409 permission_rate_limited`（0=不限） |
| `progress_throttle_secs` | int | `10` | `status/progress` 每发送方每 thread 最小间隔，超限 `409 progress_throttled`（0=不限） |
| `permission_ttl_secs` | int | `600` | `permission_request` 缺省过期秒数（单请求 `expires_in_secs` 60–3600，可夹逼，上限硬顶 3600） |
| `message_ttl_days` | int | `30` | 全 ack 且超期的消息归档为 `archive/YYYY-MM.jsonl` 后删除 |
| `peer_prune_after_days` | int | `7` | 超期未见的 peer 行删除（token 保留，可随时回来） |
| `audit_retention_days` | int | `90` | 审计日志保留 |
| `agent_log_retention_days` | int | `30` | Agent 运行日志保留天数（`POST /logs`，独立于审计日志；0=不按天清理） |
| `agent_log_rate_per_minute` | int | `60` | 单 peer 每分钟最多上报多少条 Agent 日志事件（超出 `429 + Retry-After`） |
| `agent_log_max_rows_per_peer` | int | `20000` | 单 peer 保留的 Agent 日志最大行数，超出按最旧删除（0=不限） |
| `offline_webhook_url` | string | `""` | peer 由在线转离线时 POST `{"kind","key","peer_id","offline_secs","ts"}`，并带 `X-Agent-Relay-Effect-Kind/Key` 头；由持久化 effects 队列重试，接收方按 kind+key 幂等（Phase 3） |
| `max_body_bytes` | int | `1048576` | 请求体上限 1 MiB |
| `stream_keepalive_secs` | int | `20` | SSE 保活间隔秒（0=用 20；上限 120，§4.4b） |
| `stream_max_per_peer` | int | `3` | 单身份并发流上限，超限 `429`（0=用 3，§4.4b） |
| `profile_refresh_days` | int | `7` | 简介超期天数：heartbeat 带 `profile_refresh=true` 提醒更新（0=不提醒，§8.7） |
| `max_room_members` | int | `16` | 单个群聊的成员上限（2-64，§6.8） |
| `room_fuse_max_messages` | int | `60` | 群聊和 operator 私聊（`op/<peer>`）在窗口内最多消息数，超限 `409 loop_fuse_tripped`；窗口过去即可再发，不会被整线程熔断锁死 |
| `room_fuse_window_secs` | int | `3600` | 上面这条窗口的秒数（≥60，§6.8），群聊与 operator 私聊共用 |
| `muse_keepalive_secs` | int | `900` | Muse 后台保活周期；0=关闭后台循环（手动按钮仍可用），>0 必须 ≥60（§4.9） |
| `muse_wake_cooldown_secs` | int | `300` | 同一账号两次自动唤醒的最小间隔；0=使用默认 300，>0 必须 ≥60 |
| `muse_http_timeout_secs` | int | `12` | 调 muse.ai 的单请求超时（1–60） |
| `muse_auto_wake` | bool | `true` | 全局自动唤醒开关；账号级 `auto_wake` 还可单独关闭 |
| `muse_base_url` | string | `"https://muse.ai"` | Muse 控制面地址；生产必须 https，http 仅允许 loopback（测试） |
| `muse_proxy_url` | string | `""` | 出站代理（http/https/socks5）；空=使用服务环境里的 `HTTPS_PROXY` |

## Muse 保活（可选）

控制台的「Muse 保活」页为每个 muse 成员绑定一个 muse.ai 账号（粘贴
`hatch_sess` 登录态即可，`hatch_vml` 可选）。服务端只调用两个端点：

- `GET /api/session`：续签 `hatch_vml` / `hatch_sess`，返回当前 `vm_id` / `vm_state`；
- `POST /api/hatch/vm/wake`：唤醒沙盒 VM；HTTP 200 只算“请求已接受”，
  必须再次 `/api/session` 复核 `vm_state == RUNNING` 才算成功。

行为约定：

- **401** = Cookie 失效 → 标记 `needs_reauth`，保留旧 Cookie，后台暂停该账号直到重新授权；
- **403/429/5xx/超时/非 JSON** = 瞬时失败 → 保留 Cookie 与上次有效状态，下一轮重试；
- 每次唤醒前都重新取当前 `vm_id`，不使用缓存值（沙盒重启后旧 id 会失效）；
- 成员在线→离线跃迁时自动唤醒；账号级 `auto_wake` 或全局 `muse_auto_wake` 可关闭；
- 一个 Muse 账号同一时间只应由一个保活方管理。不要和 muse2api 同时对同一账号调用
  `/api/session`，两边轮换 Cookie 会互相顶掉；
- 授权请求从 relay 服务器出口发出，不是导出 Cookie 的浏览器。出口 IP/地区被风控时
  会返回 403，可配 `muse_proxy_url` 或 `HTTPS_PROXY`；
- Cookie 用 `data_dir/muse-cookie.key`（0600）做 AES-GCM 加密，API 永不回显。
  **备份 data_dir 时连同该密钥一起备份**；密钥丢失后所有已授权账号需要重新粘贴 Cookie。

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
