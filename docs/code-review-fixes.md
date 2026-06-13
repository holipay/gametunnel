# GameTunnel 代码审查修复记录

> 2026-05-08 审查，基于 commit `fbf8bf0`。
> 修复在 `f206aa1` 中提交。

## 一、审查范围

对整个代码库进行全面审查，覆盖以下模块：

| 模块 | 文件数 | 审查重点 |
|------|--------|---------|
| `cmd/client/` (GUI) | 10 | 新增代码的编译正确性、逻辑完整性 |
| `cmd/server/` | 1 | 格式化、import 正确性 |
| `internal/client/` | 7 | 接口一致性、状态管理 |
| `internal/server/` | 8 | 并发安全、资源管理 |
| `internal/tun/` | 3 | API 适配正确性 |
| `internal/protocol/` | 3 | 序列化/反序列化边界 |
| `internal/auth/` | 1 | 密码学正确性 |

---

## 二、发现的问题与修复

### 问题 1：未使用的导入 — `net/http`（编译错误）

**文件**：`cmd/client/app.go`
**严重度**：🔴 编译失败

**原因**：`app.go` 在重构过程中移除了 HTTP 相关逻辑（迁移到 `httpserver.go`），但 `import "net/http"` 未清理。

**Go 编译器行为**：Go 不允许未使用的导入，直接报错。

**修复**：删除 `net/http` 导入。

---

### 问题 2：未使用的结构体字段 `cfgPath`（编译警告）

**文件**：`cmd/client/app.go`
**严重度**：🔴 编译失败

**原因**：`App` 结构体中声明了 `cfgPath string` 字段，但在整个代码中从未读写。

**Go 编译器行为**：未使用的局部变量会报错，未使用的结构体字段不会（但属于代码异味）。

**修复**：删除 `cfgPath` 字段。

---

### 问题 3：状态同步断链 — 托盘和 API 永远显示"未连接"（核心功能缺陷）

**文件**：`cmd/client/app.go`
**严重度**：🔴 功能失效

**现象**：客户端成功连接后，系统托盘仍显示灰色"未连接"，Web 面板状态栏始终为"🔴 未连接"。

**根因分析**：

`App` 结构体维护了 `connected`、`virtualIP`、`serverIP`、`peerCount` 等状态字段，`GetStatus()` 方法读取这些字段返回给前端和托盘。

但 `updateStatus()`、`updatePeers()`、`updateRTT()` 三个方法**从未被调用**。

`connectLoop()` 调用 `tunnel.Connect()` 后阻塞等待返回，只检查 error，没有在连接成功时更新 App 状态。

```
tunnel.Connect() 内部成功注册、创建 TUN、启动 goroutine
  → App.connected 仍为 false
  → GetStatus() 返回 connected=false
  → 托盘/前端永远显示"未连接"
```

**修复方案**：

1. 在 `internal/client/tunnel.go` 中新增 `TunnelStatus` 结构体和 `Status()` 方法：

```go
type TunnelStatus struct {
    Connected  bool
    VirtualIP  net.IP
    SubnetMask net.IPMask
    ServerIP   net.IP
    PeerCount  int
}

func (t *Tunnel) Status() TunnelStatus {
    t.mu.RLock()
    defer t.mu.RUnlock()
    return TunnelStatus{
        Connected:  t.tunDev != nil && t.virtualIP != nil,
        VirtualIP:  t.virtualIP,
        SubnetMask: t.subnetMask,
        ServerIP:   t.serverIP,
        PeerCount:  len(t.peers),
    }
}
```

2. 在 `App` 中新增 `statusLoop()`，每秒轮询 `tunnel.Status()` 并同步到 App 字段：

```go
func (a *App) statusLoop(ctx context.Context) {
    ticker := time.NewTicker(time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C:
        }
        ts := a.tunnel.Status()
        a.mu.Lock()
        // 同步 connected, virtualIP, serverIP, peerCount, uptime
        a.mu.Unlock()
    }
}
```

3. `statusLoop` 在 `connectLoop` 启动时一并启动，通过 `pollCtx` 跟随连接生命周期。

