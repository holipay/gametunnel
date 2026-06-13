# GameTunnel 数据包与连接问题审查

> 2026-05-09 审查，基于 commit `85199a0`。
> 修复在 `3f24724`（第一批）和 `ca4d44d`（第二批）中提交。
> 待推送（网络问题导致本地 commit 未同步到 GitHub）。

## 一、审查范围

从数据包处理的角度对客户端和服务端进行全面审查，重点关注：

- P2P 打洞机制的正确性
- 数据包收发路径中的并发安全
- 锁的粒度与持锁时间
- buffer 管理与内存安全
- 连接生命周期中的资源竞争

| 模块 | 文件 | 审查重点 |
|------|------|---------|
| 客户端接收 | `recv.go` | 数据包来源区分、DirectReach 检测、buffer 安全 |
| 客户端打洞 | `keepalive.go` | hole punch 流程、并发阻塞 |
| 客户端路由 | `route.go` | 广播/单播路由正确性 |
| 客户端隧道 | `tunnel.go` | sendUDP 错误处理、连接生命周期 |
| 服务端中继 | `relay.go` | 锁粒度、编码开销 |
| 服务端对等 | `peer.go` | PeerInfo 广播、RTT 测量 |
| 协议层 | `protocol/` | 数据包格式、序列化边界 |

---

## 二、发现的问题与修复

### 问题 1：DirectReach 对所有流量（含中继）标记 — P2P 检测完全失效

**文件**：`recv.go` — `handleDataFromServer` + `handleHolePunchReceived`
**严重度**：🔴 架构缺陷

**现象**：`startHolePunch` 的提前退出检查 `hasDirectPeerTraffic()` 在第一个数据包后就返回 `true`，但实际上所有流量都走服务器中继。P2P 打洞优化形同虚设。

**根因分析**：

客户端的 `receiveFromServer` 只从 `t.conn`（服务器 UDP 连接）读取。所有到达的包源地址都是服务器，无论实际是中继还是 P2P 直连。

```
receiveFromServer → 从 t.conn 读取
  ↓
所有包源地址 = 服务器
  ↓
handleDataFromServer 调用 markDirectPeerTraffic(dp.SrcIP)
  ↓
DirectReach 对每个 peer 在第一个包后设为 true
  ↓
startHolePunch 提前退出检查永远为 true
  ↓
打洞优化"看起来有效"，实际全部走服务器中继
```

同样的问题存在于 `handleHolePunchReceived` — hole punch 通知是服务器转发的，不是对端直接发来的。

**数据流对比**：

| 路径 | 包到达地址 | 实际来源 | 错误标记 DirectReach |
|------|-----------|---------|-------------------|
| 服务器中继 | 服务器 IP | 服务器转发 | ✓ 被标记 |
| P2P 直连 | peer 公网 IP | peer 直发 | 从未到达（receiveFromServer 只读服务器 conn） |

**修复方案**：重构 `receiveFromServer`，根据源地址区分数据包来源：

```go
// recv.go — receiveFromServer
n, from, err := t.conn.ReadFromUDP(buf)
// ...
if from != nil && t.serverAddr != nil && !from.IP.Equal(t.serverAddr.IP) {
    // 来自 peer 公网地址 → P2P 直连数据
    t.handleDirectData(from, msg)
} else {
    // 来自服务器 → 中继数据
    t.handleServerData(ctx, msg)
}
```

新增 `handleDirectData` 函数，仅在此路径设置 `DirectReach`：

```go
func (t *Tunnel) handleDirectData(from *net.UDPAddr, msg *protocol.Message) {
    // 验证 srcIP 是已知 peer
    // 验证包确实来自该 peer 的公网地址
    peer.DirectReach.Store(true)  // 唯一正确的 DirectReach 标记点
    t.tunDev.Write(dp.Data)
}
```

移除 `markDirectPeerTraffic` 方法（不再被任何路径调用）。

**设计决策**：DirectReach 的含义从"收到过该 peer 的数据"变为"确认 P2P 直连路径可用"。只有当数据包从 peer 的公网地址（而非服务器）到达时才标记。

