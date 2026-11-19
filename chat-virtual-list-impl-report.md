# 聊天列表虚拟化实现报告

- 分支：`feat/chat-virtual-list`（从 `origin/main` @ `16bfb0d` 起步）
- 范围：服务端线程分页 + 指挥台 `#command` 虚拟窗口 + 线程抽屉分页
- 未执行：开 PR、合并、打 tag、生产库删除
- 报告日期：2026-11-19

## 1. 结论

生产事故的两层根因都已处理：

1. **服务端不再默认返回线程最旧的 1000 条。** `GET /admin/messages?thread=` 改为游标分页，默认返回最新一页（50 条），支持 `before_seq` 向历史翻页、`after_seq` 供轮询取新消息。
2. **控制台不再把整条线程交给 Alpine `x-for`。** `#command` 只挂载可视窗口约 50 行，历史用上下 spacer 撑起滚动条；“加载更早消息”按页前插，回到底部时窗口滑回最新页。
3. **线程抽屉**初始只渲染最新 50 条，点“加载更早消息”按 `before_seq` 追加。
4. **轮询**从 3 秒全量重拉改为 `after_seq=maxSeq&page_size=200` 的增量追加，最多连续追赶 5 页。

大线程回归结果：1000 条 mock 线程在 `#command` 中只挂载 **50 个 `.cmd-msg` 节点**（上限断言 ≤100），加载时间与 50 条线程同一量级；同环境对 `origin/main` 的全量渲染 A/B 中，旧实现约 **4.5–5.3s** 才渲染完 1000 行，新实现约 **1.1–2.6s** 出现最新行，DOM 从 1000 降到 50。

## 2. 设计决策

### 2.1 分页参数与返回语义

`GET /admin/messages?thread=<root_id>`：

| 参数 | 默认 | 说明 |
| --- | --- | --- |
| `page_size` | `50` | 上限 `200`；非法/≤0 时回退默认值 |
| `before_seq` | 无 | 取 `seq < before_seq` 的更旧一页 |
| `after_seq` | 无 | 取 `seq > after_seq` 的更新一页；与 `before_seq` 同时出现时以 `after_seq` 为准 |

返回字段：

- `items`：本页消息，**始终 oldest-first**，调用方可以直接拼接。
- `total`：线程总条数（不是本页条数）。
- `has_more`：方向随请求变化。默认页 / `before_seq` 模式表示“还有更早的消息”；`after_seq` 模式表示“还有更新的消息”。
- `oldest_seq` / `newest_seq`：本页边界；空页为 0。
- 原有 `room`、每条消息的 `read` / `total` / `acked_by` 保持不变。

默认页的实现是 `ORDER BY seq DESC LIMIT page_size+1` 后反转，因此不再有“只看得到最旧 1000 条”的问题。`before_seq` 同理从游标向前取 `DESC` 再反转；`after_seq` 直接 `ASC`。多取一条用于判断 `has_more`，避免二次查询。`total` 用同一线程的 `COUNT(*)`。

`ThreadPage` 加在 `internal/store/store.go` 的 `Store` 接口上，供 admin 路由使用；旧的 `ThreadMessages(root, limit)` 仍保留给其它内部调用，但 admin 线程接口已不再走全量路径。

### 2.2 指挥台虚拟窗口

`internal/web/ui/js/command.js` 把状态拆成两层：

- `messages`：已加载的模型（最新页 + 手动加载的历史页 + 轮询追加）。
- `visibleMessages`：DOM 实际渲染的切片，`windowStart` 起 50 行。

常量：

| 常量 | 值 | 用途 |
| --- | --- | --- |
| `CMD_WINDOW` | 50 | 渲染窗口大小，也是首屏/翻页 `page_size` |
| `CMD_OVERSCAN` | 8 | 窗口滑动的提前量，避免每滚一行就换窗口 |
| `CMD_EST_HEIGHT` | 88px | 尚未测量过的高度的估值 |
| `CMD_GAP` | 12px | `.cmd-stream` 的行间距，与 CSS `--s-3` 对齐 |
| `CMD_BOTTOM_SLACK` | 32px | 判定“贴底”的容差，沿用日志页经验 |

窗口上下各有一个 `.cmd-vpad` spacer，高度来自按 `seq` 缓存的实际行高：

