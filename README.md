# GameTunnel 🎮

> [中文版](README.zh.md)

A universal LAN gaming tunnel that lets players in different locations play together as if they were on the same local network.

Supports all IP-based LAN games with built-in UDP broadcast/multicast forwarding (for games that rely on broadcast discovery, mDNS, SSDP, etc.).

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

# Or build from source (requires make):
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
make server
sudo ./bin/gtunnel-server -addr :4700

# Or build directly with go (no make needed):
git clone https://github.com/holipay/gametunnel.git
cd gametunnel
go build -o gtunnel-server ./cmd/server
sudo ./gtunnel-server -addr :4700
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

### Client (Windows / Linux)

**Option 1: Download archive (recommended)**
1. Download the client from [Releases](https://github.com/holipay/gametunnel/releases)
   - Windows 64-bit: `GameTunnel-Client-windows-amd64.zip`
   - Windows 32-bit: `GameTunnel-Client-windows-x86.zip`
   - Linux: `GameTunnel-Client-linux-amd64.tar.gz`
2. Extract to any folder
3. Edit `config.ini` with your server address
4. Run the client (Linux requires root for TUN device):
   ```bash
   # Windows: double-click gtunnel-client.exe or run in cmd
   # Linux:
   sudo ./gtunnel-client
   ```

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
  "version": "1.16",
  "uptime": "2h30m",
  "players": 3,
  "max_players": 10,
  "subnet": "10.10.0.0/24",
  "server_ip": "10.10.0.1",
  "has_auth": true,
  "send_errors": 0,
  "multi_room": false,
  "connections": [
    {
      "username": "PlayerA",
      "virtual_ip": "10.10.0.2",
      "public_addr": "1.2.3.4:12345",
      "idle": "just now",
      "ping": "23ms",
      "loss": "0%",
      "jitter": "5ms",
      "client_version": "v1.16",
      "nat_type": "cone"
    }
  ],
  "total_registrations": 42,
  "auth_failures": 0,
  "peak_players": 8,
  "total_packets_relay": 123456,
  "total_packets_dropped": 0,
  "total_kicks": 0
}
```

> **Note**: `-status-token` only controls access to the status page. It is independent of the room password (`-password`).

### Rooms API

When multi-room mode is enabled, query room list via `/api/rooms`:

```bash
curl http://1.2.3.4:4701/api/rooms
```

### Metrics API (Time Series)

The server collects time-series metrics with 1-minute sampling and a 1-hour window.

```bash
# Access metrics API (requires status page enabled)
curl http://1.2.3.4:4701/api/metrics

# With token
curl -H "Authorization: Bearer mysecret" http://1.2.3.4:4701/api/metrics
```

API response example:
```json
{
  "interval": "1m",
  "window": "1h",
  "samples": [
    {
      "t": 1747987200,
      "p": 5,
      "rp": 1234,
      "dp": 0,
      "r": 23.5,
      "l": 0.01
    }
  ]
}
```

## Multi-Room Mode

By default, all connected players share the same virtual subnet (10.10.0.0/24), distinguished by the `-room` parameter.

With multi-room mode enabled, each room gets an independent subnet with full isolation:

```bash
gtunnel-server -addr :4700 -rooms
```

- Each room is automatically assigned an independent /24 subnet
- Full isolation between rooms (different virtual IP ranges)
- `-max-rooms` (default 64) limits the number of auto-created rooms
- Ideal for scenarios requiring multiple independent matches

## QoS Bandwidth Limiting

You can limit per-client outbound bandwidth to prevent a single player from saturating the server's upstream:

```bash
# Limit each client to 1MB/s outbound bandwidth
gtunnel-server -addr :4700 -bandwidth 1048576

# No limit (default 10Mbps)
gtunnel-server -addr :4700 -bandwidth 0
```

## State Persistence

The server can save room state (online players, virtual IP assignments, etc.) to disk and automatically restore it on restart:

```bash
gtunnel-server -addr :4700 -state-dir /var/lib/gametunnel
```

- Server periodically saves state to the specified directory
- Automatically loads on restart — players don't need to reconnect
- Ideal for servers that require planned maintenance restarts

## Security

### Room Password (HMAC Authentication + End-to-End Encryption)

Setting a room password enables both **HMAC authentication** and **end-to-end encryption**:

```bash
# Server
gtunnel-server -addr :4700 -password mysecret

