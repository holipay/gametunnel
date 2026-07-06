# GameTunnel 联机功能分析

## 总体架构

```
游戏 A → TUN → 加密通道 → 服务器 → 转发 → 客户端 B → TUN → 游戏 B
                             ↕ P2P (打洞成功后直连)
```

三层架构：**游戏 ↔ TUN 虚拟网卡 ↔ 加密隧道 ↔ 中继服务器**。

## 角色职责

| 组件 | 文件 | 职责 |
|------|------|------|
| Server | `cmd/server/main.go`, `internal/server/` | 房间管理、IP 分配、数据中继、P2P 信令 |
| Client App | `cmd/client/`, `internal/client/app.go` | 连接管理、自动重连、状态跟踪 |
| Tunnel | `internal/client/tunnel.go` | 核心隧道引擎：注册、TUN I/O、数据路由 |
| TUN Device | `internal/tun/` | 虚拟网卡创建、路由配置（平台相关） |
| Protocol | `internal/protocol/` | 消息序列化、CRC32、版本兼容 |
| Crypto | `internal/crypto/` | ChaCha20-Poly1305 加解密 |
| Auth | `internal/auth/` | HMAC 挑战响应、密钥派生 |

## 端到端数据流

### 连接建立

```
Client                          Server
  │                                │
  ├── TypeRegister ──────────────► │
  │                                ├── 可选: TypeAuthChallenge
  │◄── TypeAuthResponse ──────────┤
  │                                ├── 分配虚拟 IP (10.10.0.x)
  │◄── TypeAssignIP ──────────────┤
  │                                │
  ├── 创建 TUN 设备                │
  ├── 配置路由                     │
  ├── 启动 NAT 探测                │
  ├── 启动所有 goroutine 循环      │
  │                                │
```

- 文件: `internal/client/register.go:20` (`register`), `internal/client/tunnel.go:130` (`Connect`)

### 游戏广播发现（核心场景）

```
游戏 A (10.10.0.2)              服务器                   游戏 B (10.10.0.3)
  │                                │                          │
  ├─ UDP 广播 → TUN                │                          │
  │   (255.255.255.255)            │                          │
  │                                │                          │
  ├─ routePacket                    │                          │
  │   IsRelayTarget=true            │                          │
  │   → sendToServer               │                          │
  │                                │                          │
  ├─── TypeData(broadcast) ──────► │                          │
  │                                ├── handleRelay            │
  │                                │   isBroadcast=true        │
  │                                │   → 转发到其他所有成员    │
  │                                │                          │
  │                                ├──── TypeData ──────────► │
  │                                │                          ├── handleDataFromServer
  │                                │                          ├── 解密 (如有密码)
  │                                │                          ├── rewriteBroadcast (Win)
  │                                │                          ├── 写入 TUN
  │                                │                          │
  │                                │                          ├── 游戏 B 收到 LAN 广播
```

- `receiveFromTUN`: `internal/client/recv.go:169`
- `routePacket` 广播判断: `internal/client/route.go:112`
- `IsRelayTargetRaw` 判断: `internal/netutil/broadcast.go:52`
- 服务端广播转发: `internal/server/room_relay.go:168-173`
- 对端写入 TUN: `internal/client/recv_data.go:59` (`decryptWriteAndRelease`)
- Windows 广播重写: `internal/client/recv_data.go:22` (`rewriteBroadcast`)
- Linux 有限广播路由: `internal/tun/configure_linux.go:51-76` (ip rule → TUN)

### P2P 直连 (UDP 打洞)

```
Client A                        服务器                      Client B
  │                                │                          │
  │  TypePeerRequest ────────────► │                          │
  │◄── TypePeerInfo ──────────────┤                          │
  │                                │                          │
  ├── startHolePunch               │                          │
  │   (等待 NAT 探测完成)           │                          │
  │                                │                          │
  ├── TypeHolePunch ─────────────► ├── handleHolePunch ─────► │
  │   (A 的 VIP + 公网地址)        │   (找到 B, 转发)         │   ├── handleHolePunchReceived
  │                                │                          │   ├── burstHolePunch(A)
  │◄── TypeHolePunch(B) ──────────┤                          │
  │   (B 的公网地址)               │                          │
  │                                │                          │
  ├── 收到 B 的直连包              │                          ├── 收到 A 的直连包
  │   handleDirectHolePunch        │                          │   handleDirectHolePunch
  │   DirectReach=true             │                          │   DirectReach=true
  │                                │                          │
  │◄═══════ P2P 直连数据 ═══════════════════════════════════► │
  │                                │                          │
  ├── 15s P2P keepalive            │                          ├── 15s P2P keepalive
  │   (维持 NAT 映射)              │                          │
  │                                │                          │
  ├── 25s 重试 (失败回退中继)      │                          │
```

- Hole punching: `internal/client/holepunch.go:73` (`startHolePunch`)
- 接收 P2P 数据: `internal/client/recv_data.go:94` (`handleDirectData`)
- P2P keepalive: `internal/client/p2p_keepalive.go:21` (`p2pKeepaliveLoop`, 15s interval)

## 关键文件索引

