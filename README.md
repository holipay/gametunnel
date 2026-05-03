# GameTunnel 🎮

异地组局域网打游戏。通过虚拟隧道让不同地区的玩家像在同一个局域网里一样对战。

## 架构

```
┌──────────────┐     UDP      ┌──────────────────┐     UDP      ┌──────────────┐
│  Player A    │◄────────────►│  GameTunnel       │◄────────────►│  Player B    │
│  10.10.0.2   │   (P2P/Relay)│  Server (公网VPS)  │              │  10.10.0.3   │
│  TUN Device  │              │  10.10.0.1        │              │  TUN Device  │
└──────────────┘              └──────────────────┘              └──────────────┘
```

## 工作原理

1. **Server** 运行在公网 VPS 上，监听 UDP 端口
2. **Client** 在每台玩家机器上运行，创建虚拟网卡 (TUN 设备)
3. Client 连接 Server，获得虚拟 IP (10.10.0.x)
4. 游戏流量通过虚拟网卡 → UDP 隧道 → Server 中转/直连 → 对端虚拟网卡
5. 自动尝试 UDP 打洞实现 P2P 直连，失败则通过 Server 中转

## 快速开始

### 1. 编译

```bash
make all
# 或交叉编译客户端（给不同平台的玩家）
make client-all
```

### 2. 部署 Server（公网 VPS）

```bash
# 复制到服务器
scp bin/gtunnel-server your-vps:/usr/local/bin/

# 运行
sudo gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10

# 或用 systemd
sudo cp configs/gtunnel-server.service /etc/systemd/system/
sudo systemctl enable --now gtunnel-server
```

### 3. 玩家连接

```bash
# 需要 root 权限（创建 TUN 设备需要）
sudo ./bin/gtunnel-client -server YOUR_VPS_IP:4700 -room mygame -name Player1
```

### 4. 游戏内操作

连接成功后，所有人获得 10.10.0.x 的虚拟 IP：
- **主机建房间**，其他人通过主机的虚拟 IP 加入
- 例如主机 IP 是 10.10.0.2，其他人输入 `10.10.0.2` 连接

## 参数说明

### Server
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:4700` | 监听地址 |
| `-subnet` | `10.10.0.0/24` | 虚拟子网 |
| `-max` | `10` | 最大玩家数 |

### Client
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-server` | `127.0.0.1:4700` | 服务器地址 |
| `-room` | `default` | 房间 ID |
| `-name` | 主机名 | 玩家名称 |
| `-mtu` | `1400` | 隧道 MTU |

## 防火墙

确保 VPS 开放 UDP 端口：
```bash
# iptables
sudo iptables -A INPUT -p udp --dport 4700 -j ACCEPT

# ufw
sudo ufw allow 4700/udp
```

## 常见游戏类型支持

| 游戏类型 | 支持 | 说明 |
|----------|------|------|
| 局域网对战 (如 CS, Minecraft) | ✅ | 直接用虚拟 IP 连接 |
| Steam 局域网 | ✅ | 同一虚拟子网自动发现 |
| 红警/星际等老游戏 | ✅ | IPX/UDP 广播可能需要额外配置 |
| 需要广播的游戏 | ⚠️ | 可能需要额外的广播转发支持 |

## 安全提示

- 服务器仅转发游戏流量，不存储任何用户数据
- 建议在 VPS 防火墙中限制来源 IP（可选）
- 隧道流量未加密，如需加密可叠加 WireGuard

## 开发

```bash
# 本地测试：开两个终端
# 终端 1 - Server
go run ./cmd/server -addr :4700

# 终端 2 - Client A
sudo go run ./cmd/client -server 127.0.0.1:4700 -name Alice

# 终端 3 - Client B
sudo go run ./cmd/client -server 127.0.0.1:4700 -name Bob
```

## License

MIT
