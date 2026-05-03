// GameTunnel Client — runs on each player's machine.
//
// Creates a TUN device, connects to the relay server, and tunnels
// all game traffic through the virtual network.
//
// Usage:
//   client -server 1.2.3.4:4700 -room mygame -name Player1
package main

import (
	"encoding/binary"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gametunnel/internal/protocol"
	"github.com/gametunnel/internal/tun"
)

// ── Peer State ─────────────────────────────────────────────────

type Peer struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
	LastSeen   time.Time
	// Hole punch state
	Punching bool
}

// ── Client ─────────────────────────────────────────────────────

type Client struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	tunDev     *tun.Device
	username   string
	roomID     string
	virtualIP  net.IP
	serverIP   net.IP
	peers      map[string]*Peer // virtualIP → Peer
	mu         sync.RWMutex
	done       chan struct{}
}

func main() {
	serverAddr := flag.String("server", "127.0.0.1:4700", "server address (host:port)")
	roomID := flag.String("room", "default", "room ID")
	username := flag.String("name", "", "your display name")
	mtu := flag.Int("mtu", 1400, "tunnel MTU")
	flag.Parse()

	if *username == "" {
		hostname, _ := os.Hostname()
		*username = hostname
	}

	// Resolve server address
	sAddr, err := net.ResolveUDPAddr("udp4", *serverAddr)
	if err != nil {
		log.Fatalf("invalid server address %q: %v", *serverAddr, err)
	}

	// Bind local UDP socket
	localAddr, _ := net.ResolveUDPAddr("udp4", ":0")
	conn, err := net.ListenUDP("udp4", localAddr)
	if err != nil {
		log.Fatalf("bind UDP: %v", err)
	}

	c := &Client{
		conn:       conn,
		serverAddr: sAddr,
		username:   *username,
		roomID:     *roomID,
		peers:      make(map[string]*Peer),
		done:       make(chan struct{}),
	}

	// Register with server
	if err := c.register(); err != nil {
		log.Fatalf("register failed: %v", err)
	}

	// Create TUN device
	cfg := tun.Config{
		VirtualIP:  c.virtualIP,
		SubnetMask: net.CIDRMask(24, 24),
		ServerIP:   c.serverIP,
		MTU:        *mtu,
	}
	tunDev, err := tun.New(cfg)
	if err != nil {
		log.Fatalf("create TUN: %v", err)
	}
	defer tunDev.Close()
	c.tunDev = tunDev

	log.Printf("╔═══════════════════════════════════════════╗")
	log.Printf("║       GameTunnel Connected                ║")
	log.Printf("╠═══════════════════════════════════════════╣")
	log.Printf("║  Name:    %-31s ║", c.username)
	log.Printf("║  Room:    %-31s ║", c.roomID)
	log.Printf("║  VIP:     %-31s ║", c.virtualIP)
	log.Printf("║  Server:  %-31s ║", c.serverIP)
	log.Printf("║  TUN:     %-31s ║", tunDev.Name())
	log.Printf("╚═══════════════════════════════════════════╝")
	log.Printf("")
	log.Printf("  其他玩家可以通过虚拟IP %s 连接到你", c.virtualIP)
	log.Printf("  你也可以通过他们的虚拟IP连接到他们")
	log.Printf("")

	// Run event loops
	go c.receiveFromServer()
	go c.receiveFromTUN()
	go c.keepaliveLoop()
	go c.peerDiscoveryLoop()

	// Wait for shutdown
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	log.Printf("\n[client] shutting down...")
	close(c.done)
}

// ── Registration ───────────────────────────────────────────────

