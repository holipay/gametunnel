# GameTunnel 🎮

> [中文版](README.md)

A universal LAN gaming tunnel that lets players in different locations play together as if they were on the same local network.

Supports all IP-based LAN games with built-in UDP broadcast forwarding (for games that rely on broadcast discovery).

## Quick Start

### Server (Public VPS)

#### Linux

```bash
# One-click install (downloads pre-built binary from GitHub Releases)
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo bash

# With room password:
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo ROOM_PASSWORD=yourpassword bash

# With status page:
curl -sL https://raw.githubusercontent.com/holipay/gametunnel/main/scripts/install-linux.sh | sudo STATUS_ADDR=:4701 bash

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

## Status Page

The server includes a built-in HTTP status page for viewing online players, latency, packet loss, and other real-time information.

### Basic Usage

```bash
# Start server with status page
gtunnel-server -addr :4700 -status-addr :4701

# Open in browser
http://1.2.3.4:4701/

# API (returns JSON)
curl http://1.2.3.4:4701/api/status
```

### Token Authentication

In production, set an access token to prevent unauthorized access:

```bash
# Set token at startup
gtunnel-server -addr :4700 -status-addr :4701 -status-token mysecret

# Browser access (URL parameter)
http://1.2.3.4:4701/?token=mysecret

# API access (URL parameter)
curl http://1.2.3.4:4701/api/status?token=mysecret

# API access (Authorization header)
curl -H "Authorization: Bearer mysecret" http://1.2.3.4:4701/api/status
```

The status page auto-refreshes every 5 seconds. API response example:

```json
{
  "version": "1.0.0",
  "uptime": "2h30m",
  "players": 3,
  "max_players": 10,
  "subnet": "10.10.0.0/24",
  "server_ip": "10.10.0.1",
  "has_auth": true,
  "send_errors": 0,
  "connections": [
    {
      "username": "PlayerA",
      "virtual_ip": "10.10.0.2",
      "public_addr": "1.2.3.4:12345",
      "idle": "just now",
      "ping": "23ms",
      "loss": "0%",
      "jitter": "5ms"
    }
  ]
}
```

> **Note**: `-status-token` only controls access to the status page. It is independent of the room password (`-password`).

## Security

### Room Password (HMAC Authentication + End-to-End Encryption)

Setting a room password enables both **HMAC authentication** and **end-to-end encryption**:

```bash
# Server
gtunnel-server -addr :4700 -password mysecret

