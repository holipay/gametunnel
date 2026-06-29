# GameTunnel 数据流问题修复方案

## 一、问题总览

| # | 严重度 | 问题 | 影响 |
|---|--------|------|------|
| 1 | 🔴 | FEC recovered packets buffer 泄漏 | 内存持续增长 |
| 2 | 🔴 | 服务器中继 CRC 双重嵌套 | 重连时数据损坏 |
| 3 | 🔴 | `fromServer` 判断竞态窗口 | 重连时误判包类型 |
| 4 | 🔴 | 无密码房间 P2P 缺乏认证 | 数据注入风险 |
| 5 | 🟡 | 广播包绕过带宽限制 | 放大攻击 |
| 6 | 🟡 | FEC+加密顺序无断言保护 | 未来修改易引入 bug |
| 7 | 🟡 | FEC decoder 重连状态不一致 | 内存浪费 |
| 8 | 🟡 | FEC header 附加产生堆分配 | GC 压力 |
| 9 | 🟢 | 控制包受速率限制器约束 | keepalive 被误丢 |
| 10 | 🟢 | encoded 包被多目标共享 | 未来异步化风险 |

---

## 二、修复方案

### Fix #1: FEC recovered packets buffer 泄漏

**文件**: `internal/client/recv.go`

**问题**: `handleDirectData` 和 `handleDataFromServer` 中，FEC `ProcessDataPacket` / `ProcessParityPacket` 返回的 recovered packets 由 `copyBytes` → `PktBufGet` 分配，但写入 TUN 后从未调用 `PktBufPut` 归还。

**修复**:

在 `handleDirectData` 的 FEC recovered packets 循环中，对每个 `pkt` 在使用完后归还：

```go
// handleDirectData — FEC recovery section
if fecDec != nil {
    recovered := fecDec.ProcessDataPacket(groupID, seq, rawData)
    for _, pkt := range recovered {
        if len(pkt) >= 20 {
            out := pkt
            decompressed := false
            if protocol.IsCompressed(dp.Flags) && lz4Dec != nil {
                if d, err := lz4Dec.Decompress(pkt); err == nil {
                    out = d
                    decompressed = true
                }
            }
            if _, werr := dev.Write(out); werr != nil {
                log.Printf("[fec] recovered packet write error: %v", werr)
            }
            if decompressed {
                lz4Dec.PutBuffer(out)
            }
            netutil.PktBufPut(pkt)  // ← 新增：归还 FEC buffer
        }
    }
}
```

同样的修改需要应用到：
- `handleDataFromServer` 的 FEC recovery 循环
- `handleFECPacket` 的 FEC recovery 循环

**涉及文件**:
- `internal/client/recv.go` — 3 处 FEC recovery 循环

---

### Fix #2: 服务器中继 CRC 双重嵌套

**文件**: `internal/server/room_relay.go`

**问题**: 服务器 `handleRelay` 用 `EncodeChecked` 中继数据，但 payload 本身已含客户端 CRC（来自 `buildDataPacket`/`buildEncryptedDataPacket`），导致客户端收到双重 CRC。客户端靠手动剥离修复，但这个修复依赖 `fromServer` 判断正确。

**修复方案**: 服务器中继时使用 `Encode` 而非 `EncodeChecked`，因为：
1. AEAD 已提供完整性保护（加密房间）
2. 未加密房间中 CRC 在原始包中已存在
3. 消除客户端手动剥离 CRC 的脆弱逻辑

```go
// room_relay.go — handleRelay
// 修改前:
encoded := protocol.EncodeChecked(protocol.TypeData, payload)

// 修改后:
encoded := protocol.Encode(protocol.TypeData, payload)
```

同时删除客户端 `receiveFromServer` 中的手动 CRC 剥离代码：

```go
// client/recv.go — receiveFromServer
// 删除这段:
if encrypted && msg.Type == protocol.TypeData && len(msg.Payload) >= protocol.ChecksumLen {
    msg.Payload = msg.Payload[:len(msg.Payload)-protocol.ChecksumLen]
}
```

