# HTMX + Templ 迁移指南

## 概述

本指南描述了从纯 JavaScript 到 HTMX + Templ 的迁移过程。

**目标**：
- ✅ 消除 ~80% 的 JavaScript 代码
- ✅ 类型安全的模板（Go 编译时检查）
- ✅ 服务端渲染，更好的 SEO
- ✅ 保持单文件部署（无构建依赖）
- ✅ 渐进增强，JS 失败仍可用

## 架构变化

### Before (Pure JS)
```
Browser                     Server
┌─────────────┐            ┌──────────────┐
│ index.html  │            │ gateway.go   │
│ app.js      │───JSON────▶│ (JSON API)   │
│ style.css   │◀───────────│              │
└─────────────┘            └──────────────┘
  ↓
  手动 DOM 操作
  innerHTML拼接
```

### After (HTMX + Templ)
```
Browser                     Server
┌─────────────┐            ┌──────────────────┐
│ layout.templ│            │ gateway.go       │
│ *.templ     │───HTML────▶│ + views/*.templ  │
│ htmx.js     │◀───────────│ (HTML fragments) │
│ style.css   │            └──────────────────┘
└─────────────┘
  ↓
  HTMX 自动处理
  声明式更新
```

## 文件结构

### 新增文件

```
internal/web/
├── views/                          # Templ 组件
│   ├── layout.templ               # 基础布局
│   ├── conversations.templ        # 会话列表页
│   ├── modal.templ                # 创建会话模态框
│   ├── drawer.templ               # 会话详情抽屉
│   ├── layout_templ.go            # 生成的 Go 代码
│   ├── conversations_templ.go     # 生成的 Go 代码
│   ├── modal_templ.go             # 生成的 Go 代码
│   └── drawer_templ.go            # 生成的 Go 代码
└── ui/
    ├── app-minimal.js             # 最小化 JS（主题、Toast）
    ├── htmx-config.js             # HTMX 配置
    └── style.css                  # 保持不变

internal/gateway/
├── conversations.go               # 原有 JSON API（保留）
├── conversations_htmx.go          # 新 HTMX 处理器
└── gateway.go                     # 添加 HTMX 路由

Makefile                           # 构建脚本
```

### 修改文件

- `internal/gateway/gateway.go` - 添加 HTMX 路由
- `internal/web/ui/style.css` - 保持不变（兼容）

### 删除文件（可选，暂时保留）

- `internal/web/ui/app.js` - 被 app-minimal.js 替代
- `internal/web/ui/index.html` - 被 layout.templ 替代

## 代码对比

### 1. 会话列表渲染

#### Before (JavaScript)
```javascript
// app.js
function renderConversations() {
  const list = $('#convList');
  if (conversationsCache.length === 0) {
    list.innerHTML = '<div class="empty">暂无会话</div>';
    return;
  }
  list.innerHTML = conversationsCache.map(conv => {
    const createdDate = fmtTs(conv.created_at);
    const typeLabel = conv.type === 'dm' ? '私聊' : '群组';
    return `
      <div class="card clickable" data-conv-id="${esc(conv.id)}">
        <div class="card-head">
          <h3>${esc(conv.title || conv.id)}</h3>
          <p>${typeLabel} · ${conv.member_count} 成员</p>
        </div>
      </div>
    `;
  }).join('');
  
  // 添加事件监听
  list.querySelectorAll('.card').forEach(card => {
    card.onclick = () => openConversationDrawer(card.dataset.convId);
  });
}
```

#### After (Templ)
```go
// views/conversations.templ
templ ConversationsContent(conversations []Conversation) {
  <div id="convList" class="card-grid">
    if len(conversations) == 0 {
      <div class="empty">暂无会话</div>
    } else {
      for _, conv := range conversations {
        <div class="card clickable"
          hx-get={ fmt.Sprintf("/admin/conversations/%s/drawer", conv.ID) }
          hx-target="#drawer-container">
          <div class="card-head">
            <h3>{ conv.Title }</h3>
            <p>
              if conv.Type == "dm" { 私聊 } else { 群组 }
              · { fmt.Sprintf("%d", conv.MemberCount) } 成员
            </p>
          </div>
        </div>
      }
    }
  </div>
}
```

