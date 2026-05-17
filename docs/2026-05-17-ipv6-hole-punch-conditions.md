# IPv6 打洞成功条件分析

> 2026-05-17，基于仓库源码（commit `a0a35f7`）及 `docs/ipv6-feasibility-analysis.md`、`docs/2026-05-17-ipv6-transport-layer.md` 整理。

## 一、当前架构概览

GameTunnel 采用**方案 A（仅传输层 IPv6）**：

- **虚拟子网**：始终是 IPv4（`10.10.0.x/24`），游戏看到的都是 IPv4 地址
- **传输层**：客户端 ↔ 服务端之间的 UDP 隧道支持 IPv6 地址
- **P2P 打洞**：基于公网地址的 UDP hole punching

核心改动是将所有地址 map key 从 `[4]byte` 扩展为 `[16]byte`，配合 `net.IP.To16()` 的 v4-in-v6 映射格式（`::ffff:a.b.c.d`），使 IPv4 和 IPv6 地址可以统一处理。服务端监听从 `net.ListenUDP("udp4", ...)` 改为 `net.ListenUDP("udp", ...)`，在 Linux 双栈模式下同时接受 IPv4/IPv6 连接。

## 二、IPv6 打洞成功的前提条件

### 2.1 客户端网卡条件

**必须有可用的全局 IPv6 地址**（非 link-local `fe80::`）：

```bash
# Linux 检查
ip -6 addr show | grep "inet6" | grep -v "fe80"

# Windows 检查
ipconfig | findstr "IPv6"
```

要求：

- **全局单播地址**（`2000::/3`，如 `2408:`、`240e:` 等国内运营商前缀）
- 地址状态必须是 `preferred`（非 `deprecated`）
- 接口必须已启用 IPv6（`accept_ra` 正常、RA 已分配地址）

### 2.2 客户端防火墙设置

**必须放行 UDP 出站 + 对应的回程入站**：

```bash
# Linux（iptables）
ip6tables -A OUTPUT -p udp --dport 4700 -j ACCEPT
ip6tables -A INPUT -p udp --sport 4700 -j ACCEPT

# 或使用 ufw
ufw allow out 4700/udp
ufw allow in 4700/udp
```

```powershell
# Windows 防火墙（管理员权限运行 PowerShell）
# GameTunnel 客户端需要：
# 1. 允许 gtunnel-client.exe 出站 UDP（任意端口 → 服务端:4700）
# 2. 允许入站 UDP（回程包，源端口 4700 或 P2P 对端端口）
# 首次运行时需在 Windows 防火墙弹窗中点击"允许访问"
```

**关键点**：

- IPv6 通常**没有 NAT**，但有**有状态防火墙**（stateful firewall）
- 防火墙默认只允许「已建立连接」的回程包（类似 NAT 的 "cone" 行为）
- 打洞时双方同时发包，需要**双向都放行**
- Windows Defender 防火墙是最大的障碍——`gtunnel-client.exe` 需要被**管理员权限运行**并**允许通过防火墙**

### 2.3 客户端路由器/网关设置

**家庭路由器**：

- 大多数现代路由器默认放行 IPv6 出站
- **回程入站**通常默认允许（IPv6 设计理念是端到端可达）
- 部分路由器有 IPv6 防火墙（如 OpenWrt 的 `ip6tables`），需要检查：

```bash
# OpenWrt 路由器检查
ip6tables -L -n | grep -i "drop\|reject"

# 如果有默认 DROP 规则，需要放行 GameTunnel 端口
ip6tables -I FORWARD -p udp --dport 4700 -j ACCEPT
ip6tables -I FORWARD -p udp --sport 4700 -j ACCEPT
```

### 2.4 服务端设置

**服务端 VPS 必须**：

```bash
# 1. 有全局 IPv6 地址
ip -6 addr show | grep "inet6" | grep -v "fe80"

# 2. 监听 IPv6 双栈（代码已改为 "udp"，自动双栈）
gtunnel-server -addr :4700

# 3. 防火墙放行
# iptables
ip6tables -A INPUT -p udp --dport 4700 -j ACCEPT

# 或 ufw
ufw allow 4700/udp

# 或 firewalld
firewall-cmd --add-port=4700/udp --permanent
firewall-cmd --reload
```

**内核参数**：

```bash
# 确保双栈模式（默认为 0，同时接受 IPv4/IPv6）
sysctl net.ipv6.bindv6only
# 应输出: net.ipv6.bindv6only = 0

# 如果为 1，GameTunnel 只监听 IPv6，IPv4 客户端无法连接
# 解决：设为 0 或分别监听两个端口
```

### 2.5 网络路径

- 中间链路无 IPv6 防火墙阻断
- 双方 IPv6 路由可达（`traceroute6` 或 `ping6` 验证）
- 无 ISP 级别的 IPv6 过滤

## 三、IPv6 打洞 vs IPv4 NAT 打洞

