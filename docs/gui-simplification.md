# GUI 客户端精简：从 Web 面板到原生对话框

> 2026-05-09 设计与实现，基于 commit `015ba4d`。
> 实现在 `f9ab958` 中提交。

## 一、问题

原 GUI 方案（`gui-client-design.md`）实现了 3 个 UI 面：

| 组件 | 作用 | 问题 |
|------|------|------|
| Console 窗口 | 显示日志 | 对普通玩家无意义，影响专业感 |
| Web 控制面板 (127.0.0.1:4702) | 配置 + 状态 | 为了改个配置打开浏览器，太重 |
| 系统托盘 | 快捷操作 | 和 Web 面板功能重叠 |

**用户感知**："双击 exe 后弹出一个黑窗口 + 一个浏览器 + 右下角一个图标，这啥？"

**根本矛盾**：Web 控制面板的 F12 调试能力是给开发者的，不是给玩家的。玩家只需要：双击 → 填服务器 → 玩。

## 二、方案选型

| 方案 | 优点 | 缺点 | 结论 |
|------|------|------|------|
| 保留 Web 面板 + 简化 | 改动小 | 仍有 3 个 UI 面 | ❌ 治标不治本 |
| Wails / Fyne | 功能强大 | 引入框架依赖、打包增大 | ❌ 过重 |
| **Win32 原生对话框** | 零额外依赖、原生外观、中文完美 | 需手写 syscall | ✅ 采用 |
| CLI 配置 | 最简单 | 用户体验差 | ❌ |

**核心决策**：用 `user32.dll` 的 `DialogBoxIndirectParam` 创建原生 Windows 对话框，替代 Web 控制面板。不引入任何 GUI 框架。

## 三、新架构

```
双击 gtunnel-client.exe
  │
  ├─ 隐藏 Console 窗口 (ShowWindow SW_HIDE)
  ├─ 请求管理员权限 (ShellExecute runas)
  │
  ├─ 有 config.ini 且 server 非空？
  │   ├─ 是 → 自动连接，托盘变绿
  │   └─ 否 → 弹出「设置」原生对话框
  │
  └─ 系统托盘（唯一常驻 UI）
      ├─ 🔴 未连接 / 🟡 连接中 / 🟢 已连接
      ├─ ⚡ 连接 / 🔌 断开
      ├─ ⚙ 设置...    → Win32 原生对话框
      ├─ 📄 查看日志   → notepad
      └─ ❌ 退出
```

### 3.1 UI 面对比

| | 旧方案 | 新方案 |
|---|---|---|
| 常驻 UI | Console + 托盘 | 仅托盘 |
| 配置界面 | 浏览器打开 Web 页 | Win32 原生对话框 |
| HTTP 服务器 | 需要 (127.0.0.1:4702) | 不需要 |
| 嵌入式资源 | static/index.html (~12KB) | 无 |
| 依赖 | systray + embed + net/http | 仅 systray |

### 3.2 对话框布局

```
┌─ GameTunnel 设置 ─────────────────────────┐
│                                            │
│  服务器地址: [________________________]    │
│  玩家名称:   [________________________]    │
│  房间 ID:    [________________________]    │
│  密码:       [________________________]    │
│  已连接 · 10.10.0.2 · 3人在线             │
│                                            │
│         [ 确定 ]      [ 取消 ]             │
└────────────────────────────────────────────┘
```

## 四、技术实现

### 4.1 Win32 对话框模板

对话框通过内存中的二进制模板创建，结构为：

```
DLGTEMPLATE (18 bytes 固定头)
  ├─ style: DS_MODALFRAME | WS_POPUP | WS_CAPTION | WS_SYSMENU | DS_CENTER
  ├─ cdit: 11 (4 静态标签 + 4 编辑框 + 1 状态标签 + 2 按钮)
  ├─ x, y, cx, cy: 对话框位置和大小 (DLU)
  ├─ menu: 无 (0x0000)
  ├─ class: 无 (0x0000)
  ├─ title: "GameTunnel 设置" (UTF-16)
  ├─ font: 9pt "Microsoft YaHei UI" (UTF-16)
  │
  ├─ DLGITEMTEMPLATE × 11 (每个控件)
  │   ├─ style (DWORD)
  │   ├─ x, y, cx, cy (SHORT)
  │   ├─ id (SHORT)
  │   ├─ class: 预定义 atom (0x80=Button, 0x81=Edit, 0x82=Static)
  │   ├─ text (UTF-16)
  │   └─ cbData: 0 (无额外数据)
  │
  └─ DWORD 对齐（每个控件项前填充至 4 字节边界）
```

### 4.2 关键 API 调用

```go
// 创建模态对话框（阻塞直到用户关闭）
DialogBoxIndirectParamW(hInstance, lpTemplate, hWndParent, lpDialogFunc, dwInitParam)

// 获取编辑框文本
GetDlgItemTextW(hDlg, nIDDlgItem, lpString, nMaxCount)

// 设置编辑框文本
SetDlgItemTextW(hDlg, nIDDlgItem, lpString)

// 关闭对话框
EndDialog(hDlg, nResult)
```

所有 API 通过 `syscall.NewLazyDLL("user32.dll")` 加载，无需 CGO。

### 4.3 回调函数

对话框过程通过 `syscall.NewCallback` 包装为 C 回调：

```go
func configDialogProc(cfg *client.Config) func(uintptr, uint32, uintptr, uintptr) uintptr {
    return func(hwnd uintptr, msg uint32, wParam, lParam uintptr) uintptr {
        switch msg {
        case WM_INITDIALOG:  // 初始化：填充当前配置
        case WM_COMMAND:     // 按钮点击：保存/取消
        case WM_CLOSE:       // 关闭窗口
        }
        return 0
    }
}
```

### 4.4 字体处理

对话框使用 "Microsoft YaHei UI"（微软雅黑 UI），Windows 10+ 内置，中文显示完美。

字体在 `WM_INITDIALOG` 中通过 `WM_SETFONT` 消息设置到每个控件：

```go
procSendMessage.Call(hctl, WM_SETFONT, hFont, 1)
```

## 五、改动文件清单

| 文件 | 变化 | 行数 |
|------|------|------|
| `cmd/client/dialog_windows.go` | 新增：Win32 原生对话框 | +236 |
| `cmd/client/httpserver.go` | 删除：HTTP 服务器 + SSE + API | -142 |
| `cmd/client/static/index.html` | 删除：Web 控制面板 | -498 |
| `cmd/client/run.go` | 修改：去掉 HTTP + 浏览器 | -30 |
| `cmd/client/tray.go` | 修改：去掉"打开面板"，加"设置..." | -50 |
| `cmd/client/main_windows.go` | 修改：隐藏 Console 窗口 | +6 |

**净变化**：-730 行 / +292 行

## 六、用户体验对比

| 场景 | 旧方案 | 新方案 |
|------|--------|--------|
| 首次启动 | 黑窗口 + 浏览器弹出 + 托盘 | 托盘 + 设置对话框 |
| 修改配置 | 托盘 → 打开面板 → 浏览器 | 托盘 → 设置 → 原生对话框 |
| 连接状态 | 浏览器页面实时刷新 | 托盘图标颜色 + Tooltip |
| 退出 | 关浏览器 + 关黑窗口 + 托盘退出 | 托盘 → 退出 |

**一句话总结**：从"打开浏览器改配置"变成"右键点托盘弹个框"。
