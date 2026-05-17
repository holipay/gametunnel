# IPv6 传输层支持 — 实施记录

> 2026-05-17，基于 commit `91bfd49`（实施前）→ `a0a35f7`（最终）。
> 基于 `docs/ipv6-feasibility-analysis.md` 中的方案 A。

## 一、目标

仅改变「客户端 ↔ 服务端」之间的 UDP 传输层，使其支持 IPv6 地址。以下内容**完全不变**：

- 虚拟局域网（`10.10.0.x/24`）
- 协议序列化（虚拟 IP 仍 4 字节）
- IP 分配算法（仍为 `/24` bitmap）
- 广播转发逻辑（仍为 IPv4 广播）

## 二、设计思考

### 2.1 核心思路：`[4]byte` → `[16]byte` + `To16()` 映射

整个改动的核心技巧是：用 `[16]byte` 作为 map key 替代原来的 `[4]byte`，配合 Go 标准库的 `net.IP.To16()` 方法。

`To16()` 对 IPv4 地址返回 16 字节的 v4-in-v6 映射格式（`::ffff:a.b.c.d`）：

```go
net.IPv4(192, 168, 1, 1).To16()
// → [0 0 0 0 0 0 0 0 0 0 0xff 0xff 192 168 1 1]  (16 bytes)
```

这意味着：
- 现有 IPv4 地址映射后仍可正确匹配（同一个 IP 总是产生相同的 16 字节 key）
- 新增的原生 IPv6 地址也能正确处理
- 不需要任何条件分支或特殊处理

### 2.2 双栈监听：`"udp4"` → `"udp"`

Go 的 `net.ListenUDP("udp", ":4700")` 在 Linux 上默认监听 `[::]:4700`，内核双栈模式下同时接受 IPv4 和 IPv6 连接。这由 `net.ipv6.bindv6only` 内核参数控制，大多数发行版默认为 0（双栈）。

**潜在风险**：OpenWrt 某些版本可能设置 `bindv6only=1`，导致只监听 IPv6。如果需要保险，可以显式确认内核参数，或分两个 socket。但对主流 Linux/Windows 环境，`"udp"` 是正确且简洁的做法。

### 2.3 P2P 检测的兼容性

`receiveFromServer` 中通过比较 `from.IP` 和 `t.serverAddr.IP` 来区分「服务端中转」和「P2P 直连」：

```go
if from != nil && t.serverAddr != nil && !from.IP.Equal(t.serverAddr.IP) {
    // Direct P2P
}
```

`net.IP.Equal()` 天然支持 IPv4/IPv6 比较，无需额外改动。但需注意：如果服务端监听 `0.0.0.0`，`t.serverAddr.IP` 可能是 `0.0.0.0` 或 `::`，而 `from.IP` 是具体地址，两者 `Equal` 返回 false，所有包都会被当成 P2P。实际部署中服务端 bind 到具体 IP，不会触发此问题。

### 2.4 MTU 影响

IPv6 头（40 字节）比 IPv4 头（20 字节）大 20 字节。默认 MTU 1400 不变，走 IPv6 隧道时有效载荷少 20 字节。对游戏小包（通常几十到几百字节）影响可忽略，大包分片概率略增。

### 2.5 `rateKey` 内存开销

从 `[4]byte` 变 `[16]byte`，每个 key 多 12 字节。对游戏场景（几十个连接）完全不是问题，map 的 hash 计算性能影响可忽略。

## 三、改动方案

### 3.1 `internal/server/ratelimit.go` — 地址 key 扩展

```go
// 修改前
type rateKey struct {
    IP   [4]byte
    Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
    var k rateKey
    copy(k.IP[:], addr.IP.To4())
    k.Port = uint16(addr.Port)
    return k
}

// 修改后
type rateKey struct {
    IP   [16]byte // 支持 IPv4 (映射为 v4-in-v6) 和 IPv6
    Port uint16
}

func addrToRateKey(addr *net.UDPAddr) rateKey {
    var k rateKey
    copy(k.IP[:], addr.IP.To16())
    k.Port = uint16(addr.Port)
    return k
}
```

### 3.2 `internal/server/server.go` — 双栈监听 + 客户端表 key

**监听改为双栈**：
```go
// 修改前
conn, err := net.ListenUDP("udp4", udpAddr)

// 修改后
conn, err := net.ListenUDP("udp", udpAddr)
```

**客户端表 key 改为 `[16]byte`**：
```go
// 修改前
clients map[[4]byte]*Client

// 修改后
clients map[[16]byte]*Client
```

**`ip4Key()` 统一改为 `ipKey()`**：
```go
// 修改前
func ip4Key(ip net.IP) [4]byte {
    ip4 := ip.To4()
    return [4]byte{ip4[0], ip4[1], ip4[2], ip4[3]}
}

// 修改后
func ipKey(ip net.IP) [16]byte {
    var k [16]byte
    copy(k[:], ip.To16())
    return k
}
```