| 维度 | IPv4 NAT 打洞 | IPv6 "打洞"（防火墙穿透） |
|------|--------------|------------------------|
| 障碍 | NAT 地址转换 | 有状态防火墙 |
| 原理 | 同时发包建立 NAT 映射 | 同时发包建立防火墙状态条目 |
| 地址 | 服务端看到的是 NAT 后的公网 IP | 服务端看到的是客户端真实 IPv6 地址 |
| 成功率 | 受 NAT 类型限制（Symmetric NAT 难打） | **通常很高**（无 NAT，只需防火墙放行） |
| 失败场景 | 双方都是 Symmetric NAT | 防火墙严格策略拒绝入站 UDP |

### 打洞流程（源码分析）

GameTunnel 的打洞分 3 个阶段（`keepalive.go` 中定义）：

```go
var holePunchIntervals = []time.Duration{
    100 * time.Millisecond,  // Phase 1: 快速穿透 cone NAT
    250 * time.Millisecond,  // Phase 2: 适配端口受限 NAT
    500 * time.Millisecond,  // Phase 3: 对应 symmetric NAT 或不稳网络
}
const holePunchBurstPerPhase = 5  // 每阶段发 5 个包
```

流程：

1. 服务端告知双方对方的公网地址（`PeerInfo` 消息）
2. 客户端 A 调用 `startHolePunch()`，向 B 的公网地址发送 `TypeHolePunch` 包
3. B 收到后调用 `handleHolePunchReceived()`，立即回打（双向穿透）
4. 每阶段结束后检查 `hasDirectPeerTraffic()`，已通则提前结束
5. 打洞成功后 `p2pKeepaliveLoop()` 每 15 秒发送 keepalive 保持映射
6. 失败则回退到服务器中转，每 25 秒重试打洞（`holePunchRetryLoop`）

**对 IPv6 的影响**：打洞 payload 中携带的是 4 字节虚拟 IP（IPv4），与传输层地址无关。IPv6 传输层仅影响「包从哪个公网地址发出」，打洞逻辑本身无需修改。

## 四、条件总结

```
✅ 客户端
├── 有全局 IPv6 地址（2000::/3，非 link-local）
├── 防火墙允许 gtunnel-client.exe 出站 UDP
├── 防火墙允许对应回程入站 UDP（或 stateful firewall 自动放行）
├── 路由器 IPv6 防火墙未阻断入站 UDP
└── 网卡已启用 IPv6（accept_ra=1 或手动配置）

✅ 服务端
├── VPS 有全局 IPv6 地址
├── 监听 IPv6 双栈（net.ListenUDP("udp", ":4700")）
├── 防火墙放行 UDP 4700 入站
└── net.ipv6.bindv6only = 0（默认值）

✅ 网络路径
├── 中间链路无 IPv6 防火墙阻断
├── 双方 IPv6 路由可达
└── 无 ISP 级别的 IPv6 过滤
```

## 五、常见失败原因

| 排名 | 原因 | 症状 | 解决方案 |
|------|------|------|---------|
| 1 | Windows 防火墙阻断 | 能连服务端但 P2P 打洞失败 | 以管理员权限运行，允许通过防火墙 |
| 2 | 路由器 IPv6 防火墙 | 有 IPv6 地址但打洞不通 | 检查路由器 ip6tables 规则，放行 UDP |
| 3 | 无全局 IPv6 地址 | 只有 fe80:: 地址 | 检查 ISP 是否支持 IPv6，路由器 RA 配置 |
| 4 | ISP 不支持 IPv6 | 完全无 IPv6 地址 | 联系运营商或使用 6to4/Teredo 隧道 |
| 5 | 服务端 bindv6only=1 | IPv4 客户端全部断连 | `sysctl -w net.ipv6.bindv6only=0` |

## 六、诊断脚本

### Linux/macOS 客户端

```bash
echo "=== IPv6 地址 ==="
ip -6 addr show | grep "inet6" | grep -v "fe80"

echo "=== IPv6 路由 ==="
ip -6 route show default

echo "=== 连通性测试 ==="
ping6 -c 3 <服务器IPv6地址>

echo "=== 防火墙规则 ==="
ip6tables -L -n 2>/dev/null || echo "无 ip6tables 权限"
```

### Windows 客户端

```powershell
ipconfig | findstr "IPv6"
Test-NetConnection -ComputerName <服务器IPv6地址> -Port 4700 -InformationLevel Detailed
```

### 服务端

```bash
echo "=== IPv6 地址 ==="
ip -6 addr show | grep "inet6" | grep -v "fe80"

echo "=== 监听状态 ==="
ss -ulnp | grep 4700

echo "=== 防火墙 ==="
ip6tables -L INPUT -n | grep 4700

echo "=== 内核参数 ==="
sysctl net.ipv6.bindv6only
```

## 七、结论

**IPv6 打洞比 IPv4 NAT 打洞简单得多。** 只要双方有全局 IPv6 地址、防火墙放行 UDP，打洞几乎必定成功（无 Symmetric NAT 问题）。最大障碍是**本地防火墙配置**，特别是 Windows 防火墙。

与 IPv4 NAT 打洞的 Symmetric NAT（运营商级 NAT）困境不同，IPv6 环境下唯一的阻断因素是有状态防火墙的入站策略，而大多数消费级路由器和操作系统的默认配置都允许 IPv6 入站（或至少允许已建立连接的回程包）。
