# GameTunnel 🎮

星际争霸1 异地局域网对战工具。让不同地区的玩家像在同一个局域网里一样对战。

## 快速开始

### 服务器（公网 VPS，Linux）

```bash
# 一键安装
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo bash

# 或手动：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700
```

### 玩家（Windows 电脑）

**方式一：PowerShell 一键安装**
```powershell
irm https://raw.githubusercontent.com/holipay/gametunnel/main/install-client.ps1 | iex -Server 你的服务器IP
```

**方式二：手动下载**
1. 从 [Releases](https://github.com/holipay/gametunnel/releases) 下载 `gtunnel-client.exe`
2. 以管理员身份运行：
```cmd
gtunnel-client.exe -server 你的服务器IP:4700
```

连接成功后：
1. 打开**星际争霸1**
2. 进入 **Multiplayer** → **Local Area Network**
3. 建主或加入游戏，跟真局域网一样

## 原理

```
玩家A Windows (10.10.0.2)              玩家B Windows (10.10.0.3)
  │                                        │
  ├─ wintun 虚拟网卡                        ├─ wintun 虚拟网卡
  │  (10.10.0.2/24)                         │  (10.10.0.3/24)
  │                                        │
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

### 客户端（Windows）
```cmd
gtunnel-client.exe -server 1.2.3.4:4700
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-server` | `127.0.0.1:4700` | 服务器地址 |
| `-mtu` | `1400` | 隧道MTU |
| `-name` | 计算机名 | 玩家名称 |

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

**Q: 客户端需要管理员权限吗？**
A: 是的，创建虚拟网卡（wintun）需要管理员权限。首次运行时 Windows 会弹 UAC 提示。

**Q: 延迟多少？**
A: 取决于到服务器的延迟。如果服务器在国内 VPS，通常 20-50ms。打洞成功后 P2P 直连延迟更低。

**Q: 支持其他游戏吗？**
A: 支持所有基于 IP 的局域网游戏。广播转发已内置。

**Q: 支持 Windows 几？**
A: Windows 10 及以上（依赖 wintun 驱动）。

## 开发

```bash
# 编译服务端（Linux）
make server

# 编译客户端（Windows，交叉编译）
make client

# 编译所有架构的客户端
make client-all
```

## License

MIT