- `topPad = offset(windowStart)`
- `bottomPad = contentHeight - offset(windowStart + rendered)`
- `offset()` 是 `msgHeights` 的前缀和；未测量的行用估值。
- 每轮渲染后用 `measureVisibleRows()` 读 `offsetHeight` 回填 `msgHeights`，Markdown 长短行不会让滚动条长期失真。

窗口滑动时做锚定：记录当前第一个可见行的 `seq` 与视口位置，改 `windowStart` 后等两拍（spacer 样式/行高更新是响应式更新），再用临时的 `scroll-behavior: auto` 瞬间修正 `scrollTop`，避免 CSS smooth 动画产生中间滚动事件、让窗口自己追自己。程序化滚动期间用 `_vtAdjusting` 标志吞掉 120ms 内的 scroll 事件。

保留的行为：

- 打开/切换会话：立即贴底，不从顶部平滑滚下来。
- 已在底部时收到新消息：跟随到底部，`newBelow=false`。
- 用户停在历史：不抢滚动位置，`newBelow=true`，显示“有新消息 · 回到底部”。
- 折叠/展开、复制、ack 弹窗、已读数逻辑未改，只是从“对全部行”变成“对窗口内的行”。

“加载更早消息”：

- 以 `oldestSeq` 为游标请求 `before_seq=oldestSeq&page_size=50`。
- 前插并去重，更新 `oldestSeq` / `hasOlder` / `totalMessages`。
- 用户原本在顶部时，把新加载块放在锚点上方；原本在中间时，保持锚点行的视口位置。
- 加载后 DOM 仍只保留一个窗口（测试断言 ≤100，实际 50）。

### 2.3 轮询取新

`pollUpdate()` 仍是 3 秒一次，但 room/peer 线程不再调用 `loadActiveMessages`：

```text
after_seq = newestSeq（没有则 maxMessageSeq）
page_size = 200
最多连续 5 页；fresh = items 中 seq > cursor
追加到 messages，推进 newestSeq，更新 totalMessages
```

- 只有新行会进入模型和窗口；不会重新拉取 1000 条 payload、不会重新 Markdown 全部历史。
- 每轮最多追赶 1000 条新消息，下一轮从推进后的游标继续。
- 在底部则跟随；在历史则保持滚动位置并显示跳转 chip。
- inbox / broadcast 仍走原来的小页刷新路径（它们的 payload 本来就有界）。

### 2.4 线程抽屉分页

`internal/web/ui/js/threads.js`：

- 初始 `GET /admin/messages?thread=...&page_size=50`，只渲染最新 50 条。
- 元信息显示“共 N 条消息 · 已显示 M 条 · 参与者…”。
- 顶部 sticky “加载更早消息（还有 X 条）”，点击用 `before_seq=oldestSeq&page_size=50` 前插。
- 空线程、审批按钮、复制按钮、失败 toast 沿用原来的行为。

取舍：抽屉是**分页**而不是严格虚拟滚动；每次点击增加 50 行，用户显式翻页才会增长。它解决了初始 1000 行 `innerHTML` 的问题，也符合“初始只渲染最新 50 条，加加载更多”的要求；代价是连续翻很多页后 DOM 会线性增长，但这是用户主动操作，且抽屉是短生命周期视图。`#command` 主消息流使用完整窗口化。

### 2.5 为什么选窗口化而不是绝对定位虚拟列表

- 现有消息是 flex 列布局，Markdown 行高差异大；绝对定位方案需要重写 CSS、行高测量、滚动锚定和响应式两套逻辑。
- 窗口化 + 上下 spacer 只改渲染切片，保留现有 `x-for`、事件委托、ack 弹窗和移动端样式，风险小。
- 行高按 `seq` 缓存，滚动条能随实际渲染逐步校准；对 1000 条量级已经足够。
- 代价：未测量的历史行用估值，快速拖动滚动条时滚动条长度会有小幅变化；窗口锚定修正把可见内容跳动压到最低。若未来单线程到数万条，再考虑固定行高或 `IntersectionObserver` 的绝对定位实现。

## 3. 改动文件

