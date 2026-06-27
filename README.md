# GameTunnel 🎮

> [English](README_EN.md)

通用局域网游戏隧道工具。让不同地区的玩家像在同一个局域网里一样联机对战。

支持所有基于 IP 的局域网游戏，内置广播转发（适用于依赖 UDP 广播发现的游戏）。

## 快速开始

### 服务器（公网 VPS）

#### Linux

```bash
# 一键安装（从 GitHub Releases 下载预编译二进制）
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo bash

# 带房间密码：
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo ROOM_PASSWORD=你的密码 bash

# 带状态页面：
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo STATUS_ADDR=:4701 bash

# 或手动编译（需要 make）：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700

# 或直接用 go 编译（无需 make）：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
go build -o gtunnel-server ./cmd/server
sudo ./gtunnel-server -addr :4700
```

#### OpenWrt 路由器（中高端）

适用于 NanoPi R2S/R4S/R5S、树莓派 4/5、GL.iNet 等 ARM 架构 OpenWrt 设备。

```bash
# 在线安装（路由器 SSH 执行）
wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh

# 带房间密码：
ROOM_PASSWORD=你的密码 wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh

# 或手动编译部署：
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server-openwrt-arm64  # aarch64 设备
# make server-openwrt-armv7  # armv7 设备
scp bin/gtunnel-server-openwrt-arm64 root@路由器IP:/usr/bin/gtunnel-server
```

安装脚本自动完成：二进制部署、procd init 脚本创建、防火墙 UCI 规则配置、开机自启。

管理命令：
```bash
/etc/init.d/gtunnel-server start     # 启动
/etc/init.d/gtunnel-server stop      # 停止
/etc/init.d/gtunnel-server restart   # 重启
logread | grep gtunnel               # 查看日志
```

> **推荐设备**：NanoPi R2S/R4S（ARM64，百元价位）、树莓派 4/5、GL.iNet 系列。不推荐 MIPS 架构低端路由器。

### 客户端（Windows / Linux）