**`keepaliveLoop` 中 `staleClient` key 类型同步更新**：
```go
type staleClient struct {
    key [16]byte  // 原为 [4]byte
    c   *Client
}
```

### 3.3 `internal/server/peer.go` / `register.go` / `relay.go`

所有调用 `ip4Key()` 的位置改为 `ipKey()`：

| 文件 | 调用位置 | 说明 |
|------|---------|------|
| `peer.go` | `delete(s.clients, ip4Key(c.VirtualIP))` | 断连清理 |
| `register.go` | `s.clients[ip4Key(vip)] = c` | 注册新客户端 |
| `relay.go` | `s.clients[ip4Key(dstIP)]` | 单播转发查找 |
| `relay.go` | `s.clients[ip4Key(dstIP)]` | 打洞目标查找 |

### 3.4 `internal/client/tunnel.go` — peer 表 key + 双栈连接

```go
// 修改前
serverIP4  [4]byte
peers      map[[4]byte]*Peer

// 修改后
serverIPKey [16]byte
peers       map[[16]byte]*Peer
```

连接建立改为双栈：
```go
// 修改前
sAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
conn, err := net.ListenUDP("udp4", &net.UDPAddr{})

// 修改后
sAddr, err := net.ResolveUDPAddr("udp", serverAddr)
conn, err := net.ListenUDP("udp", &net.UDPAddr{})
```

### 3.5 `internal/client/register.go`

```go
// 修改前
clientAddr, _ = net.ResolveUDPAddr("udp4", acp.ClientAddr)
t.serverIP4 = ip4Key(t.serverIP)

// 修改后
clientAddr, _ = net.ResolveUDPAddr("udp", acp.ClientAddr)
t.serverIPKey = ipKey(t.serverIP)
```

### 3.6 `internal/client/recv.go`

所有 `ip4Key()` → `ipKey()`，`serverIP4` → `serverIPKey`：

| 函数 | 修改 |
|------|------|
| `handleDirectData` | `ip4Key(dp.SrcIP)` → `ipKey(dp.SrcIP)` |
| `handleDataFromServer` | `ip4Key(dp.SrcIP)` → `ipKey(dp.SrcIP)`，`t.serverIP4` → `t.serverIPKey` |
| `handlePeerInfo` | `map[[4]byte]*Peer` → `map[[16]byte]*Peer`，`ip4Key()` → `ipKey()` |

### 3.7 `internal/client/keepalive.go`

三处 `ip4Key()` → `ipKey()`：
- `startHolePunch`：peer 查找
- `handleHolePunchReceived`：peer 查找
- `hasDirectPeerTraffic`：peer 查找

### 3.8 `internal/client/route.go`

```go
// 修改前
dstKey := ip4Key(dstIP)
if dstKey == t.serverIP4 {

// 修改后
dstKey := ipKey(dstIP)
if dstKey == t.serverIPKey {
```

### 3.9 `internal/protocol/messages.go` — PeerInfo 反序列化

```go
// 修改前
a, err := net.ResolveUDPAddr("udp4", addrStr)

// 修改后
a, err := net.ResolveUDPAddr("udp", addrStr)
```

PeerInfo 中 `PublicAddr` 以 string 格式序列化（如 `[2408:xxxx::1]:12345`），客户端解析时必须支持 IPv6 地址格式。

## 四、改动文件汇总

| 文件 | 改动量 | 核心改动 |
|------|--------|---------|
| `internal/server/ratelimit.go` | ~10 行 | `rateKey.IP` → `[16]byte`，`addrToRateKey` 用 `To16()` |
| `internal/server/server.go` | ~15 行 | `ListenUDP("udp")`，`clients map[[16]byte]`，`ipKey()` |
| `internal/server/peer.go` | 1 行 | `ip4Key` → `ipKey` |
| `internal/server/register.go` | 1 行 | `ip4Key` → `ipKey` |
| `internal/server/relay.go` | 2 行 | `ip4Key` → `ipKey` |
| `internal/client/tunnel.go` | ~20 行 | `ListenUDP("udp")`，`ResolveUDPAddr("udp")`，`peers map[[16]byte]` |
| `internal/client/register.go` | 3 行 | `ResolveUDPAddr("udp")`，`serverIPKey` |
| `internal/client/recv.go` | ~10 行 | `ipKey()`，`serverIPKey`，peer map key |
| `internal/client/keepalive.go` | 3 行 | `ip4Key` → `ipKey` |
| `internal/client/route.go` | 2 行 | `ipKey()`，`serverIPKey` |
| `internal/protocol/messages.go` | 1 行 | `ResolveUDPAddr("udp")` |

**总计**：11 个文件，约 70 行改动。

## 五、测试文件同步更新