**注意**: 这是一个协议变更，需要服务器和客户端同步更新。旧客户端连接新服务器时，新服务器不发 CRC，旧客户端的 `DecodeChecked` 会因为没有 CRC 而报错。

**兼容方案**: 新服务器对旧客户端（`clientVersion < 某个版本`）仍使用 `EncodeChecked`，新客户端使用 `DecodeLenient`（已有此逻辑）。具体做法：

```go
// room_relay.go — handleRelay
// 根据客户端版本选择编码方式
if sender.clientVersion >= protocol.MinNoCRCRelayVersion {
    encoded = protocol.Encode(protocol.TypeData, payload)
} else {
    encoded = protocol.EncodeChecked(protocol.TypeData, payload)
}
```

在 `protocol.go` 中新增版本常量：
```go
const MinNoCRCRelayVersion uint16 = 0x0108 // v1.8+: relay 不附加 CRC
```

客户端侧保持 `DecodeLenient` 不变（已支持有/无 CRC 两种格式）。

**涉及文件**:
- `internal/protocol/protocol.go` — 新增版本常量
- `internal/server/room_relay.go` — 修改 `handleRelay`
- `internal/client/recv.go` — 删除手动 CRC 剥离

---

### Fix #3: `fromServer` 判断竞态窗口

**文件**: `internal/client/recv.go`

**问题**: `receiveFromServer` 中 `fromServer` 判断依赖 `t.serverAddr.Load()`，在重连瞬间可能不一致。如果判断失败，服务器中继包会进入 `handleDirectData` 路径，使用错误的 cipher 解密。

**修复方案**: 在 `Connect` 中将 `serverAddr` 作为参数传入 `receiveFromServer`，而不是每次从 `t.serverAddr` 动态读取。当前代码已经把 `conn` 作为参数传入（避免 Connect 替换 conn 的竞态），`serverAddr` 应该同样处理。

```go
// tunnel.go — Connect 中:
startGoroutine("receiveFromServer", func() {
    t.receiveFromServer(runCtx, conn, sAddr)  // ← 传入 snapshot
})

// recv.go — receiveFromServer:
func (t *Tunnel) receiveFromServer(ctx context.Context, conn *net.UDPConn, serverAddr *net.UDPAddr) {
    // ...
    fromServer := from != nil && from.IP.Equal(serverAddr.IP) && from.Port == serverAddr.Port
    // ...
}
```

这样 `fromServer` 的判断使用的是 Connect 时 snapshot 的服务器地址，与 `conn` 的生命周期一致，不会因重连而变化。

**涉及文件**:
- `internal/client/tunnel.go` — 修改 `receiveFromServer` 调用
- `internal/client/recv.go` — 修改 `receiveFromServer` 签名

---

### Fix #4: 无密码房间 P2P 数据包认证

**文件**: `internal/client/recv.go`

**问题**: 无密码房间中 P2P 数据包无加密、无 token 验证，攻击者可伪造注入。

**修复方案**: 在 P2P 路径中也验证 session token（v1.7+ 客户端已在数据包中携带 token）。

```go
// recv.go — handleDirectData
// 在验证 peer 地址之后、解密之前，增加 token 验证:

// Session token 验证 (v1.7+)
if dp.Flags&protocol.DataFlagHasToken != 0 && len(dp.Data) >= 16 {
    // 对于无密码房间，token 在 payload 明文中
    // 对于加密房间，token 在 payload 明文中（flags+token 在加密区域外）
    t.mu.RLock()
    myToken := t.sessionToken
    t.mu.RUnlock()
    // 提取 packet 中的 token（在 data 前 16 字节）
    // 注意：需要重新解析，因为 UnmarshalDataPooled 已经把 token 跳过了
}
```

**更简单的方案**: 在 `UnmarshalDataPooled` 中，当 `DataFlagHasToken` 设置时，将 token 也解析出来存入 `DataPayload` 结构体：

```go
// protocol/messages_data.go
type DataPayload struct {
    SrcIP  net.IP
    DstIP  net.IP
    Flags  byte
    Token  [16]byte // ← 新增：session token
    Data   []byte
}
```