# Client
gtunnel-client -server 1.2.3.4:4700 -password mysecret
```

#### Authentication Flow

1. Client sends registration request (room ID + username)
2. Server returns a 16-byte random challenge
3. Client derives a key via HKDF-SHA256 from the password, computes HMAC(challenge + roomID + username + client address)
4. Server verifies the HMAC and assigns a virtual IP on success
5. Auth timeout is 15 seconds — if exceeded, the client must retry

The password is never transmitted over the network. Even if a MITM intercepts the challenge and response, reversing the password is computationally infeasible.

#### End-to-End Encryption (E2E Encryption)

With a password set, all game data is automatically encrypted with **ChaCha20-Poly1305**:

- **Key derivation**: HKDF-SHA256(password, info="GameTunnel:"+roomID) → 32-byte key
- **Algorithm**: ChaCha20-Poly1305 (AEAD, 256-bit key + 128-bit auth tag)
- **Nonce design**: 8-byte incrementing counter + 4-byte direction tag (prevents nonce reuse between send/receive)
- **Server transparency**: Server only relays encrypted bytes — it cannot decrypt game data
- **P2P encryption**: Traffic via hole-punched P2P connections uses the same encryption

Encrypted packet format: `[1B version] [12B nonce] [N B ciphertext] [16B Poly1305 tag]`

> **Tip**: Simply set a password to get full protection. The password secures both authentication and encryption.

### Packet Integrity

All protocol packets include a CRC32 checksum; corrupted or tampered packets are silently dropped.

### Known Limitations

- **No replay protection**: The protocol has no sequence numbers; CRC32 does not prevent replay attacks.
- **No forward secrecy**: If the password is compromised, historical traffic can be decrypted. Use WireGuard for forward secrecy.

### Server Protections

- HMAC challenge-response authentication (password never transmitted)
- End-to-end encryption (ChaCha20-Poly1305, password-derived key)
- Rate limiting: 500 packets/sec per client
- Registration rate limit: 5 registrations per IP per second
- Per-IP connection limit: default 3 (configurable via `-max-per-ip`)
- Unauthenticated connection cap: max players × 3
- Source IP binding: relayed packets' srcIP must match the sender's virtual IP (prevents IP spoofing)
- Username / room ID length limit: 32 characters

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

### P2P Direct Connection (UDP Hole Punching)

GameTunnel automatically attempts to establish P2P direct connections between clients, bypassing the server relay for lower latency:

1. Server informs both clients of each other's public addresses
2. Both clients simultaneously send UDP packets to punch through NAT mappings
3. Hole punching runs in 3 phases (100ms / 250ms / 500ms intervals) with increasing density
4. On success, traffic switches to P2P with periodic keepalives to maintain NAT mappings
5. If the P2P path times out, falls back to server relay and retries periodically

Environments suitable for P2P:
- Both sides have Full Cone or Restricted Cone NAT
- At least one side is not behind Symmetric NAT

Environments where P2P typically fails:
- Both sides are behind Symmetric NAT (common with carrier-grade NAT)

## Parameters

### Server
```bash
gtunnel-server -addr :4700 -subnet 10.10.0.0/24 -max 10 -password secret
```
| Flag | Default | Description |
|------|---------|-------------|
| `-addr` | `:4700` | Listen address |
| `-subnet` | `10.10.0.0/24` | Virtual subnet (only /24 supported) |
| `-max` | `10` | Max players |
| `-password` | _(empty)_ | Room password (empty = no auth, no encryption) |
| `-status-addr` | _(disabled)_ | HTTP status page address, e.g. `:4701` |
| `-status-token` | _(empty)_ | Status page access token (empty = no auth) |
| `-max-per-ip` | `3` | Max connections per public IP |
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
| `-mtu` | `1400` | Tunnel MTU (576-9000) |
| `-lang` | `zh` | Language (`zh` Chinese / `en` English) |
| `-version` | | Show version |

Config file priority: `config.ini` (same directory as exe) > `%APPDATA%\GameTunnel\config.json`

The config file supports a `lang` field to set the language: `zh` (Chinese, default) or `en` (English).

### Full Config File Example

```ini
# Server address (IP:port or domain:port)
server=1.2.3.4:4700

# Player name (max 32 chars, defaults to computer name)
name=Player001

# Room ID (players in the same room can communicate, default: default)
room=starcraft

# Room password (empty = no password; setting it enables auth + encryption)
password=myroomsecret

# Language (zh=Chinese, en=English)
lang=en

# Tunnel MTU (576-9000, default 1400, usually no need to change)
mtu=1400
```

## Firewall

The server needs UDP port 4700 (tunnel) open. If the status page is enabled, also open the corresponding TCP port:

```bash
# iptables
iptables -A INPUT -p udp --dport 4700 -j ACCEPT
iptables -A INPUT -p tcp --dport 4701 -j ACCEPT   # status page

# ufw
ufw allow 4700/udp
ufw allow 4701/tcp    # status page

# firewalld
firewall-cmd --add-port=4700/udp --permanent
firewall-cmd --add-port=4701/tcp --permanent   # status page
firewall-cmd --reload
```

## FAQ

**Q: Why not use ZeroTier/Tailscale?**
A: You can, but GameTunnel is lighter, has no dependencies, and is specifically optimized for LAN game broadcast forwarding — it just works.

**Q: Does the client need admin privileges?**
A: Yes. Creating a virtual NIC requires Windows administrator rights (UAC prompt).

**Q: What's the latency?**
A: Depends on the round-trip to the server. With a domestic VPS, typically 20-50ms. P2P direct connection via hole punching has even lower latency.

**Q: Which games are supported?**
A: All IP-based LAN games. Broadcast forwarding is built-in, supporting games that rely on UDP broadcast discovery (e.g. StarCraft, Red Alert, Age of Empires, etc.).

**Q: Which operating systems are supported?**
A: Server: Linux and OpenWrt routers (mid-to-high-end ARM devices). Client: Windows 10+.

**Q: Is data secure?**
A: With a password set, both authentication (HMAC-SHA256) and data transmission (ChaCha20-Poly1305) are end-to-end encrypted. The server cannot decrypt game data.

**Q: Can multiple rooms share one server?**
A: Yes. Different `-room` values are isolated from each other. However, all rooms share the same `-password` (if set).

**Q: What if P2P hole punching fails?**
A: Automatically falls back to server relay with slightly higher latency. GameTunnel periodically retries hole punching.

**Q: How do I check server status?**
A: Use `-status-addr :4701` to enable the status page, then access via browser or curl. See the "Status Page" section above.

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
