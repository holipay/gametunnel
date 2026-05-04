// GameTunnel Server — 公网中转服务器
//
// 职责：
//  1. 接受客户端注册，分配虚拟IP
//  2. 中转游戏流量
//  3. 转发广播包（局域网游戏发现的关键）
//  4. 提供对等节点发现（NAT打洞）
//
// Usage:
//
//	server -addr :4700 -subnet 10.10.0.0/24 -max 10
package main

import (
	"flag"
	"log"
	"net"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
	"golang.org/x/sync/semaphore"
)

// ── 客户端状态 ──────────────────────────────────────────────────

type Client struct {
	Username   string
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	LastSeen   time.Time
}

// ── 服务器 ─────────────────────────────────────────────────────

type Server struct {
	conn       *net.UDPConn
	clients    map[string]*Client // virtualIP string → Client
	mu         sync.RWMutex
	subnet     *net.IPNet
	nextOctet  byte
	maxPlayers int
	serverIP   net.IP
	sem        *semaphore.Weighted // 并发限制
}

func main() {
	addr := flag.String("addr", ":4700", "监听地址 (UDP)")
	subnetStr := flag.String("subnet", "10.10.0.0/24", "虚拟子网 (CIDR)")
	maxPlayers := flag.Int("max", 10, "最大玩家数")
	flag.Parse()

	_, subnet, err := net.ParseCIDR(*subnetStr)
	if err != nil {
		log.Fatalf("子网无效 %q: %v", *subnetStr, err)
	}

	// 服务器在子网中占 .1
	serverIP := make(net.IP, 4)
	copy(serverIP, subnet.IP.To4())
	serverIP[3] = 1

	udpAddr, err := net.ResolveUDPAddr("udp4", *addr)
	if err != nil {
		log.Fatalf("解析地址失败: %v", err)
	}

	conn, err := net.ListenUDP("udp4", udpAddr)
	if err != nil {
		log.Fatalf("监听失败: %v", err)
	}
	defer conn.Close()

	s := &Server{
		conn:       conn,
		clients:    make(map[string]*Client),
		subnet:     subnet,
		nextOctet:  2,
		maxPlayers: *maxPlayers,
		serverIP:   serverIP,
		sem:        semaphore.NewWeighted(200), // 最多 200 个并发包处理
	}

	log.Println("╔═══════════════════════════════════════════╗")
	log.Println("║       GameTunnel Server 已启动            ║")
	log.Println("╠═══════════════════════════════════════════╣")
	log.Printf("║  监听:    %-31s ║", *addr)
	log.Printf("║  子网:    %-31s ║", subnet.String())
	log.Printf("║  服务器:  %-31s ║", serverIP)
	log.Printf("║  上限:    %-31d ║", *maxPlayers)
	log.Println("╚═══════════════════════════════════════════╝")

	go s.keepaliveLoop()

	buf := make([]byte, 65535)
	for {
		n, remoteAddr, err := conn.ReadFromUDP(buf)
		if err != nil {
			continue
		}
		if n < 1 {
			continue
		}
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		// 限制并发，防止恶意客户端打爆内存
		if !s.sem.TryAcquire(1) {
			continue // 队列满，丢包
		}
		go func() {
			defer s.sem.Release(1)
			s.handlePacket(pkt, remoteAddr)
		}()
	}
}

// ── 包处理 ─────────────────────────────────────────────────────

func (s *Server) handlePacket(data []byte, from *net.UDPAddr) {
	msg, err := protocol.Decode(data)
	if err != nil {
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
	}
}

// ── 注册 ───────────────────────────────────────────────────────

