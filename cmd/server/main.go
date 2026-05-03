// GameTunnel Server — runs on a public VPS.
//
// Responsibilities:
//   1. Accept client registrations, assign virtual IPs
//   2. Relay packets between clients (when P2P fails)
//   3. Provide peer discovery for NAT hole punching
//   4. Keepalive management
//
// Usage:
//   server -addr :4700 -subnet 10.10.0.0/24 -max 10
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"sync"
	"time"

	"github.com/gametunnel/internal/protocol"
)

// ── Client State ───────────────────────────────────────────────

type Client struct {
	Username   string
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	LastSeen   time.Time
}

// ── Server ─────────────────────────────────────────────────────

type Server struct {
	conn       *net.UDPConn
	clients    map[string]*Client // virtualIP string → Client
	mu         sync.RWMutex
	subnet     *net.IPNet
	nextOctet  byte // next IP to assign (starts at .2, .1 is server)
	maxPlayers int
	serverIP   net.IP
}

func main() {
	addr := flag.String("addr", ":4700", "listen address (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "virtual subnet (CIDR)")
	maxPlayers := flag.Int("max", 10, "max players per room")
	flag.Parse()

	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf("invalid subnet %q: %v", *subnetStr, err)
	}

	// Server gets .1 in the subnet
	serverIP := make(net.IP, 4)
	copy(serverIP, subnet.IP.To4())
	serverIP[3] = 1

	udpAddr, err := net.ResolveUDPAddr("udp4", *addr)
	if err != nil {
		log.Fatalf("resolve addr: %v", err)
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer conn.Close()

	s := &Server{
		conn:       conn,
		clients:    make(map[string]*Client),
		subnet:     subnet,
		nextOctet:  2, // .1 is server, .2 is first client
		maxPlayers: *maxPlayers,
		serverIP:   serverIP,
	}

	log.Printf("╔═══════════════════════════════════════════╗")
	log.Printf("║       GameTunnel Server Started           ║")
	log.Printf("╠═══════════════════════════════════════════╣")
	log.Printf("║  Listen:  %-31s ║", *addr)
	log.Printf("║  Subnet:  %-31s ║", subnet.String())
	log.Printf("║  Server:  %-31s ║", serverIP)
	log.Printf("║  Max:     %-31d ║", *maxPlayers)
	log.Printf("╚═══════════════════════════════════════════╝")

	// Start keepalive checker
	go s.keepaliveLoop()

	// Main receive loop
	buf := make([]byte, 65535)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			log.Printf("[server] read error: %v", err)
			continue
		}
		if n < 1 {
			continue
		}
		// Copy packet data to avoid buffer reuse
		pkt := make([]byte, n)
		copy(pkt, buf[:n])
		go s.handlePacket(pkt, remoteAddr)
	}
}

// ── Packet Handler ─────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	msg, err := protocol.Decode(data)
	if err != nil {
		log.Printf("[server] decode error from %s: %v", from, err)
		return
	}

	switch msg.Type {
	case protocol.TypeRegister:
		s.handleRegister(msg.Payload, from)
	case protocol.TypeKeepAlive:
		s.handleKeepAlive(from)
	case protocol.TypePeerRequest:
		s.handlePeerRequest(from)
	case protocol.TypeData:
		s.handleRelay(msg.Payload, from)
	case protocol.TypeHolePunch:
		s.handleHolePunch(msg.Payload, from)
	default:
		log.Printf("[server] unknown type 0x%02x from %s", msg.Type, from)
	}
}

// ── Register ───────────────────────────────────────────────────

func (s *Server) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		log.Printf("[server] bad register from %s: %v", from, err)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	// Check if already registered (reconnect)
	for _, c := range s.clients {
		if c.PublicAddr.String() == from.String() {
			// Update last seen
			c.LastSeen = time.Now()
			log.Printf("[server] reconnect from %s (%s)", reg.Username, from)
			// Re-send IP assignment
			assign := &protocol.AssignIPPayload{
				VirtualIP:  c.VirtualIP,
				SubnetMask: s.subnet.Mask,
				ServerIP:   s.serverIP,
			}
			s.send(protocol.Encode(protocol.TypeAssignIP, assign.Marshal()), from)
			// Send peer info
			s.sendPeerInfo(from, c.VirtualIP)
			return
		}
	}

	// Check capacity
	if len(s.clients) >= s.maxPlayers {
		kick := &protocol.KickPayload{Reason: "room is full"}
		s.send(protocol.Encode(protocol.TypeKick, kick.Marshal()), from)
		return
	}

	// Assign next available IP
	vip := s.nextVirtualIP()
	if vip == nil {
		kick := &protocol.KickPayload{Reason: "no IPs available"}
		s.send(protocol.Encode(protocol.TypeKick, kick.Marshal()), from)
		return
	}

	client := &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
	}
	s.clients[vip.String()] = client

	log.Printf("[server] + %s (%s) → %s  [total: %d]",
		reg.Username, from, vip, len(s.clients))

	// Send IP assignment
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: s.subnet.Mask,
		ServerIP:   s.serverIP,
	}
	s.send(protocol.Encode(protocol.TypeAssignIP, assign.Marshal()), from)

	// Broadcast updated peer info to ALL clients
	s.broadcastPeerInfo()

	// Send room info
	room := &protocol.RoomInfoPayload{
		PlayerCount: byte(len(s.clients)),
		MaxPlayers:  byte(s.maxPlayers),
		RoomID:      "default",
	}
	s.send(protocol.Encode(protocol.TypeRoomInfo, room.Marshal()), from)
}

