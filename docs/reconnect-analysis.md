# GameTunnel 断线重连分析与修复记录

> 2026-05-08 分析，基于 commit `bf439b6`。
> 修复陆续在 `d24a329`, `338bee8`, `26bff2c`, `c77f37b` 中提交。

## 一、现状：断线即退出

客户端的连接流程是**一次性**的：

```
cmd/client/main.go → run() → t.Connect(ctx, ...) → 阻塞等待 ctx.Done() → 程序退出
```

`Connect()` 内部（`tunnel.go`）：
1. `register()` — 一次性注册，3 次重试，失败直接返回 error
2. 创建 TUN 设备（`defer tunDev.Close()` — Connect 返回就销毁）
3. 启动 4 个 goroutine：`receiveFromServer`、`receiveFromTUN`、`keepaliveLoop`、`peerDiscoveryLoop`
4. `<-ctx.Done()` — 阻塞直到用户 Ctrl+C 或 context 取消

**没有任何一个环节**检测到"服务器已失联"并触发重连。断线后用户看到的是程序直接退出。

---

## 二、三个层面的问题与修复

### 层面 1：服务端存活检测 ✅ 已修复

**原始问题**：

| 机制 | 现状 | 问题 |
|------|------|------|
| 客户端 → 服务端 keepalive | 每 10s 发 `TypeKeepAlive` | 只管发，不管服务端是否回应 |
| 服务端 → 客户端 ping | 每 5s 发 `TypePing`，客户端回 `TypePong` | 客户端只 echo，**不用来判断服务端存活** |
| `receiveFromServer` | `ReadFromUDP` 出错时 `continue` | 静默吞掉错误，不做任何判断 |

**缺失**：客户端没有"最后一次收到服务端消息的时间戳"，也没有"连续未收到响应的计数器"。服务端挂掉后，客户端会无限期地往黑洞发包。

**修复方案**（commit `d24a329`）：

通过**错误计数器 + backoff** 间接实现存活检测。当 UDP conn 上的 `ReadFromUDP` 持续报错时（服务端挂了、网络断了、conn 被关闭），`receiveFromServer` goroutine 在 10 次连续失败后退出，触发 `Connect()` 返回，进入重连循环。

```go
// recv.go — receiveFromServer / receiveFromTUN
const maxConsecutiveErrors = 10
const errorBackoff = 100 * time.Millisecond

// 连续错误超过阈值 → goroutine 退出 → Connect() 返回 → 重连
consecutiveErrors++
if consecutiveErrors > maxConsecutiveErrors {
    log.Printf("[tunnel] 服务端连接读取连续失败 %d 次，退出: %v", consecutiveErrors, err)
    return
}
time.Sleep(errorBackoff)
```

**检测延迟**：最坏情况 ~1 秒（10 次 × 100ms）。对于游戏场景足够快。

**未来可增强**：主动追踪 `lastServerResponse` 时间戳 + `serverWatchdog` goroutine，实现更精确的存活判定（见第八节）。

### 层面 2：自动重连循环 ✅ 已修复

**原始问题**：

`cmd/client/main.go` 的 `run()` 函数调用 `Connect()` 一次，返回即退出。没有 `for` 循环，没有 backoff，没有重试。

**修复方案**（commit `26bff2c`）：

`run()` 中用 `for` 循环包裹 `Connect()`，指数退避重试：

```go
// cmd/client/main.go
const baseDelay = 2 * time.Second
const maxDelay  = 60 * time.Second

for attempt := 0; ; attempt++ {
    if attempt > 0 {
        delay := baseDelay << (attempt - 1)  // 2s, 4s, 8s, 16s, 32s, 60s...
        if delay > maxDelay { delay = maxDelay }
        fmt.Printf("⏳ %v 后重连 (第%d次)...\n", delay, attempt)
        select {
        case <-ctx.Done(): return nil      // 用户 Ctrl+C
        case <-time.After(delay):
        }
    }

    err = t.Connect(ctx, cfg.ServerAddr, *mtuFlag, tunFactory)
    if ctx.Err() != nil { return nil }     // 用户主动断开
    log.Printf("连接断开: %v", err)
    // 循环继续 → backoff → 重连
}
```