func (s *Server) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	s.mu.Lock()

	// 重连检测
	for _, c := range s.clients {
		if c.PublicAddr.String() == from.String() {
			c.LastSeen = time.Now()
			assign := &protocol.AssignIPPayload{
				VirtualIP:  c.VirtualIP,
				SubnetMask: s.subnet.Mask,
				ServerIP:   s.serverIP,
			}
			selfIP := c.VirtualIP
			s.send(protocol.Encode(protocol.TypeAssignIP, assign.Marshal()), from)
			// snapshot + release before sending
			targets := s.buildPeerInfoTargetsLocked()
			s.mu.Unlock()
			s.sendPeerInfo(from, selfIP, targets)
			return
		}
	}

	// 容量检查
	if len(s.clients) >= s.maxPlayers {
		s.mu.Unlock()
		kick := &protocol.KickPayload{Reason: "房间已满"}
		s.send(protocol.Encode(protocol.TypeKick, kick.Marshal()), from)
		return
	}

	// 分配IP
	vip := s.nextVirtualIP()
	if vip == nil {
		s.mu.Unlock()
		kick := &protocol.KickPayload{Reason: "IP已耗尽"}
		s.send(protocol.Encode(protocol.TypeKick, kick.Marshal()), from)
		return
	}

	s.clients[vip.String()] = &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
	}

	log.Printf("[+] %s (%s) → %s  [在线: %d]",
		reg.Username, from, vip, len(s.clients))

	// 发送IP分配
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: s.subnet.Mask,
		ServerIP:   s.serverIP,
	}
	s.send(protocol.Encode(protocol.TypeAssignIP, assign.Marshal()), from)

	// snapshot clients, then release lock before network I/O
	targets := s.buildPeerInfoTargetsLocked()
	s.mu.Unlock()
	s.broadcastPeerInfo(targets)
}

// ── 心跳 ───────────────────────────────────────────────────────

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
		// 先用读锁收集 dead 列表
		s.mu.RLock()
		now := time.Now()
		var dead []string
		for ip, c := range s.clients {
			if now.Sub(c.LastSeen) > 45*time.Second {
				log.Printf("[-] %s (%s) 超时断开", c.Username, c.VirtualIP)
				dead = append(dead, ip)
			}
		}
		s.mu.RUnlock()

		if len(dead) == 0 {
			continue
		}

		// 写锁: 删除 + snapshot
		s.mu.Lock()
		for _, ip := range dead {
			delete(s.clients, ip)
		}
		targets := s.buildPeerInfoTargetsLocked()
		s.mu.Unlock()

		s.broadcastPeerInfo(targets)
	}
}

// ── 对等节点请求 ────────────────────────────────────────────────

func (s *Server) handlePeerRequest(from *net.UDPAddr) {
	s.mu.RLock()
	var selfIP net.IP
	for _, c := range s.clients {
		if c.PublicAddr.String() == from.String() {
			selfIP = c.VirtualIP
			break
		}
	}
	s.mu.RUnlock()

	if selfIP == nil {
		return
	}

	s.sendPeerInfo(from, selfIP, nil)
}

// ── 中转（核心）────────────────────────────────────────────────

func (s *Server) handleRelay(payload []byte, from *net.UDPAddr) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}

	dstKey := dp.DstIP.String()
	relayPayload := &protocol.DataPayload{
		SrcIP: dp.SrcIP,
		DstIP: dp.DstIP,
		Data:  dp.Data,
	}
	encoded := protocol.Encode(protocol.TypeData, relayPayload.Marshal())

	// 广播包：转发给所有客户端（局域网游戏发现依赖这个）
	if dstKey == "255.255.255.255" || s.isBroadcast(dp.DstIP) {
		s.mu.RLock()
		targets := make([]*net.UDPAddr, 0, len(s.clients))
		for _, c := range s.clients {
			if c.PublicAddr.String() != from.String() {
				targets = append(targets, c.PublicAddr)
			}
		}
		s.mu.RUnlock()

		for _, addr := range targets {
			s.send(encoded, addr)
		}
		return
	}

	// 单播：转发给目标客户端
	s.mu.RLock()
	dst, ok := s.clients[dstKey]
	s.mu.RUnlock()

	if !ok {
		return
	}

	s.send(encoded, dst.PublicAddr)
}

// ── NAT 打洞 ───────────────────────────────────────────────────

