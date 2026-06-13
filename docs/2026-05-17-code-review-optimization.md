# GameTunnel 代码审查 — 优化建议

> 2026-05-17，基于 commit `a0a35f7` 的源码审查。

## 一、严重问题（Bug）

### 1.1 服务端 `handleHolePunch` 丢弃 IPv6 打洞包

**文件**：`internal/server/relay.go`

```go
func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
    if len(payload) < 4 {
        return
    }
    dstIP := net.IP(payload[:4])

    srcIP4 := from.IP.To4()    // ← IPv6 地址返回 nil
    if srcIP4 == nil {          // ← IPv6 客户端直接被丢弃！
        return
    }
    // ...
    copy(punchData[:4], srcIP4) // ← 只拷贝 4 字节，IPv6 地址被截断
}
```

**问题**：
- IPv6 客户端的打洞请求被静默丢弃（`To4()` 返回 nil → 直接 return）
- 即使修复了丢弃问题，`copy(punchData[:4], srcIP4)` 也只能放 4 字节，IPv6 的 16 字节地址放不下
- **结果**：IPv6 传输层虽然已支持，但 P2P 打洞在 IPv6 场景下完全失效

**修复方案**：

```go
func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
    if len(payload) < 4 {
        return
    }
    dstIP := net.IP(payload[:4])

    srcIP := from.IP.To16()    // ← 统一用 16 字节
    if srcIP == nil {
        return
    }

    s.mu.RLock()
    dst, ok := s.clients[ipKey(dstIP)]
    s.mu.RUnlock()
    if !ok {
        return
    }

    addrStr := from.String()
    punchData := make([]byte, 16+len(addrStr))
    copy(punchData[:16], srcIP)         // ← 16 字节 IP
    copy(punchData[16:], []byte(addrStr))
    s.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}
```

**注意**：客户端 `handleHolePunchReceived` 和 `startHolePunch` 中读取 payload 前 4 字节作为虚拟 IP（虚拟 IP 始终是 IPv4 4 字节），这部分不受影响。但服务端转发的 punchData 格式变了（4→16 字节 IP），客户端的 `handleHolePunchReceived` 需同步调整——不过客户端实际上不解析 punchData 中的 srcIP（只用 payload[:4] 中的虚拟 IP 来查找 peer），所以客户端无需改动。

**影响**：当前 IPv6 客户端之间无法 P2P 打洞，只能走服务器中转。

---

### 1.2 服务端 `handleRelay` 广播路径锁内遍历

**文件**：`internal/server/relay.go`

```go
func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
    s.mu.RLock()
    sender := s.addrMap[addrToRateKey(from)]
    // ...
    if isBroadcast {
        for _, c := range s.clients {           // ← 遍历所有客户端
            if addrToRateKey(c.PublicAddr) != fromKey {
                targets = append(targets, c.PublicAddr)
            }
        }
    }
    s.mu.RUnlock()
    // ...
    encoded := protocol.EncodeChecked(protocol.TypeData, payload)
    for _, addr := range targets {
        s.sendCheckedRaw(encoded, addr)         // ← 每个目标发一次
    }
}
```

**问题**：
- 广播时在 RLock 下遍历全部客户端收集 targets，高并发时可能阻塞写操作
- 对每个目标单独调用 `sendCheckedRaw`，无法利用 `writev` 等批量发送 API
- `addrToRateKey` 在循环内对每个客户端调用，可优化为预计算

**优化建议**：
- 将 targets 收集改为 copy PublicAddr 指针数组（已有 `maxInlineTargets` 栈优化，做得不错）
- 考虑将 `addrToRateKey` 比较改为指针比较（`c.PublicAddr != from` 的 `*net.UDPAddr` 指针比较，因为注册时赋值的 `from` 和后续 relay 时的 `from` 是不同对象，所以当前用值比较是正确的——但可以缓存 fromKey 避免重复计算）

---

## 二、客户端性能优化

### 2.1 `UnmarshalData` 每次分配新切片

**文件**：`internal/protocol/messages.go`

