# POSTGRES.md — 用 Postgres 替换 SQLite（P2 预留）

`internal/store.Store` 是唯一的存储边界：除 `internal/store` 外，
任何模块不得写 SQL、不得 import `database/sql`。因此 Postgres 替换
只需新增一个实现，无需改动协议、网关、Guard、Queue。

## 接口约束（实现者必读）

实现 `store.Store` 的全部方法（见 `internal/store/store.go` 接口定义）。
语义契约：

- `InsertMessage` 必须保证 `UNIQUE(sender, id)`，冲突返回可被
  `strings.Contains(err, "UNIQUE")` 识别的错误（queue 靠它转 409）。
- `VisibleTo` 必须与 SQLite 版逐行一致：direct + broadcast（排除自发）、
  `seq > since`、`approval_state NOT IN ('pending','rejected')`、
  未被该 peer ack，`ORDER BY seq ASC LIMIT n`。
- `seq` 必须是全局单调自增（Postgres 用 `BIGSERIAL` / `GENERATED ALWAYS AS IDENTITY`）。
- `Ack` 必须是幂等的 `INSERT ... ON CONFLICT DO NOTHING`。
- `FuseWatermark` / `verify_state` 用 `ON CONFLICT ... DO UPDATE` 语义。
- 时间全用 Unix 秒（`BIGINT`），不要用 `TIMESTAMPTZ`（避免时钟语义漂移）。

## 建议表结构

```sql
CREATE TABLE peers (...);            -- id TEXT PRIMARY KEY
CREATE TABLE tokens (...);           -- id BIGSERIAL PRIMARY KEY
CREATE TABLE invites (...);          -- code_hash TEXT PRIMARY KEY
CREATE TABLE messages (
  seq BIGSERIAL PRIMARY KEY,
  id TEXT NOT NULL, sender TEXT NOT NULL, recipient TEXT NOT NULL,
  kind TEXT DEFAULT 'task', in_reply_to TEXT DEFAULT '', root_id TEXT DEFAULT '',
  requires_approval INT DEFAULT 0, approval_state TEXT DEFAULT 'n/a',
  payload TEXT NOT NULL, created_at BIGINT NOT NULL,
  UNIQUE(sender, id)
);
CREATE TABLE acks (message_seq BIGINT REFERENCES messages(seq), peer_id TEXT NOT NULL, acked_at BIGINT NOT NULL, PRIMARY KEY(message_seq, peer_id));
CREATE TABLE audit_log (seq BIGSERIAL PRIMARY KEY, ts BIGINT NOT NULL, actor TEXT NOT NULL, action TEXT NOT NULL, detail TEXT DEFAULT '');
CREATE TABLE thread_fuses (root_id TEXT PRIMARY KEY, cleared_seq BIGINT DEFAULT 0, updated_at BIGINT DEFAULT 0);
CREATE TABLE verify_state (peer_id TEXT PRIMARY KEY, smoke_id TEXT DEFAULT '', updated_at BIGINT DEFAULT 0);
CREATE INDEX ON messages(recipient, seq);
CREATE INDEX ON messages(root_id, seq);
CREATE INDEX ON audit_log(ts);
```

## 接线

1. 新建 `internal/store/pg.go`：`func OpenPostgres(dsn string) (Store, error)`。
2. `cmd/agent-relay/main.go` 按 config 新增 `database_url` 时切换
   `store.Open` → `OpenPostgres`（其余代码零改动）。
3. 保留 SQLite 为默认与测试后端；`guard/queue/auth` 的单测继续跑 SQLite。

## 何时做

出现以下任一再做：单机 SQLite 写 contention 可观测、
多实例部署需求、消息量让 `VACUUM` 窗口不可接受。
 在此之前不要做（DESIGN §13：这是唯一值得引入外部依赖的特性，
 且只为 worker-pool leases 铺路）。