| 文件 | 改动 |
| --- | --- |
| `internal/store/store.go` | 新增 `ThreadPage` 类型、`Store.ThreadPage` 接口和 SQLite 实现（默认最新页、before/after 游标、+1 判 `has_more`、`total`） |
| `internal/store/store_test.go` | `TestThreadPageCursor`：默认最新页、向前/向后翻页、边界、空线程、默认 limit |
| `internal/gateway/admin_pages.go` | `parseThreadPage`：`page_size` 默认 50/上限 200，`after_seq` 优先 |
| `internal/gateway/gateway.go` | admin 线程接口改用 `ThreadPage`，返回 `total/has_more/oldest_seq/newest_seq/page_size` |
| `internal/gateway/gateway_test.go` | `TestAdminThreadCursorPagination`：60 条线程的默认页、200 上限、before/after、空线程、参数优先级 |
| `internal/web/ui/js/command.js` | 虚拟窗口、测高/spacer、锚定、加载更早、`after_seq` 轮询、发送/贴底适配 |
| `internal/web/ui/index.html` | “加载更早消息”、上下 spacer、`visibleMessages`、`data-vseq` |
| `internal/web/ui/style.css` | sticky 加载更早按钮、spacer 样式 |
| `internal/web/ui/js/threads.js` | 抽屉最新 50 条 + `before_seq` 分页 + “加载更早消息” |
| `internal/web/ui/_command-state.test.mjs` | 120 条 DM 的分页/窗口/轮询/历史位置单测 |
| `tests/mobile/console-virtual-list.spec.mjs` | 新增 Playwright 回归：1000 条窗口有界、加载更早、轮询 after_seq、抽屉分页 |
| `docs/API.md` | 线程接口分页参数、返回字段、`has_more` 方向 |
| `docs/DESIGN.md` | §9 admin 接口表与线程虚拟窗口说明 |
| `chat-virtual-list-impl-report.md` | 本报告 |

未改：prompt、receiver、`relay-watch.sh`、guard 阈值、`#muse-chat`、既有 `console-mobile.spec.mjs` 断言。

## 4. 验证结果

### 4.1 环境说明

本机 PATH 中没有 Go（环境重置后 `/usr/local/go` 不存在）。验证时下载 Go 1.23.12 到仓库内 `.gotool/`（已加入 `.git/info/exclude`），`GOCACHE=.gocache`（`.gitignore` 已忽略），未提交任何工具链文件。移动端 Playwright 用 `/opt/meta-chromium/chrome` + 上轮的 WebGL policy 参数，以及只在本地使用的 document 导航 `route.fulfill` fixture；临时配置和 `.local/` 拷贝在验证后已删除，未提交。

### 4.2 Go

| 命令 | 结果 |
| --- | --- |
| `gofmt -l <changed go files>` | 无输出 |
| `go vet ./...` | 通过 |
| `go build ./...` | 通过 |
| `go test ./...` | 全部包通过；gateway 12.609s、muse 19.006s、store 3.201s、web 6.636s |
| `go test -race ./internal/muse/...` | 通过，46.648s |
| `go test -race ./internal/gateway/...` | 通过，65.602s |

### 4.3 UI 单测

```text
make test-ui-js
```

7 个脚本全部通过：`_command-state`、`_logs-state`、`_message-render`、`_muse-state`、`_resources-state`、`_update-panel`、`_work-state`。

### 4.4 移动端 Playwright（本地）

```text
cd tests/mobile
npx playwright test console-mobile.spec.mjs console-virtual-list.spec.mjs \
  --config=playwright.local.config.mjs --reporter=list
```

结果：**10 passed (34.2s)**。

- 既有 `console-mobile.spec.mjs`：6 passed（iphone-se 320px + iphone-12 390px，原断言未改）。
- 新增 `console-virtual-list.spec.mjs`：4 passed：
  1. `1000-message stream mounts a bounded window and loads like a short one`
  2. `load earlier walks before_seq and keeps the DOM window bounded`
  3. `polling asks for after_seq=maxSeq and appends only new rows`
  4. `thread drawer opens on the newest 50 rows and pages older history`

新增测试的关键断言：