```go
func UnmarshalData(data []byte) (*DataPayload, error) {
    // ...
    pktData := make([]byte, len(data)-8)  // ← 每个包都分配
    copy(pktData, data[8:])
    return &DataPayload{
        SrcIP: net.IP(append([]byte(nil), data[0:4]...)),  // ← 又分配
        DstIP: net.IP(append([]byte(nil), data[4:8]...)),  // ← 又分配
        Data:  pktData,
    }, nil
}
```

**问题**：这是热路径（每个游戏数据包都经过），每次调用 3 次堆分配。在 60fps 游戏中可能每秒数百次。

**优化方案**：使用 `sync.Pool` 复用 DataPayload 对象，或提供零拷贝版本：

```go
// 方案 A: sync.Pool
var dataPayloadPool = sync.Pool{
    New: func() interface{} { return &DataPayload{} },
}

func UnmarshalDataPooled(data []byte) (*DataPayload, error) {
    if len(data) < 8 {
        return nil, ErrPacketTooShort
    }
    dp := dataPayloadPool.Get().(*DataPayload)
    dp.SrcIP = append(dp.SrcIP[:0], data[0:4]...)
    dp.DstIP = append(dp.DstIP[:0], data[4:8]...)
    dp.Data = append(dp.Data[:0], data[8:]...)
    return dp, nil
}

// 方案 B: 接受外部 buffer（更彻底）
func UnmarshalDataInto(dp *DataPayload, data []byte) error {
    if len(data) < 8 {
        return ErrPacketTooShort
    }
    dp.SrcIP = append(dp.SrcIP[:0], data[0:4]...)
    dp.DstIP = append(dp.DstIP[:0], data[4:8]...)
    dp.Data = append(dp.Data[:0], data[8:]...)
    return nil
}
```

### 2.2 `sendCtrl` 每次创建新 Timer

**文件**：`internal/client/tunnel.go`

```go
func (t *Tunnel) sendCtrl(data []byte, addr *net.UDPAddr) {
    timer := time.NewTimer(50 * time.Millisecond)  // ← 每次创建
    defer timer.Stop()
    select {
    case t.ctrlCh <- sendJob{data: data, addr: addr}:
    case <-timer.C:
        // ...
    }
}
```

**问题**：`time.NewTimer` 涉及堆分配和 runtime timer 注册。`sendCtrl` 在 keepalive、pong、hole punch 等路径高频调用。

**优化方案**：使用 `time.After` 的 channel 缓存版本，或直接用非阻塞发送 + 统计：

```go
// 方案: 预创建 timer 复用（sendCtrl 从 sendLoop 单线程调用时不需要，
// 但从多个 goroutine 调用时需要 sync.Pool）
var ctrlTimerPool = sync.Pool{
    New: func() interface{} { return time.NewTimer(0) },
}

func (t *Tunnel) sendCtrl(data []byte, addr *net.UDPAddr) {
    timer := ctrlTimerPool.Get().(*time.Timer)
    timer.Reset(50 * time.Millisecond)
    select {
    case t.ctrlCh <- sendJob{data: data, addr: addr}:
    case <-timer.C:
        n := t.sendErrors.Add(1)
        if n == 1 || n%100 == 0 {
            log.Printf(...)
        }
    }
    // 必须 drain timer channel，否则下次 Get() 可能收到过期值
    if !timer.Stop() {
        select {
        case <-timer.C:
        default:
        }
    }
    ctrlTimerPool.Put(timer)
}
```

### 2.3 `p2pKeepaliveLoop` 和 `startHolePunch` 重复构建包

**文件**：`internal/client/keepalive.go`

```go
func (t *Tunnel) sendP2PKeepalives() {
    // ...
    // 复用缓存的打洞包（在 handleAssignIP 中构建一次）
    packet := t.cachedPunchPacket
    for _, peer := range directPeers {
        t.sendCtrl(packet, peer.PublicAddr)
    }
}
```

**优化方案**：在 `Tunnel` 结构中缓存 hole punch packet：