| 文件 | 改动 |
|------|------|
| `internal/server/server_test.go` | `map[[4]byte]` → `map[[16]byte]`，`ip4Key` → `ipKey`，`TestAddrToRateKey` expected 值更新 |
| `internal/client/client_test.go` | `map[[4]byte]` → `map[[16]byte]`，`ip4Key` → `ipKey`，`serverIP4` → `serverIPKey`，ipKey 测试用例 expected 更新 |

### 实施中遇到的问题

**问题：`sed` 正则导致双指针**

使用 `sed -i 's/map\[\[4]byte\]*/map[[16]byte]*/g'` 替换 map 类型时，`*` 被当作正则量词（匹配零个或多个 `]`），导致 `map[[4]byte]*Peer` 变成了 `map[[16]byte]**Peer`（双指针）。

修复：`sed -i 's/\*\*Peer/*Peer/g'`。

**教训**：对包含 `*` 的 Go 类型做 sed 替换时，应使用精确匹配或分步替换，避免正则元字符歧义。

## 六、构建系统增强：版本信息嵌入

### 背景

Makefile 原有 `VERSION` 变量通过 `-ldflags "-X main.Version=$(VERSION)"` 注入，但缺少 commit hash 和构建时间，无法精确定位二进制对应的源码版本。

### 改动

**Makefile**：新增 `COMMIT` 和 `BUILD_TIME` 变量：

```makefile
VERSION    := $(shell git describe --tags --always --dirty 2>/dev/null || echo "dev")
COMMIT     := $(shell git rev-parse --short HEAD 2>/dev/null || echo "unknown")
BUILD_TIME := $(shell date -u '+%Y-%m-%dT%H:%M:%SZ')
LDFLAGS    := -ldflags "-s -w \
    -X main.Version=$(VERSION) \
    -X main.Commit=$(COMMIT) \
    -X main.BuildTime=$(BUILD_TIME)"
```

**服务端 `cmd/server/main.go`**：

```go
var (
    Version   = "dev"
    Commit    = "unknown"
    BuildTime = "unknown"
)
```

`-version` 输出：`gtunnel-server v1.0.0 (commit: a0a35f7, built: 2026-05-17T09:48:00Z)`

启动 banner 也新增 commit 和 build time 两行。

**客户端 `cmd/client/run.go`**：

新增相同的三个变量，启动日志中输出版本信息。

### 注意事项

- 无 git tag 时 `git describe --tags --always` 仅输出 commit hash（如 `91bfd49`）
- 打 tag 后（`git tag v1.0.0`）输出语义化版本（`v1.0.0`），有新 commit 后变为 `v1.0.0-1-g91bfd49`
- `-dirty` 后缀表示工作区有未提交修改

## 七、额外修复：`readResponse` 死循环

### 问题

`internal/client/register.go` 的 `readResponse` 函数中存在一个 `for` 循环，但循环体的每条路径都直接 `return`，循环永远只执行一次。这是 Go 静态分析工具报告的 "surrounding loop is unconditionally terminated" 警告。

### 修复

去掉无意义的 `for` 循环和外层缩进，保留函数语义不变：

```go
// 修改前
func (t *Tunnel) readResponse(ctx context.Context, buf []byte) (*protocol.Message, error) {
    for {
        select {
        case <-ctx.Done():
            return nil, ctx.Err()
        default:
        }
        n, _, err := t.conn.ReadFromUDP(buf)
        if err != nil {
            return nil, err
        }
        msg, err := protocol.DecodeChecked(buf[:n])
        if err != nil {
            return nil, fmt.Errorf(...)
        }
        return msg, nil
    }
}

// 修改后
func (t *Tunnel) readResponse(ctx context.Context, buf []byte) (*protocol.Message, error) {
    select {
    case <-ctx.Done():
        return nil, ctx.Err()
    default:
    }
    n, _, err := t.conn.ReadFromUDP(buf)
    if err != nil {
        return nil, err
    }
    msg, err := protocol.DecodeChecked(buf[:n])
    if err != nil {
        return nil, fmt.Errorf(...)
    }
    return msg, nil
}
```

## 八、提交记录

| Commit | 内容 |
|--------|------|
| `53ca319` | feat: IPv6 transport layer support（13 个文件，核心改动） |
| `c6254b0` | fix: test file sed regex error (`**Peer` → `*Peer`) |
| `91bfd49` | fix: remove dead loop in `readResponse` |
| `a0a35f7` | feat: embed commit hash and build time in version info |

## 九、技术参考

- [net.IP.To16() 文档](https://pkg.go.dev/net#IP.To16) — IPv4 自动映射为 v4-in-v6 格式
- [net.ListenUDP 双栈行为](https://pkg.go.dev/net#ListenUDP) — `"udp"` 在 `bindv6only=0` 时同时监听 IPv4/IPv6
- [net.IP.Equal()](https://pkg.go.dev/net#IP.Equal) — 跨 IPv4/IPv6 地址比较
- `docs/ipv6-feasibility-analysis.md` — 方案 A 的可行性分析
