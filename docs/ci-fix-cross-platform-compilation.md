# CI 修复与跨平台编译兼容性记录

> 2026-05-10 会话修复，基于 commit `92f6fb8`（v0.5 分支）。
> 修复分 3 次提交：`dcb675b`、`7bce3a3`、`d50b745`。
> Release v0.6 已发布。

## 一、背景

CI 的 Release 工作流在 v0.5 分支上失败，需要定位并修复编译错误，最终发布 v0.6。

## 二、问题排查流程

### Step 1：获取失败信息

通过 GitHub Actions API 查询失败的 run，定位到 `Release #8`：

```
Workflow: release.yml
Branch: v0.5
Commit: 92f6fb8 "ci: 更新CI和Release工作流支持跨平台构建"
Status: completed / failure
```

6 个 job 中 `test` 失败，其余 5 个因 `needs: test` 依赖被跳过。

### Step 2：定位编译错误

通过 GitHub Check Runs Annotations API 获取错误详情：

```
internal/tun/tun_linux.go:56:15: undefined: unix.ByteToString
```

`go.mod` 中 `golang.org/x/sys v0.43.0` 不包含 `unix.ByteToString` 函数（该函数在 Go 1.24+ 引入，但 `x/sys` 包未暴露此符号）。

---

## 三、修复的问题

### 问题 1：`unix.ByteToString` 未定义 — Linux（编译失败）

**文件**：`internal/tun/tun_linux.go:56`
**严重度**：🔴 编译失败

**原因**：`unix.ByteToString` 不在 `golang.org/x/sys v0.43.0` 中。该函数用于将 `[16]byte` 的 null-terminated 字节数组转为 string。

**修复**：

```go
// 替换前
name := unix.ByteToString(ifr.Name[:])

// 替换后
n := bytes.IndexByte(ifr.Name[:], 0)
if n < 0 {
    n = len(ifr.Name)
}
name := string(ifr.Name[:n])
```

同时在 import 中添加 `"bytes"`。

---

### 问题 2：`unix.ByteToString` 未定义 — Darwin（编译失败）

**文件**：`internal/tun/tun_darwin.go:70`
**严重度**：🔴 编译失败

**原因**：与 Linux 相同。macOS 的 utun 接口名称获取使用了相同的 `unix.ByteToString` 调用。

**修复**：与 Linux 相同，使用 `bytes.IndexByte` 截断 null 字节。

---

### 问题 3：`unix.SYSPROTO_CONTROL` 未定义 — Darwin（编译失败）

**文件**：`internal/tun/tun_darwin.go:34, 63`
**严重度**：🔴 编译失败

**原因**：`SYSPROTO_CONTROL` 是 macOS 特有的 socket 协议常量（值为 2），用于创建 utun 设备。`golang.org/x/sys/unix` 包未定义此常量。

**修复**：在文件中添加常量定义：

```go
const SYSPROTO_CONTROL = 2
```

用于两处：
- `unix.Socket(unix.AF_SYSTEM, unix.SOCK_DGRAM, SYSPROTO_CONTROL)` — 创建 utun socket
- `unix.Syscall6(unix.SYS_GETSOCKOPT, ..., SYSPROTO_CONTROL, 2, ...)` — 获取接口名称

---

### 问题 4：未使用的 import（编译失败）

**文件**：`cmd/client/main_other.go`
**严重度**：🔴 编译失败（Linux/macOS 构建）

**原因**：文件导入了 `fmt`、`log`、`os` 但未使用。Go 编译器不允许未使用的 import。

**修复**：删除三个未使用的 import，只保留 `"net"` 和项目内部包。

---

### 问题 5：测试断言与当前逻辑不匹配（测试失败）

**文件**：`internal/client/client_test.go`
**严重度**：🟡 测试失败

#### 5a: `TestRoutePacket_PeerP2P`

**现象**：`expected packet on peer conn for P2P, got none`

**根因**：`routePacket()` 的 P2P 路径要求 `peer.DirectReach.Load() == true`，但测试只设置了 `PublicAddr`，未设置 `DirectReach`。包走了 server relay 路径而非 P2P 直连。

**修复**：在 peer 初始化后设置 `DirectReach`：

```go
peer := &Peer{
    VirtualIP:  peerIP,
    PublicAddr: peerAddr,
    Username:   "peer1",
}
peer.DirectReach.Store(true)
tunnel.peers = map[[4]byte]*Peer{
    ip4Key(peerIP): peer,
}
```

#### 5b: `TestHandleDataFromServer`

**现象**：`expected 7 bytes written to TUN, got 0`

**根因**：`handleDataFromServer()` 会验证 `srcKey == t.serverIP4` 或 srcIP 是已知 peer。测试未设置 `tunnel.serverIP`，也未添加 peer，导致数据包被丢弃。

**修复**：在测试中设置 serverIP：

```go
serverIP := net.IPv4(10, 0, 0, 1).To4()
tunnel.serverIP = serverIP
tunnel.serverIP4 = ip4Key(serverIP)
```

---

### 问题 6：设置窗口取消时弹出 config.ini（UI 行为问题）

**文件**：`cmd/client/tray.go`
**严重度**：🟢 UX 问题

**现象**：用户打开"设置"对话框，直接关闭或点取消，会弹出 config.ini 文件。

**根因**：`tray.go` 中 `showSettingsDialog()` 返回 `false` 时，`else` 分支调用 `openConfigFile()` 作为 fallback。

**修复**：删除 `else` 分支，取消时直接关闭对话框：

```go
// 修复前
if showSettingsDialog(statusText) {
    cfg := client.LoadConfig()
    t.app.cfg = cfg
    log.Printf("[tray] 配置已更新")
} else {
    openConfigFile()  // ← 删除此分支
}

// 修复后
if showSettingsDialog(statusText) {
    cfg := client.LoadConfig()
    t.app.cfg = cfg
    log.Printf("[tray] 配置已更新")
}
```

---

## 四、提交历史

| Commit | 内容 | CI 状态 |
|--------|------|---------|
| `dcb675b` | 修复 `unix.ByteToString`（Linux）+ 删除未使用 import | ❌ Darwin 仍有 ByteToString + SYSPROTO_CONTROL |
| `7bce3a3` | 修复 Darwin 的 ByteToString + SYSPROTO_CONTROL | ❌ 测试断言不匹配 |
| `d50b745` | 修复测试断言（DirectReach + serverIP） | ✅ 全部通过 |

---

## 五、经验总结

### `golang.org/x/sys/unix` 兼容性

- `unix.ByteToString` 不是标准 API，在 `x/sys v0.43.0` 中不存在
- `unix.SYSPROTO_CONTROL` 是 macOS 特有常量，`x/sys` 未定义
- 推荐做法：使用 `bytes.IndexByte` 手动截断 null 字节，定义本地常量

### CI 跨平台构建策略

- 平台特定代码（`_linux.go`、`_darwin.go`、`_windows.go`）的编译错误只在对应平台的 job 中暴露
- Linux CI 中 `go test ./internal/...` 不会编译 `_darwin.go`，需要单独的 job 验证
- 建议 CI 中增加交叉编译检查步骤

### 测试与实现同步

- 当产品代码的条件判断逻辑变更时（如新增 `DirectReach` 检查），测试必须同步更新
- 测试中的 mock/stub 需要覆盖所有代码路径的前置条件
