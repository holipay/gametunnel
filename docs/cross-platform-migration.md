# GameTunnel 客户端跨平台迁移记录

> 2026-05-10 实施，基于 commit `8dc5360`。
> 分 5 次提交完成：`ef9397b` → `3902e99` → `66d1320` → `8d3af84` → `2ca0caa`。

## 一、背景

GameTunnel GUI 客户端原本仅支持 Windows，代码中存在大量平台耦合：

| 问题 | 具体表现 |
|------|---------|
| 设置窗体空窗口 | `dialog_windows.go` 用 `DLGTEMPLATE` 字节拼接控件，某些 Windows 版本/配置下控件不显示 |
| systray API 不一致 | `systray.SetIcon()` 仅 Windows 定义，macOS 只有 `SetTemplateIcon()` |
| 变量重复声明 | `user32` 在 `dialog_windows.go` 和 `main_windows.go` 中重复声明 |
| `tray.go` 无 build tag | 直接调用 `showConfigDialog()`（仅 Windows），darwin/Linux 编译报 undefined |
| TUN 仅 Windows | `internal/tun/` 全部 `//go:build windows`，用 `netsh`/`wintun` |
| `main_other.go` 空壳 | 只打印"仅支持 Windows"就退出 |

**目标**：让客户端能在 Windows / Linux / macOS 三平台编译运行。

---

## 二、修复明细

### 2.1 设置窗体重写（commit `ef9397b`）

**文件**：`cmd/client/dialog_windows.go`

**问题**：原代码手动拼接 `DLGTEMPLATE` + `DLGITEMTEMPLATE` 字节定义 11 个控件。每个控件需要精确的 DWORD 对齐、UTF-16LE 编码、正确的 class atom。不同 Windows 版本对模板的解析行为不一致，导致弹出空窗口。

**根因分析**：

原代码使用 `addItem()` 逐个写入控件定义：

```go
func addItem(buf *leBuffer, x, y, cx, cy int16, id uint16, classAtom uint16, style uint32, text string) {
    buf.align4()
    buf.writeUint32(0)             // dwExtendedStyle
    buf.writeUint32(style)         // style
    buf.writeInt16(x)              // 位置...
    // ... 共 520 字节，11 个控件
}
```

虽然字节结构验证正确（520 字节、11 控件、所有偏移 DWORD 对齐），但 `DialogBoxIndirectParamW` 对模板的解析存在平台差异。

**方案**：最小 `DLGTEMPLATE`（`cdit=0`）+ `WM_INITDIALOG` 中用 `CreateWindowEx` 逐个创建控件。

```go
// buildDialogTemplate 只创建空对话框，不含控件定义
func buildDialogTemplate(dluW, dluH int, title string) []byte {
    var buf leBuffer
    buf.writeUint32(DS_SHELLFONT | DS_MODALFRAME | WS_POPUP | WS_CAPTION | WS_SYSMENU | DS_CENTER)
    buf.writeUint32(0)     // dwExtendedStyle
    buf.writeUint16(0)     // cdit = 0（无控件）
    // ... 仅 ~96 字节
}

// WM_INITDIALOG 中用 CreateWindowEx 创建控件
func makeCtl(className, text string, style uint32, x, y, w, h int, parent uintptr, id uintptr) {
    procCreateWindowEx.Call(...)
}
```

**收益**：
- 模板从 520 字节减至 ~96 字节
- 彻底避免 `DLGITEMTEMPLATE` 对齐/编码兼容性问题
- DLU→像素通过 `GetTextMetrics` 精确计算
- `CreateFont("Segoe UI", 9pt)` 替代未初始化的 `hFont`

**删除的函数**：`addStatic`、`addEdit`、`addButton`、`addItem`、`align4`

---

### 2.2 macOS systray 编译修复（commit `80162a6`）

**文件**：`cmd/client/tray.go`、`cmd/client/tray_darwin.go`、`cmd/client/tray_nondarwin.go`

**问题**：`systray.SetIcon()` 仅在 `systray_windows.go` 定义。macOS 的 `systray_darwin.go` 只提供 `SetTemplateIcon()`。

**systray v1.2.2 平台 API 差异**：

| 函数 | Windows | macOS | Linux |
|------|---------|-------|-------|
| `SetIcon([]byte)` | ✅ `systray_windows.go` | ❌ | ✅ `systray_nonwindows.go` |
| `SetTemplateIcon([]byte, []byte)` | ✅ | ✅ | ✅ |
| `SetTitle(string)` | ✅ | ✅ `systray_nonwindows.go` | ✅ |
| `SetTooltip(string)` | ✅ | ✅ | ✅ |

**方案**：提取 `setTrayIcon()` 包装函数，平台特定文件实现：

