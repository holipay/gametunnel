# 服务端 Windows 支持 + 客户端平台精简 + i18n 国际化

> 2026-05-11 实施，分 3 次提交完成：`219b51a` → `2ecc8b6` → `80961c6`。

## 一、概述

本次变更包含三项独立但相关的改动：

| 改动 | 范围 | 提交 |
|------|------|------|
| 服务端增加 Windows 编译目标 | Makefile, CI/Release, socket 层 | `219b51a` |
| 客户端移除 Linux / macOS 支持 | 源码删除, Makefile, CI/Release | `219b51a` |
| 新增英文 README | README_EN.md | `2ecc8b6` |
| 全局 i18n 中英文切换 | 新增 i18n 包, 20 个源文件改造 | `80961c6` |

## 二、服务端增加 Windows

### 2.1 背景

服务端原本仅支持 Linux 编译（`socket_linux.go` 用 `//go:build linux`）。`socket_other.go` 作为 fallback 是空实现（`//go:build !linux`），在 Windows 上可以编译但不调优 UDP 缓冲区。

### 2.2 改动

**新增 `internal/server/socket_windows.go`**：

```go
//go:build windows

package server

import (
    "net"
    "syscall"
)

func setSocketBuffers(conn *net.UDPConn) error {
    raw, err := conn.SyscallConn()
    // ...
    opErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, rcvBufSize)
    opErr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.SOL_SOCKET, syscall.SO_SNDBUF, sndBufSize)
}
```

与 Linux 版本的差异：`int(fd)` → `syscall.Handle(fd)`，这是 Windows 和 Linux 在 socket API 上的唯一区别。

**修改 `socket_other.go` 构建约束**：

```
- //go:build !linux
+ //go:build !linux && !windows
```

确保 Windows 走专用实现，其余平台（如 macOS）走 fallback。

**Makefile 新增目标**：

```makefile
server-windows-amd64:
    CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build ... -o $(BINARY_DIR)/gtunnel-server-windows-amd64.exe ./cmd/server

server-windows-arm64:
    CGO_ENABLED=0 GOOS=windows GOARCH=arm64 go build ... -o $(BINARY_DIR)/gtunnel-server-windows-arm64.exe ./cmd/server
```

服务端是纯 Go（CGO_ENABLED=0），可以任意交叉编译。

**CI/Release**：`release.yml` 新增 `release-server-windows` job，产出 `GameTunnel-Server-windows-{amd64,arm64}.zip`。

### 2.3 验证要点

- Windows 服务端与 Linux 服务端功能完全一致，协议层无平台差异
- `singleinstance` 包已有 `lock_windows.go`，无需额外适配
- 状态页 HTTP 服务无平台依赖

## 三、客户端移除 Linux / macOS

### 3.1 背景

客户端原本支持 Windows / Linux / macOS 三平台。Linux 和 macOS 需要 CGO（gtk3 + libayatana-appindicator3 或 Xcode CLI Tools），编译环境复杂，用户量极小。

### 3.2 删除的文件

| 文件 | 说明 |
|------|------|
| `cmd/client/main_other.go` | `//go:build !windows` 入口，Linux/macOS 使用 |
| `cmd/client/platform_other.go` | `//go:build !windows` 平台代码（日志路径等） |
| `cmd/client/tray_darwin.go` | `//go:build darwin` macOS `SetTemplateIcon` |
| `internal/tun/tun_linux.go` | `//go:build linux` `/dev/net/tun` TUN 实现 |
| `internal/tun/tun_darwin.go` | `//go:build darwin` utun socket TUN 实现 |

### 3.3 保留的文件