```go
type Tunnel struct {
    // ...
    cachedPunchPacket []byte // 初始化一次，复用
}

// 在 Connect() 或 handleAssignIP() 中初始化：
t.cachedPunchPacket = protocol.EncodeChecked(protocol.TypeHolePunch, t.virtualIP.To4())
```

### 2.4 `receiveFromTUN` 单 goroutine 瓶颈

**文件**：`internal/client/recv.go`

```go
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
    buf := make([]byte, readBufSize)
    for {
        n, err := t.tunDev.Read(buf)    // ← 阻塞读
        // ... 验证、路由 ...
        t.routePacket(buf[:n], srcIP, dstIP)  // ← 同步处理
    }
}
```

**问题**：
- 单 goroutine 串行：Read → 验证 → 路由 → 下一次 Read
- `routePacket` 包含加密和 UDP 发送，可能阻塞
- 高吞吐场景下 TUN 读取被处理延迟拖慢

**优化方案**：使用 worker pool 模式（类似服务端的 `pktCh`）：

```go
func (t *Tunnel) receiveFromTUN(ctx context.Context) {
    buf := make([]byte, readBufSize)
    for {
        n, err := t.tunDev.Read(buf)
        // ... 快速验证 ...

        // 拷贝到新 buffer，释放 buf 给下一次 Read
        pkt := make([]byte, n)
        copy(pkt, buf[:n])

        select {
        case t.tunCh <- tunJob{data: pkt, srcIP: srcIP, dstIP: dstIP}:
        default: // 丢弃（背压）
        }
    }
}

// worker goroutine
func (t *Tunnel) tunWorker(ctx context.Context) {
    for {
        select {
        case <-ctx.Done():
            return
        case job := <-t.tunCh:
            t.routePacket(job.data, job.srcIP, job.dstIP)
        }
    }
}
```

**权衡**：增加一次内存拷贝（Read → pkt），但解耦了读取和处理。对游戏小包（几十到几百字节）拷贝开销可忽略。

### 2.5 `handlePeerInfo` 锁范围过大

**文件**：`internal/client/recv.go`

```go
func (t *Tunnel) handlePeerInfo(ctx context.Context, payload []byte) {
    // ...
    t.mu.Lock()
    // ... 构建 newPeers map ...
    for key, peer := range t.peers {      // ← 锁内遍历日志
        if _, ok := newPeers[key]; !ok {
            log.Printf(...)               // ← 日志 I/O 在锁内
        }
    }
    t.peers = newPeers
    t.mu.Unlock()
    // ...
}
```

**优化**：将移除 peer 的日志移到锁外：

```go
var removedPeers []*Peer

t.mu.Lock()
for key, peer := range t.peers {
    if _, ok := newPeers[key]; !ok {
        removedPeers = append(removedPeers, peer)
    }
}
t.peers = newPeers
t.mu.Unlock()

for _, peer := range removedPeers {
    log.Printf(i18n.T().LogPeerLeave2, peer.Username, peer.VirtualIP)
}
```

### 2.6 `connectLoop` 退避策略可改进

**文件**：`cmd/client/app.go`

```go
const (
    baseDelay   = 2 * time.Second
    maxDelay    = 60 * time.Second
    fastRetries = 3
)

for attempt := 0; ; attempt++ {
    if attempt > 0 {
        delay := baseDelay << (attempt - 1)  // ← 指数退避：2s, 4s, 8s, 16s, 32s, 60s, 60s...
    }
}
```

**问题**：
- 指数退避从 2s 开始，第 5 次就到 32s，对用户来说等待时间感知很差
- 服务端重启后客户端可能要等 30+ 秒才重连

**优化方案**：加入抖动（jitter）+ 更温和的退避曲线：

```go
delay := baseDelay + time.Duration(attempt)*baseDelay/2  // 线性增长: 2s, 3s, 4s, 5s...
if delay > maxDelay {
    delay = maxDelay
}
// 加入 ±20% 抖动避免惊群
jitter := time.Duration(rand.Int63n(int64(delay) / 5))
delay = delay - delay/10 + jitter
```

---

## 三、服务端优化

### 3.1 服务端 `handleRelay` 广播时重复调用 `addrToRateKey`

