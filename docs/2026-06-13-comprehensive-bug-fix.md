# 2026-06-13 综合 Bug 修复报告

> 2026-06-13 实施，基于 commit `1c9e391`。
> 分四次提交完成：`2a1bf92`、`7cbac5a`、`35eefa5`、`421eb4e`。

## 一、修复总览

| 优先级 | 数量 | 说明 |
|--------|------|------|
| 🔴 HIGH | 5 | 数据竞争、资源泄漏、功能失效 |
| 🟡 MEDIUM | 2 | nil panic、goroutine 无法取消 |
| 🟢 LOW | 4 | 静默错误、goroutine 泄漏、格式修复 |
| **合计** | **11 + 1** | **11 个 bug + 1 个格式修复** |

**测试结果**: 60 个测试全部通过，服务端编译成功。

---

## 二、HIGH 级别修复

### 2.1 数据竞争：`a.tunnel` 在 `statusLoop` 中无锁读取

**文件**: `cmd/client/app.go:193`
**提交**: `2a1bf92`

**问题**: `statusLoop()` 在 `RLock` 下捕获 `tun := a.tunnel`，但后续调用 `a.tunnel.Status()` 时又无锁读取了 `a.tunnel`，与 `Connect()` 中的写操作构成数据竞争。

**修复**: 使用已捕获的局部变量 `tun.Status()` 替代 `a.tunnel.Status()`。

```go
// 修复前
ts := a.tunnel.Status()

// 修复后
ts := tun.Status()
```

---

### 2.2 数据竞争：`a.cfg` 无同步写入

**文件**: `cmd/client/app.go`、`cmd/client/tray.go`
**提交**: `2a1bf92`

**问题**: `a.cfg` 在 `Connect()`、`tray.go` 的 settings 回调中无锁写入，但在 `GetStatus()` 中通过 `RLock` 读取，构成数据竞争。

**修复**:
- `Connect()`: cfg/tunnel/ctx 更新移入 `mu.Lock` 块
- `Disconnect()`: 所有状态变更在锁内完成，慢操作在锁外执行
- `connectLoop()`: 启动时在 `RLock` 下捕获 cfg/tun/ctx，后续使用局部变量
- `tray.go`: 3 处 `tr.app.cfg = cfg` 写入均加锁保护

```go
// Connect() — 锁内原子更新
a.mu.Lock()
a.cfg = cfg
a.tunnel = client.New(cfg)
a.ctx, a.cancel = context.WithCancel(context.Background())
a.mu.Unlock()

// connectLoop() — 启动时捕获
a.mu.RLock()
cfg := a.cfg
tun := a.tunnel
ctx := a.ctx
a.mu.RUnlock()
```

---

### 2.3 UDP Socket 泄漏：`createTUN` 失败时未关闭 conn

**文件**: `internal/client/tunnel.go:199,206`
**提交**: `2a1bf92`

**问题**: `Connect()` 中 `net.ListenUDP` 创建的 conn 在 `createTUN()` 失败时未关闭，导致文件描述符泄漏。每次重连失败都会泄漏一个 UDP socket。

**修复**: 在两个 `createTUN` 失败分支中增加 `conn.Close()`。

```go
case tunAlive && ipChanged:
    if err := t.createTUN(mtu); err != nil {
        conn.Close()  // 新增
        return err
    }
case !tunAlive:
    if err := t.createTUN(mtu); err != nil {
        conn.Close()  // 新增
        return err
    }
```

---

### 2.4 服务端超时检测无效：`keepaliveLoop` 只记录日志不触发重连

**文件**: `internal/client/keepalive.go:133-156`
**提交**: `2a1bf92`

**问题**: `keepaliveLoop` 检测到服务端 30 秒无响应时只记录日志，不取消 context。注释声称"ReadFromUDP goroutine 会最终退出"，但 UDP `ReadFromUDP` 在无数据时永久阻塞，导致客户端进入僵尸状态。

**修复**: `keepaliveLoop` 增加 `cancel context.CancelFunc` 参数，检测到超时后调用 `cancel()` 并返回。

```go
func (t *Tunnel) keepaliveLoop(ctx context.Context, cancel context.CancelFunc) {
    // ...
    if lastSeen != nil && time.Since(*lastSeen) > serverTimeout {
        log.Printf(i18n.T().LogServerTimeout, serverTimeout)
        cancel()  // 新增：触发 connectLoop 重连
        return
    }
}

// tunnel.go 调用处
go func() {
    t.keepaliveLoop(runCtx, runCancel)  // 传递 runCancel
    onGoroutineExit("keepaliveLoop")
}()
```

---

### 2.5 CloseTUN 数据竞争：`t.tunDev` 并发读写导致 nil panic

**文件**: `internal/client/tunnel.go`、`internal/client/recv.go`
**提交**: `35eefa5`

**问题**: `CloseTUN()` 写 `t.tunDev = nil` 不持锁，而 `receiveFromTUN`、`handleDirectData`、`handleDataFromServer` 并发读取 `t.tunDev`，导致 TOCTOU nil 指针 panic。

**修复**:
- `CloseTUN()`: 使用 `t.mu.Lock()` 保护 nil 赋值，在锁外调用 `Close()`
- `handleDirectData`/`handleDataFromServer`: 在 `t.mu.RLock()` 下捕获 `dev`，使用局部指针
- `receiveFromTUN`: 在 `t.mu.RLock()` 下捕获 `dev`，替代直接读取

