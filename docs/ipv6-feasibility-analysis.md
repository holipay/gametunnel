# GameTunnel IPv6 支持可行性分析

> 2026-05-10 分析。

## 一、背景

GameTunnel 当前仅支持 IPv4，客户端和服务端之间的 UDP 隧道、虚拟子网分配、协议序列化均硬编码 IPv4（4 字节地址）。本文档评估添加 IPv6 支持的可行性、改动范围和推荐方案。

## 二、IPv4 硬编码分布

### 2.1 数据结构层

| 文件 | 硬编码形式 | 说明 |
|------|-----------|------|
| `internal/server/server.go` | `clients map[[4]byte]*Client` | 服务端客户端表，key 是 4 字节 IPv4 |
| `internal/server/server.go` | `ipBitmap []uint64`（256 位） | `/24` 子网的 IP 分配 bitmap |
| `internal/server/ratelimit.go` | `rateKey struct { IP [4]byte; Port uint16 }` | 速率限制的地址 key |
| `internal/client/tunnel.go` | `peers map[[4]byte]*Peer` | 客户端 peer 表 |
| `internal/client/tunnel.go` | `serverIP4 [4]byte` | 缓存的服务器 IP（快速比较） |
| `internal/client/route.go` | `ip4Key(dstIP)` 路由查找 | 路由决策依赖 4 字节 key |

### 2.2 网络连接层

| 文件 | 代码 | 问题 |
|------|------|------|
| `internal/server/server.go` | `net.ResolveUDPAddr("udp4", ...)` | 强制 IPv4 |
| `internal/server/server.go` | `net.ListenUDP("udp4", ...)` | 强制 IPv4 |
| `internal/client/tunnel.go` | `net.ListenUDP("udp4", ...)` | 强制 IPv4 |
| `internal/client/register.go` | `net.ResolveUDPAddr("udp4", acp.ClientAddr)` | 认证时地址解析 |

### 2.3 IP 分配算法

`server.go` 中的 `nextAvailableIP()` 为 `/24` 子网专门优化：

```go
func (s *Server) nextAvailableIP() net.IP {
    base := s.subnet.IP.To4()
    for i, word := range s.ipBitmap {
        if word != ^uint64(0) {
            bit := bits.TrailingZeros64(^word)
            octet := i*64 + bit
            if octet >= 2 && octet < 255 {
                return net.IPv4(base[0], base[1], base[2], byte(octet))
            }
        }
    }
    return nil
}
```

限制条件：
- 固定 4 个 `uint64`（256 位），仅覆盖 `/24`
- 启动时强制校验 `bits != 32 || ones != 24`
- IPv6 子网（通常 `/64` = 2^64 地址）完全无法使用

### 2.4 协议序列化（`gametunnel-protocol` 外部库）

线上格式全部硬编码 4 字节 IP：

| 结构体 | 线格式 | IPv4 字节数 |
|--------|--------|------------|
| `AssignIPPayload` | `VirtualIP(4B) + SubnetMask(4B) + ServerIP(4B)` | 固定 12 字节 |
| `DataPayload` | `SrcIP(4B) + DstIP(4B) + Data` | 固定 8 字节头 |
| `PeerInfoEntry` | `VirtualIP(4B)` + 变长 | 前 4 字节固定 |
| `HolePunch` payload | 前 4 字节为虚拟 IP | 固定 4 字节 |
| `broadcast.go` | `IsBroadcast`、`IsMulticast` 调用 `dst.To4()` | IPv6 直接返回 false |

**影响**：线上格式必须变更，协议版本号需 bump（v1 → v2）。`gametunnel` 和 `gametunnel-protocol` 两个仓库需联动修改。

### 2.5 TUN 配置（平台相关）

| 平台 | 当前 IPv4 命令 | IPv6 对应命令 |
|------|---------------|--------------|
| Windows | `netsh interface ip set address ... static 10.10.0.2 255.255.255.0` | `netsh interface ipv6 add address ... fd00::2/64` |
| Windows | `route add 10.10.0.0/24 mask 255.255.255.0 ...` | `route add fd00::/64 fd00::2` |
| Linux | `ip addr add 10.10.0.2/24 dev tun0` | `ip addr add fd00::2/64 dev tun0` |
| macOS | `ifconfig utunX 10.10.0.2 10.10.0.1 netmask 255.255.255.0` | `ifconfig utunX inet6 fd00::2/64` |