**退避序列**：`0s → 2s → 4s → 8s → 16s → 32s → 60s → 60s → ...`（无限重试）

### 层面 3：TUN 设备复用 ✅ 已修复

**原始问题**：

TUN 设备在 `Connect()` 内创建和销毁（`defer tunDev.Close()`）。每次重连都要：
1. 销毁旧 wintun 网卡
2. 重新 `tun.CreateTUN("GameTunnel", mtu)`
3. 重新 `netsh` 配置 IP、路由、metric
4. 期间游戏会短暂断开

**修复方案**（commit `26bff2c`）：

TUN 生命周期从 `Connect()` 剥离，跨重连持久化：

```go
// tunnel.go — Tunnel 结构体新增字段
type Tunnel struct {
    // ...
    lastAssignedIP net.IP                                // 上次分配的虚拟 IP
    newTUNFunc     func(TunConfig) (TunDevice, error)    // 缓存的工厂函数
}
```

`Connect()` 内部的 TUN 决策逻辑：

```go
ipChanged := t.lastAssignedIP != nil && !t.virtualIP.Equal(t.lastAssignedIP)
tunAlive := t.tunDev != nil

switch {
case tunAlive && !ipChanged:
    // ✅ 最佳：TUN 存活 + IP 未变 → 直接复用，零中断
    log.Printf("[tunnel] 复用 TUN 设备 (IP %s 未变)", t.virtualIP)

case tunAlive && ipChanged:
    // ⚠️ IP 变了 → 关闭旧 TUN，重建
    log.Printf("[tunnel] IP 变更 %s → %s，重建 TUN 设备", t.lastAssignedIP, t.virtualIP)
    t.tunDev.Close()
    t.createTUN(mtu)

case !tunAlive:
    // 首次连接或 TUN 已丢失 → 创建新的
    t.createTUN(mtu)
}
```

新增 `CloseTUN()` 方法，仅在程序最终退出时调用（不在每次重连时调用）。

---

## 三、服务端相关代码

服务端的超时清理逻辑（`server.go` `keepaliveLoop`）：
- 每 15s 扫描一次
- `LastSeen` 超过 45s 的客户端被踢除
- 客户端每 10s 发 keepalive → 有 ~35s 的容错窗口

**服务端无需改动**，超时机制已经合理。问题全在客户端。

---

## 四、已修复的独立 Bug

### Bug 1：`receiveFromTUN` / `receiveFromServer` 错误死循环（CPU 100%）

**原代码**：
```go
n, err := t.tunDev.Read(buf)
if err != nil {
    select {
    case <-ctx.Done():
        return
    default:
        continue   // ← TUN 持续报错时，这里会空转烧 CPU
    }
}
```

**场景**：wintun 驱动异常、网卡被系统禁用、UDP conn 被关闭（重连时）。
**后果**：CPU 占用飙升到 100%，游戏卡死，但程序不退出。

**修复**（commit `d24a329`）：加错误计数器 + 100ms backoff，超过 10 次连续错误则退出。

### Bug 2：`Connect()` goroutine 退出后不返回

**原代码**：
```go
go t.receiveFromServer(ctx)
go t.receiveFromTUN(ctx)
go t.keepaliveLoop(ctx)
go t.peerDiscoveryLoop(ctx)

<-ctx.Done()  // 只响应用户取消，goroutine 异常退出时永远阻塞
```

**修复**（commit `d24a329`）：用 `runCtx` 派生 context，任一 goroutine 退出时 `runCancel()` 通知所有人。

### Bug 3：广播包 dstIP 被统一改写为 255.255.255.255