| 文件 | 说明 |
|------|------|
| `cmd/client/main_windows.go` | Windows 入口（UAC 提升、隐藏控制台） |
| `cmd/client/dialog_windows.go` | Win32 原生设置对话框 |
| `cmd/client/platform_windows.go` | Windows 平台代码 |
| `cmd/client/tray_nondarwin.go` | `//go:build !darwin` 托盘图标（Windows 使用） |
| `internal/tun/tun.go` | `//go:build windows` wintun TUN 实现 |
| `internal/tun/configure.go` | `//go:build windows` netsh 路由配置 |
| `internal/tun/firewall.go` | `//go:build windows` 防火墙规则 |
| `internal/tun/metric_windows.go` | `//go:build windows` 网卡 metric 管理 |
| `internal/singleinstance/lock_windows.go` | Windows 单实例锁 |

### 3.4 Makefile 变更

移除的目标：`client-linux`, `client-linux-amd64`, `client-linux-arm64`, `client-darwin`, `client-darwin-amd64`, `client-darwin-arm64`, `release-client-linux`, `release-client-darwin`。

`client-all` 简化为仅包含 Windows：`client client-windows-arm64`。

`release` 从 `release-client release-client-linux release-client-darwin release-server` 简化为 `release-client release-server`。

### 3.5 CI/Release 变更

- `ci.yml`：移除 `build-client-linux` 和 `build-client-darwin` job
- `release.yml`：移除 `release-client-linux` 和 `release-client-darwin` job，publish 不再依赖它们

## 四、英文 README

### 4.1 方案

保留 `README.md` 为中文主文档，新增 `README_EN.md` 为英文版。两个文件顶部互相链接：

```markdown
# README.md 顶部
> [English](README_EN.md)

# README_EN.md 顶部
> [中文版](README.md)
```

### 4.2 内容

英文 README 完整覆盖：Quick Start（Server + Client）、Security、How It Works、Parameters、Firewall、FAQ、Development。与中文版结构一致，翻译自然流畅，非机翻风格。

## 五、i18n 国际化

### 5.1 架构设计

```
internal/i18n/
  └── i18n.go    # 语言包 + 框架（单文件，约 300 行）
```

核心 API：

```go
// 设置语言
i18n.Set(i18n.ParseLang("en"))

// 获取当前语言字符串
t := i18n.T()
fmt.Println(t.KickRoomFull)  // "Room is full" 或 "房间已满"
```

**设计决策**：

| 方案 | 优点 | 缺点 | 结论 |
|------|------|------|------|
| JSON/YAML 外部文件 | 可热更新 | 需要文件 I/O，嵌入麻烦 | ❌ |
| Go struct + 编译时常量 | 类型安全，IDE 补全，零运行时开销 | 新增语言需改代码 | ✅ 采用 |
| `map[string]string` | 灵活 | 无类型安全，key 容易拼错 | ❌ |

### 5.2 语言包结构

```go
type Strings struct {
    // 服务端
    ServerBanner     string
    ServerAddr       string
    KickRoomFull     string
    // ...

    // 状态页 HTML
    StatusTitle      string
    StatusPlayers    string
    // ...

    // 客户端托盘
    TrayTitle        string
    TrayConnect      string
    // ...

    // 客户端对话框
    DlgTitle         string
    DlgServerAddr    string
    // ...

    // 日志消息
    LogP2PSuccess    string
    LogTunnelDisconnect string
    // ...
}
```

中文（`zhStrings`）和英文（`enStrings`）各一个实例，默认中文（向后兼容）。

### 5.3 改造范围

共改造 20 个源文件：

**服务端（6 个文件）**：
- `cmd/server/main.go` — `-lang` 参数，启动 banner
- `internal/server/server.go` — Config 增加 Lang 字段，日志消息
- `internal/server/status.go` — HTML 模板动态渲染，使用 `{{.T.StatusTitle}}` 等
- `internal/server/register.go` — kick 原因（12 个），日志消息
- `internal/server/peer.go` — 断开日志