---

### 问题 2：`handleHolePunchReceived` 阻塞接收 goroutine 250ms

**文件**：`keepalive.go` — `handleHolePunchReceived`
**严重度**：🔴 丢包风险

**现象**：收到 hole punch 通知时，接收 goroutine 同步执行 5 × 50ms = 250ms 的 back-punch 发送。期间所有服务器发来的包在 UDP buffer 排队，buffer 满则丢包。

**影响**：游戏高频广播场景下（StarCraft 等），250ms 的阻塞可导致：
- keepalive 包延迟 → 服务端误判超时
- PeerInfo 更新延迟 → peer 列表过期
- 游戏数据包丢失 → 卡顿

**修复**：将 back-punch 移到独立 goroutine：

```go
func (t *Tunnel) handleHolePunchReceived(ctx context.Context, payload []byte) {
    // ... 验证 peer 存在 ...
    go func() {
        punchPayload := make([]byte, 4)
        copy(punchPayload, t.virtualIP.To4())
        packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)
        for i := 0; i < holePunchBurstPerPhase; i++ {
            select {
            case <-ctx.Done():
                return
            default:
            }
            t.sendUDP(packet, peer.PublicAddr)
            time.Sleep(50 * time.Millisecond)
        }
    }()
}
```

> **注**：`ctx` 参数在 2026-06-13 会话中添加（commit `7cbac5a`），使打洞 goroutine 在隧道断开时能正确取消。原修复中 goroutine 无法被取消，隧道关闭后仍会发送过期打洞包。

**权衡**：goroutine 持有 peer 引用，可能在 `handlePeerInfo` 替换 peers map 后指向旧对象。但旧 peer 的 PublicAddr 仍有效（NAT mapping 仍在），且 `sendUDP` 是线程安全的，所以实际无害。

---

### 问题 3：`handleRelay` 在 RLock 内执行 CRC32 编码

**文件**：`server/relay.go` — `handleRelay`
**严重度**：🟡 性能瓶颈

**原代码**：

```go
s.mu.RLock()
// ... 验证 srcIP ...
encoded := protocol.EncodeChecked(protocol.TypeData, payload)  // CRC32 + 内存分配
// ... 遍历目标 ...
s.mu.RUnlock()
```

`EncodeChecked` 包含 CRC32 计算和 `make([]byte, ...)` 内存分配。在 RLock 内执行意味着：
- 所有写操作（keepalive 更新 `LastSeen`、peer 加入/离开）被阻塞
- 多个 worker goroutine 的中继操作互相竞争 RLock

**修复**：分两阶段 — 持锁收集目标，释放后编码发送：

```go
// Phase 1: 验证 + 收集目标（RLock）
s.mu.RLock()
// ... 验证 sender, srcIP ...
var targets []*net.UDPAddr
// ... 收集 targets ...
s.mu.RUnlock()

// Phase 2: 编码 + 发送（无锁）
encoded := protocol.EncodeChecked(protocol.TypeData, payload)
for _, addr := range targets {
    s.sendCheckedRaw(encoded, addr)
}
```

---

### 问题 4：`handlePeerInfo` 在 WLock 内启动 hole punch goroutine

**文件**：`recv.go` — `handlePeerInfo`
**严重度**：🟡 不必要的锁持有时间

**原代码**：

```go
t.mu.Lock()
defer t.mu.Unlock()
// ...
go t.startHolePunch(ctx, entry.VirtualIP)  // 在 WLock 内启动
```

`go` 语句本身开销很小（创建 goroutine），但在 WLock 内执行会延迟其他需要读锁的操作（`routePacket`、`handleDataFromServer` 等）。

**修复**：收集需要打洞的 peer 列表，释放锁后再启动：

```go
var newPeerIPs []net.IP
t.mu.Lock()
// ... 处理 peer 列表，收集 newPeerIPs ...
t.peers = newPeers
t.mu.Unlock()

for _, peerIP := range newPeerIPs {
    go t.startHolePunch(ctx, peerIP)
}
```

