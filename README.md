# GameTunnel 🎮

星际争霸1 异地局域网对战工具。让不同地区的玩家像在同一个局域网里一样对战。

## 快速开始

### 服务器（公网 VPS）

```bash
# 一键安装
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo bash

# 或手动：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700
```

### 玩家（自己的电脑）

```bash
# 一键安装
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.sh | sudo bash -s -- 你的服务器IP

# 或手动：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make client
sudo ./bin/gtunnel-client -server 你的服务器IP:4700
```

连接成功后：
1. 打开**星际争霸1**
2. 进入 **Multiplayer** → **Local Area Network**
3. 建主或加入游戏，跟真局域网一样

## 原理

```
玩家A (10.10.0.2)                    玩家B (10.10.0.3)
  │                                      │
  ├─ TUN虚拟网卡                         ├─ TUN虚拟网卡
  │  (10.10.0.2/24)                      │  (10.10.0.3/24)
  │                                      │
  └──UDP隧道──►  公网VPS Server  ◄──UDP隧道──┘
                (10.10.0.1)
                信令 + 中转 + 广播转发
```

- 所有人获得 10.10.0.x 的虚拟IP
- 星际1的 UDP 广播（端口 6112）通过服务器转发给所有人
- 自动尝试 UDP 打洞实现 P2P 直连
- 打洞失败则通过服务器中转（10人以内延迟可接受）

## 参数

### 服务器
```bash
gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:4700` | 监听端口 |
| `-subnet` | `10.10.0.0/24` | 虚拟子网 |
| `-max` | `10` | 最大玩家数 |

### 客户端
```bash
gtunnel-client -server 1.2.3.4:4700
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-server` | `127.0.0.1:4700` | 服务器地址 |
| `-mtu` | `1400` | 隧道MTU |

## 防火墙

服务器需要开放 UDP 4700：
```bash
# iptables
iptables -A INPUT -p udp --dport 4700 -j ACCEPT

# ufw
ufw allow 4700/udp

# firewalld
firewall-cmd --add-port=4700/udp --permanent
firewall-cmd --reload
```

## 常见问题

**Q: 为什么不用 ZeroTier/Tailscale？**
A: 可以用，但这个工具更轻量、无依赖、专门为星际1优化了广播转发。

**Q: 需要 root 权限吗？**
A: 客户端需要（创建虚拟网卡需要）。服务器不需要。

**Q: 延迟多少？**
A: 取决于到服务器的延迟。如果服务器在国内 VPS，通常 20-50ms。打洞成功后 P2P 直连延迟更低。

**Q: 支持其他游戏吗？**
A: 支持所有基于 IP 的局域网游戏。广播转发已内置。

## License

MIT