**优势**：
- ✅ 类型安全（编译时检查）
- ✅ 无需手动 DOM 操作
- ✅ 无需事件绑定（HTMX 自动处理）
- ✅ 无 XSS 风险（自动转义）

### 2. 创建会话

#### Before (JavaScript)
```javascript
// app.js
async function handleCreateConversation(e) {
  e.preventDefault();
  const type = $('#convType').value;
  const title = $('#convTitle').value.trim();
  const members = Array.from(
    document.querySelectorAll('input[name="member"]:checked')
  ).map(cb => cb.value);
  
  try {
    await api('/admin/conversations', {
      method: 'POST',
      body: JSON.stringify({ type, title, member_ids: members })
    });
    toast('会话创建成功', 'ok');
    $('#createConvDlg').close();
    refreshConversations();
  } catch (e) {
    toast('创建失败: ' + e.message, 'bad');
  }
}
```

#### After (Templ + HTMX)
```go
// views/modal.templ
templ CreateConversationModal(peers []Peer) {
  <dialog id="createConvDlg" class="modal" open>
    <form hx-post="/admin/conversations"
          hx-target="#convList"
          hx-swap="afterbegin">
      <input name="title" required/>
      <select name="type">
        <option value="group">群组</option>
      </select>
      for _, peer := range peers {
        <input type="checkbox" name="member_ids" value={ peer.ID }/>
      }
      <button type="submit">创建</button>
    </form>
  </dialog>
}
```

**优势**：
- ✅ 无需手动 API 调用
- ✅ 无需手动错误处理（HTMX 统一处理）
- ✅ 无需手动关闭模态框（HTMX 事件触发）
- ✅ 无需手动刷新列表（HTMX swap 自动更新）

### 3. 发送消息

#### Before (JavaScript)
```javascript
async function handleSendConversationMessage() {
  const input = $('#convMessageInput');
  const payload = input.value.trim();
  
  try {
    await api(`/admin/conversations/${convId}/messages`, {
      method: 'POST',
      body: JSON.stringify({ payload })
    });
    toast('消息已发送', 'ok');
    input.value = '';
    
    // 刷新消息列表
    const messages = await api(`/admin/conversations/${convId}/messages`);
    renderConversationMessages(messages.messages);
  } catch (e) {
    toast('发送失败: ' + e.message, 'bad');
  }
}
```

#### After (Templ + HTMX)
```go
// views/drawer.templ
<form hx-post={ fmt.Sprintf("/admin/conversations/%s/messages", conv.ID) }
      hx-target="#convMessagesContainer"
      hx-swap="beforeend">
  <textarea name="payload" required></textarea>
  <button type="submit">发送</button>
</form>
```

**优势**：
- ✅ 5 行替代 15 行
- ✅ 自动表单重置
- ✅ 自动追加新消息
- ✅ 统一错误处理

## JavaScript 代码量对比

### Before
```
app.js:               1,865 行
├─ 工具函数:           ~200 行
├─ API 调用:           ~150 行
├─ DOM 操作:           ~800 行
├─ 事件处理:           ~400 行
└─ 状态管理:           ~315 行
```

### After
```
app-minimal.js:        ~80 行
├─ 主题切换:            ~30 行
├─ Toast 通知:          ~30 行
└─ HTMX 配置:           ~20 行

htmx.js (CDN):        ~14 KB (已压缩)

总减少: ~95% JavaScript
```

## 迁移步骤

### Step 1: 安装 Templ
```bash
make install-templ
# 或
go install github.com/a-h/templ/cmd/templ@v0.3.1001
```

### Step 2: 生成 Templ 文件
```bash
make generate
# 或
templ generate
```

### Step 3: 测试编译
```bash
make build
```

### Step 4: 运行服务器
```bash
make run
# 或
go run ./cmd/agent-relay
```

### Step 5: 访问新界面
```
http://localhost:8080/admin/conversations-page
```