| 功能 | 文件 | 行号 |
|------|------|------|
| 服务端初始化 | `cmd/server/main.go` | 36-165 |
| 客户端入口 | `cmd/client/run.go` | 28-89 |
| 连接 + 注册 | `internal/client/tunnel.go` | 130-173 |
| 注册握手 | `internal/client/register.go` | 20-80 |
| IP 分配处理 | `internal/client/register.go` | 195-276 |
| 创建 TUN | `internal/client/tunnel.go` | 370-388 |
| TUN 读取 + 分发 | `internal/client/recv.go` | 169-255 |
| 服务端数据接收 | `internal/client/recv.go` | 31-121 |
| 数据路由 | `internal/client/route.go` | 81-127 |
| 广播/组播判断 | `internal/netutil/broadcast.go` | 1-75 |
| 打洞 | `internal/client/holepunch.go` | 73-230 |
| P2P 直连数据处理 | `internal/client/recv_data.go` | 94-162 |
| 中继数据处理 | `internal/client/recv_data.go` | 167-197 |
| 广播重写 (Win) | `internal/client/recv_data.go` | 22-57 |
| Linux 有限广播路由 | `internal/tun/configure_linux.go` | 51-76 |
| 服务端房间 | `internal/server/room.go` | 1-410 |
| 服务端中继 | `internal/server/room_relay.go` | 1-331 |
| 服务端 P2P 信令 | `internal/server/room_relay.go` | 220-250 |
| 服务端保活 | `internal/server/keepalive_server.go` | 1-257 |
| 客户端保活 | `internal/client/keepalive.go` | 1-58 |
| sendQueue 关闭同步 | `internal/server/sendqueue.go` | 42-43, 127-138, 253-265 |
| 自动重连 | `internal/client/app.go` | 235-348 |
| 重连退避 | `internal/client/reconnect.go` | 1-67 |
| 配置 | `internal/client/config.go` | — |
| Linux TUN 路由 | `internal/tun/configure_linux.go` | 1-120 |
| Windows TUN 路由 | `internal/tun/tun.go` | 1-222 |
| 加密 | `internal/crypto/crypto.go` | — |
| 协议消息 | `internal/protocol/messages*.go` | — |
| 集成测试 | `internal/server/integration_test.go` | 1-360+ |

## 安全机制

| 机制 | 实现位置 | 说明 |
|------|----------|------|
| HMAC 认证 | `internal/auth/auth.go` | 挑战-响应，密码永不传输 |
| 传输加密 | `internal/crypto/crypto.go` | ChaCha20-Poly1305 + ECDH |
| 防 IP 伪造 | `internal/server/room_relay.go:132` | 服务端校验 srcIP == sender VIP |
| 会话令牌 | `internal/server/room_relay.go:139-161` | 16 字节随机 token 防数据伪造 |
| 速率限制 | `internal/server/server.go` | 500 pps/客户端 |
| 注册限速 | `internal/server/room.go` | 5 reg/s/IP |
| CRC32 | `internal/protocol/protocol.go` | 未加密房间提供完整性 |

## 已知限制

### 1. 无重放保护
- **文件**: README.md "Known Limitations"
- **问题**: 无序列号，CRC32 不防重放
- **影响**: 中。局域网游戏场景下攻击者需在同一网络

### 2. 广播放大无上限
- **文件**: `internal/server/room_relay.go:200-216`
- **问题**: 一个广播包放大 N-1 倍发给所有成员。外层有 500pps 速率限制但不精确
- **影响**: 低-中。10 人以内场景可接受。恶意客户端可通过 500 广播/s × 9 = 4500 pps 输出

### 3. 仅 IPv4
- **文件**: 多处 `To4()` 检查
- **问题**: 虚拟子网仅支持 IPv4 (/24)
- **影响**: 低。绝大多数 LAN 游戏仍基于 IPv4

### 4. 无 P2P 广播
- **文件**: `internal/client/route.go:112`
- **问题**: 所有广播/组播必须经服务器中转，即使与所有对端都有 P2P 直连
- **影响**: 低。广播流量占比小，服务器中转延迟可接受

## 测试覆盖

| 包 | 测试文件 | 关键测试 |
|----|----------|----------|
| server | `server_test.go` | 中继单播/广播、打洞、P2P、token 校验 |
| server | `integration_test.go` | 注册（无密码/有密码）、IP 回收、中继、广播 |
| client | `client_test.go` | 待确认 |
| protocol | `protocol_test.go` | 编解码、CRC32 |
| netutil | `broadcast_test.go` | `IsBroadcast`、`IsRelayTarget` |
| crypto | `crypto_bench_test.go` | 加解密性能 |
| sendqueue | `sendqueue_test.go` | 优先级队列、带宽限制 |

## 改进建议

1. **服务端广播速率限制** — 在 `room_relay.go:200` 处添加 per-client 广播频率跟踪，限制每秒广播包数
2. **P2P 广播优化** — 可选优化：如果房间内所有对端均已 DirectReach，可直接 P2P 广播
3. **添加重放保护** — 在协议层添加序列号窗口

## 近期修复 (PR #143-#150)

| PR | 修复内容 | 影响 |
|----|----------|------|
| #143 | `255.255.255.255` 有限广播通过 `ip rule` 重定向到 TUN | Linux 广播发现可用 |
| #144 | `math/rand` → `rand/v2`，移除自定义 `min` | 代码现代化 |
| #145 | `decrementIPConnCount` 计数器下溢保护 | 防止 maxPerIP 绕过 |
| #146 | sendQueue 关闭用 channel 同步替代 `time.Sleep(100ms)` | 可靠关闭 |
| #147 | TCPTransport 添加 30s 读空闲超时 | 防止僵尸连接 |
| #148 | 缓存 `encrypted` 标志避免每次包处理重新计算 | 热路径优化 |
| #149 | 移除 `cleanStalePeers` 中无用变量 | 代码清理 |
| #150 | 模板执行失败返回 HTTP 500 | 正确错误响应 |