# Client (config.ini)
server=1.2.3.4:4700
password=mysecret
```

#### Authentication Flow

1. Client sends registration request (room ID + username)
2. Server returns a 16-byte random challenge
3. Client derives a key via HKDF-SHA256 from the password, computes HMAC(challenge + roomID + username + client address)
4. Server verifies the HMAC and assigns a virtual IP on success
5. Auth timeout is 15 seconds — if exceeded, the client must retry

The password is never transmitted over the network. Even if a MITM intercepts the challenge and response, reversing the password is computationally infeasible.

#### End-to-End Encryption (E2E Encryption)

With a password set, all game data is automatically encrypted with **ChaCha20-Poly1305**, with **ECDH** forward secrecy:

- **Key derivation**: HKDF-SHA256(password, info="GameTunnel:"+roomID) → 32-byte key
- **Forward secrecy**: ECDH (X25519) key exchange — ephemeral key pairs generated per connection
- **Algorithm**: ChaCha20-Poly1305 (AEAD, 256-bit key + 128-bit auth tag)
- **Nonce design**: 8-byte incrementing counter + 4-byte direction tag (prevents nonce reuse between send/receive)
- **Server transparency**: Server only relays encrypted bytes — it cannot decrypt game data
- **P2P encryption**: Traffic via hole-punched P2P connections uses the same encryption

Encrypted packet format: `[1B version] [12B nonce] [N B ciphertext] [16B Poly1305 tag]`

> **Tip**: Simply set a password to get full protection. The password secures both authentication and encryption.

### Packet Integrity

All protocol packets include a CRC32 checksum; corrupted or tampered packets are silently dropped. For encrypted relay packets, CRC is omitted (AEAD provides integrity).

### Known Limitations

- **No replay protection**: The protocol has no sequence numbers; CRC32 does not prevent replay attacks.

### Server Protections

- HMAC challenge-response authentication (password never transmitted)
- End-to-end encryption (ChaCha20-Poly1305, password-derived key)
- Rate limiting: 500 packets/sec per client (token bucket)
- Registration rate limit: 5 registrations per IP per second
- Per-IP connection limit: default 3 (configurable via `-max-per-ip`)
- Unauthenticated connection cap: max players × 3
- Source IP binding: relayed packets' srcIP must match the sender's virtual IP (prevents IP spoofing)
- Username / room ID length limit: 32 characters
- Password strength checking with warnings for weak passwords

### TCP Fallback

When UDP is blocked by firewalls, the client automatically falls back to TCP:

```bash
# Server: enable TCP fallback
gtunnel-server -addr :4700 -tcp-addr :4700

# Client: automatically attempts TCP after UDP failure
```

TCP fallback uses a transparent transport bridge — the game protocol is unaffected. A read idle timeout prevents stale connections.

## How It Works

```
Player A (10.10.0.2)           Player B (10.10.0.3)
  │                                │
  ├─ TUN virtual NIC               ├─ TUN virtual NIC
  │  (10.10.0.2/24)                │  (10.10.0.3/24)
  │                                │
  └──UDP tunnel──►  Public VPS  ◄──UDP tunnel──┘
                   (10.10.0.1)         │
                   Signaling + Relay    └── TCP fallback (when UDP is blocked)
```

- Everyone gets a 10.10.0.x virtual IP
- UDP broadcasts and multicasts (mDNS, SSDP, UPnP) are forwarded by the server to all players in the same room
- Automatic UDP hole punching for P2P direct connection
- Falls back to server relay if hole punching fails (acceptable latency for ≤10 players)
- Automatically falls back to TCP when UDP is blocked by firewalls
- Client auto-reconnects on disconnect with exponential backoff
- Session rebinding handles client IP/port changes gracefully

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
| `-addr` | `:4700` | UDP listen address |
| `-subnet` | `10.10.0.0/24` | Virtual subnet (CIDR, /24 only) |
| `-max` | `10` | Max players |
| `-password` | _(empty)_ | Room password (empty = no auth, no encryption) |
| `-tcp-addr` | _(empty)_ | TCP fallback listen address (e.g. `:4700`, empty = disabled) |
| `-rooms` | `false` | Multi-room mode (each room gets independent subnet) |
| `-max-rooms` | `64` | Max auto-created rooms in multi-room mode |
| `-bandwidth` | `0` | Per-client outbound bandwidth limit in bytes/sec (0 = default 10Mbps) |
| `-state-dir` | _(disabled)_ | Room state persistence directory (survives restarts) |
| `-status-addr` | _(disabled)_ | HTTP status page address, e.g. `:4701` |
| `-status-token` | _(empty)_ | Status page access token (empty = no auth) |
| `-max-per-ip` | `3` | Max connections per public IP |
| `-lang` | `zh` | Language (`zh` / `en`) |
| `-verbose` | `false` | Enable verbose relay logging |
| `-log-file` | _(empty)_ | Log file path (empty = stderr only) |
| `-pprof-addr` | _(disabled)_ | pprof HTTP address for runtime profiling (e.g. `localhost:6060`) |
| `-version` | | Show version info and exit |

### Client

The client is configured via `config.ini` (located next to the executable). No command-line flags are supported.

| Field | Default | Description |
|-------|---------|-------------|
| `server` | _(required)_ | Server address (`host:port`, supports IPv6 like `[::1]:4700`) |
| `port` | `4700` | Server port (used if `server` is host-only) |
| `name` | Computer name | Player name (max 32 chars) |
| `room` | `default` | Room ID (players in the same room can communicate, max 32 chars) |
| `password` | _(empty)_ | Room password (empty = no password; setting it enables auth + encryption) |
| `lang` | `zh` | Language (`zh` / `en`) |
| `mtu` | `1400` | Tunnel MTU (576-9000, usually no need to change) |
| `log-file` | _(empty)_ | Log file path (empty = stderr only) |
| `verbose` | `false` | Verbose logging |
| `pprof-addr` | _(disabled)_ | pprof HTTP address for runtime profiling (e.g. `localhost:6061`) |

Config file priority: `config.ini` (same directory as exe) > `%APPDATA%\GameTunnel\config.json` (Windows) / `~/.config/GameTunnel/config.json` (Linux)

### Full Config File Example

```ini
# Server address (host:port, supports IPv6 like [::1]:4700)
server=1.2.3.4:4700

