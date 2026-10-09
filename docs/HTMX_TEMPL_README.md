# HTMX + Templ 实现说明

## 快速开始

### 1. 首次设置

```bash
# 安装 templ 并生成文件
make setup
```

### 2. 开发

```bash
# 方式 A: 手动生成
make generate
make run

# 方式 B: 自动监听（推荐开发时使用）
# Terminal 1
make watch

# Terminal 2
go run ./cmd/agent-relay
```

### 3. 访问新界面

```
http://localhost:8080/admin/conversations-page
```

## 文件说明

### Templ 组件文件（需要生成）

```
internal/web/views/
├── layout.templ          # 基础布局（HTML框架、侧边栏、图标）
├── conversations.templ   # 会话列表页面
├── modal.templ           # 创建会话模态框
└── drawer.templ          # 会话详情抽屉
```

运行 `make generate` 后会生成对应的 `*_templ.go` 文件。

### JavaScript 文件（无需编译）

```
internal/web/ui/
├── app-minimal.js        # 最小化 JS（主题切换、Toast通知）
└── htmx-config.js        # HTMX 配置
```

### Go 处理器

```
internal/gateway/
├── conversations.go       # 原有 JSON API（保留，向后兼容）
├── conversations_htmx.go  # 新增 HTMX 处理器
└── gateway.go             # 路由注册
```

## 工作流程

### 修改 Templ 模板

1. 编辑 `internal/web/views/*.templ`
2. 运行 `make generate` 生成 Go 代码
3. 重新编译运行

### 修改 JavaScript

直接编辑 `internal/web/ui/*.js`，刷新浏览器即可。

### 修改 CSS

直接编辑 `internal/web/ui/style.css`，刷新浏览器即可。

## Make 命令

```bash
make help              # 显示所有可用命令
make setup             # 首次设置（安装 templ + 生成文件）
make generate          # 生成 templ 文件
make build             # 编译项目
make build-prod        # 编译生产版本
make test              # 运行测试
make clean             # 清理生成的文件
make watch             # 监听文件变化自动生成
make run               # 运行服务器
make fmt               # 格式化代码
```

## 架构说明

### 路由对比

#### 原有 JSON API（保留）
```
POST   /conversations
GET    /conversations
POST   /admin/conversations
...
```

#### 新增 HTMX 路由
```
GET    /admin/conversations-page          # 完整页面
GET    /admin/conversations/new           # 创建模态框
POST   /admin/conversations-htmx          # 返回新卡片 HTML
GET    /admin/conversations/{id}/drawer   # 会话抽屉
GET    /admin/conversations/{id}/messages-list  # 消息列表
POST   /admin/conversations/{id}/send     # 发送消息，返回消息 HTML
```

### 数据流

```
浏览器                              服务器
┌─────────────┐                    ┌──────────────────────┐
│ HTMX        │                    │ Gateway              │
│ hx-get      │───HTML Request────▶│ handleConversations  │
│             │                    │   ↓                  │
│             │                    │ views.Render()       │
│             │◀───HTML Fragment───│   ↓                  │
│ swap        │                    │ Templ Component      │
└─────────────┘                    └──────────────────────┘
```

### Templ 编译流程

```
*.templ 文件
    ↓
templ generate
    ↓
*_templ.go 文件（Go 代码）
    ↓
go build
    ↓
可执行文件
```

## 示例

### 创建新组件

1. **创建 Templ 文件**

```go
// internal/web/views/example.templ
package views

templ ExampleComponent(data string) {
  <div class="example">
    <h2>{ data }</h2>
  </div>
}
```

2. **生成 Go 代码**

```bash
make generate
```

3. **在处理器中使用**

```go
// internal/gateway/example.go
func (s *Server) handleExample(w http.ResponseWriter, r *http.Request) {
    views.ExampleComponent("Hello World").Render(r.Context(), w)
}
```

4. **注册路由**

```go
// internal/gateway/gateway.go
mux.HandleFunc("GET /admin/example", s.requireAdmin(s.handleExample))
```

### HTMX 按钮示例

```html
<!-- 点击后加载内容到目标元素 -->
<button hx-get="/admin/data"
        hx-target="#container"
        hx-swap="innerHTML">
  加载数据
</button>

<div id="container">
  <!-- 内容将被替换 -->
</div>
```

### HTMX 表单示例

```html
<!-- 提交后追加响应到列表末尾 -->
<form hx-post="/admin/items"
      hx-target="#item-list"
      hx-swap="beforeend">
  <input name="title" required/>
  <button type="submit">添加</button>
</form>

<div id="item-list">
  <!-- 新项目将被追加到这里 -->
</div>
```

## 常见问题

### Q: 为什么需要 `make generate`？

A: Templ 将模板编译成 Go 代码，这样：
- ✅ 编译时类型检查
- ✅ 无运行时模板解析开销
- ✅ 单一可执行文件（模板嵌入）

### Q: 是否可以跳过 templ，直接写 Go？

A: 可以，但不推荐。Templ 提供：
- 更清晰的 HTML 语法
- 自动 HTML 转义
- 组件化支持
- IDE 支持

### Q: 生成的文件需要提交到 Git 吗？

A: **是的**。`*_templ.go` 文件应该提交，这样：
- CI/CD 无需安装 templ
- 代码审查可见完整逻辑
- `go build` 可直接工作

### Q: HTMX 会增加页面大小吗？

A: 略微增加，但值得：
- HTMX: ~14 KB (gzipped)
- 减少的 JS: ~50 KB
- **净减少: ~36 KB**

### Q: 如何调试 HTMX 请求？

A: 浏览器开发工具：
1. Network 标签查看请求/响应
2. Console 查看 `htmx:*` 事件
3. 或添加 `htmx.logger` 启用调试日志

```javascript
htmx.logger = function(elt, event, data) {
    if(console) {
        console.log(event, elt, data);
    }
}
```

## 性能优化

### 1. 减少 HTML 片段大小

```go
// Bad: 返回完整页面
templ FullPage() { ... }

// Good: 仅返回需要更新的片段
templ CardComponent() { ... }
```

### 2. 使用 OOB swap 更新多个元素

```go
// 返回主内容 + 额外更新
templ ResponseWithOOB() {
  <div id="main-content">...</div>
  <div id="sidebar" hx-swap-oob="true">...</div>
}
```

### 3. 缓存模板渲染结果（可选）

```go
var cachedHTML []byte

func (s *Server) handleCached(w http.ResponseWriter, r *http.Request) {
    if cachedHTML == nil {
        var buf bytes.Buffer
        views.Component().Render(r.Context(), &buf)
        cachedHTML = buf.Bytes()
    }
    w.Write(cachedHTML)
}
```

## 参考资料

- [Templ 官方文档](https://templ.guide/)
- [HTMX 官方文档](https://htmx.org/)
- [HTMX 示例](https://htmx.org/examples/)
- [迁移指南](./HTMX_MIGRATION.md)

## 获取帮助

如有问题，请查看：
1. `make help` - 可用命令
2. `docs/HTMX_MIGRATION.md` - 详细迁移指南
3. 生成的 `*_templ.go` 文件 - 查看编译后的代码