Windows 的 IPv6 路由命令与 IPv4 完全不同，是最复杂的部分。

### 2.6 广播与组播

当前机制：

| 类型 | 地址 | 用途 |
|------|------|------|
| 全局广播 | `255.255.255.255` | 游戏房间发现 |
| 子网广播 | `10.10.0.255` | 子网内广播 |
| mDNS 组播 | `224.0.0.251` | macOS/部分游戏发现 |

IPv6 没有广播，只有组播：

| IPv6 组播 | 对应用途 |
|-----------|---------|
| `ff02::1` | 所有节点（替代全局广播） |
| `ff02::fb` | mDNS IPv6 组播 |
| `ff02::1:ffXX:XXXX` | solicited-node 组播（邻居发现） |

`protocol.IsRelayTarget()` 需要重写以支持 IPv6 组播地址判断。

### 2.7 NAT 打洞

| 环节 | IPv4 当前实现 | IPv6 变化 |
|------|--------------|-----------|
| HolePunch payload | 4 字节虚拟 IP | 需扩展为 16 字节 |
| `handleHolePunch` | `srcIP4 := from.IP.To4()` → nil 直接丢弃 | 需支持 IPv6 地址 |
| 打洞策略 | 穿越 NAT | IPv6 通常无 NAT，但需穿越防火墙 pinhole |

## 三、推荐方案

### 方案 A：仅隧道传输层支持 IPv6（推荐）

**范围**：客户端和服务端之间的 UDP 连接支持 IPv6 传输，虚拟子网仍用 IPv4。

**改动量**：8-10 个文件，1-2 天。

**改动清单**：

| 文件 | 改动 |
|------|------|
| `internal/server/server.go` | `"udp4"` → `"udp"`，`rateKey` 扩展为 `[16]byte` |
| `internal/client/tunnel.go` | `"udp4"` → `"udp"`，peer map key 改为 `[16]byte` |
| `internal/server/ratelimit.go` | `rateKey.IP` 从 `[4]byte` 改为 `[16]byte` |
| `internal/client/register.go` | `ResolveUDPAddr` 改为自动检测 |
| `internal/server/register.go` | 无需改动（使用 `*net.UDPAddr`，已兼容） |

**不改动**：
- 协议序列化（虚拟 IP 仍为 4 字节）
- TUN 配置（虚拟子网仍为 IPv4）
- IP 分配算法（仍为 `/24` bitmap）
- 广播/组播逻辑（仍为 IPv4 广播）

**效果**：服务端可以部署在纯 IPv6 VPS 上，客户端可以通过 IPv6 连接到服务端。虚拟局域网内部仍为 IPv4，游戏无感。

### 方案 B：双栈传输 + IPv6 虚拟子网

**范围**：全面改造，支持 IPv6 虚拟 IP。

**改动量**：20+ 文件，3-5 天。涉及协议库重写、IP 分配算法重写、三平台 TUN 配置重写。

**不推荐原因**：绝大多数局域网游戏不支持 IPv6，IPv6 虚拟子网实际用途有限。

### 方案 C：双栈传输 + 同时支持两种虚拟子网

**范围**：最大改动，支持 IPv4/IPv6 虚拟子网并存。

**改动量**：30+ 文件，1-2 周。

**不推荐原因**：复杂度过高，收益不明确。

## 四、IPv6 对游戏发现机制的影响

| 发现方式 | IPv4 | IPv6 | 影响 |
|----------|------|------|------|
| UDP 广播 | `255.255.255.255` | 无等价物 | 需改为组播 `ff02::1` |
| 子网广播 | `x.x.x.255` | 无等价物 | 同上 |
| mDNS | `224.0.0.251` | `ff02::fb` | 需添加 IPv6 组播路由 |
| TCP 直连 | 支持 | 支持 | 无影响 |

**结论**：如果虚拟子网使用 IPv6，所有依赖广播的局域网游戏将无法发现房间。这是不推荐方案 B/C 的核心原因。

## 五、结论

| 维度 | 评估 |
|------|------|
| 技术可行性 | ✅ 完全可行 |
| 方案 A 复杂度 | 🟢 低（1-2 天） |
| 方案 B 复杂度 | 🟡 中高（3-5 天） |
| 方案 C 复杂度 | 🔴 高（1-2 周） |
| 推荐方案 | A（仅传输层 IPv6） |
| 核心约束 | 虚拟子网必须保持 IPv4，否则游戏广播发现失效 |