func (c *Client) register() error {
	reg := &protocol.RegisterPayload{
		RoomID:   c.roomID,
		Username: c.username,
	}
	packet := protocol.Encode(protocol.TypeRegister, reg.Marshal())

	// Send registration and wait for response
	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	// Retry up to 3 times
	for attempt := 0; attempt < 3; attempt++ {
		_, err := c.conn.WriteToUDP(packet, c.serverAddr)
		if err != nil {
			return fmt.Errorf("send register: %w", err)
		}

		buf := make([]byte, 1500)
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[client] register attempt %d timed out, retrying...", attempt+1)
				continue
			}
			return fmt.Errorf("read response: %w", err)
		}

		msg, err := protocol.Decode(buf[:n])
		if err != nil {
			return fmt.Errorf("decode response: %w", err)
		}

		switch msg.Type {
		case protocol.TypeAssignIP:
			assign, err := protocol.UnmarshalAssignIP(msg.Payload)
			if err != nil {
				return fmt.Errorf("unmarshal assign: %w", err)
			}
			c.virtualIP = assign.VirtualIP
			c.serverIP = assign.ServerIP
			return nil

		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("kicked: %s", kick.Reason)

		default:
			return fmt.Errorf("unexpected response type: %s", protocol.TypeName(msg.Type))
		}
	}

	return fmt.Errorf("registration failed after 3 attempts")
}

// ── Receive from Server ────────────────────────────────────────

func (c *Client) receiveFromServer() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
				log.Printf("[client] server read error: %v", err)
				continue
			}
		}

		msg, err := protocol.Decode(buf[:n])
		if err != nil {
			continue
		}

		switch msg.Type {
		case protocol.TypePeerInfo:
			c.handlePeerInfo(msg.Payload)
		case protocol.TypeData:
			c.handleDataFromServer(msg.Payload)
		case protocol.TypeHolePunch:
			c.handleHolePunch(msg.Payload)
		case protocol.TypeRoomInfo:
			c.handleRoomInfo(msg.Payload)
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			log.Printf("[client] kicked: %s", kick.Reason)
			close(c.done)
		}
	}
}

// ── Peer Info ──────────────────────────────────────────────────

func (c *Client) handlePeerInfo(payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Build new peer map
	newPeers := make(map[string]*Peer)
	for _, entry := range info.Peers {
		key := entry.VirtualIP.String()
		if existing, ok := c.peers[key]; ok {
			// Keep existing state, update address
			existing.PublicAddr = entry.PublicAddr
			existing.Username = entry.Username
			existing.LastSeen = time.Now()
			newPeers[key] = existing
		} else {
			newPeers[key] = &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: entry.PublicAddr,
				Username:   entry.Username,
				LastSeen:   time.Now(),
			}
			log.Printf("[client] new peer: %s (%s) at %s",
				entry.Username, entry.VirtualIP, entry.PublicAddr)
			// Start hole punch for new peer
			go c.startHolePunch(entry.VirtualIP)
		}
	}
	c.peers = newPeers
}

// ── Data from Server (Relay) ───────────────────────────────────

func (c *Client) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	// Write the original packet to TUN device
	if dp.Data != nil && len(dp.Data) > 0 {
		c.tunDev.Write(dp.Data)
	}
}

// ── Hole Punch ─────────────────────────────────────────────────

func (c *Client) handleHolePunch(payload []byte) {
	if len(payload) < 4 {
		return
	}
	// We received a hole punch from a peer — the peer's public addr
	// is embedded. We don't need to do anything special here;
	// the NAT mapping is now established on both sides.
	log.Printf("[client] hole punch received from peer")
}

func (c *Client) startHolePunch(peerIP net.IP) {
	c.mu.RLock()
	peer, ok := c.peers[peerIP.String()]
	c.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	// Send hole punch packets to the peer's public address
	// This creates NAT mappings on both sides
	punchPayload := make([]byte, 4)
	copy(punchPayload, peerIP.To4())
	packet := protocol.Encode(protocol.TypeHolePunch, punchPayload)

	log.Printf("[client] punching hole to %s (%s)...", peer.Username, peer.PublicAddr)
	for i := 0; i < 5; i++ {
		c.conn.WriteToUDP(packet, peer.PublicAddr)
		c.conn.WriteToUDP(packet, c.serverAddr) // also via server
		time.Sleep(200 * time.Millisecond)
	}
}

// ── Room Info ──────────────────────────────────────────────────

