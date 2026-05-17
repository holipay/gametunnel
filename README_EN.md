# GameTunnel 🎮

> [中文版](README.md)

A universal LAN gaming tunnel that lets players in different locations play together as if they were on the same local network.

Supports all IP-based LAN games with built-in UDP broadcast forwarding (for games that rely on broadcast discovery).

## Quick Start

### Server (Public VPS)

#### Linux

```bash
# One-click install (downloads pre-built binary from GitHub Releases)
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-server.sh | sudo bash

# With room password:
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-server.sh | sudo ROOM_PASSWORD=yourpassword bash

# With status page:
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/install-server.sh | sudo STATUS_ADDR=:4701 bash

# Or build from source:
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700
```

#### OpenWrt Router (Mid-to-High-End)

For NanoPi R2S/R4S/R5S, Raspberry Pi 4/5, GL.iNet and other ARM-based OpenWrt devices.

```bash
# Online install (run via router SSH)
wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh

# With room password:
ROOM_PASSWORD=yourpassword wget -qO- https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-openwrt.sh | sh

# Or build and deploy manually:
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server-openwrt-arm64  # for aarch64 devices
# make server-openwrt-armv7  # for armv7 devices
scp bin/gtunnel-server-openwrt-arm64 root@router-ip:/usr/bin/gtunnel-server
```

The install script handles: binary deployment, procd init script, UCI firewall rules, and auto-start.

Management:
```bash
/etc/init.d/gtunnel-server start     # Start
/etc/init.d/gtunnel-server stop      # Stop
/etc/init.d/gtunnel-server restart   # Restart
logread | grep gtunnel               # View logs
```

> **Recommended devices**: NanoPi R2S/R4S (ARM64, budget-friendly), Raspberry Pi 4/5, GL.iNet series. Low-end MIPS routers are not recommended.

### Player (Windows PC)

