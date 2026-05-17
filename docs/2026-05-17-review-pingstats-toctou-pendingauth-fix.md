```markdown
# GameTunnel Code Review & Bugfix Report
## ringbuf · toctou · auth-underflow · timer-leak · ip-validation · atomic-cipher

---

## 修复总览

| 优先级 | 类别 | 数量 |
|--------|------|------|
| 🔴 确认 Bug | 缺陷 | 6 |
| 🟡 潜在风险 | 竞态/泄漏 | 6 |
| 🟢 优化建议 | 性能/可维护性 | 6 |

**涉及文件**: 7 files, +54 -20

---

## 🔴 确认 Bug

### 1. PingStats 环形缓冲区遍历错误

**文件**: `internal/server/server.go:62-68`

```go
func (c *Client) PingStats() (lossRate float64, jitter time.Duration) {
    total := c.pingIdx
    if total == 0 {
        return 0, 0
    }
    n := total
    if n > pingHistorySize {
        n = pingHistorySize
    }
    for i := 0; i < n; i++ {
        rtt := c.pingHistory[i]  // ← 错误：从 0 开始遍历
```

`pingIdx` 是递增计数器，`pingHistory` 是大小 12 的环形缓冲区。当 `pingIdx > 12` 时，从 `i=0` 遍历读到的是不连续的旧数据。

**修复**: 从 `pingIdx % pingHistorySize` 开始按时间顺序遍历。

**影响**: 状态页面的 loss rate 和 jitter 在长时间运行后完全错误。

---

### 2. getEncodedPeerInfo TOCTOU 竞态

**文件**: `internal/server/peer.go:88-107`

```go
func (s *Server) getEncodedPeerInfo() []byte {
    now := time.Now()
    s.peerInfoMu.Lock()
    if s.peerInfoEncoded != nil && now.Sub(s.peerInfoCachedAt) < peerInfoCacheTTL {
        encoded := s.peerInfoEncoded
        s.peerInfoMu.Unlock()
        return encoded
    }
    s.peerInfoMu.Unlock()  // ← 释放锁后无保护

    // Cache miss — rebuild（多个 goroutine 可能同时执行）
    s.mu.RLock()
    // ...
    s.mu.RUnlock()

    s.peerInfoMu.Lock()
    s.peerInfoEncoded = encoded
    s.peerInfoCachedAt = now
    s.peerInfoMu.Unlock()
    return encoded
}
```

Cache miss 路径上释放 `peerInfoMu` 后再 rebuild，多个 goroutine 可能同时序列化相同的 PeerInfo。

**修复**: 在同一个 `peerInfoMu` 锁内完成 cache miss 的 rebuild。

**影响**: 浪费 CPU，高并发时重复编码。

---

### 3. pendingAuth 下溢保护

**文件**: `internal/server/register.go` + `peer.go` (5 处)

```go
// 旧代码
if c.auth == authChallengeSent {
    s.pendingAuth--
}
```

如果异常导致 `pendingAuth` 减到负数，后续 `>= maxPending` 检查永久失效，auth flood 保护静默退化。

**修复**: 所有 `s.pendingAuth--` 添加下限保护。

```go
if s.pendingAuth > 0 {
    s.pendingAuth--
}
```

**影响**: 下溢后攻击者可无限发送 auth 请求。

---

### 4. handleRelay 广播回传风险

**文件**: `internal/server/relay.go:30-38`

已通过 `addrToRateKey` 排除发送者，但 `sender` 查找依赖 IP+Port 作为 key。当前架构下每 IP+Port 只有一个 client，实际安全，但代码缺少注释说明此约束。

---

### 5. 客户端不验证分配的 IP

**文件**: `internal/client/register.go:49-58`

客户端信任服务端返回的任意 IP，未验证 `VirtualIP` 是否属于 `SubnetMask` 定义的子网。

**修复**: 添加 `VirtualIP` 和 `ServerIP` 的子网归属验证，验证失败时 log error 并断开连接。

---

### 6. 密码明文存储

**文件**: `internal/client/config.go:100`

```go
fmt.Fprintf(&b, "password=%s\n", cfg.RoomPassword)
```

房间密码以明文写入配置文件。HMAC 协议本身不传输密码，但本地明文存储有风险。

---

## 🟡 潜在风险

### 7. rateLimitLoop 竞态风险

**文件**: `internal/server/ratelimit.go:48-52`

Swap 后在无锁状态下清除旧 buffer。当前无其他读取者，实际安全，但设计脆弱。

### 8. sendCtrl time.After 内存泄漏

**文件**: `internal/client/tunnel.go:219`

```go
case <-time.After(50 * time.Millisecond):
```

每次调用创建定时器，到期前不可 GC。hole punch burst 场景下大量定时器堆积。

**修复**: 改用 `time.NewTimer` + `defer timer.Stop()`。

### 9. handleRelay 零拷贝切片引用

**文件**: `internal/server/relay.go:15-16`

`srcIP`/`dstIP` 是对 `payload` 的零拷贝引用。由于 `pkt` 每次 ReadFromUDP 后新分配，实际安全，但值得加注释。

### 10. nextAvailableIP 自解释性

**文件**: `internal/server/server.go:193`

```go
if octet >= 2 && octet < 255 {
```

octet=0/1/255 在 `New()` 中已标记 used，但 `>= 2` 检查的含义不够自解释。

### 11. 状态页面模板缓存无并发保护

**文件**: `internal/server/status.go:130-136`

`getStatusTmpl` 在 HTTP handler 中并发调用。当前只读场景安全，但 `lang` 动态变化时存在竞态。

### 12. IPv4 total length 未验证

**文件**: `internal/client/recv.go:155-162`

缺少对 IP total length 字段（`buf[2:4]`）与实际读取长度的一致性检查。

**修复**: 添加 total length 验证。

---

## 🟢 优化建议

### 13. handleRelay 编码优化 ✅

已在锁外只编码一次，发送给所有 target。做得不错。

### 14. PeerInfoPayload.Marshal() 重复 String()

**文件**: `internal/protocol/messages.go:88`

`peer.PublicAddr.String()` 在计算大小和写入时各调用一次，可在循环中缓存。

### 15. Cipher 锁优化

**文件**: `internal/crypto/crypto.go:54`

```go
// 旧代码
type Cipher struct {
    aead     cipher.AEAD
    mu       sync.Mutex
    counter  uint64
    dirTag   []byte
}
```

`counter` 递增改为 `atomic.Uint64.Add(1)`，减少加密路径锁竞争。需确认 `dirTag` 在构造后不变。

### 16. sendLoop drain 改进

**文件**: `internal/client/tunnel.go:173-185`

ctx 取消后 `default` 立即返回，剩余包被丢弃。

**修复**: 改为 200ms 超时 drain，确保 disconnect 包能发出。注意 `writeUDP` 在 UDP 场景下实际不会阻塞。

### 17. peerInfoCacheTTL

**文件**: `internal/server/peer.go:86`

TTL = interval (50ms)，刚好覆盖 tick 间隔。可考虑设为 `interval * 2` 提高命中率。

---

## 修复清单

| # | 优先级 | 修复内容 | 文件 |
|---|--------|---------|------|
| 1 | 🔴 | PingStats 环形缓冲区遍历起始位置 | `server.go` |
| 2 | 🔴 | getEncodedPeerInfo 锁内 rebuild | `peer.go` |
| 3 | 🔴 | pendingAuth 下溢保护 (5 处) | `register.go` + `peer.go` |
| 4 | 🟡 | sendCtrl 改用 NewTimer | `tunnel.go` |
| 5 | 🟡 | 客户端 IP 子网验证 | `register.go` |
| 6 | 🟢 | Cipher counter 改 atomic | `crypto.go` |
| 7 | 🟢 | IPv4 total length 验证 + drain 超时 | `recv.go` + `tunnel.go` |
```