然后在 `handleDirectData` 中验证 `dp.Token` 是否匹配已知 peer 的 token。

**涉及文件**:
- `internal/protocol/messages_data.go` — `DataPayload` 增加 Token 字段
- `internal/client/recv.go` — `handleDirectData` 增加 token 验证

---

### Fix #5: 广播包带宽限制

**文件**: `internal/server/room_relay.go`

**问题**: 广播包完全绕过 `bwLimiter`，可被滥用为放大攻击。

**修复方案**: 对广播包仍然检查发送者的带宽配额（不是对每个接收者检查）。

```go
// room_relay.go — handleRelay
if isBroadcast {
    // 广播包检查发送者带宽（而非每个接收者）
    packetSize := len(encoded)
    if r.bwLimiter != nil && !r.bwLimiter.Allow(from, packetSize) {
        return  // 发送者超出带宽限制，丢弃整个广播
    }
    for _, addr := range targets {
        r.sendCheckedRaw(encoded, addr)
    }
} else {
    // 单播：检查接收者带宽
    for _, addr := range targets {
        if r.bwLimiter == nil || r.bwLimiter.Allow(addr, packetSize) {
            r.sendCheckedRaw(encoded, addr)
        }
    }
}
```

**涉及文件**:
- `internal/server/room_relay.go` — `handleRelay`

---

### Fix #6: FEC header 附加优化（减少堆分配）

**文件**: `internal/client/route.go`

**问题**: 每次 FEC header 附加都 `make([]byte, len(sendData)+5)`，高频场景产生 GC 压力。

**修复方案**: 利用 LZ4 压缩输出的底层数组余量，用 `append` 代替 `make+copy`：

```go
// route.go — routePacket, FEC header 附加部分
// 修改前:
tmp := make([]byte, len(sendData)+5)
copy(tmp, sendData)
copy(tmp[len(sendData):], fecHeader[:])
sendData = tmp

// 修改后: 如果 sendData 有余量，直接 append
if cap(sendData) >= len(sendData)+5 {
    sendData = append(sendData, fecHeader[:]...)
} else {
    // 仅当余量不足时才分配
    tmp := make([]byte, len(sendData)+5)
    copy(tmp, sendData)
    copy(tmp[len(sendData):], fecHeader[:])
    sendData = tmp
}
```

同时修改 LZ4 encoder 的 `Compress` 方法，让返回的 slice 保留一些余量：

```go
// lz4.go — Compress
// 在分配 result 时多预留 5 字节（给 FEC header 用）
result := make([]byte, 2+len(compressed), 2+len(compressed)+5)
```

**涉及文件**:
- `internal/client/route.go` — FEC header 附加
- `internal/netutil/lz4.go` — Compress 预留余量

---

### Fix #7: FEC decoder 重连状态同步

**文件**: `internal/client/tunnel.go`

**问题**: 重连时 decoder 重建但 encoder 的 groupID 持续递增，导致旧 group 缓存浪费内存。

**修复方案**: 重连时同时重置 encoder 的 groupID：

```go
// tunnel.go — Connect
t.fecDecoder.Close()
t.fecDecoder = netutil.NewFECDecoder(0)
t.fecEncoder = netutil.NewFECEncoder(0)  // ← 新增：重置 encoder
```

或者更保守的方案——只在 FEC decoder 中增加快速清理逻辑，在收到第一个新 groupID 时清理所有旧 group：

```go
// fec.go — ProcessDataPacket
func (d *FECDecoder) ProcessDataPacket(groupID uint32, seq byte, data []byte) [][]byte {
    d.mu.Lock()
    defer d.mu.Unlock()
    if d.closed { return nil }

    // 快速清理：如果新 groupID 与已缓存的 group 差距过大，清空旧 group
    if len(d.groups) > 0 {
        for id := range d.groups {
            if groupID > id && groupID-id > 100 {
                releaseGroupBuffers(d.groups[id])
                delete(d.groups, id)
            }
        }
    }
    // ... 原有逻辑
}
```