**原代码**：
```go
func (t *Tunnel) relayBroadcast(pkt []byte, srcIP net.IP) {
    dp := &protocol.DataPayload{
        SrcIP: srcIP,
        DstIP: net.IPv4(255, 255, 255, 255),  // ← 原始 dstIP 被丢弃
        Data:  pkt,
    }
```

原始包的 dstIP 可能是 `10.10.0.255`（子网广播）或 `224.0.0.251`（mDNS），被统一写死成 `255.255.255.255`。某些游戏可能因为 dstIP 不匹配而丢弃包。

**修复**（commit `d24a329`）：`relayBroadcast` 接受原始 `dstIP` 参数并保留。

### Bug 4：`handleDataFromServer` P2P 路径无 srcIP 验证

**原代码**：
```go
func (t *Tunnel) handleDataFromServer(payload []byte) {
    dp, err := protocol.UnmarshalData(payload)
    if len(dp.Data) > 0 && t.tunDev != nil {
        t.tunDev.Write(dp.Data)  // ← 直接写入 TUN，不验证 srcIP
    }
}
```

服务端中转时有 `srcIP == VirtualIP` 验证，但 P2P 直连路径绕过了服务器，任何人可以伪造 srcIP 注入恶意包。

**修复**（commit `338bee8`）：写入 TUN 前检查 srcIP 是否为服务端 IP 或已知 peer。未知 srcIP 静默丢弃。

---

## 五、重连流程图

```
用户启动客户端
  │
  ▼
Connect() ──→ register() ──→ 成功 ──→ TUN 决策 ──→ 启动 goroutine
  │                                      │
  │           ┌──────────────────────────┘
  │           │
  │     ┌─────┴─────┐
  │     │ IP 未变？  │
  │     └─────┬─────┘
  │       是  │  否
  │    复用TUN │  重建TUN
  │           │
  ▼           ▼
  runCtx 阻塞运行
  │
  │  ┌─────────────────────────────────────────┐
  │  │ goroutine 异常退出（TUN 错误/UDP 错误）   │
  │  │ → runCancel() → Connect() 返回           │
  │  └─────────────────────────────────────────┘
  │
  ▼
重连循环检测到 Connect() 返回
  │
  ├─ ctx.Err() != nil → 用户 Ctrl+C → CloseTUN() → 退出
  │
  └─ err != nil → 服务端断连
       │
       ▼
     backoff 等待 (2s → 4s → 8s → ... → 60s)
       │
       ▼
     重新 Connect()
       │
       ├─ register() 成功
       │    ├─ IP 未变 → 复用 TUN → 游戏无感知 ✅
       │    └─ IP 变了 → 重建 TUN → 游戏短暂中断 ⚠️
       │
       └─ register() 失败 → 继续 backoff 重试
```

---

## 六、星际争霸1 数据流分析

### 通信模式

| 阶段 | 目标地址 | 端口 | 说明 |
|------|---------|------|------|
| 游戏发现 | `255.255.255.255` | 6112 UDP | 主机广播"我建了房间" |
| 游戏发现 | `224.0.0.251` | 5353 UDP | mDNS 发现 |
| 游戏数据 | 主机 IP | 6112 UDP | 加入后单播状态同步 |

### TUN 路由配置

```
目标网络              子网掩码           网关         接口 Metric
0.0.0.0             0.0.0.0          192.168.1.1   以太网  100
10.10.0.0           255.255.255.0    10.10.0.2     TUN     1
10.10.0.255         255.255.255.255  10.10.0.2     TUN     1
224.0.0.251         255.255.255.255  10.10.0.2     TUN     1
255.255.255.255     255.255.255.255  10.10.0.2     TUN     1
```

所有广播/多播/虚拟子网流量被 Metric=1 吸入 TUN。

### 广播包完整路径