```go
// CloseTUN — 锁内置 nil，锁外 Close
func (t *Tunnel) CloseTUN() {
    t.mu.Lock()
    dev := t.tunDev
    t.tunDev = nil
    t.lastAssignedIP = nil
    t.mu.Unlock()
    if dev != nil {
        dev.Close()
    }
}

// handleDataFromServer — RLock 下捕获
t.mu.RLock()
dev := t.tunDev
t.mu.RUnlock()
if dev == nil {
    protocol.PutDataPayload(dp)
    return
}
```

---

## 三、MEDIUM 级别修复

### 3.1 `pendingAuth` 下溢：`CleanupStale` 缺少 `> 0` 守卫

**文件**: `internal/server/room.go:809`
**提交**: `7cbac5a`

**问题**: `CleanupStale()` 中 `r.pendingAuth--` 无守卫，其他所有递减点均有 `> 0` 检查。下溢后 `pendingAuth` 为负数，绕过 `maxPending` 限流。

**修复**: 添加 `if r.pendingAuth > 0` 守卫。

```go
for _, sa := range staleAuths {
    if cur, ok := r.addrMap[sa.key]; ok && cur == sa.c {
        delete(r.addrMap, sa.key)
        if r.pendingAuth > 0 {  // 新增
            r.pendingAuth--
        }
    }
}
```

---

### 3.2 打洞 goroutine 无法取消

**文件**: `internal/client/keepalive.go:109-115`、`internal/client/recv.go:105`
**提交**: `7cbac5a`

**问题**: `handleHolePunchReceived` 启动的 goroutine 无 `ctx.Done()` 检查，隧道断开后仍发送过期打洞包。

**修复**: `handleHolePunchReceived` 增加 `ctx context.Context` 参数，goroutine 内检查 `ctx.Done()`。

```go
func (t *Tunnel) handleHolePunchReceived(ctx context.Context, payload []byte) {
    // ...
    go func() {
        packet := t.cachedPunchPacket
        for i := 0; i < holePunchBurstPerPhase; i++ {
            select {
            case <-ctx.Done():  // 新增
                return
            default:
            }
            t.sendCtrl(packet, peer.PublicAddr)
            time.Sleep(50 * time.Millisecond)
        }
    }()
}
```

---

## 四、LOW 级别修复

### 4.1 `StatusResponse.JSON()` 静默丢弃 Marshal 错误

**文件**: `cmd/client/app.go:318`
**提交**: `7cbac5a`

**修复**: 增加错误日志，返回 `{}` 而非空字节。

### 4.2 `regRateLimitLoop` goroutine 无法停止

**文件**: `internal/server/room.go:147-203`
**提交**: `7cbac5a`

**修复**: 新增 `done chan struct{}` 和 `Stop()` 方法，loop 内检查 `done` channel。

### 4.3 日志文件操作错误被静默忽略

**文件**: `cmd/client/run.go:50-59`
**提交**: `7cbac5a`

**修复**: `MkdirAll`/`Rename` 错误增加日志输出。

### 4.4 `tunDev` nil 检查（首次修复）

**文件**: `internal/client/recv.go:286`
**提交**: `7cbac5a`

**修复**: `receiveFromTUN` 循环内 `t.tunDev.Read()` 前增加 nil 检查。（后续在 `35eefa5` 中升级为 RLock 下捕获。）

### 4.5 tray.go 缩进格式修复

**文件**: `cmd/client/tray.go:95-104`
**提交**: `421eb4e`

**修复**: settings dialog 回调内 `if showSettingsDialog` 块缩进不一致，统一为正确缩进。

---

## 五、跳过的低优先级问题

以下问题经审查确认非实际 bug，未修复：

| # | 问题 | 原因 |
|---|------|------|
| 1 | `pktPool` buffer 恢复模式脆弱 | `handlePacket` 不会 reslice，模式正确 |
| 2 | 广播超 32 客户端截断 | `append` 超过数组容量时自动分配堆内存，无截断 |
| 3 | 数据包饥饿（sendLoop 优先级） | 控制包优先是合理设计选择 |

---

## 六、改动文件清单

| 文件 | 提交 | 改动说明 |
|------|------|---------|
| `cmd/client/app.go` | `2a1bf92` `7cbac5a` | 数据竞争修复 (a.tunnel/a.cfg)；JSON 错误处理 |
| `cmd/client/tray.go` | `2a1bf92` `421eb4e` | cfg 写入加锁；缩进修复 |
| `cmd/client/run.go` | `7cbac5a` | 日志文件错误处理 |
| `internal/client/tunnel.go` | `2a1bf92` `35eefa5` | UDP socket 泄漏修复；CloseTUN 数据竞争修复 |
| `internal/client/keepalive.go` | `2a1bf92` `7cbac5a` | 服务端超时触发重连；打洞 goroutine 取消 |
| `internal/client/recv.go` | `7cbac5a` `35eefa5` | tunDev nil 检查；tunDev RLock 下捕获；handleHolePunchReceived ctx 参数 |
| `internal/server/room.go` | `7cbac5a` | pendingAuth 下溢守卫；regRateLimitLoop 停止机制 |

---

## 七、最终代码状态

- **已修复 bug**: 11 个（5 HIGH + 2 MEDIUM + 4 LOW）
- **测试**: 60 个全部通过
- **编译**: 服务端 Linux 编译成功，客户端需 Windows 环境
- **代码审查**: 全面扫描无新增高优先级问题
