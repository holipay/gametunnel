# GameTunnel GUI 客户端设计方案

> 2026-05-08 设计与实现，基于 commit `fbf8bf0`。
> 后续修复在 `f206aa1` 中提交。

## 一、需求分析

CLI 客户端存在以下用户体验问题：

| 问题 | 说明 |
|------|------|
| 配置门槛高 | 需要手动编辑 `config.ini` 或传命令行参数 |
| 状态不可见 | 连接状态、虚拟 IP、在线人数只能看日志 |
| 操作繁琐 | 需要打开终端运行 exe，断线后需手动重启 |
| 无一键加入 | 每次都要指定服务器地址、房间等参数 |

**目标**：系统托盘驻留 + Web 控制面板，实现配置、状态监控、一键加入。

---

## 二、方案选型

| 方案 | 优点 | 缺点 | 结论 |
|------|------|------|------|
| **systray + 内嵌 HTTP** | 零额外依赖、纯 Go、~5MB 打包、浏览器 F12 调试 | UI 受限于 Web 技术 | ✅ 采用 |
| Wails | 框架成熟、内置 tray | 需 WebView2 运行时、引入框架依赖 | ❌ |
| Fyne | 纯 Go 原生控件 | 打包 ~15MB、API 学习成本 | ❌ |
| Walk | Win32 原生 | API 设计较老、维护不活跃 | ❌ |

**核心决策**：HTTP 服务仅绑定 `127.0.0.1`，不暴露到网络，安全性有保障。

---

## 三、架构设计

### 3.1 整体架构

```
┌─────────────────────────────────────────────────────┐
│  Windows 桌面                                        │
│                                                      │
│  ┌──────────────┐    ┌──────────────────────────┐   │
│  │  系统托盘     │    │  浏览器 (Web 控制面板)     │   │
│  │  🟢 已连接    │    │  http://127.0.0.1:4702   │   │
│  │  3人在线      │    │                          │   │
│  │  12ms         │    │  ┌ 配置表单 ────────────┐│   │
│  │               │    │  │ 服务器: [        ]   ││   │
│  │  右键菜单:    │    │  │ 玩家名: [        ]   ││   │
│  │  ├ 打开面板   │    │  │ 房间:   [        ]   ││   │
│  │  ├ 一键加入   │    │  │ 密码:   [        ]   ││   │
│  │  ├ 断开连接   │    │  │ [保存] [一键加入]    ││   │
│  │  ├ 查看日志   │    │  └──────────────────────┘│   │
│  │  └ 退出       │    │                          │   │
│  └──────────────┘    │  ┌ 连接状态 ────────────┐│   │
│                       │  │ 🟢 已连接            ││   │
│                       │  │ IP: 10.10.0.2       ││   │
│                       │  │ 延迟: 12ms           ││   │
│                       │  │ 运行: 02:15:30       ││   │
│                       │  └──────────────────────┘│   │
│                       └──────────────────────────┘   │
│                                                      │
│  ┌──────────────────────────────────────────────┐   │
│  │  App (业务层)                                  │   │
│  │  ├ connectLoop()     自动重连 + 指数退避       │   │
│  │  ├ statusLoop()      轮询 tunnel.Status()     │   │
│  │  └ GetStatus()       SSE 实时推送             │   │
│  └──────────────────────────────────────────────┘   │
│                      │                               │
│  ┌──────────────────────────────────────────────┐   │
│  │  client.Tunnel (已有逻辑，零改动)              │   │
│  │  ├ register() / receiveFromServer()          │   │
│  │  ├ receiveFromTUN() / routePacket()          │   │
│  │  └ keepaliveLoop() / peerDiscoveryLoop()     │   │
│  └──────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────┘
```

### 3.2 数据流

```
用户点击"一键加入"
  │
  ▼
浏览器 → POST /api/connect
  │
  ▼
HTTPServer → app.Connect(cfg)
  │
  ▼
App.connectLoop() → tunnel.Connect(ctx, ...)
  │
  ▼
tunnel.Status() ← statusLoop() 每秒轮询
  │
  ▼
SSE /api/status → 浏览器实时更新 UI
  │
  ▼
systray.statusLoop() → 更新托盘图标 + Tooltip
```

### 3.3 状态同步机制

`App` 结构体维护两套状态：