# Player name (max 32 chars, defaults to computer name)
name=Player001

# Room ID (players in the same room can communicate, default: default)
room=starcraft

# Room password (empty = no password; setting it enables auth + encryption)
password=myroomsecret

# Language (zh or en)
lang=en

# Tunnel MTU (576-9000, default 1400, usually no need to change)
mtu=1400

# Log file path (empty = stderr only)
#log-file=

# Verbose logging (true / false)
#verbose=false

# pprof HTTP address for runtime profiling (empty = disabled)
#pprof-addr=
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

**Q: Does the client need admin/root privileges?**
A: Yes. Creating a virtual NIC requires admin privileges (Windows UAC prompt / Linux sudo).

**Q: What's the latency?**
A: Depends on the round-trip to the server. With a domestic VPS, typically 20-50ms. P2P direct connection via hole punching has even lower latency.

**Q: Which games are supported?**
A: All IP-based LAN games. Broadcast and multicast forwarding (UDP broadcast, mDNS, SSDP, UPnP) is built-in, supporting games like StarCraft, Red Alert, Age of Empires, and more.

**Q: Which operating systems are supported?**
A: Server: Linux and OpenWrt routers (mid-to-high-end ARM devices). Client: Windows 10+ and Linux.

**Q: Is data secure?**
A: With a password set, both authentication (HMAC-SHA256) and data transmission (ChaCha20-Poly1305) are end-to-end encrypted. The server cannot decrypt game data.

**Q: Can multiple rooms share one server?**
A: Yes. Different `-room` values are isolated from each other. With `-rooms` mode enabled, each room also gets an independent virtual subnet for full isolation.

**Q: What if P2P hole punching fails?**
A: Automatically falls back to server relay with slightly higher latency. GameTunnel periodically retries hole punching.

**Q: The client doesn't respond when I double-click it / won't run?**
A: Windows may silently block executables downloaded from the internet (Mark of the Web / Zone Identifier). Right-click `gtunnel-client.exe` → **Properties** → check **"Unblock"** at the bottom → OK, then run again.

**Q: Linux client says "operation not permitted"?**
A: Linux requires root privileges to create a virtual NIC. Run with `sudo ./gtunnel-client`.

**Q: How do I check server status?**
A: Use `-status-addr :4701` to enable the status page, then access via browser or curl. See the "Status Page" section above.

**Q: What happens if the client disconnects?**
A: The client automatically reconnects with exponential backoff. Session rebinding ensures seamless recovery even if your IP/port changes.

**Q: Does the server support IPv6?**
A: Yes. The server address supports IPv6 with brackets, e.g. `server=[2408::1]:4700`.

## Additional Documentation

Detailed technical documentation is available in the `docs/` directory:
- [Network Analysis](docs/network-analysis.md) — deep dive into data flow, connection establishment, and component architecture
- [Use Cases](docs/use-cases.md) — design philosophy, protocol support matrix, and route architecture
- [Branch Management Guide](docs/branch-management-guide.md) — project workflow and PR hygiene

## Development

### Requirements

- Go 1.26+

### Build

```bash
# Build directly with go (recommended, no make needed)
go build -o bin/gtunnel-server ./cmd/server
go build -o bin/gtunnel-client.exe ./cmd/client     # Linux native build
GOOS=windows GOARCH=amd64 go build -o bin/gtunnel-client.exe ./cmd/client  # cross-compile for Windows

# Or use make (convenient for batch builds and releases)
make server          # Build server
make client          # Build Windows client
make server-openwrt  # Build all OpenWrt architectures
make all             # Build all targets

# Show version
./bin/gtunnel-server -version
```

### Dependencies

- **Windows**: `wintun` module version is pinned by `wireguard-go`, locked in go.mod

If you encounter dependency issues:
```bash
GOPROXY=direct go mod tidy
```

## License

MIT
