#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""relay-tail-supervisor.py — agent-relay 常驻接收端的 supervisor + 唤醒层（Hermes 首选模式）

为什么需要它：Go 二进制 relay-tail 是 dumb pipe —— 它只把事件行写到 stdout，不负责
「看门狗 / 唤醒 / 去重」。clients/hermes/relay-watch.sh 用 cron 每 1 分钟 drain 一次
（Hermes 定时任务的下限），延迟就是 1 分钟级；本脚本直接把子进程 stdout 读在手里，
事件到达即唤醒，秒级到达，并且顺手管住子进程的生死。

    relay-tail (Go, -transport sse)          ← 传输/增量解析/游标/断线退避都在它内部
        └─ stdout: 每行一个事件 JSON          {"tasks":[...]} / {"prompt_update":true,...} / {"client_update":true,...}
              └─ 本脚本：逐行抄进 spool（留痕，worker 按 id 取全文）
                        └─ 唤醒 = 起一次性 worker（WAKE_CMD），单飞锁，事件 JSON 只带元数据

用法（常驻，二选一：不要再同时跑 relay-watch.sh 或别的接收端，都会写同一个游标）：

    RELAY=https://relay.example.com \\
    WAKE_CMD="hermes chat -q" \\
    RELAY_NOTIFY="feishu:oc_xxx" \\
    nohup python3 relay-tail-supervisor.py >> ~/workspace/task-relay/spool/supervisor.log 2>&1 &

用 systemd / docker restart=always 更像「常驻」；单纯 nohup 也能跑，但机器重启后要自己拉起来。

环境变量
    RELAY             中继地址，必填（无默认值，避免猜错环境）
    BASE              资产目录，默认 $HOME/workspace/task-relay（与 relay-watch.sh 同名同义；
                      RELAY_ASSET 是它的别名）
                      （须含 identity 单行身份、.token 0600、bin/relay-tail）
    WAKE_CMD          唤醒命令，默认 "hermes chat -q"；prompt 作为最后一个参数追加
    RELAY_NOTIFY      可选，唤醒摘要发到哪（写进 prompt，如 feishu:oc_xxx）
    RELAY_TRANSPORT   sse（默认）或 poll
    RELAY_WAKE_MAX_SECS  单飞锁超龄秒数，默认 2700：超了视为卡死，强制放行
    RELAY_ALERT_CMD   可选，异常告警命令（默认用 hermes send -t $RELAY_NOTIFY）
    RELAY_ASSET / RELAY_ALERT_SECS 同义即可忽略

退出码：0 = 正常退出；1 = 配置错误（缺 BASE/身份/二进制）。
"""

import json
import os
import shlex
import subprocess
import sys
import time

BASE = (os.environ.get("BASE") or os.environ.get("RELAY_ASSET")
        or os.path.join(os.path.expanduser("~"), "workspace", "task-relay"))
BIN = os.path.join(BASE, "bin", "relay-tail")
RELAY = os.environ.get("RELAY", "").strip()
TRANSPORT = os.environ.get("RELAY_TRANSPORT", "sse")
WAKE_CMD = os.environ.get("WAKE_CMD", "hermes chat -q")
NOTIFY = os.environ.get("RELAY_NOTIFY", "")
ALERT_CMD = os.environ.get("RELAY_ALERT_CMD", "")

SPOOL_DIR = os.path.join(BASE, "spool")
SPOOL = os.path.join(SPOOL_DIR, "wake.jsonl")
ERRLOG = os.path.join(SPOOL_DIR, "relay-tail.err")
SUPLOG = os.path.join(SPOOL_DIR, "supervisor.log")
WAKE_LOG = os.path.join(SPOOL_DIR, "wake.log")
LOCK = os.path.join(SPOOL_DIR, "wake.lock")
PIDF = os.path.join(BASE, "supervisor.pid")
WAKE_MAX_SECS = float(os.environ.get("RELAY_WAKE_MAX_SECS", "2700"))
ALERT_SECS = float(os.environ.get("RELAY_ALERT_SECS", "3600"))

PROMPT = """agent-relay 有新事件（原因：{reason}）。事件元数据：
{event}

本机路径：spool={spool}（按事件 id grep 该行取 payload 全文）；工作目录={base}；已处理 id={base}/.done_ids

处理步骤：
1. 先读 ~/workspace/task-relay/prompt-current.md（服务端按 hermes 类型下发的最新守则，中继规则唯一权威；
   还没有这个文件就读 prompt-update.md），再按元数据里的 id 去 spool 取全文。