func (s *Server) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	// 确保源地址是 IPv4
	srcIP4 := from.IP.To4()
	if srcIP4 == nil {
		return // 不支持 IPv6 源地址
	}

	s.mu.RLock()
	dst, ok := s.clients[dstIP.String()]
	s.mu.RUnlock()

	if !ok {
		return
	}

	// 转发打洞包，附带源地址
	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], srcIP4)
	copy(punchData[4:], []byte(addrStr))
	s.send(protocol.Encode(protocol.TypeHolePunch, punchData), dst.PublicAddr)
}

// ── 广播对等节点信息 ────────────────────────────────────────────

// peerInfoTarget holds the data needed to send peer info to one client.
type peerInfoTarget struct {
	addr     *net.UDPAddr
	selfIP   net.IP
}

// buildPeerInfoTargetsLocked snapshots client data while lock is held.
// Callers release the lock before calling broadcastPeerInfo / sendPeerInfo.
func (s *Server) buildPeerInfoTargetsLocked() []peerInfoTarget {
	targets := make([]peerInfoTarget, 0, len(s.clients))
	for _, c := range s.clients {
		targets = append(targets, peerInfoTarget{addr: c.PublicAddr, selfIP: c.VirtualIP})
	}
	return targets
}

// peerSnapshot holds a peer's public info for building PeerInfoPayload.
type peerSnapshot struct {
	virtualIP  net.IP
	publicAddr *net.UDPAddr
	username   string
}

// buildPeerList builds the peer list for a given selfIP from the snapshot.
func buildPeerList(snapshot []peerSnapshot, selfIP net.IP) *protocol.PeerInfoPayload {
	peers := &protocol.PeerInfoPayload{}
	for _, s := range snapshot {
		if s.virtualIP.Equal(selfIP) {
			continue
		}
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  s.virtualIP,
			PublicAddr: s.publicAddr,
			Username:   s.username,
		})
	}
	return peers
}

// broadcastPeerInfo sends peer info to all targets (call AFTER releasing lock).
func (s *Server) broadcastPeerInfo(targets []peerInfoTarget) {
	// Build peer list snapshot once
	s.mu.RLock()
	snapshot := make([]peerSnapshot, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, peerSnapshot{
			virtualIP:  c.VirtualIP,
			publicAddr: c.PublicAddr,
			username:   c.Username,
		})
	}
	s.mu.RUnlock()

	for _, t := range targets {
		peers := buildPeerList(snapshot, t.selfIP)
		s.send(protocol.Encode(protocol.TypePeerInfo, peers.Marshal()), t.addr)
	}
}

// sendPeerInfo sends peer info to a single target (call AFTER releasing lock).
func (s *Server) sendPeerInfo(to *net.UDPAddr, selfIP net.IP, targets []peerInfoTarget) {
	s.mu.RLock()
	snapshot := make([]peerSnapshot, 0, len(s.clients))
	for _, c := range s.clients {
		snapshot = append(snapshot, peerSnapshot{
			virtualIP:  c.VirtualIP,
			publicAddr: c.PublicAddr,
			username:   c.Username,
		})
	}
	s.mu.RUnlock()

	peers := buildPeerList(snapshot, selfIP)
	s.send(protocol.Encode(protocol.TypePeerInfo, peers.Marshal()), to)
}

// ── IP 分配 ────────────────────────────────────────────────────

func (s *Server) nextVirtualIP() net.IP {
	base := s.subnet.IP.To4()
	for s.nextOctet < 255 {
		candidate := net.IPv4(base[0], base[1], base[2], s.nextOctet)
		s.nextOctet++
		if candidate.Equal(s.serverIP) {
			continue
		}
		if _, taken := s.clients[candidate.String()]; !taken {
			return candidate
		}
	}
	return nil
}

// ── 广播地址判断 ────────────────────────────────────────────────

func (s *Server) isBroadcast(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	if ip4.Equal(net.IPv4bcast) {
		return true
	}
	// 子网广播
	bcast := make(net.IP, 4)
	for i := 0; i < 4; i++ {
		bcast[i] = ip4[i] | ^s.subnet.Mask[i]
	}
	return ip4.Equal(bcast)
}

// ── 发送 ───────────────────────────────────────────────────────

func (s *Server) send(data []byte, to *net.UDPAddr) {
	s.conn.WriteToUDP(data, to)
}