**设计决策**：使用轮询而非回调，因为 `client.Tunnel` 是独立包，不应为 GUI 耦合回调接口。1 秒轮询对 CPU 开销可忽略。

---

### 问题 4：`Disconnect()` 未调用 `CloseTUN()` — TUN 设备泄漏

**文件**：`cmd/client/app.go`
**严重度**：🔴 资源泄漏 + 状态错误

**现象**：断开连接后，`tunnel.Status()` 仍报告 `Connected: true`（因为 `tunDev != nil`），托盘显示绿色"已连接"但实际已断。

**根因**：`App.Disconnect()` 调用了 `tunnel.Disconnect()`（发送 leave 包 + 关闭 UDP conn），但没有调用 `tunnel.CloseTUN()`（关闭 TUN 设备 + 置 nil）。

`Disconnect()` vs `CloseTUN()` 的职责：

| 方法 | 行为 | 调用时机 |
|------|------|---------|
| `Disconnect()` | 发送 TypeDisconnect、关闭 UDP conn | 每次断开时 |
| `CloseTUN()` | 关闭 TUN 设备、置 `tunDev=nil`、清除 `lastAssignedIP` | 程序最终退出时 |

在 CLI 版中，`CloseTUN()` 只在程序退出时调用一次（因为重连复用 TUN）。但在 GUI 版中，用户可能主动点击"断开"，此时需要清理 TUN 状态。

**修复**：在 `App.Disconnect()` 中补充 `tunnel.CloseTUN()` 调用。

---

### 问题 5：`Connect()` 未清理旧 Tunnel — 资源泄漏

**文件**：`cmd/client/app.go`
**严重度**：🔴 资源泄漏

**现象**：用户多次点击"一键加入"，每次创建新的 `client.Tunnel`（新的 UDP conn + 可能的新 TUN 设备），旧的不释放。

**根因**：`Connect()` 直接执行 `a.tunnel = client.New(cfg)`，旧 tunnel 的 `Disconnect()` 和 `CloseTUN()` 从未被调用。

**修复**：在创建新 tunnel 前，先清理旧的：

```go
func (a *App) Connect(cfg *client.Config) {
    // ...
    // Clean up old tunnel before creating new one
    a.cancel()
    a.tunnel.Disconnect()
    a.tunnel.CloseTUN()

    a.tunnel = client.New(cfg)
    a.ctx, a.cancel = context.WithCancel(context.Background())
    // ...
}
```

---

### 问题 6：竞态条件 — `statusLoop` 读 `a.tunnel` 无同步

**文件**：`cmd/client/app.go`
**严重度**：🟡 数据竞争

**场景**：
- `statusLoop()` 每秒读 `a.tunnel`（无锁）
- `Connect()` 写 `a.tunnel = client.New(cfg)`（持锁保护 `connecting`，但 `a.tunnel` 赋值在锁外）

两个 goroutine 并发访问 `a.tunnel` 指针，构成数据竞争。

**Go 竞态检测器**：`go run -race` 会报告此问题。

**修复**：`statusLoop` 读 `a.tunnel` 时加 `RLock`：

```go
a.mu.RLock()
tun := a.tunnel
a.mu.RUnlock()
if tun == nil { continue }
ts := tun.Status()
```

> **注**：此修复在 2026-06-13 会话中进一步完善（commit `2a1bf92`）。原修复仅保护了 `statusLoop` 中的读取，但 `connectLoop` 和 `Disconnect` 仍然存在对 `a.tunnel`/`a.cfg` 的无锁访问。2026-06-13 的修复将 `Connect()` 中的 cfg/tunnel/ctx 更新移入 `mu.Lock` 块，`Disconnect()` 中所有状态变更在锁内完成，`connectLoop()` 启动时在 `RLock` 下捕获所有共享状态。详见 `2026-06-13-comprehensive-bug-fix.md`。

---

### 问题 7：`main_other.go` 死代码导入

**文件**：`cmd/client/main_other.go`
**严重度**：🟡 代码异味

**原代码**：