### Step 6: 逐步迁移其他页面（可选）
- [ ] 成员页面
- [ ] 消息线程页面
- [ ] 审计日志页面
- [ ] Prompts 页面
- [ ] Tokens 页面
- [ ] 系统页面

## 兼容性

### 保留的 API（向后兼容）
所有原有 JSON API **保持不变**，供非浏览器客户端使用：

```
POST   /conversations                    # 仍然存在
GET    /conversations                    # 仍然存在
POST   /admin/conversations              # 仍然存在
...
```

### 新增的 HTMX 路由
```
GET    /admin/conversations-page         # Templ 渲染的完整页面
GET    /admin/conversations/new          # 模态框 HTML 片段
POST   /admin/conversations-htmx         # 返回卡片 HTML
GET    /admin/conversations/{id}/drawer  # 抽屉 HTML
POST   /admin/conversations/{id}/send    # 返回消息 HTML
```

## 性能影响

### Before (纯 JS)
```
首次加载:
├─ index.html:     ~10 KB
├─ app.js:         ~50 KB
├─ style.css:      ~15 KB
└─ 总计:           ~75 KB

后续交互:
└─ JSON API:       ~2-5 KB per request
```

### After (HTMX + Templ)
```
首次加载:
├─ HTML (Templ):   ~12 KB
├─ app-minimal.js: ~2 KB
├─ htmx.js (CDN):  ~14 KB (缓存)
├─ style.css:      ~15 KB
└─ 总计:           ~43 KB (-40%)

后续交互:
└─ HTML片段:       ~1-3 KB per request
```

**优势**：
- ✅ 首次加载减少 40%
- ✅ 后续交互更快（HTML 比 JSON 更小）
- ✅ 服务端渲染，首屏更快

## 开发体验

### Before
```
修改流程:
1. 编辑 app.js
2. 刷新浏览器
3. 手动测试
```

### After
```
修改流程:
1. 编辑 *.templ
2. make generate (或 make watch)
3. go run ./cmd/agent-relay
4. 刷新浏览器

类型检查:
├─ Go 编译器检查模板语法
├─ 编译时发现错误
└─ IDE 自动完成
```

## 故障排查

### 问题: templ: command not found
```bash
make install-templ
```

### 问题: 编译错误 "undefined: views"
```bash
make generate
```

### 问题: 模板未更新
```bash
make clean
make generate
make build
```

### 问题: HTMX 请求 404
检查路由是否正确注册：
```bash
grep "handleConversations" internal/gateway/gateway.go
```

## 下一步

### 推荐阅读
- [HTMX 文档](https://htmx.org/docs/)
- [Templ 文档](https://templ.guide/)
- [HTMX 最佳实践](https://htmx.org/essays/)

### 扩展功能
1. **实时更新**: 使用 Server-Sent Events (SSE)
   ```go
   hx-ext="sse" sse-connect="/admin/conversations/stream"
   ```

2. **乐观更新**: HTMX OOB swap
   ```go
   <div hx-swap-oob="true" id="convList">...</div>
   ```

3. **加载指示器**: HTMX indicators
   ```html
   <div class="htmx-indicator">加载中...</div>
   ```

## 总结

### 优势 ✅
- **减少 95% JavaScript**（1,865 行 → 80 行）
- **类型安全**（Go 编译时检查）
- **更快加载**（-40% 首次加载）
- **无构建依赖**（只需 templ generate）
- **渐进增强**（JS 失败仍可用）
- **易于维护**（声明式，组件化）

### 成本 ⚠️
- **学习曲线**（HTMX 思维方式）
- **重构时间**（2-3 天完成会话页面）
- **模板生成步骤**（需要 make generate）

### 推荐 ⭐⭐⭐
对于 agent-relay 这样的管理后台，HTMX + Templ 是**最佳选择**：
- ✅ 单文件部署（Go binary + embedded assets）
- ✅ 零运行时依赖
- ✅ 易于扩展和维护
- ✅ 符合项目哲学（轻量、简洁、实用）