**Option 1: Download archive (recommended)**
1. Download the client from [Releases](https://github.com/holipay/gametunnel/releases)
   - 64-bit: `GameTunnel-Client-windows-amd64.zip`
   - **32-bit (older PCs/retro games)**: `GameTunnel-Client-windows-x86.zip`
2. Extract to any folder (3 files total)
3. Edit `config.ini` with your server address
4. Double-click `gtunnel-client.exe` — it will auto-request admin privileges and connect

Config file `config.ini` example:
```ini
server=1.2.3.4:4700
name=PlayerName
room=default
password=
lang=en
```

Once connected, launch your game and enter LAN mode.

## Security

### Room Password (HMAC Authentication)

When the server has a password set, clients must pass HMAC challenge-response authentication:

```bash
# Server
gtunnel-server -addr :4700 -password mysecret

# Client
gtunnel-client -server 1.2.3.4:4700 -password mysecret
```

Authentication flow:
1. Client sends registration request (room ID + username)
2. Server returns a 16-byte random challenge
3. Client derives a key via HKDF-SHA256 from the password, computes HMAC(challenge + roomID + username + client address)
4. Server verifies the HMAC and assigns a virtual IP on success
5. Auth timeout is 15 seconds — if exceeded, the client must retry

The password is never transmitted over the network. Even if a MITM intercepts the challenge and response, reversing the password is computationally infeasible.

### Packet Integrity

All protocol packets include a CRC32 checksum; corrupted or tampered packets are silently dropped.

### Known Limitations

- **No encryption**: Game data is relayed in plaintext with only CRC32 integrity checks. Use WireGuard or similar VPN for encryption.
- **No replay protection**: The protocol has no sequence numbers; CRC32 does not prevent replay attacks.

### Server Protections

- HMAC challenge-response authentication (password never transmitted)
- Rate limiting: 500 packets/sec per client
- Unauthenticated connection cap: max players × 3
- Source IP binding: relayed packets' srcIP must match the sender's virtual IP (prevents IP spoofing)
- Username / room ID length limit: 32 characters
- Config file permissions 0600 (owner-only read)

## How It Works

```
Player A (10.10.0.2)           Player B (10.10.0.3)
  │                                │
  ├─ TUN virtual NIC               ├─ TUN virtual NIC
  │  (10.10.0.2/24)                │  (10.10.0.3/24)
  │                                │
  └──UDP tunnel──►  Public VPS  ◄──UDP tunnel──┘
                   (10.10.0.1)
                   Signaling + Relay + Broadcast
```

- Everyone gets a 10.10.0.x virtual IP
- UDP broadcasts are forwarded by the server to all players in the same room
- Automatic UDP hole punching for P2P direct connection
- Falls back to server relay if hole punching fails (acceptable latency for ≤10 players)

## Parameters

### Server
```bash
gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10 -password secret
```
| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:4700` | Listen address |
| `-subnet` | `10.10.0.0/24` | Virtual subnet (CIDR) |
| `-max` | `10` | Max players |
| `-password` | _(empty)_ | Room password (empty = no auth) |
| `-status-addr` | _(disabled)_ | HTTP status page address, e.g. `:4701` |
| `-status-token` | _(empty)_ | Status page access token (empty = no auth) |
| `-lang` | `zh` | Language (`zh` Chinese / `en` English) |
| `-version` | | Show version |

### Client
```bash
gtunnel-client -server 1.2.3.4:4700 -name PlayerName -room roomID -password secret
```
| Flag | Default | Description |
|------|---------|-------------|
| `-server` | _(required or config file)_ | Server address |
| `-name` | Computer name | Player name (max 32 chars) |
| `-room` | `default` | Room ID (players in the same room can communicate, max 32 chars) |
| `-password` | _(empty)_ | Room password |
| `-mtu` | `1400` | Tunnel MTU |
| `-version` | | Show version |

Config file priority: `config.ini` (same directory as exe) > `%APPDATA%\GameTunnel\config.json`

The config file supports a `lang` field to set the language: `zh` (Chinese, default) or `en` (English).

## Firewall

The server needs UDP port 4700 open:
```bash
# iptables
iptables -A INPUT -p udp --dport 4700 -j ACCEPT

# ufw
ufw allow 4700/udp

# firewalld
firewall-cmd --add-port=4700/udp --permanent
firewall-cmd --reload
```

## FAQ

**Q: Why not use ZeroTier/Tailscale?**
A: You can, but this tool is lighter, has no dependencies, and is specifically optimized for LAN game broadcast forwarding.

**Q: Does the client need admin privileges?**
A: Yes. Creating a virtual NIC requires Windows administrator rights (UAC prompt).

**Q: What's the latency?**
A: Depends on the round-trip to the server. With a domestic VPS, typically 20-50ms. P2P direct connection via hole punching has even lower latency.

**Q: Which games are supported?**
A: All IP-based LAN games. Broadcast forwarding is built-in, supporting games that rely on UDP broadcast discovery (e.g. StarCraft, Red Alert, Age of Empires, etc.).

**Q: Which operating systems are supported?**
A: Server: Linux and OpenWrt routers (mid-to-high-end ARM devices). Client: Windows 10+.

**Q: Is data secure?**
A: Authentication is secure (HMAC-SHA256 + HKDF), but game data is transmitted in plaintext. Use WireGuard for encryption.

## Development

### Requirements

- Go 1.25+

### Build

```bash
# Build server (Linux)
make server

# Build OpenWrt server (ARM64 / ARMv7)
make server-openwrt-arm64
make server-openwrt-armv7
make server-openwrt           # All OpenWrt architectures

# Build Windows client (cross-compilable from any platform)
make client

# Build all targets
make all

# Show version
./bin/gtunnel-server -version
./bin/gtunnel-client.exe -version
```

### Dependencies

- **Windows**: `wintun` module version is pinned by `wireguard-go`, locked in go.mod

If you encounter dependency issues:
```bash
GOPROXY=direct go mod tidy
```

## License

MIT