**涉及文件**:
- `internal/client/tunnel.go` — Connect 中重置 encoder
- `internal/netutil/fec.go` — 可选的快速清理

---

### Fix #8: 控制包绕过速率限制器

**文件**: `internal/client/tunnel.go`

**问题**: `sendUDP` 中控制包和数据包都受 `sendLimiter` 约束，channel 满时控制包被丢弃。

**修复方案**: 新增 `sendCtrlUDP` 方法，绕过速率限制器直接入队 `sendCh`：

```go
// tunnel.go
func (t *Tunnel) sendCtrlUDP(data []byte, addr *net.UDPAddr) {
    // 控制包不经过速率限制器，但仍然受 channel 容量约束
    select {
    case t.sendCh <- sendJob{data: data, addr: addr}:
    default:
        // channel 满时降级到 ctrlCh（有 50ms 阻塞窗口）
        t.sendCtrl(data, addr)
    }
}
```

然后在 keepaliveLoop、peerDiscoveryLoop 等发送控制包的地方使用 `sendCtrlUDP` 替代 `sendCtrl`。

**涉及文件**:
- `internal/client/tunnel.go` — 新增方法 + 修改调用点

---

## 三、过度设计简化建议（可选，非紧急）

以下不是 bug 修复，而是降低复杂度的重构建议，可以在 bug 修复之后逐步推进：

### S1: 删除自实现 LZ4，使用标准库

```go
// 替换 internal/netutil/lz4.go 为:
import "github.com/pierrec/lz4/v4"

type LZ4Encoder struct{ w *lz4.Writer }
type LZ4Decoder struct{ r *lz4.Reader }
```

约 200 行自写代码 → 约 30 行封装代码。消除压缩/解压的边界条件 bug 风险。

### S2: 简化加密模式

当前有 3 种 cipher（encCipher、decCipher、p2pCipher）+ ECDH + HKDF + HMAC 认证。

建议简化为：
- 无密码 → 明文 + CRC32
- 有密码 → AES-256-GCM（单密钥，方向 tag 嵌入 AAD）
- 删除 ECDH 前向保密（游戏隧道不需要）

### S3: FEC 可配置化

将 FEC 作为可选功能（配置文件开关），默认关闭。保留代码但不默认启用，减少默认路径的复杂度。

### S4: 协议版本化清理

删除 `isNewFormat` 启发式检测，改用明确的版本号字段。增加协议版本协商，新旧版本不混用。

---

## 四、修复优先级与实施顺序

```
Phase 1 — 数据完整性（必须）:
  Fix #1  FEC buffer 泄漏          — 纯增量修改，无兼容性风险
  Fix #2  CRC 双重嵌套             — 需要版本协调
  Fix #3  fromServer 竞态           — 纯增量修改

Phase 2 — 安全加固（建议）:
  Fix #4  P2P token 认证            — 需要协议扩展
  Fix #5  广播带宽限制              — 服务器侧修改

Phase 3 — 性能优化（可选）:
  Fix #6  FEC 堆分配优化            — 客户端侧优化
  Fix #7  FEC 状态同步              — 客户端侧优化
  Fix #8  控制包速率限制            — 客户端侧优化

Phase 4 — 简化重构（长期）:
  S1-S4  过度设计清理              — 大规模重构
```

---

## 五、测试建议

每个修复需要的验证：

| Fix | 验证方式 |
|-----|----------|
| #1  | 发送 10000 个包，丢包率 10%，监控内存是否稳定 |
| #2  | 加密房间重连后发送 1000 个包，校验 TUN 收到的数据与发送端一致 |
| #3  | 快速重连 10 次，检查是否有"解密失败"日志 |
| #4  | 无密码房间中用 scapy 伪造 P2P 包，验证被拒绝 |
| #5  | 持续发送广播目标包，检查服务器是否按带宽限制丢弃 |
| #6  | 对比修改前后的 allocs/op（go test -bench） |
| #7  | 重连后检查 FEC decoder 的 groups map 是否为空 |
| #8  | 填满 sendCh 后发送 keepalive，验证不被丢弃 |