```go
import "github.com/holipay/gametunnel/internal/client"

func main() {
    fmt.Println("...")
    os.Exit(1)                    // ← 程序在这里退出
    _ = client.Config{}           // ← 永远不执行
}
```

`_ = client.Config{}` 的目的是抑制"unused import"编译错误，但它在 `os.Exit(1)` 之后，属于死代码。

**修复**：直接删除 `client` 导入和死代码行。

---

### 问题 8：前端引用已删除的 `s.peers` 数组

**文件**：`cmd/client/static/index.html`
**严重度**：🟡 功能异常

**现象**：Web 面板的"在线玩家"区域无法显示数据，控制台报 `s.peers is undefined`。

**根因**：第一版 `StatusResponse` 包含 `Peers []PeerStatus` 字段，前端 JS 遍历 `s.peers` 数组。重构后改为 `PeerCount int`，但前端未同步更新。

**修复**：将前端从遍历 `s.peers` 数组改为读取 `s.peer_count` 数字，显示在线人数和房间名。

---

### 问题 9：`SaveConfig` 返回值未检查

**文件**：`cmd/client/app.go`
**严重度**：🟢 低（静默失败）

**原代码**：`client.SaveConfig(cfg)` 返回 `error` 但被忽略。

**修复**：检查错误并记录日志：

```go
if err := client.SaveConfig(cfg); err != nil {
    log.Printf("[app] 保存配置失败: %v", err)
}
```

---

## 三、原始编译错误（非审查范围，但一并记录）

### TUN 批量 I/O API 变更

**文件**：`internal/tun/tun.go`
**严重度**：🔴 编译失败

`golang.zx2c4.com/wireguard/tun` 更新后，`Read`/`Write` 签名从单包改为批量：

```
旧：Read(buf []byte, offset int) (int, error)
新：Read(bufs [][]byte, sizes []int, offset int) (int, error)

旧：Write(data []byte, offset int) (int, error)
新：Write(bufs [][]byte, offset int) (int, error)
```

代码已有 `readPackets [1][]byte`、`readSizes [1]int`、`writePackets [1][]byte` 字段但未使用。修复后用单包数组包装调用批量接口。

### 服务端格式化动词错误

**文件**：`cmd/server/main.go`
**严重度**：🟡 逻辑 bug

```go
// 错误：%x 是十六进制，用于 string 会打出乱码
log.Fatalf("子网解析失败 %x: %v", *subnetStr, err)
// 正确：
log.Fatalf("子网解析失败 %s: %v", *subnetStr, err)
```

---

## 四、审查方法论

### 4.1 检查清单

| 检查项 | 方法 | 发现问题 |
|--------|------|---------|
| 编译正确性 | 检查 import 使用、字段引用 | #1, #2 |
| 跨文件依赖 | grep 函数调用 vs 定义 | #3, #8 |
| 资源生命周期 | 追踪 Create/Close 配对 | #4, #5 |
| 并发安全 | 检查共享变量的锁保护 | #6 |
| 死代码 | 检查不可达路径 | #7 |
| 返回值处理 | 检查 error 返回值 | #9 |

### 4.2 不在审查范围

以下问题已知但未修复（非本次变更引入）：

| 问题 | 说明 |
|------|------|
| `directPeerTrafficMap` 死代码 | `keepalive.go` 中写入但从未读取，实际用 `Peer.DirectReach` |
| `client_test.go` mock 接口 | 测试中的 `mockTunDevice` 使用旧单包接口，与 `TunDevice` 接口一致（无问题） |
| 服务端 `pendingAuth` 无锁 | `int` 类型在 `s.mu` 保护下操作（已安全） |

---

## 五、改动文件清单

| 文件 | 改动类型 | 说明 |
|------|---------|------|
| `cmd/client/app.go` | 重写 | 修复 #1 #2 #3 #4 #5 #6 #9 |
| `cmd/client/main_other.go` | 修改 | 修复 #7 |
| `cmd/client/static/index.html` | 修改 | 修复 #8 |
| `cmd/client/tray.go` | 修改 | 适配新的 `PeerCount` 字段 |
| `internal/client/tunnel.go` | 修改 | 新增 `TunnelStatus` + `Status()` 方法 |