---

### 问题 5：TUN 写入错误被静默忽略

**文件**：`recv.go` — `handleDataFromServer`
**严重度**：🟡 静默丢包

**原代码**：

```go
t.tunDev.Write(dp.Data)  // 返回值被忽略
```

TUN 设备写入失败时（设备关闭、buffer 满），数据被静默丢弃，用户无法感知。

**修复**：

```go
if _, err := t.tunDev.Write(dp.Data); err != nil {
    log.Printf("[tunnel] TUN 写入失败: %v", err)
}
```

---

### 问题 6：`sendUDP` 错误只记录一次就静默

**文件**：`tunnel.go` — `sendUDP`
**严重度**：🟡 持续故障不可见

**原代码**：

```go
if t.sendErrors.Add(1) == 1 {
    log.Printf("[tunnel] 发送失败: %v", err)
}
```

只有第一次发送失败记录日志，后续全部静默。如果 UDP socket 持续不可用（网络断开），用户只看到一条日志。

**修复**：

```go
n := t.sendErrors.Add(1)
if n == 1 || n%100 == 0 {
    log.Printf("[tunnel] 发送失败 (累计%d次): %v", n, err)
}
```

---

### 问题 7：`receiveFromTUN` buffer 复用存在潜在数据竞争

**文件**：`recv.go` — `receiveFromTUN`
**严重度**：🟡 脆弱设计

**分析**：

```go
buf := make([]byte, 65535)  // 循环复用
for {
    n, err := t.tunDev.Read(buf)
    // ...
    t.routePacket(buf[:n], srcIP, dstIP)  // 传 slice 引用
}
```

当前安全：`routePacket` → `sendToServer` → `DataPayload.Marshal()` → `sendUDP` 在当前 goroutine 同步执行，`Marshal()` 会拷贝数据。

但脆弱：如果未来 `sendUDP` 改为异步，或 `routePacket` 启动 goroutine 发送，buffer 会被下次 `Read` 覆盖。

**修复**：在传给 `routePacket` 前拷贝：

```go
pkt := make([]byte, n)
copy(pkt, buf[:n])
t.routePacket(pkt, srcIP, dstIP)
```

---

### 问题 8：读 buffer 过大（65535 字节）

**文件**：`recv.go`
**严重度**：🟢 内存浪费

**分析**：`receiveFromServer` 和 `receiveFromTUN` 各分配 65535 字节 buffer。MTU 默认 1400，协议包最大约 1500 字节。每个连接浪费约 120KB。

**修复**：缩减为 4096 字节，留足 headroom：

```go
const readBufSize = 4096
```

---

## 三、未修复的已知问题

以下问题在审查中发现但未修复（影响较低或需要更大范围重构）：

### 3.1 服务器 `conn.WriteToUDP` 并发写无保护

**文件**：`server/server.go` — `sendChecked` / `sendCheckedRaw`

多个 worker goroutine + keepaliveLoop + pingLoop + peerInfoLoop 并发调用 `WriteToUDP`。Go 的 `net.UDPConn.WriteToUDP` 内部有锁，当前安全，但依赖 runtime 实现细节。客户端用 `connMu` 保护，服务器没有。

**建议**：添加 `connMu sync.Mutex` 保护，与客户端保持一致。

### 3.2 `startHolePunch` 与 `handlePeerInfo` 的 peers map 竞争

`startHolePunch` 获取 peer 引用后无锁持有，`handlePeerInfo` 可能替换整个 peers map。旧 peer 的 `PublicAddr` 可能过期。

**实际影响**：低。peer 地址更新意味着 NAT mapping 变了，打洞本身需要重试。旧地址的包只是被丢弃。

### 3.3 `Disconnect()` 与 `sendUDP()` 的关闭竞争

`Disconnect()` 调用 `t.conn.Close()` 不持 `connMu`，可能与另一个 goroutine 的 `sendUDP` 并发。Go 的 `WriteToUDP` 在 closed conn 上返回 error，不会 panic，但会产生误导性错误日志。