```
星际A (10.10.0.2)
  │ UDP broadcast: src=10.10.0.2:6112, dst=255.255.255.255:6112
  ▼
Windows 路由决策 → 255.255.255.255 metric=1 → TUN
  ▼
TUN 设备 → tunDev.Read()
  ▼
receiveFromTUN() → routePacket()
  │ IsRelayTarget(255.255.255.255) == true
  ▼
relayBroadcast() → DataPayload{src=10.10.0.2, dst=255.255.255.255}
  → sendUDP → 公网 → 服务器
  ▼
服务器 handleRelay()
  │ IsRelayTarget == true → 转发给同房间所有人
  │ srcIP == sender.VirtualIP ✓ (反伪造)
  ▼
玩家B receiveFromServer() → handleDataFromServer()
  → srcIP 验证 ✓ → tunDev.Write() → TUN → 星际B 收到广播
```

### 单播包路径（P2P 打洞成功时）

```
星际A → TUN → routePacket()
  │ dst=10.10.0.3, peer.DirectReach == true
  → sendUDP(encoded, peer.PublicAddr)  ← 直连，不经过服务器
  ▼
玩家B 公网地址 → receiveFromServer → handleDataFromServer
  → srcIP 验证（known peer）✓ → TUN → 星际B
```

### 丢包与断线场景

| 场景 | 后果 | 修复后 |
|------|------|--------|
| 客户端→服务器广播丢包 | 下一次广播（~1-2s）会成功 | 无变化，UDP 本就允许丢包 |
| 服务器→客户端广播丢包 | 同上 | 无变化 |
| 游戏数据丢包 | 星际争霸自身有超时，短暂卡顿 | 无变化 |
| 服务器重启 | 客户端无感知，需手动重启 | **自动重连**，backoff 后恢复 |
| 网络短暂中断 | 客户端退出 | **自动重连**，TUN 复用时游戏无感 |
| TUN 设备异常 | CPU 100% 死循环 | **goroutine 退出 → 重连** |

---

## 七、改动文件清单

| 文件 | 改动 | Commit |
|------|------|--------|
| `internal/client/recv.go` | 错误计数+backoff（防 CPU 空转）；srcIP 验证（防 P2P 注入） | d24a329, 338bee8 |
| `internal/client/tunnel.go` | goroutine 生命周期管理（runCtx）；TUN 设备复用 | d24a329, 26bff2c |
| `internal/client/route.go` | relayBroadcast 保留原始 dstIP | d24a329 |
| `cmd/client/main.go` | 重连循环 + 指数 backoff；TUN 工厂缓存 | 26bff2c |
| `docs/reconnect-analysis.md` | 本分析文档 | 4370d51, c77f37b |

服务端代码不需要改动。

---

## 八、未来可增强

### 8.1 主动存活检测（serverWatchdog）

当前的存活检测是**被动**的：依赖 `ReadFromUDP` 连续报错来触发。如果网络没有断开但服务端停止响应（如服务端进程假死），UDP read 不会报错，只是收不到数据。

**增强方案**：追踪 `lastServerResponse` 时间戳 + 独立 watchdog：

```go
type Tunnel struct {
    lastServerResponse time.Time
}

func (t *Tunnel) serverWatchdog(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if time.Since(t.lastServerResponse) > 30*time.Second {
                log.Printf("[tunnel] 服务器无响应超时")
                return  // 触发重连
            }
        }
    }
}
```

需要在 `receiveFromServer` 中每次成功读取消息时更新 `t.lastServerResponse = time.Now()`。

### 8.2 重连配置项

在 `config.ini` 中暴露重连参数：

```ini
# 重连基础延迟（秒）
reconnect_delay=2
# 最大重连延迟（秒）
reconnect_max_delay=60
# 无限重连（true）还是有限次（false）
reconnect_infinite=true
```

### 8.3 重连时请求原 IP

注册时告知服务端"我上次的 IP 是 X"，服务端优先分配相同的 IP。这样即使 IP 被释放后还没被别人抢走，也能保证 TUN 复用。