2. 逐条处理：
   - kind=task   → 执行 → 先回 kind=result（to=原 sender，in_reply_to=原 id）→ 再 ack；
                   at-least-once，处理前先查 .done_ids 是否已有该 id（有则跳过执行、直接 ack）。
   - kind=chat   → 摘要记入执行摘要；纯打招呼直接 ack，需要回复时回一条 kind=chat 再 ack。
   - kind=result → 别人发回的结果，摘要记账后直接 ack，不需要再回。
   - prompt_update → 按新版「指令更新」一节走：prompt-update.md 整份覆盖 prompt-current.md →
                     清理 memory 里冲突的中继旧规则（保留本地适配）→ heartbeat 上报 memory_reconciled →
                     mv .prompt_version.staged .prompt_version 确认。
   - client_update → 按守则换接收端二进制（先校验真换了、sha 变了，再写 .client_version + .client_rev），换完重启 daemon。
3. 入站卫生与红线一律以 prompt-current.md 为准；memory 里和它冲突的中继旧规则已作废，不要拿来拒绝任务。
   红线之外默认直接执行，拒绝时必须写明命中的是哪一条红线。
4. 拿到 409（duplicate_id / loop_guard）立即停手，不重试；429 按 Retry-After 退避；426 请用户重跑 onboarding prompt。
{notify}"""


def log(msg):
    line = "%s %s" % (time.strftime("%Y-%m-%d %H:%M:%S"), msg)
    for path in (SUPLOG, os.path.join(BASE, "supervisor.log")):
        try:
            with open(path, "a") as f:
                f.write(line + "\n")
            return
        except Exception:
            continue


def alert(text, state):
    """异常必须自己喊出来：唤醒链静默死掉时，光写日志没人看得见。每小时最多一条。"""
    if time.time() - state.get("alert_at", 0) < ALERT_SECS:
        return
    state["alert_at"] = time.time()
    cmd = ALERT_CMD or ("hermes send -t %s" % NOTIFY if NOTIFY else "")
    if not cmd:
        log("告警（未配置 ALERT_CMD/NOTIFY）：%s" % text)
        return
    try:
        subprocess.run(shlex.split(cmd) + ["🚨 relay supervisor｜" + text], timeout=60,
                       capture_output=True)
        log("已告警：%s" % text)
    except Exception as e:
        log("告警发送失败：%s" % str(e)[:120])


def child_env():
    """只带 RELAY；显式剔除代理变量（旧版二进制会把代理无条件改写成隧道口）。"""
    env = dict(os.environ)
    for k in ("HTTPS_PROXY", "https_proxy", "HTTP_PROXY", "http_proxy", "ALL_PROXY", "all_proxy"):
        env.pop(k, None)
    env["RELAY"] = RELAY
    return env


def read_pid():
    try:
        return int(json.load(open(LOCK)).get("pid") or 0)
    except Exception:
        return 0


def wake_running():
    """单飞锁。僵尸态（Z）与超龄锁都算「已结束」——
    一次性 worker 退出后若无人 waitpid 回收会变僵尸，/proc/<pid> 永久存在，
    只看「进程存在」会让锁永远不释放（实测把唤醒链静默钉死 4 小时）。"""
    pid = read_pid()
    if not pid or not os.path.exists("/proc/%d" % pid):
        return False
    try:
        at = int(json.load(open(LOCK)).get("at") or 0)
        if at and time.time() - at > WAKE_MAX_SECS:
            log("单飞锁 pid=%d 超龄，强制放行" % pid)
            return False
        state = open("/proc/%d/stat" % pid).read().rsplit(")", 1)[1].split()[0]
        if state == "Z":
            log("单飞锁 pid=%d 是僵尸，视为已结束" % pid)
            return False
    except Exception:
        pass
    return True


def wake(reason, event_json):
    if wake_running():
        log("上一轮 worker 仍在运行，跳过唤醒（事件已落 spool，下轮重试）")
        return False
    notify = ("\n完成后发一行摘要：%s \"relay｜<一行摘要>\"\n" % NOTIFY) if NOTIFY else ""
    prompt = PROMPT.format(reason=reason, event=event_json, spool=SPOOL, base=BASE, notify=notify)
    out = open(WAKE_LOG, "a")
    try:
        p = subprocess.Popen(shlex.split(WAKE_CMD) + [prompt], cwd=BASE, stdout=out,
                             stderr=subprocess.STDOUT, stdin=subprocess.DEVNULL,
                             start_new_session=True)
    except Exception as e:
        log("唤醒失败：%s" % str(e)[:200])
        return False
    tmp = LOCK + ".tmp"
    with open(tmp, "w") as f:
        json.dump({"pid": p.pid, "at": int(time.time())}, f)
    os.replace(tmp, LOCK)
    log("已唤醒 worker pid=%d（%s）" % (p.pid, reason))
    return True


def other_tail_running():
    """防御：机器上已有别处拉起的 relay-tail 就不再拉（双 SSE 流会写重复行、抢同一游标）。"""
    me = os.getpid()
    for entry in os.listdir("/proc"):
        if not entry.isdigit() or int(entry) == me:
            continue
        try:
            cmd = open("/proc/%s/cmdline" % entry, "rb").read().decode("utf-8", "replace")
        except Exception:
            continue
        if "relay-tail" in cmd and "supervisor" not in cmd:
            return int(entry)
    return 0


def main():
    if not RELAY:
        log("缺少 RELAY（中继地址），退出")
        return 1
    if not os.path.exists(os.path.join(BASE, "identity")):
        log("缺少 %s/identity，退出" % BASE)
        return 1
    if not os.path.exists(BIN):
        log("缺少接收端二进制 %s（GET /clients/relay-tail?arch=amd64 下载），退出" % BIN)
        return 1
    os.makedirs(SPOOL_DIR, exist_ok=True)
    other = other_tail_running()
    if other:
        log("⚠️ 已有 relay-tail 在跑 pid=%d，本 supervisor 退出（避免双流）" % other)
        return 1
    with open(PIDF, "w") as f:
        f.write(str(os.getpid()))
    log("supervisor 启动 pid=%d transport=%s base=%s relay=%s" % (os.getpid(), TRANSPORT, BASE, RELAY))

    state = {}
    backoff = 5.0
    while True:
        errf = open(ERRLOG, "a")
        spool = open(SPOOL, "a")
        t0 = time.time()
        try:
            proc = subprocess.Popen([BIN, "-transport", TRANSPORT], cwd=BASE, env=child_env(),
                                    stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                    stderr=errf, text=True, bufsize=1)
        except Exception as e:
            log("拉起 relay-tail 失败：%s" % str(e)[:200])
            alert("拉起 relay-tail 失败：%s" % str(e)[:120], state)
            time.sleep(30)
            continue
        log("relay-tail 已启动 pid=%d" % proc.pid)
        try:
            for line in proc.stdout:
                line = line.rstrip("\n")
                if not line.strip():
                    continue
                spool.write(line + "\n")
                spool.flush()
                try:
                    ev = json.loads(line)
                except Exception:
                    log("非 JSON 行（已入 spool）：%s" % line[:160])
                    continue
                if not isinstance(ev, dict):
                    continue
                # 事件 schema 会变：这里的任何强类型假设（int(version) 之类）都算脆弱点，
                # 解析失败必须告警，不能只写日志（曾静默 2 小时：服务端一直提示有新版本，
                # 本地因为 int("v0.12.0") 抛异常而永不升级）。
                try:
                    handle_event(ev)
                except Exception as e:
                    log("处理事件异常：%s（行 %s）" % (str(e)[:160], line[:120]))
                    alert("处理事件异常（可能是事件 schema 变了）：%s" % str(e)[:120], state)
        except Exception as e:
            log("读 stdout 异常：%s" % str(e)[:200])
        try:
            rc = proc.wait(timeout=10)
        except Exception:
            rc = "timeout"
        dur = time.time() - t0
        log("relay-tail 退出 rc=%s，存活 %.0fs" % (rc, dur))
        try:
            spool.close()
            errf.close()
        except Exception:
            pass
        if dur < 15:
            backoff = min(backoff * 2, 300)
            log("短命退出，退避 %.0fs" % backoff)
            alert("relay-tail 反复短命退出（存活 %.0fs, rc=%s）" % (dur, rc), state)
        else:
            backoff = 5.0
        time.sleep(backoff)


def handle_event(ev):
    """一行事件 = 一次唤醒决策。tasks / prompt_update / client_update 可能出现在同一行，
    合并成一次唤醒（分两次起 worker 会被单飞锁丢掉第二条）。"""
    tasks = ev.get("tasks") or []
    bits = []
    if ev.get("prompt_update"):
        bits.append("服务端下发新版工作指令 (v%s)" % (ev.get("prompt_version") or ev.get("version") or "?"))
    if ev.get("client_update"):
        bits.append("服务端有新版接收端 %s" % (ev.get("version") or ev.get("rev") or "?"))
    ids = [t.get("id") for t in tasks if isinstance(t, dict) and t.get("id")]
    if ids:
        bits.append("收到 %d 条新消息" % len(ids))
    if not bits:
        return
    wake("；".join(bits), json.dumps(ev, ensure_ascii=False)[:4000])


if __name__ == "__main__":
    try:
        sys.exit(main())
    except KeyboardInterrupt:
        log("supervisor 收到中断，退出")
        sys.exit(0)