### 3.4 `startHolePunch` 中的 `hasDirectPeerTraffic` 检查时机

打洞分 3 个 phase（100ms / 250ms / 500ms），每个 phase 后检查 `hasDirectPeerTraffic`。但 `handleDirectData` 设置 `DirectReach` 的前提是收到对端的直连数据包。在打洞阶段，对端还没有发送直连数据（只发了 hole punch），所以 `DirectReach` 不会被设置，提前退出逻辑仍然不生效。

要让 P2P 检测真正工作，需要在打洞完成后让一端主动发送一个直连数据包（如 keepalive），对端收到后标记 `DirectReach`，后续打洞才能提前退出。这需要协议层面的改动。

---

## 四、改动文件清单

| 文件 | Commit | 改动说明 |
|------|--------|---------|
| `internal/client/recv.go` | `3f24724` + `ca4d44d` | 重构接收循环，区分服务器/直连路径；新增 handleDirectData、handleServerData；修复 buffer 拷贝、缩减 buffer 大小、TUN 写入错误检查 |
| `internal/client/keepalive.go` | `3f24724` + `ca4d44d` | 修复 DirectReach 设置（后移除 markDirectPeerTraffic）；hole punch back-punch 移到 goroutine |
| `internal/client/tunnel.go` | `3f24724` | 改进 sendUDP 错误日志 |
| `internal/server/relay.go` | `ca4d44d` | CRC32 编码移到 RLock 外部 |

---

## 五、审查方法论

### 5.1 数据包路径追踪

对每种数据包类型追踪完整路径，标注每一步的锁状态、goroutine 归属、buffer 来源：

| 包类型 | 发送方 | 到达地址 | 处理函数 | 锁状态 |
|--------|--------|---------|---------|--------|
| TypeData（中继） | 服务器 | 服务器 IP | handleDataFromServer | RLock 读 peers |
| TypeData（直连） | peer | peer 公网 IP | handleDirectData | RLock 读 peers |
| TypeHolePunch | 服务器 | 服务器 IP | handleHolePunchReceived | RLock 读 peers |
| TypePeerInfo | 服务器 | 服务器 IP | handlePeerInfo | WLock 写 peers |
| TypePing | 服务器 | 服务器 IP | handleServerData | 无锁 |

### 5.2 并发安全检查

对每个共享变量检查所有访问路径：

| 变量 | 保护方式 | 访问路径 | 安全性 |
|------|---------|---------|--------|
| `t.peers` | `t.mu` (RWMutex) | handlePeerInfo(W), routePacket(R), handleDataFromServer(R), handleDirectData(R) | ✓ |
| `t.conn` | `t.connMu` (Mutex) | sendUDP(Lock), Disconnect(Close) | ⚠ Close 不持锁 |
| `Peer.DirectReach` | atomic.Bool | handleDirectData(Store), hasDirectPeerTraffic(Load) | ✓ |
| `t.tunDev` | `t.mu` (RWMutex) | receiveFromTUN(RLock+Read), handleDataFromServer(RLock+Write), handleDirectData(RLock+Write), CloseTUN(Lock+nil) | ✓ (2026-06-13 修复) |
| 服务端 `s.clients` | `s.mu` (RWMutex) | handleRelay(R), handleRegister(W), handleDisconnect(W) | ✓ |

### 5.3 Buffer 生命周期追踪

追踪每个 buffer 的分配、使用、拷贝点：

| Buffer | 分配位置 | 复用 | 拷贝点 | 安全性 |
|--------|---------|------|--------|--------|
| receiveFromServer buf | goroutine 启动时 | 循环复用 | DecodeChecked 内部 | ✓ |
| receiveFromTUN buf | goroutine 启动时 | 循环复用 | routePacket 前 copy | ✓（修复后） |
| DataPayload.Data | UnmarshalData 内 | 每次新分配 | Marshal 内部 | ✓ |
| 服务端 pkt | Run 主循环 | 每次新分配 | handlePacket 直接使用 | ✓ |