| 字段 | 更新方式 | 说明 |
|------|---------|------|
| `connecting` | `Connect()`/`connectLoop()` 直接写 | 连接中标志 |
| `lastErr` | `connectLoop()` 写入 | 最后一次错误信息 |
| `connected` | `statusLoop()` 从 `tunnel.Status()` 同步 | 是否已连接 |
| `virtualIP` | `statusLoop()` 从 `tunnel.Status()` 同步 | 虚拟 IP |
| `peerCount` | `statusLoop()` 从 `tunnel.Status()` 同步 | 在线人数 |
| `uptime` | `statusLoop()` 首次连接时记录 | 连接时长 |

`statusLoop()` 在 `connectLoop()` 启动时一并启动，通过 `pollCtx` 跟随连接生命周期。

---

## 四、文件结构

```
cmd/client/
├── main_windows.go        # Windows 入口：UAC 提权 + TUN 工厂 + run()
├── main_other.go          # 非 Windows：提示信息 + 退出
├── run.go                 # 跨平台启动：日志 + 防火墙 + App + HTTP + Tray
├── app.go                 # 业务层：连接/断开/状态管理/重连循环
├── httpserver.go          # HTTP 服务：API 路由 + SSE + 静态文件
├── tray.go                # 系统托盘：图标 + 菜单 + 状态轮询
├── icons.go               # 动态生成 16x16 托盘图标（纯 Go，无外部资源）
├── platform_windows.go    # Windows 平台适配：防火墙规则 + 打开日志
├── platform_other.go      # 非 Windows 平台适配：no-op
└── static/
    └── index.html         # Web 控制面板（HTML + CSS + JS 单文件）
```

### 4.1 依赖关系

```
main_windows.go ──→ run.go ──→ app.go ──→ internal/client/tunnel.go
       │                │           │
       │                │           └──→ internal/client/config.go
       │                │
       │                ├──→ httpserver.go (embed static/)
       │                └──→ tray.go
       │
       └──→ internal/tun/ (TUN 创建，仅 Windows)
```

### 4.2 编译约束

| 文件 | Build Tag | 说明 |
|------|-----------|------|
| `main_windows.go` | `//go:build windows` | Windows 入口 |
| `main_other.go` | `//go:build !windows` | 非 Windows 提示 |
| `platform_windows.go` | `//go:build windows` | 防火墙、打开日志 |
| `platform_other.go` | `//go:build !windows` | no-op 实现 |
| `run.go` | 无 | 跨平台 |
| `app.go` | 无 | 跨平台 |
| `httpserver.go` | 无 | 跨平台 |
| `tray.go` | 无 | 跨平台（systray 支持多平台） |
| `icons.go` | 无 | 跨平台（纯 Go image 生成） |

**交叉编译**：`GOOS=windows go build -o gtunnel-client.exe ./cmd/client`

---

## 五、API 设计

### 5.1 REST 端点

| 方法 | 路径 | 说明 |
|------|------|------|
| GET | `/api/config` | 获取当前配置 |
| POST | `/api/config` | 保存配置（JSON body） |
| POST | `/api/connect` | 一键连接 |
| POST | `/api/disconnect` | 断开连接 |
| GET | `/api/status` | SSE 实时状态流 |

### 5.2 SSE 状态推送

```json
{
  "connected": true,
  "connecting": false,
  "last_error": "",
  "virtual_ip": "10.10.0.2",
  "subnet": "10.10.0.0/24",
  "server_ip": "10.10.0.1",
  "peer_count": 3,
  "uptime": "02:15:30",
  "player_name": "Player1",
  "room_id": "default",
  "server_addr": "1.2.3.4:4700"
}
```

推送频率：每秒一次，通过 `text/event-stream` 格式。

### 5.3 配置 POST 格式

```json
{
  "server_addr": "1.2.3.4:4700",
  "player_name": "Player1",
  "room_id": "default",
  "password": ""
}
```

---

## 六、系统托盘设计

### 6.1 图标状态

| 状态 | 图标颜色 | Tooltip | 触发条件 |
|------|---------|---------|---------|
| 未连接 | 灰色 | `GameTunnel - 未连接` | 初始状态 / 断开后 |
| 连接中 | 黄色 | `GameTunnel - 连接中...` | 点击连接后 |
| 已连接 | 绿色 | `GameTunnel - 10.10.0.2 · 3人在线` | 注册成功后 |