- 1000 条线程首屏只挂载 20–100 个 `.cmd-msg`，实际 **50**；50 条线程同为 **50**。
- 首屏 `.cmd-msg` 最后一行是 `data-vseq="1000"`，`data-vseq="900"` 不在 DOM。
- 滚到顶部点“加载更早消息”，请求带 `before_seq=951&page_size=50`；加载后 `#940` 在 DOM 中（count=1，约窗口上沿），`#950` 可见，节点数仍 ≤100。
- 再滚回底部，`#1000` 可见，节点数仍 ≤100（窗口滑回最新页，而不是把全部历史留在 DOM）。
- 轮询请求带 `after_seq=1000&page_size=200`；推入 1001/1002 后 `#1002` 可见，节点数 ≤100。
- 抽屉初始 50 行、`共 1000 条`、`data-copy-msg="1000"` 存在、`900` 不存在；点一次“加载更早”变 100 行，请求 `before_seq=951&page_size=50`。

### 4.5 大线程 A/B 数据

为回答“是否明显低于全量渲染”，用 `git archive origin/main` 在本地另一个端口起旧控制台，使用同一套 Playwright 和同一组 1000 条 mock 消息：

- 旧实现：等待 `#cmdStreamContainer .cmd-msg` 数量到 **1000**。
- 新实现：等待 `data-vseq="1000"` 可见，统计挂载节点数。

| 轮次 | 旧全量渲染 | 新虚拟窗口 |
| --- | ---: | ---: |
| 1 | 4543ms | 2574ms |
| 2 | 5250ms | 1632ms |
| 3 | 5098ms | 1472ms |
| 4 | 5122ms | 1106ms |

旧实现中位数约 **5.1s**，新实现约 **1.5s**（同一台本地机器、cold-ish 页面 boot、mock API；绝对值会随机器波动）。新实现的挂载节点数由回归测试单独断言：稳定为 **50**（一次 A/B 采样在 `toBeVisible` 后立刻 `count()` 读到 0，是 Alpine 重渲染的瞬时窗口，不是节点增长；专门回归测试断言 20–100）。生产 15s 的绝对值无法在本机复现，但方向一致：旧实现的耗时随 1000 行 Markdown/监听器线性增长，新实现只处理 50 行，因此与线程长度基本无关。

### 4.6 未覆盖/边界

- 没有在 CI 上跑移动端套件；本次是本地 meta-chromium 结果。CI 的 Playwright 配置仍按原方式运行。
- 没有保留“全量渲染”的生产回退路径；A/B 只在本地临时 worktree 中做，未提交。
- 抽屉是分页而非严格虚拟化：连续加载很多页后 DOM 会增长；主消息流不受影响。
- `ThreadPage` 每次请求多一次 `COUNT(*)`；在 `idx_messages_root(root_id, seq)` 上，1000 条量级可接受；若未来单线程到十万级，可改成窗口函数或缓存。
- `ThreadMessages` 方法仍存在于 store 接口，供其它内部调用；admin 路由已不再使用它。

## 5. 生产清理 op/Lisa 刷屏方案（只设计，不执行）

> 本节是操作设计，**没有在生产库上执行任何删除**。需要用户另行拍板，并由有生产权限的人执行。

### 5.1 删除范围

目标线程：`root_id='op/lisa'`（以实际 peer id 为准；如果生产 id 是 `op/Lisa`，所有条件同步替换）。

删除条件：

```sql
root_id = 'op/lisa'
AND kind = 'status'
AND created_at >= <11-18 22:46 本地时间对应 epoch>
AND created_at <  <11-19 14:51 本地时间对应 epoch>
```

预期约 **984 条**。保留：

- 该时间窗之前的零星 status/其它消息；
- 时间窗内非 `kind='status'` 的消息（如果有）；
- 15:20 的迁移回执（在窗口结束之后，天然排除）；
- 其它线程、其它 peer 的全部消息。

时间换算：聊天记录用的是服务器本地时间（按部署惯例是 Asia/Shanghai, UTC+8）。若年份为 2026：

```text
2026-11-18 22:46 CST = 1795013160
2026-11-19 14:51 CST = 1795071060
2026-11-19 15:20 CST = 1795072800
```

SQLite 里可用：

```sql
-- 先 dry-run 核对年份/时区；下面把本地时间按 UTC+8 换成 epoch
WHERE created_at >= strftime('%s','2026-11-18 22:46:00') - 8*3600
  AND created_at <  strftime('%s','2026-11-19 14:51:00') - 8*3600
```