```go
if isBroadcast {
    for _, c := range s.clients {
        if addrToRateKey(c.PublicAddr) != fromKey {  // ← 每次计算 hash
            targets = append(targets, c.PublicAddr)
        }
    }
}
```

**优化**：直接比较 `*net.UDPAddr` 指针（因为 `from` 和 `c.PublicAddr` 是不同对象，指针不同所以当前用值比较——但可以改用指针标记 sender）：

```go
// 注册时标记 sender 指针
if isBroadcast {
    for _, c := range s.clients {
        if c != sender {  // ← 指针比较，O(1)
            targets = append(targets, c.PublicAddr)
        }
    }
}
```

### 3.2 服务端 Worker Pool 的 `pktCh` 内存拷贝

```go
// server.go Run()
pkt := make([]byte, n)         // ← 每个包分配一次
copy(pkt, buf[:n])
select {
case s.pktCh <- pktJob{data: pkt, addr: remoteAddr}:
default:
}
```

**问题**：每个入站包一次堆分配。对高吞吐场景（中转大量游戏数据）有 GC 压力。

**优化方案**：使用 `sync.Pool` 复用 buffer：

```go
var pktPool = sync.Pool{
    New: func() interface{} { return make([]byte, 65535) },
}

// Run() 中：
for {
    n, remoteAddr, err := s.conn.ReadFromUDP(buf)
    // ...
    pkt := pktPool.Get().([]byte)
    n2 := copy(pkt, buf[:n])
    select {
    case s.pktCh <- pktJob{data: pkt[:n2], addr: remoteAddr}:
    default:
        pktPool.Put(pkt)
    }
}

// worker 处理完后：
defer pktPool.Put(job.data[:cap(job.data)])
```

---

## 四、协议层优化

### 4.1 `EncodeChecked` 每次分配

```go
func EncodeChecked(typ byte, payload []byte) []byte {
    return AppendChecksum(Encode(typ, payload))  // 2 次分配
}
```

已有 `AppendEncodeChecked` 做了优化（单次 append），但客户端热路径（`routePacket`）未使用它。

**优化**：在 `routePacket` 中使用 `AppendEncodeChecked` 预分配 buffer：

```go
// routePacket 中：
dst := make([]byte, 0, protocol.HeaderLen+len(data)+protocol.ChecksumLen)
packet := protocol.AppendEncodeChecked(dst, protocol.TypeData, payload.Marshal())
```

### 4.2 CRC32 可用硬件加速

`hash/crc32.ChecksumIEEE` 在支持 `sse4.2` 的 x86 CPU 上已自动使用硬件指令（Go runtime 自动检测）。在 ARM 上可能较慢，但对游戏小包影响不大。无需额外优化。

---

## 五、总结（按优先级排序）

| 优先级 | 问题 | 影响 | 改动量 |
|--------|------|------|--------|
| **P0** | `handleHolePunch` 丢弃 IPv6 打洞包 | IPv6 P2P 完全失效 | ~10 行 |
| **P1** | `UnmarshalData` 热路径每包 3 次分配 | 高频游戏包 GC 压力 | ~20 行 |
| **P1** | `sendCtrl` 每次创建 Timer | 高频调用路径开销 | ~15 行 |
| **P2** | `receiveFromTUN` 单 goroutine 瓶颈 | 高吞吐场景延迟 | ~30 行 |
| **P2** | `p2pKeepaliveLoop`/`startHolePunch` 重复构建包 | 小幅内存浪费 | ~10 行 |
| **P2** | `handlePeerInfo` 锁内日志 I/O | 锁竞争 | ~10 行 |
| **P3** | 服务端 `handleRelay` 广播 `addrToRateKey` 重复计算 | 小幅 CPU 浪费 | ~3 行 |
| **P3** | `connectLoop` 退避策略过于激进 | 用户体验 | ~10 行 |
| **P3** | 服务端 Worker Pool 每包分配 | GC 压力 | ~15 行 |

**P0 建议立即修复**，P1 建议下一版本修复，P2/P3 可排入后续迭代。