图标为 16×16 PNG，由 `icons.go` 程序化生成（圆角矩形 + 箭头），无需外部 `.ico` 文件。

### 6.2 菜单项

| 菜单项 | 行为 |
|--------|------|
| 📊 打开面板 | `openBrowser("http://127.0.0.1:4702")` |
| ⚡ 一键加入 | `app.Connect(cfg)`（若无配置则先打开面板） |
| 🔌 断开连接 | `app.Disconnect()` |
| 📄 查看日志 | `notepad %APPDATA%\GameTunnel\gametunnel.log` |
| ❌ 退出 | `Disconnect()` + `CloseTUN()` + `systray.Quit()` |

---

## 七、Web 控制面板

### 7.1 UI 布局

```
┌─ 状态栏 ───────────────────────────────┐
│  🟢 已连接 · 10.10.0.2                  │
└────────────────────────────────────────┘

┌─ 服务器配置 ───────────────────────────┐
│  服务器: [________________]             │
│  玩家名: [________________]             │
│  房间:   [________________]             │
│  密码:   [________________]             │
│              [💾 保存] [⚡ 一键加入]    │
└────────────────────────────────────────┘

┌─ 连接信息 ─────────────────────────────┐
│  ┌──────────┐  ┌──────────┐            │
│  │ 10.10.0.2│  │  12ms    │            │
│  │ 虚拟 IP  │  │  延迟    │            │
│  └──────────┘  └──────────┘            │
│  ┌──────────┐  ┌──────────┐            │
│  │ 10.10.0.1│  │ 02:15:30 │            │
│  │ 服务器   │  │ 运行时间 │            │
│  └──────────┘  └──────────┘            │
└────────────────────────────────────────┘

┌─ 在线玩家 ─────────────────────────────┐
│  ┌──────────┐  ┌──────────┐            │
│  │    3     │  │ default  │            │
│  │ 在线人数 │  │  房间    │            │
│  └──────────┘  └──────────┘            │
└────────────────────────────────────────┘
```

### 7.2 技术细节

- 单文件 `index.html`（~12KB），内嵌 CSS + JS
- SSE (`EventSource`) 实时接收状态，每秒更新
- 回车键触发连接
- 暗色主题（`#0f0f1a` 背景），适合长时间使用
- 响应式布局，最大宽度 560px

---

## 八、安全考量

| 风险 | 措施 |
|------|------|
| HTTP 端口暴露到网络 | 仅绑定 `127.0.0.1:4702`，外部不可访问 |
| 配置明文存储 | `config.ini` 本地文件，与 CLI 版一致 |
| 密码传输 | 仅在 localhost 传输，不经网络 |
| XSS | 状态数据通过 `textContent` 渲染，非 `innerHTML` |
| CSRF | 仅本地访问，风险极低 |

---

## 九、新增依赖

| 依赖 | 版本 | 用途 | 体大小 |
|------|------|------|--------|
| `github.com/getlantern/systray` | v1.5.3 | 系统托盘（跨平台） | ~200KB |

无 CGO 依赖。总打包增量 ~1MB。

---

## 十、改动文件清单

| 文件 | 类型 | 说明 |
|------|------|------|
| `cmd/client/main.go` | 删除 | 原 CLI 入口，被 main_windows.go 替代 |
| `cmd/client/main_windows.go` | 新增 | Windows 入口 + UAC 提权 |
| `cmd/client/main_other.go` | 新增 | 非 Windows 提示 |
| `cmd/client/run.go` | 新增 | 跨平台启动逻辑 |
| `cmd/client/app.go` | 新增 | 业务层（连接/断开/状态） |
| `cmd/client/httpserver.go` | 新增 | HTTP API + SSE |
| `cmd/client/tray.go` | 新增 | 系统托盘 |
| `cmd/client/icons.go` | 新增 | 图标生成 |
| `cmd/client/platform_windows.go` | 新增 | Windows 平台适配 |
| `cmd/client/platform_other.go` | 新增 | 非 Windows 平台适配 |
| `cmd/client/static/index.html` | 新增 | Web 控制面板 |
| `internal/client/tunnel.go` | 修改 | 新增 `TunnelStatus` + `Status()` 方法 |
| `go.mod` | 修改 | 添加 systray 依赖 |