**方式一：下载压缩包（推荐）**
1. 从 [Releases](https://github.com/holipay/gametunnel/releases) 下载客户端
   - Windows 64 位：`GameTunnel-Client-windows-amd64.zip`
   - Windows 32 位：`GameTunnel-Client-windows-x86.zip`
   - Linux：`GameTunnel-Client-linux-amd64.tar.gz`
2. 解压到任意文件夹
3. 用文本编辑器编辑 `config.ini`，填入服务器地址
4. 运行客户端（Linux 需要 root 权限创建虚拟网卡）：
   ```bash
   # Windows：双击 gtunnel-client.exe 或在 cmd 中运行
   # Linux：
   sudo ./gtunnel-client
   ```

配置文件 `config.ini` 示例：
```ini
server=1.2.3.4:4700
name=玩家名
room=default
password=
lang=zh
```

连接成功后，打开游戏进入局域网模式即可。

## 状态页面

服务端内置 HTTP 状态页面，可查看在线玩家、延迟、丢包率等实时信息。

### 基本用法

```bash
# 启动服务端，开放状态页面
gtunnel-server -addr :4700 -status-addr :4701

# 浏览器访问
http://1.2.3.4:4701/

# API 获取 JSON 数据
curl http://1.2.3.4:4701/api/status
```

### Token 认证

生产环境建议设置访问令牌，防止未授权查看服务器信息：

```bash
# 启动时设置 token
gtunnel-server -addr :4700 -status-addr :4701 -status-token mysecret

# 浏览器访问（URL 参数）
http://1.2.3.4:4701/?token=mysecret

# API 访问（URL 参数）
curl http://1.2.3.4:4701/api/status?token=mysecret

# API 访问（Authorization Header）
curl -H "Authorization: Bearer mysecret" http://1.2.3.4:4701/api/status
```

状态页面每 5 秒自动刷新。API 返回示例：

```json
{
  "version": "1.3",
  "uptime": "2h30m",
  "players": 3,
  "max_players": 10,
  "subnet": "10.10.0.0/24",
  "server_ip": "10.10.0.1",
  "has_auth": true,
  "send_errors": 0,
  "connections": [
    {
      "username": "玩家A",
      "virtual_ip": "10.10.0.2",
      "public_addr": "1.2.3.4:12345",
      "idle": "刚刚",
      "ping": "23ms",
      "loss": "0%",
      "jitter": "5ms"
    }
  ]
}
```

> **安全提示**：`-status-token` 仅控制状态页面的访问权限，与房间密码（`-password`）无关。两者独立运作。

### 指标 API（时间序列）

服务端内置时间序列指标采集，每分钟采样一次，保留 1 小时数据。

```bash
# 启用状态页面后访问指标 API
curl http://1.2.3.4:4701/api/metrics

# 带 token
curl -H "Authorization: Bearer mysecret" http://1.2.3.4:4701/api/metrics
```

API 返回示例：
```json
{
  "interval": "1m",
  "window": "1h",
  "samples": [
    {
      "ts": "2026-05-20T12:00:00Z",
      "players": 5,
      "relay_pkts": 1234,
      "dropped_pkts": 0,
      "avg_rtt": 23.5,
      "avg_loss": 0.01
    }
  ]
}
```

## 多房间模式

默认模式下，所有连接的玩家共享同一个虚拟子网（10.10.0.0/24），通过 `-room` 参数区分房间。

启用多房间模式后，每个房间获得独立的子网，完全隔离：

```bash
gtunnel-server -addr :4700 -rooms
```

- 每个房间自动分配独立的 /24 子网
- 不同房间之间完全隔离（不同的虚拟 IP 段）
- 适合需要多场独立对战的场景

## QoS 带宽限制

可以限制每个客户端的出站带宽，防止单个玩家占满服务器上行：

```bash
# 限制每客户端 1MB/s 出站带宽
gtunnel-server -addr :4700 -bandwidth 1048576

# 不限制（默认 10Mbps）
gtunnel-server -addr :4700 -bandwidth 0
```

## 状态持久化

服务端可以将房间状态（在线玩家、虚拟 IP 分配等）保存到磁盘，重启后自动恢复：

```bash
gtunnel-server -addr :4700 -state-dir /var/lib/gametunnel
```

- 服务端定期保存状态到指定目录
- 重启后自动加载，玩家无需重新连接
- 适合需要计划性维护重启的服务器

## 安全

### 房间密码（HMAC 认证 + 端到端加密）

设置房间密码后，同时启用 **HMAC 认证** 和 **端到端加密**：

```bash
# 服务端
gtunnel-server -addr :4700 -password mysecret

# 客户端（config.ini）
server=1.2.3.4:4700
password=mysecret
```

#### 认证流程

1. 客户端发送注册请求（房间ID + 用户名）
2. 服务端返回 16 字节随机 challenge
3. 客户端用 HKDF-SHA256 从密码派生密钥，计算 HMAC(challenge + 房间ID + 用户名 + 客户端地址)
4. 服务端验证 HMAC，通过后分配虚拟 IP
5. 认证超时 15 秒，超时需重新发起

密码不会在网络上传输。即使被中间人截获 challenge 和响应，也无法在合理时间内逆向密码。

#### 端到端加密（E2E Encryption）

设置密码后，所有游戏数据自动使用 **ChaCha20-Poly1305** 端到端加密：

- **密钥派生**：HKDF-SHA256(password, info="GameTunnel:"+roomID) → 32 字节密钥
- **加密算法**：ChaCha20-Poly1305（AEAD，256 位密钥 + 128 位认证标签）
- **Nonce 设计**：8 字节递增计数器 + 4 字节方向标签（防止收发方向 nonce 重用）
- **服务端透明中转**：服务端只转发加密字节，无法解密游戏数据
- **P2P 直连同样加密**：打洞成功后的 P2P 流量使用相同密钥加密

加密数据包格式：`[1B 版本号] [12B nonce] [N B 密文] [16B Poly1305 标签]`

> **注意**：不留空密码即可获得完整保护。密码同时保护认证和加密两个层面。

### 数据包完整性

所有协议包都包含 CRC32 校验和，丢弃损坏/篡改的包。

### 已知限制

- **无重放保护**：协议不含序列号，CRC32 不能防重放攻击。
- **无前向保密**：密码泄露后历史流量可被解密。如需前向保密请配合 WireGuard 使用。

### 服务端防护

- HMAC challenge-response 认证（密码不传输）
- 端到端加密（ChaCha20-Poly1305，密码派生密钥）
- 速率限制：每客户端 500 包/秒
- 注册速率限制：每 IP 每秒 5 次注册
- 每 IP 连接数限制：默认 3（`-max-per-ip` 可调）
- 未认证连接数上限：最大玩家数 × 3
- 源 IP 绑定：中转包的 srcIP 必须匹配发送者虚拟 IP（防 IP 伪造）
- 用户名/房间 ID 长度限制：32 字符

## 原理

```
玩家A (10.10.0.2)              玩家B (10.10.0.3)
  │                                │
  ├─ TUN 虚拟网卡                   ├─ TUN 虚拟网卡
  │  (10.10.0.2/24)                │  (10.10.0.3/24)
  │                                │
  └──UDP隧道──►  公网VPS Server  ◄──UDP隧道──┘
                (10.10.0.1)
                信令 + 中转 + 广播转发
```

- 所有人获得 10.10.0.x 的虚拟IP
- UDP 广播通过服务器转发给同房间所有人
- 自动尝试 UDP 打洞实现 P2P 直连
- 打洞失败则通过服务器中转（10人以内延迟可接受）

### P2P 直连（UDP 打洞）

GameTunnel 会自动尝试在两个客户端之间建立 P2P 直连，绕过服务器中转以降低延迟：

1. 服务器告知双方对方的公网地址
2. 双方同时向对方发送 UDP 包，打通 NAT 映射
3. 打洞分 3 个阶段（100ms / 250ms / 500ms 间隔），逐步增加尝试密度
4. 打洞成功后自动切换为 P2P 直连，并定期发送 keepalive 保持 NAT 映射
5. 若 P2P 路径超时失效，自动回退到服务器中转，并定期重试打洞

适合 P2P 直连的网络环境：
- 双方都是 Full Cone NAT 或 Restricted Cone NAT
- 至少一方不是 Symmetric NAT

不适合的环境：
- 双方都是 Symmetric NAT（运营商级 NAT 常见）

## 参数

### 服务器
```bash
gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10 -password secret
```
| 参数 | 默认值 | 说明 |
|------|--------|------|
| `-addr` | `:4700` | 监听端口 |
| `-subnet` | `10.10.0.0/24` | 虚拟子网（仅支持 /24） |
| `-max` | `10` | 最大玩家数 |
| `-password` | _(空)_ | 房间密码（留空=无认证无加密） |
| `-rooms` | `false` | 多房间模式（每个房间独立子网） |
| `-bandwidth` | `0` | 每客户端出站带宽限制（字节/秒，0=默认 10Mbps） |
| `-state-dir` | _(禁用)_ | 房间状态持久化目录（重启后恢复房间状态） |
| `-status-addr` | _(禁用)_ | 状态页面地址 (HTTP)，如 `:4701` |
| `-status-token` | _(空)_ | 状态页访问令牌（留空=无认证） |
| `-max-per-ip` | `3` | 每个公网 IP 最大连接数 |
| `-lang` | `zh` | 语言（`zh` 中文 / `en` 英文） |
| `-version` | | 显示版本 |

### 客户端

客户端通过 `config.ini` 配置（位于可执行文件同目录），不支持命令行参数。

| 字段 | 默认值 | 说明 |
|------|--------|------|
| `server` | _(必填)_ | 服务器地址（IP:端口 或 域名:端口） |
| `name` | 计算机名 | 玩家名称（最长 32 字符） |
| `room` | `default` | 房间ID（同房间玩家互通，最长 32 字符） |
| `password` | _(空)_ | 房间密码（留空=无密码，设置后自动启用认证+加密） |
| `lang` | `zh` | 语言（`zh` 中文 / `en` 英文） |
| `mtu` | `1400` | 隧道MTU（576-9000，一般无需修改） |

配置文件优先级：`config.ini`（exe 同目录）> `%APPDATA%\GameTunnel\config.json`

配置文件支持 `lang` 字段设置语言：`zh`（中文，默认）或 `en`（英文）。

### 配置文件完整示例

```ini
# 服务器地址（IP:端口 或 域名:端口）
server=1.2.3.4:4700

# 玩家名称（最长 32 字符，默认使用计算机名）
name=星际玩家001

# 房间 ID（同一房间的玩家互通，默认 default）
room=starcraft

# 房间密码（留空=无密码，设置后自动启用认证+加密）
password=myroomsecret

# 语言（zh=中文, en=英文）
lang=zh

# 隧道 MTU（576-9000，默认 1400，一般无需修改）
mtu=1400
```

## 防火墙

服务器需要开放 UDP 4700（隧道端口），如果启用了状态页面还需要开放对应 TCP 端口：

```bash
# iptables
iptables -A INPUT -p udp --dport 4700 -j ACCEPT
iptables -A INPUT -p tcp --dport 4701 -j ACCEPT   # 状态页面

# ufw
ufw allow 4700/udp
ufw allow 4701/tcp    # 状态页面

# firewalld
firewall-cmd --add-port=4700/udp --permanent
firewall-cmd --add-port=4701/tcp --permanent   # 状态页面
firewall-cmd --reload
```

## 常见问题

**Q: 为什么不用 ZeroTier/Tailscale？**
A: 可以用，但 GameTunnel 更轻量、无依赖，专门为局域网游戏优化了广播转发，开箱即用。

**Q: 客户端需要管理员/root 权限吗？**
A: 是的。创建虚拟网卡需要管理员权限（Windows UAC 提示 / Linux sudo）。

**Q: 延迟多少？**
A: 取决于到服务器的延迟。如果服务器在国内 VPS，通常 20-50ms。打洞成功后 P2P 直连延迟更低。

**Q: 支持哪些游戏？**
A: 支持所有基于 IP 的局域网游戏。广播转发已内置，适用于依赖 UDP 广播发现的游戏（如星际争霸、红警、帝国时代等）。

**Q: 支持哪些操作系统？**
A: 服务端支持 Linux 和 OpenWrt 路由器（中高端 ARM 设备）。客户端支持 Windows 10+ 和 Linux。

**Q: 数据安全吗？**
A: 设置密码后，认证（HMAC-SHA256）和数据传输（ChaCha20-Poly1305）都是端到端加密的，服务端无法解密游戏数据。不留空密码即可。

**Q: 多个房间可以共用一个服务器吗？**
A: 可以。不同 `-room` 的玩家互相隔离。启用 `-rooms` 模式后，每个房间还会获得独立的虚拟子网，完全隔离。

**Q: P2P 打洞失败怎么办？**
A: 自动回退到服务器中转，延迟会略高但不影响游戏。GameTunnel 会定期重试打洞。

**Q: 双击客户端没反应 / 无法运行？**
A: Windows 可能会静默阻止从网络下载的程序。右键 `gtunnel-client.exe` → **属性** → 底部勾选 **"解除锁定"（Unblock）** → 确定，然后重新运行。

**Q: Linux 客户端提示 "operation not permitted"？**
A: Linux 需要 root 权限创建虚拟网卡，使用 `sudo ./gtunnel-client` 运行。

**Q: 如何查看服务器状态？**
A: 使用 `-status-addr :4701` 启动状态页面，浏览器或 curl 访问。详见上方「状态页面」章节。

## 开发

### 环境要求

- Go 1.26+

### 编译

```bash
# 直接用 go 编译（推荐，无需 make）
go build -o bin/gtunnel-server ./cmd/server
go build -o bin/gtunnel-client.exe ./cmd/client     # Linux 本地编译
GOOS=windows GOARCH=amd64 go build -o bin/gtunnel-client.exe ./cmd/client  # 交叉编译 Windows

# 或使用 make（适合批量编译和发布）
make server          # 编译服务端
make client          # 编译 Windows 客户端
make server-openwrt  # 编译所有 OpenWrt 架构
make all             # 编译所有目标

# 查看版本
./bin/gtunnel-server -version
```

### 依赖说明

- **Windows**：`wintun` 模块版本由 `wireguard-go` 锁定，已固定在 go.mod 中

如遇依赖问题：
```bash
GOPROXY=direct go mod tidy
```

## License

MIT
