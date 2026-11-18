# Muse 唤醒 Noise 验证：受控 live 验收

> 状态：待在中继机上执行。本沙盒出口被代理挡在 muse.ai 之外（phase 0 已由主人
> 2026-11-18 约 16:50 豁免），因此这里记录的是**前置条件、步骤和通过标准**，不是已完成的
> live 结果。
>
> 关联：`docs/DESIGN.md` §4.9、`docs/API.md`「Muse 账号授权与保活」。
> 代码分支：`feat/muse-noise-verify`。合并前必须完成本文的 3 次连续验证与 11 台 sweep 计时。

## 1. 目标与硬门

一次 `wake=true` 的续期只有在下面全部成立时才算成功：

1. `/api/session` 第二次读取仍为 `status=assigned`；
2. wake POST 返回 200（`wake_requested=true`）；
3. `wss://hatch.metaaivm.com/v1/noise` 完成标准 Noise XX 握手；
4. 一次只读 RPC（`chat.unread_count`，404/405 时 `identity`）返回 2xx/JSON。

`vm_state` 只展示，不参与判定。合并硬门：

- 连续 3 次 `wake_state=noise_confirmed`、`wake_ok=true`、`last_wake_error` 为空；
- 11 台账号的 `KeepaliveAll` sweep 在 10 分钟内完成（超过则按设计 §6.9 拆出
  `opMu`，本次不拆）；
- 日志、审计、`last_wake_error` 中无 token/cookie/完整 WSS URL/响应正文；
- `go test ./...`、`go test -race ./internal/muse/...`、`go vet ./...`、
  `make test-ui-js` 全绿（CI 同样要求）。

## 2. 前置条件

- 在中继机（不是 laptop、不是本沙盒）上执行，出口与生产 `agent-relay` 进程一致
  （同一台机、同一代理、同一 network namespace）。
- 使用一个已分配 VM 的真实 Muse 账号；优先选 `vm_state=UNKNOWN` 的账号，才能覆盖
  本次要解决的场景。账号已通过控制台授权，`last_error` 为空、`auth_state=ok`。
- 同一账号没有第二个保活方（muse2api、另一个 relay、手工脚本都不能同时跑），否则
  cookie 轮换会互相顶掉。
- 用 0600 临时文件或环境变量保存 cookie，**不要**写进仓库、shell history、日志或
  PR；验收结束后删除临时文件。
- 分支已构建：`go build ./...` 通过；如需跑临时实例，使用独立 `data_dir`，不要覆盖
  生产数据目录。

## 3. 步骤

1. **本地检查（可在中继机或 CI 跑）**

   ```bash
   go vet ./...
   go test ./... -count=1
   go test -race ./internal/muse/... -count=1
   make test-ui-js
   ```

2. **准备受控实例**。在非生产端口/独立 `data_dir` 启动本次分支的二进制，绑定测试
   账号。不要把测试实例指向生产 `data_dir`，也不要让它的后台 keepalive 与生产实例
   同时跑同一账号。

3. **第一次验证**：通过控制台“立即保活”或
   `POST /admin/muse/accounts/{id}/keepalive` 触发一次。记录返回的 `result`：
   - `ok=true`（session 正常）；
   - `wake_requested=true`；
   - `wake_ok=true`；
   - `wake_state=noise_confirmed`；
   - `wake_error` 为空。

   再在列表里确认持久化字段：`last_wake_ok=true`、`last_wake_state=noise_confirmed`、
   `last_wake_error` 为空、`last_error` 仍为空。

4. **连续 3 次**：重复步骤 3 至少三次（间隔按控制台允许的方式；每次都看 `result` 和
   列表行）。三次都满足才算通过；任何一次 `unconfirmed` 都算不通过，先保留
   `last_wake_error` 原文（脱敏后）再排查。

5. **失败注入（不写生产数据）**：在受控实例里临时把 gateway 指向不可达的 loopback
   端口或让 mock 返回 500，触发一次 wake，确认：
   - `wake_state=unconfirmed`、`wake_ok=false`；
   - `last_error` 仍为空、`auth_state` 仍为 `ok`；
   - 控制台账号 badge 是黄色“唤醒未确认”，不是红色“待重新授权”；
   - `last_wake_error` 只含 `noise <stage>: <class> (attempt N/3, Xs)`，无 secret。
   完成后恢复配置并重新跑一次成功验证。

6. **11 台 sweep 计时**：对 11 个已授权账号执行一次强制
   `POST /admin/muse/keepalive`，记录 `started_at`/`finished_at`（或墙钟时间）与
   `summary`。11 台最坏估算约 11×(25+0.2)s ≈ 4.6 分钟；实测 >10 分钟则本次不合并，
   按设计 §6.9 拆出独立验证方法后再测。

7. **secret 卫生检查**：对最近一次 sweep 的 gateway 日志、audit 行、
   `last_wake_error`、`GET /admin/muse/accounts` 响应做 grep：

   ```bash
   # 只检查本地受控实例日志；不要把真实值贴进 PR。
   grep -Ei 'auth_token|hatch_sess|hatch_gw|access_token|Authorization:' <controlled-log>
   ```

   期望无命中；`last_wake_error` 长度 ≤500。若命中，立即停止验收并删除日志中的片段。

## 4. 记录模板

| 项 | 值 |
|---|---|
| 中继机 / 出口 IP 或代理 |  |
| 账号（脱敏） / `vm_id` |  |
| 执行时间（UTC） |  |
| 第 1 次：耗时 / `wake_state` / `wake_ok` / `last_wake_error` |  |
| 第 2 次：… |  |
| 第 3 次：… |  |
| 11 台 sweep：`started_at` → `finished_at` / 总时长 |  |
| 失败注入结果（badge/last_error/secret） |  |
| secret grep 结果 |  |

## 5. 不通过时

- **三次中任意失败**：保留脱敏后的 `last_wake_error`、阶段、耗时和出口信息；不要合并。
  若错误是 `dial 403`/WAF/HTML，先换出口或代理复测；若是 `handshake bad_decrypt`，
  保留复现步骤，先查帧/Noise 状态机，不要盲目重试。
- **11 台 sweep >10 分钟**：不合并；按设计 §6.9 把验证移出 `opMu`（targeted
  `SetMuseWakeResult`），并保留 cookie 轮换在锁内 seal。
- **secret 泄漏**：不合并；清理日志/审计，修复脱敏后再从头验收。

## 6. 已知未核实项

- msg1 空载荷是否对所有 VM 都成立（muse-cli 实测账号通过；备选是 32 字节
  `client_nonce`）。
- `hatch/token` 的 `vmAddress` 是否必须与 `endpoint_url` 完全一致（本次按
  `endpoint_url` 优先、缺失再拼）。
- `app_id`/`request_id` 是否被服务端校验（本次基线不加）。
- Go `crypto/tls` 的 ClientHello 是否能稳定通过 WAF；这只能由本文的中继机验收回答。
- access/hatch token 的实际 TTL（25 秒预算内复用，未测过期边界）。