```go
// tray_darwin.go
//go:build darwin
func setTrayIcon(iconBytes []byte) {
    systray.SetTemplateIcon(iconBytes, iconBytes) // 支持深色/浅色模式
}

// tray_nondarwin.go
//go:build !darwin
func setTrayIcon(iconBytes []byte) {
    systray.SetIcon(iconBytes)
}
```

---

### 2.3 变量声明冲突修复（commit `66d1320`）

**文件**：`cmd/client/main_windows.go`、`cmd/client/dialog_windows.go`

**问题**：`user32 = syscall.NewLazyDLL("user32.dll")` 在两个文件中都声明了。Go 同一 package 内不允许重复声明包级变量，导致 `procShowWindow redeclared`。

**方案**：将 `user32` 统一移至 `main_windows.go`，`dialog_windows.go` 不再声明，直接引用。

---

### 2.4 tray.go 跨平台编译修复（commit `8d3af84`）

**文件**：`cmd/client/tray.go`、`cmd/client/dialog_windows.go`、`cmd/client/platform_other.go`

**问题**：`tray.go` 无 build tag，但直接调用 `showConfigDialog()`（仅在 `dialog_windows.go` 中定义），darwin/Linux 编译报 `undefined: showConfigDialog`。

**方案**：提取 `showSettingsDialog()` 平台 wrapper：

```go
// tray.go — 通用逻辑，调用 wrapper
if showSettingsDialog(statusText) { ... }

// dialog_windows.go — Windows 实现
func showSettingsDialog(statusText string) bool {
    return showConfigDialog(statusText)
}

// platform_other.go — 非 Windows 实现
func showSettingsDialog(statusText string) bool {
    openConfigFile()  // 打开配置文件作为 fallback
    return false
}
```

---

### 2.5 跨平台 TUN + main_other.go（commit `2ca0caa`）

**文件**：

| 文件 | 作用 |
|------|------|
| `internal/tun/tun_common.go` | **新增** — `Config` 和 `DefaultMTU` 跨平台共享类型 |
| `internal/tun/tun_linux.go` | **新增** — Linux TUN（`/dev/net/tun` + `ioctl`） |
| `internal/tun/tun_darwin.go` | **新增** — macOS TUN（`utun` socket） |
| `internal/tun/firewall_other.go` | **新增** — 非 Windows 防火墙 no-op |
| `internal/tun/tun.go` | 移除重复的 `Config`/`DefaultMTU`（已提取到 `tun_common.go`） |
| `cmd/client/main_other.go` | 从"打印错误退出"改为真正的 `main()` |
| `cmd/client/platform_other.go` | `openConfigFile` macOS 用 `open` 命令 |

**TUN 平台实现对比**：

| 平台 | 设备 | IP 配置 | 路由 |
|------|------|---------|------|
| Windows | `wintun` 驱动 + `wireguard/tun` | `netsh interface ip set address` | `route add` + metric 管理 |
| Linux | `/dev/net/tun` + `TUNSETIFF` ioctl | `ip addr add` | `ip route add` |
| macOS | `utun` socket（`AF_SYSTEM` + `SYSPROTO_CONTROL`） | `ifconfig` | `route -n add -interface` |

**macOS utun 特殊处理**：utun 设备在 `Read`/`Write` 时需要跳过/添加 4 字节协议族头（`AF_INET=2`）：

```go
func (d *Device) Read(buf []byte) (int, error) {
    n, err := unix.Read(d.fd, buf)
    // 跳过 4 字节协议族头
    if n > 4 {
        copy(buf, buf[4:n])
        return n - 4, nil
    }
    return 0, nil
}
```

---

## 三、平台编译指南

```bash
# Windows（交叉编译）
GOOS=windows GOARCH=amd64 go build -ldflags "-H=windowsgui" ./cmd/client

# Linux（需 gcc + gtk3 + libayatana-appindicator3）
sudo apt-get install gcc libgtk-3-dev libayatana-appindicator3-dev
go build ./cmd/client

# macOS（需 Xcode CLI tools）
go build ./cmd/client
```

**运行要求**：
- Linux/macOS 需 root 权限（创建 TUN 设备）
- Windows 需管理员权限（创建 wintun 虚拟网卡）

---

## 四、遗留事项

| 项目 | 状态 | 说明 |
|------|------|------|
| macOS 设置对话框 | ⚠️ fallback | 当前打开配置文件，无原生 GUI 对话框 |
| Linux 防火墙 | ⚠️ no-op | 未实现 `iptables`/`nftables` 规则自动配置 |
| macOS 防火墙 | ⚠️ no-op | 未实现 `pf` 规则自动配置 |
| `icons.go` ICO 格式 | ✅ 兼容 | systray 三平台均支持 ICO，暂无需改 PNG |
| `systray` cgo 依赖 | ⚠️ 注意 | Linux 需 `gcc` + `gtk3` + `libayatana-appindicator3` 开发头文件 |