// ── Keepalive ──────────────────────────────────────────────────

func (s *Server) handleKeepAlive(from *net.UDPAddr) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.clients {
		if c.PublicAddr.String() == from.String() {
			c.LastSeen = time.Now()
			break
		}
	}
}

func (s *Server) keepaliveLoop() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		s.mu.Lock()
		now := time.Now()
		var dead []string
		for ip, c := range s.clients {
			if now.Sub(c.LastSeen) > 45*time.Second {
				log.Printf("[server] - %s (%s) timed out", c.Username, c.VirtualIP)
				dead = append(dead, ip)
			}
		}
		for _, ip := range dead {
			delete(s.clients, ip)
		}
		changed := len(dead) > 0
		s.mu.Unlock()

		if changed {
			s.broadcastPeerInfo()
		}
	}
}

// ── Peer Request ───────────────────────────────────────────────

func (s *Server) handlePeerRequest(from *net.UDPAddr) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	var selfIP net.IP
	for _, c := range s.clients {
		if c.PublicAddr.String() == from.String() {
			selfIP = c.VirtualIP
			break
		}
	}
	if selfIP == nil {
		return
	}
	s.sendPeerInfo(from, selfIP)
}

// ── Relay ──────────────────────────────────────────────────────

func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}

	dstKey := dp.DstIP.String()
	s.mu.RLock()
	dst, ok := s.clients[dstKey]
	s.mu.RUnlock()

	if !ok {
		return // destination not found
	}

	// Relay: wrap in Data message and forward
	relayPayload := &protocol.DataPayload{
		SrcIP: dp.SrcIP,
		DstIP: dp.DstIP,
		Data:  dp.Data,
	}
	s.send(protocol.Encode(protocol.TypeData, relayPayload.Marshal()), dst.PublicAddr)
}

// ── Hole Punch ─────────────────────────────────────────────────

func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	// Hole punch packet: forward to the destination so both sides
	// create NAT mappings for each other
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	s.mu.RLock()
	dst, ok := s.clients[dstIP.String()]
	s.mu.RUnlock()

	if !ok {
		return
	}

	// Forward hole punch with source info
	punchData := make([]byte, 4+len(from.String()))
	copy(punchData[:4], from.IP.To4())
	copy(punchData[4:], from.String())
	s.send(protocol.Encode(protocol.TypeHolePunch, punchData), dst.PublicAddr)
}

// ── Broadcast Peer Info ────────────────────────────────────────

func (s *Server) broadcastPeerInfo() {
	s.mu.RLock()
	defer s.mu.RUnlock()

	for _, client := range s.clients {
		s.sendPeerInfo(client.PublicAddr, client.VirtualIP)
	}
}

func (s *Server) sendPeerInfo(to *net.UDPAddr, selfIP net.IP) {
	peers := &protocol.PeerInfoPayload{}
	for _, c := range s.clients {
		if c.VirtualIP.Equal(selfIP) {
			continue // skip self
		}
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  c.VirtualIP,
			PublicAddr: c.PublicAddr,
			Username:   c.Username,
		})
	}
	s.send(protocol.Encode(protocol.TypePeerInfo, peers.Marshal()), to)
}

// ── IP Allocation ──────────────────────────────────────────────

func (s *Server) nextVirtualIP() net.IP {
	base := s.subnet.IP.To4()
	for s.nextOctet < 255 {
		candidate := net.IPv4(base[0], base[1], base[2], s.nextOctet)
		s.nextOctet++
		// Skip server IP (.1)
		if candidate.Equal(s.serverIP) {
			continue
		}
		// Check not already assigned
		if _, taken := s.clients[candidate.String()]; !taken {
			return candidate
		}
	}
	return nil
}

// ── Send Helper ────────────────────────────────────────────────

func (s *Server) send(data []byte, to *net.UDPAddr) {
	_, err := s.conn.WriteToUDP(data, to)
	if err != nil {
		log.Printf("[server] send to %s failed: %v", to, err)
	}
}
