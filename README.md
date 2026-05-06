# GameTunnel 🎮

通用局域网游戏隧道工具。让不同地区的玩家像在同一个局域网里一样联机对战。

支持所有基于 IP 的局域网游戏，内置广播转发（适用于依赖 UDP 广播发现的游戏）。

## 快速开始

### 服务器（公网 VPS，Linux）

```bash
# 一键安装（从 GitHub Releases 下载预编译二进制）
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo bash

# 带房间密码：
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo ROOM_PASSWORD=你的密码 bash

# 带状态页面：
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install.sh | sudo STATUS_ADDR=:4701 bash

# 或手动编译：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700
```

### 玩家（Windows 电脑）

**方式一：下载压缩包（推荐）**
1. 从 [Releases](https://github.com/holipay/gametunnel/releases) 下载 `GameTunnel-windows-amd64.zip`
2. 解压到任意文件夹（共 3 个文件）
3. 用记事本编辑 `config.ini`，填入服务器地址
4. 双击 `gtunnel-client.exe`，自动请求管理员权限后连接

配置文件 `config.ini` 示例：
```ini
server=1.2.3.4:4700
name=玩家名
room=default
password=
```

连接成功后，打开游戏进入局域网模式即可。

## 安全

### 房间密码（HMAC 认证）

服务端设置密码后，客户端连接时需要通过 HMAC challenge-response 验证：

```bash
# 服务端
gtunnel-server -addr :4700 -password mysecret

# 客户端
gtunnel-client.exe -server 1.2.3.4:4700 -password mysecret
```

认证流程：
1. 客户端发送注册请求（房间ID + 用户名）
2. 服务端返回 16 字节随机 challenge
3. 客户端用 HKDF-SHA256 从密码派生密钥，计算 HMAC(challenge + 房间ID + 用户名 + 客户端地址)
4. 服务端验证 HMAC，通过后分配虚拟 IP
5. 认证超时 15 秒，超时需重新发起

密码不会在网络上传输。即使被中间人截获 challenge 和响应，也无法在合理时间内逆向密码。

### 数据包完整性

所有协议包都包含 CRC32 校验和，丢弃损坏/篡改的包。

### 已知限制

- **无加密**：游戏数据明文中转，仅 CRC32 防损坏。如需加密请配合 WireGuard 等 VPN 使用。
- **无重放保护**：协议不含序列号，CRC32 不能防重放攻击。

### 服务端防护

- HMAC challenge-response 认证（密码不传输）
- 速率限制：每客户端 500 包/秒
- 未认证连接数上限：最大玩家数 × 3
- 源 IP 绑定：中转包的 srcIP 必须匹配发送者虚拟 IP（防 IP 伪造）
- 用户名/房间 ID 长度限制：32 字符
- 配置文件权限 0600（仅所有者可读）

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
- UDP 广播通过服务器转发给同房间所有人
- 自动尝试 UDP 打洞实现 P2P 直连
- 打洞失败则通过服务器中转（10人以内延迟可接受）

## 参数

### 服务器
```bash
gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10 -password secret
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:4700` | 监听端口 |
| `-subnet` | `10.10.0.0/24` | 虚拟子网 |
| `-max` | `10` | 最大玩家数 |
| `-password` | _(空)_ | 房间密码（留空=无认证） |
| `-version` | | 显示版本 |

### 客户端（Windows）
```cmd
gtunnel-client.exe -server 1.2.3.4:4700 -name 玩家名 -room 房间ID -password secret
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-server` | _(必填或配置文件)_ | 服务器地址 |
| `-name` | 计算机名 | 玩家名称（最长 32 字符） |
| `-room` | `default` | 房间ID（同房间玩家互通，最长 32 字符） |
| `-password` | _(空)_ | 房间密码 |
| `-mtu` | `1400` | 隧道MTU |
| `-version` | | 显示版本 |

也可以通过配置文件设置：`%APPDATA%\GameTunnel\config.json`

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
A: 可以用，但这个工具更轻量、无依赖，专门为局域网游戏优化了广播转发。

**Q: 客户端需要管理员权限吗？**
A: 是的，创建虚拟网卡（wintun）需要管理员权限。首次运行时 Windows 会弹 UAC 提示。

**Q: 延迟多少？**
A: 取决于到服务器的延迟。如果服务器在国内 VPS，通常 20-50ms。打洞成功后 P2P 直连延迟更低。

**Q: 支持哪些游戏？**
A: 支持所有基于 IP 的局域网游戏。广播转发已内置，适用于依赖 UDP 广播发现的游戏（如星际争霸、红警、帝国时代等）。

**Q: 支持 Windows 几？**
A: Windows 10 及以上（依赖 wintun 驱动）。

**Q: 数据安全吗？**
A: 认证安全（HMAC-SHA256 + HKDF），但游戏数据明文传输。如需加密请配合 WireGuard 使用。

## 开发

### 环境要求

- Go 1.25+

### 编译

```bash
# 编译服务端（Linux）
make server

# 编译客户端（Windows，交叉编译）
make client

# 编译所有架构的客户端
make client-all

# 查看版本
./bin/gtunnel-server -version
./bin/gtunnel-client.exe -version
```

### 依赖说明

`wintun` 模块的版本由 `wireguard-go` 锁定，已固定在 go.mod 中。如遇依赖问题：

```bash
GOPROXY=direct go mod tidy
```

## License

MIT
