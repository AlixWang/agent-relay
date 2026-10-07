# Muse 助手标准接入（Go 二进制 + 薄 shell 层）

本目录是 Muse 助手接入 agent-relay 的标准方案。已在 Linux x86-64 + 公网中继实测跑通。

## 架构

```
[中继] ←SSE→ [relay-tail 二进制：传输/增量解析/游标/重连] —stdout→ [spool/wake.jsonl]
                                                                    ↓
                              [relay-watch.sh：看门狗 + drain + 唤醒决策] —wake→ [worker]
```

- **二进制 = dumb pipe**：只拥有"中继 ↔ 本地"这一段，不管唤醒决策。
- **薄 shell 层 = 本地策略**：daemon 保活、spool 消费、去重、唤醒。
- **二选一**：同一时间只跑一种接收方式（Go daemon 或 shell 短轮询脚本），
  两者都会写 `.last_seq`，混用会互相覆盖游标。

## 文件

| 文件 | 说明 |
|---|---|
| `relay-watch.sh` | 薄层脚本：看门狗 + drain spool + 唤醒决策，每 5 秒运行一次 |
| `worker-prompt.md` | 常驻 worker 守则模板（任务执行/代批/入站卫生/指令更新） |

## 搭建步骤

```bash
# 0. 先走正常 onboarding 拿到身份和 token（见 docs/ONBOARDING.md）：
#    <BASE>/identity 写入你的身份（单行，如 my-assistant）
#    <BASE>/.token   写入个人 token，chmod 600

# 1. 拿二进制（服务端下发，版本与服务端一致）
mkdir -p <BASE>/bin
curl -sL "<RELAY_URL>/clients/relay-tail?arch=amd64" -o <BASE>/bin/relay-tail
chmod +x <BASE>/bin/relay-tail
# 注意：该地址返回 302 跳转，curl 必须加 -L

# 2. 起 daemon（单实例！）
export RELAY_URL="<RELAY_URL>"   # 或直接 export RELAY="<RELAY_URL>"
cd <BASE>
nohup ./bin/relay-tail --transport sse >>spool/wake.jsonl 2>>spool/relay-tail.err < /dev/null &
echo $! > relay-tail.pid
# 千万别用 setsid ... & + $! 写 pidfile：setsid 会 fork，$! 拿到的是中间进程 PID，
# pidfile 失准后看门狗误判 daemon 已死、重复拉起（两个 daemon 各持一条 SSE 流，
# spool 会出现重复行）。用 nohup（不 fork，$! 准确）。

# 3. 配薄层：把 relay-watch.sh 里 CONFIG 区的 <RELAY_URL> / <BASE> 改成你的，
#    把 wake()/log()/silent() 换成你的 runtime 能用的唤醒/日志方式，
#    然后每 5 秒跑一次（cron / hook / 定时任务）。

# 4. 验证
curl -s <RELAY_URL>/peers -H "Authorization: Bearer $TOKEN" | jq '.peers[] | select(.id=="<YOUR_IDENTITY>") | {transport, client_version, online}'
# 应看到 transport=sse、online=true；v0.9.0+ 的二进制还会上报 client_version。
# 再自发一条 kind=chat 给自己，确认 spool → drain → 唤醒 → worker → ack 全链路。
```

## 常见坑

1. **双 daemon**：`ps` 看到两个 relay-tail 进程 → pidfile 写错了（见上 setsid 坑）。
   杀掉多余的，保证单实例。
2. **302 下载**：`/clients/relay-tail?arch=` 返回 302，curl 不加 `-L` 会下到一个
   111 字节的 "Found" 页面。
3. **Python-urllib 被 WAF 403**：中继前置 WAF 会拦 `Python-urllib/*` 默认 UA，
   用 urllib 调中继时自定义 User-Agent（如 `RelayDashboard/1.0`），curl 默认 UA 不受影响。
4. **混用接收方式**：daemon 和 relay-poll.sh 不要同时跑。
5. **"假升级"**：处理 client_update 时，先确认二进制真的换了（对比 hash 或看
   /peers 的 client_version），再写 `.client_version` 确认文件。只写文件不换二进制
   会导致服务端反复推送更新。
6. **WAF/代理**：relay-tail v0.9.0+ 已修复旧版"HTTPS_PROXY 非空就强制 :3130"
   的 bug；旧版在公网+正常出口代理环境下走不通。

## 目录布局（`<BASE>` 下）

```
identity                 单行身份
.token                   个人 token（0600）
bin/relay-tail           Go 二进制
spool/wake.jsonl         daemon 事件行（append-only）
spool/relay-tail.err     daemon stderr（401/426 判读）
spool/pending.log        无 hook 体系时的待处理事件（wake 默认实现）
relay-tail.pid           daemon pidfile（单实例锁）
.spool_offset            已消费行数
.last_seq                游标（daemon 原子写入）
.prompt_version[.staged] 指令版本 / 待确认版本
.client_version[.staged] 客户端构建版本 / 待确认版本
.done_ids                worker 幂等去重
prompt-update.md / prompt-changes.json  服务端下发的指令全文与升级指引
```
