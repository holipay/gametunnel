# GameTunnel 断线重连分析与修复记录

> 2026-05-08 分析，基于 commit `bf439b6`。

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

## 二、需要改的三个层面

### 层面 1：服务端存活检测（客户端发现服务器挂了）

| 机制 | 现状 | 问题 |
|------|------|------|
| 客户端 → 服务端 keepalive | 每 10s 发 `TypeKeepAlive` | 只管发，不管服务端是否回应 |
| 服务端 → 客户端 ping | 每 5s 发 `TypePing`，客户端回 `TypePong` | 客户端只 echo，**不用来判断服务端存活** |
| `receiveFromServer` | `ReadFromUDP` 出错时 `continue` | 静默吞掉错误，不做任何判断 |

**缺失**：客户端没有"最后一次收到服务端消息的时间戳"，也没有"连续未收到响应的计数器"。服务端挂掉后，客户端会无限期地往黑洞发包。

### 层面 2：自动重连循环（断线后自动重试）

`cmd/client/main.go` 的 `run()` 函数：

```go
err = t.Connect(ctx, cfg.ServerAddr, *mtuFlag, func(tunCfg client.TunConfig) (client.TunDevice, error) {
    // ... 创建 TUN
})
if err != nil {
    return fmt.Errorf("连接失败: %w", err)  // 直接退出
}
```

`Connect()` 返回 = 程序结束。没有 `for` 循环，没有 backoff，没有重试。

### 层面 3：TUN 设备复用（重连时尽量不重建虚拟网卡）

```go
// tunnel.go Connect()
tunDev, err := newTUN(tunCfg)
defer tunDev.Close()  // Connect 返回就关
```

TUN 设备在 `Connect()` 内创建和销毁。每次重连都要：
1. 销毁旧 wintun 网卡
2. 重新 `tun.CreateTUN("GameTunnel", mtu)`
3. 重新 `netsh` 配置 IP、路由、metric
4. 期间游戏会短暂断开

如果虚拟 IP 没变（服务端分配了相同的 IP），这些操作完全多余。

---

## 三、服务端相关代码

服务端的超时清理逻辑（`server.go` `keepaliveLoop`）：
- 每 15s 扫描一次
- `LastSeen` 超过 45s 的客户端被踢除
- 客户端每 10s 发 keepalive → 有 ~35s 的容错窗口

**服务端无需改动**，超时机制已经合理。问题全在客户端。

---

## 四、已修复的 Bug（commit d24a329, 338bee8）

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

**修复**：加错误计数器 + 100ms backoff，超过 10 次连续错误则退出。

### Bug 2：`Connect()` goroutine 退出后不返回

**原代码**：
```go
go t.receiveFromServer(ctx)
go t.receiveFromTUN(ctx)
go t.keepaliveLoop(ctx)
go t.peerDiscoveryLoop(ctx)

<-ctx.Done()  // 只响应用户取消，goroutine 异常退出时永远阻塞
```

**修复**：用 `runCtx` 派生 context，任一 goroutine 退出时 `runCancel()` 通知所有人。

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

**修复**：`relayBroadcast` 接受原始 `dstIP` 参数并保留。

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

**修复**：写入 TUN 前检查 srcIP 是否为服务端 IP 或已知 peer。

---

## 五、改造方案（待实现）

### 层面 1：服务端存活检测

在 `Tunnel` 结构体上新增字段：

```go
type Tunnel struct {
    lastServerResponse time.Time      // 最后一次收到服务端消息的时间
    serverAlive        atomic.Bool    // 服务端是否存活
}
```

利用服务端每 5s 发送的 `TypePing`：如果连续 30s 没收到任何 Ping/Pong/PeerInfo/Data，判定失联。

新增 `serverWatchdog` goroutine：

```go
func (t *Tunnel) serverWatchdog(ctx context.Context) {
    ticker := time.NewTicker(5 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            if time.Since(t.lastServerResponse) > 30*time.Second {
                t.serverAlive.Store(false)
                return  // 触发外层重连
            }
        }
    }
}
```

### 层面 2：自动重连循环

改造 `cmd/client/main.go` 的 `run()`：

```go
const baseDelay = 2 * time.Second
const maxDelay = 60 * time.Second

for attempt := 0; ; attempt++ {
    if attempt > 0 {
        delay := baseDelay * time.Duration(1<<uint(attempt-1))
        if delay > maxDelay { delay = maxDelay }
        fmt.Printf("⏳ %v 后重连 (第%d次)...\n", delay, attempt)
        select {
        case <-ctx.Done(): return nil
        case <-time.After(delay):
        }
    }

    err = t.Connect(ctx, cfg.ServerAddr, *mtuFlag, tunFactory)
    if ctx.Err() != nil { return nil }  // 用户主动断开
    log.Printf("连接断开: %v", err)
}
```

### 层面 3：TUN 设备复用

将 TUN 生命周期从 `Connect()` 剥离：

```go
type Tunnel struct {
    tunDev     TunDevice
    lastVIP    net.IP  // 上次分配的虚拟 IP
}

// Connect 内部：
if t.tunDev != nil && t.virtualIP.Equal(t.lastVIP) {
    // IP 没变，复用 TUN，零中断
} else {
    // IP 变了或首次连接，创建 TUN
}
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
  → tunDev.Write() → TUN → 星际B 收到广播
```

### 单播包路径（P2P 打洞成功时）

```
星际A → TUN → routePacket()
  │ dst=10.10.0.3, peer.DirectReach == true
  → sendUDP(encoded, peer.PublicAddr)  ← 直连，不经过服务器
  ▼
玩家B 公网地址 → receiveFromServer → handleDataFromServer → TUN → 星际B
```

### 丢包场景

| 场景 | 后果 |
|------|------|
| 客户端→服务器广播丢包 | 下一次广播（~1-2s）会成功，短暂看不到房间 |
| 服务器→客户端广播丢包 | 同上 |
| 游戏数据丢包 | 星际争霸自身有超时，短暂卡顿 |
| 服务器重启 | 客户端无感知（无重连机制），需手动重启 |

---

## 七、改动文件清单

| 文件 | 改动 | Commit |
|------|------|--------|
| `internal/client/recv.go` | 错误计数+backoff；srcIP 验证 | d24a329, 338bee8 |
| `internal/client/tunnel.go` | Connect() 用 runCtx 管理 goroutine 生命周期 | d24a329 |
| `internal/client/route.go` | relayBroadcast 保留原始 dstIP | d24a329 |
| `internal/client/keepalive.go` | 待改：新增 serverWatchdog | — |
| `cmd/client/main.go` | 待改：重连循环 + backoff | — |
| `internal/client/config.go` | 待改：新增重连配置项 | — |

服务端代码不需要改动。