如果 dry-run 的 `MIN/MAX` 对不上，停止，不要删。

### 5.2 删前只读核对

在只读连接上执行并留存输出：

```sql
-- 全线程概况
SELECT COUNT(*) AS total,
       MIN(seq) AS min_seq, MAX(seq) AS max_seq,
       datetime(MIN(created_at),'unixepoch','+8 hours') AS first_local,
       datetime(MAX(created_at),'unixepoch','+8 hours') AS last_local
FROM messages WHERE root_id='op/lisa';

-- 命中范围、按 kind 分组
SELECT kind, COUNT(*) AS n,
       MIN(seq) AS min_seq, MAX(seq) AS max_seq,
       datetime(MIN(created_at),'unixepoch','+8 hours') AS first_local,
       datetime(MAX(created_at),'unixepoch','+8 hours') AS last_local
FROM messages
WHERE root_id='op/lisa'
  AND created_at >= strftime('%s','2026-11-18 22:46:00') - 8*3600
  AND created_at <  strftime('%s','2026-11-19 14:51:00') - 8*3600
GROUP BY kind ORDER BY n DESC;

-- 应保留的边界样本：窗口前后各 20 条
SELECT seq, datetime(created_at,'unixepoch','+8 hours') AS local_time,
       kind, sender, substr(payload,1,100) AS preview
FROM messages
WHERE root_id='op/lisa'
  AND (created_at <  strftime('%s','2026-11-18 22:46:00') - 8*3600
       OR created_at >= strftime('%s','2026-11-19 14:51:00') - 8*3600)
ORDER BY seq DESC LIMIT 40;
```

把命中的 seq 列表导出成 CSV/JSONL，并记录 sha256：

```bash
sqlite3 -header -csv "$DB" \
  "SELECT seq,created_at,kind,sender,payload FROM messages
   WHERE root_id='op/lisa' AND kind='status'
     AND created_at >= ... AND created_at < ... ORDER BY seq" \
  > /var/lib/agent-relay/backups/op-lisa-victims.csv
sha256sum /var/lib/agent-relay/backups/op-lisa-victims.csv
```

### 5.3 备份

推荐先停服务再备份，保证备份之后没有新写入（清理窗口内本来也不该有新消息）：

```bash
DB=/var/lib/agent-relay/agent-relay.db
BACKUP=/var/lib/agent-relay/backups/op-lisa-precleanup-$(date -u +%Y%m%dT%H%M%SZ).db
sudo install -d -m 0700 /var/lib/agent-relay/backups
sudo systemctl stop agent-relay
sudo sqlite3 "$DB" ".backup '$BACKUP'"
sudo sqlite3 "$BACKUP" "PRAGMA integrity_check;"
sudo sha256sum "$DB" "$BACKUP" | tee "$BACKUP.sha256"
```

如果确实不能停服务，至少用 SQLite online backup 并在删除事务前确认没有新消息；但回滚时会丢掉备份之后写入的消息，因此不推荐。

### 5.4 删除事务

只删消息本身不够：`acks` 和 `message_tickets` 有 `message_seq` 外键引用，必须先清。

```sql
PRAGMA foreign_keys=ON;
BEGIN IMMEDIATE;

CREATE TEMP TABLE victims(seq INTEGER PRIMARY KEY);
INSERT INTO victims
SELECT seq FROM messages
WHERE root_id='op/lisa' AND kind='status'
  AND created_at >= strftime('%s','2026-11-18 22:46:00') - 8*3600
  AND created_at <  strftime('%s','2026-11-19 14:51:00') - 8*3600;

-- 再确认一次：必须是预期 ~984，且不含 15:20 回执
SELECT COUNT(*) AS victims, MIN(seq), MAX(seq) FROM victims;

DELETE FROM acks            WHERE message_seq IN (SELECT seq FROM victims);
DELETE FROM message_tickets WHERE message_seq IN (SELECT seq FROM victims);
DELETE FROM messages        WHERE seq         IN (SELECT seq FROM victims);

-- 审计：不写全量 seq（可能很长），写条件、数量、范围、备份名
INSERT INTO audit_log(ts, actor, action, detail)
VALUES (
  strftime('%s','now'),
  'operator',
  'messages.cleanup',
  'thread=op/lisa kind=status created_at=[1795013160,1795071060) '
  || 'deleted=' || (SELECT COUNT(*) FROM victims)
  || ' seq=' || (SELECT MIN(seq) FROM victims) || '..' || (SELECT MAX(seq) FROM victims)
  || ' backup=op-lisa-precleanup-*.db'
);

COMMIT;
```