func (c *Client) handleRoomInfo(payload []byte) {
	info, err := protocol.UnmarshalRoomInfo(payload)
	if err != nil {
		return
	}
	log.Printf("[client] room %s: %d/%d players", info.RoomID, info.PlayerCount, info.MaxPlayers)
}

// ── Receive from TUN ───────────────────────────────────────────

func (c *Client) receiveFromTUN() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-c.done:
			return
		default:
		}

		n, err := c.tunDev.Read(buf)
		if err != nil {
			select {
			case <-c.done:
				return
			default:
				log.Printf("[client] TUN read error: %v", err)
				continue
			}
		}
		if n < 20 {
			continue // too small for IP header
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		// Extract destination IP from IP header
		dstIP := net.IP(pkt[16:20])
		srcIP := net.IP(pkt[12:16])

		// Route to peer or relay through server
		c.routePacket(pkt, srcIP, dstIP)
	}
}

// ── Route Packet ───────────────────────────────────────────────

func (c *Client) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Check if destination is the server
	if dstIP.Equal(c.serverIP) {
		// Send directly to server
		dp := &protocol.DataPayload{
			SrcIP: srcIP,
			DstIP: dstIP,
			Data:  pkt,
		}
		c.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), c.serverAddr)
		return
	}

	// Check if we know the peer's public address
	c.mu.RLock()
	peer, ok := c.peers[dstIP.String()]
	c.mu.RUnlock()

	if ok && peer.PublicAddr != nil {
		// Try direct send (P2P) — if hole punch succeeded, this goes direct
		dp := &protocol.DataPayload{
			SrcIP: srcIP,
			DstIP: dstIP,
			Data:  pkt,
		}
		c.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		// Relay through server
		dp := &protocol.DataPayload{
			SrcIP: srcIP,
			DstIP: dstIP,
			Data:  pkt,
		}
		c.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), c.serverAddr)
	}
}

// ── Keepalive ──────────────────────────────────────────────────

func (c *Client) keepaliveLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	packet := protocol.Encode(protocol.TypeKeepAlive, nil)
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.conn.WriteToUDP(packet, c.serverAddr)
		}
	}
}

// ── Peer Discovery ─────────────────────────────────────────────

func (c *Client) peerDiscoveryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()

	packet := protocol.Encode(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-c.done:
			return
		case <-ticker.C:
			c.conn.WriteToUDP(packet, c.serverAddr)
		}
	}
}

// ── Status ─────────────────────────────────────────────────────

func (c *Client) printStatus() {
	c.mu.RLock()
	defer c.mu.RUnlock()

	var sb strings.Builder
	sb.WriteString("\n═══ GameTunnel Status ═══\n")
	sb.WriteString(fmt.Sprintf("  VIP:     %s\n", c.virtualIP))
	sb.WriteString(fmt.Sprintf("  Peers:   %d\n", len(c.peers)))
	for _, p := range c.peers {
		sb.WriteString(fmt.Sprintf("           %s (%s) — %s\n",
			p.Username, p.VirtualIP, p.PublicAddr))
	}
	sb.WriteString("═════════════════════════\n")
	fmt.Print(sb.String())
}

// ── Helper: Parse IP from bytes ────────────────────────────────

func ipFromBytes(b []byte) net.IP {
	if len(b) < 4 {
		return nil
	}
	return net.IPv4(b[0], b[1], b[2], b[3])
}

// ── Helper: IP header extract ──────────────────────────────────

func extractIPHeader(pkt []byte) (src, dst net.IP, proto byte, err error) {
	if len(pkt) < 20 {
		return nil, nil, 0, fmt.Errorf("packet too short")
	}
	version := pkt[0] >> 4
	if version != 4 {
		return nil, nil, 0, fmt.Errorf("not IPv4: %d", version)
	}
	src = net.IP(append([]byte(nil), pkt[12:16]...))
	dst = net.IP(append([]byte(nil), pkt[16:20]...))
	proto = pkt[9]
	return src, dst, proto, nil
}

// ── Little-endian uint16 ───────────────────────────────────────

func putUint16(b []byte, v uint16) {
	binary.LittleEndian.PutUint16(b, v)
}