**客户端（13 个文件）**：
- `internal/client/config.go` — 新增 `Lang` 字段，INI 解析 `lang=`
- `cmd/client/main_windows.go` — 从 config 读取 lang 并 `i18n.Set()`
- `cmd/client/tray.go` — 托盘菜单全部使用 i18n
- `cmd/client/dialog_windows.go` — 对话框标签、按钮使用 i18n
- `cmd/client/run.go` — 启动日志
- `cmd/client/app.go` — 连接日志
- `internal/client/tunnel.go` — TUN/连接日志
- `internal/client/keepalive.go` — P2P/清理日志
- `internal/client/register.go` — 注册/认证日志和错误消息
- `internal/client/recv.go` — 接收/对等体日志
- `internal/client/log.go` — 启动日志

**配置（1 个文件）**：
- `configs/config.ini` — 新增 `lang=zh` 示例

### 5.4 用法

**服务端**：

```bash
# 中文（默认）
gtunnel-server -addr :4700

# 英文
gtunnel-server -addr :4700 -lang en
```

**客户端**：

```ini
# config.ini
server=1.2.3.4:4700
name=PlayerName
room=default
lang=en
```

语言设置影响：系统托盘菜单、设置对话框、状态页 HTML、所有日志消息、kick 原因。

### 5.5 状态页模板缓存

状态页 HTML 使用 Go `html/template`，每次请求解析模板有性能开销。增加模板缓存：

```go
var statusTmplCache struct {
    lang i18n.Lang
    tmpl *template.Template
}

func getStatusTmpl(lang i18n.Lang) *template.Template {
    if statusTmplCache.tmpl != nil && statusTmplCache.lang == lang {
        return statusTmplCache.tmpl
    }
    tmpl := template.Must(template.New("status").Parse(statusHTML))
    statusTmplCache.lang = lang
    statusTmplCache.tmpl = tmpl
    return tmpl
}
```

语言切换后自动重建缓存，无需手动刷新。

### 5.6 托盘变量名冲突修复

`tray.go` 中 Tray 结构体原有字段 `t`，与 i18n 字符串变量 `t := i18n.T()` 冲突。统一将 Tray 的 receiver 改为 `tr`：

```go
func (tr *Tray) setup() {
    s := i18n.T()
    // ...
}
```

## 六、已知限制

1. **语言切换需重启**：客户端修改 `config.ini` 的 `lang=` 后需重启生效（托盘设置对话框保存后会立即刷新部分 UI，但完整切换需重启）。
2. **kick 原因语言**：kick 原因由服务端生成，跟随服务端 `-lang` 设置。如果服务端和客户端语言不同，客户端收到的 kick 消息会是服务端的语言。
3. **tun/ 目录残留**：`internal/tun/` 下的 `tun_linux.go` 和 `tun_darwin.go` 已删除，但 `configure.go`、`firewall.go` 等仍有大量中文注释。这些是代码注释，不影响运行时，保留原样。
4. **外部依赖未翻译**：`systray`、`wireguard-go` 等第三方库的内部消息不受 i18n 控制。

## 七、文件变更清单

```
 新增  internal/i18n/i18n.go
 新增  internal/server/socket_windows.go
 新增  README_EN.md
 修改  .github/workflows/ci.yml
 修改  .github/workflows/release.yml
 修改  Makefile
 修改  README.md
 修改  cmd/client/app.go
 修改  cmd/client/dialog_windows.go
 修改  cmd/client/main_windows.go
 修改  cmd/client/run.go
 修改  cmd/client/tray.go
 修改  cmd/server/main.go
 修改  configs/config.ini
 修改  internal/client/config.go
 修改  internal/client/keepalive.go
 修改  internal/client/log.go
 修改  internal/client/recv.go
 修改  internal/client/register.go
 修改  internal/client/tunnel.go
 修改  internal/server/peer.go
 修改  internal/server/register.go
 修改  internal/server/server.go
 修改  internal/server/socket_other.go
 修改  internal/server/status.go
 删除  cmd/client/main_other.go
 删除  cmd/client/platform_other.go
 删除  cmd/client/tray_darwin.go
 删除  internal/tun/tun_darwin.go
 删除  internal/tun/tun_linux.go
```