执行前把 `victims` 的 count/min/max 与 5.2 的 dry-run 对齐；不一致就 `ROLLBACK`。`audit_log` 的 `actor` 用实际执行人标识，`detail` 里带备份文件名和 sha256 前缀。

### 5.5 删后验证

```sql
PRAGMA integrity_check;       -- 期望 ok
PRAGMA foreign_key_check;     -- 期望空

-- 总条数应从 ~1036 降到 ~52（以实际为准）
SELECT COUNT(*) FROM messages WHERE root_id='op/lisa';

-- 命中条件应为 0
SELECT COUNT(*) FROM messages
WHERE root_id='op/lisa' AND kind='status'
  AND created_at >= strftime('%s','2026-11-18 22:46:00') - 8*3600
  AND created_at <  strftime('%s','2026-11-19 14:51:00') - 8*3600;

-- 15:20 回执还在，窗口前后边界样本还在
SELECT seq, datetime(created_at,'unixepoch','+8 hours'), kind, substr(payload,1,80)
FROM messages WHERE root_id='op/lisa' ORDER BY seq DESC LIMIT 20;

-- 没有孤儿
SELECT COUNT(*) FROM acks a LEFT JOIN messages m ON m.seq=a.message_seq WHERE m.seq IS NULL;
SELECT COUNT(*) FROM message_tickets t LEFT JOIN messages m ON m.seq=t.message_seq WHERE m.seq IS NULL;
```

接口/UI 验证：

```bash
curl -s -b <admin-cookie> "$BASE/admin/messages?thread=op/lisa&page_size=50" \
  | jq '{total,has_more,page_size,first:.items[0].seq,last:.items[-1].seq}'
```

- `total` 应为清理后的条数（约 52）。
- 打开 `/#command?peer=lisa`，应直接显示最新回执，首屏 DOM `.cmd-msg` 约 50 个，加载时间在 1–2s 量级。
- 点“加载更早消息”能翻到清理前的零星消息，不应出现空白页/500。
- 线程抽屉 `共 N 条消息` 与接口 `total` 一致。
- 如 op/Lisa 的熔断水位仍让新 status 被节流，先观察；**不要把 `POST /admin/fuse/reset` 当成清理手段**，它只移水位不删历史。

### 5.6 回滚

- 删除事务提交前失败：直接 `ROLLBACK`。
- 提交后验证失败：停服务，用 5.3 的备份覆盖 `agent-relay.db`（连同 `-wal`/`-shm` 处理好），再启动；因为删除期间服务已停，备份之后没有新写入，回滚是完整的。
- 如果当时没停服务：备份之后的新消息会丢，必须先评估；必要时用 5.2 导出的受害行 CSV 之外的消息做人工核对。

### 5.7 为什么本次不加 admin 删除接口

现有代码只有 store 层的 `DeleteMessages(seqs)`，没有暴露 admin HTTP 删除接口。为一个一次性清理新增可批量删消息的管理端点，会扩大长期攻击面，还要补 CSRF、dry-run、审计、确认串、限流等一整套测试；不适合搭在本次 UI 性能修复里一起上线。

如果后续需要“可重复、可审计”的清理能力，建议另开小分支实现：

```text
POST /admin/messages/cleanup
body: { thread, kind, since, until, dry_run, confirm }
```

要求：仅 operator session；`dry_run=true` 只返回 count/seq 范围；真实删除要求 `confirm` 与 dry-run token 匹配；单次上限（如 2000 条）；事务内清 `acks`/`message_tickets`；写 `audit_log`（actor、条件、数量、范围、备份名）；返回删除数量。该接口**本次未实现**，避免把删数据能力随性能修复一起发布。

## 6. 提交与后续

- 代码、测试、文档和本报告一起提交到 `feat/chat-virtual-list` 并 push。
- 按任务要求：**不开 PR、不合并、不打 tag**。
- 生产清理未执行，等用户拍板后按第 5 节由有权限的人操作。
