// GameTunnel Client — 星际争霸1 局域网对战专用
//
// 只需指定服务器地址，自动组网，自动转发广播包。
// 星际1通过UDP广播(6112)发现局域网游戏，本客户端确保广播包能到达所有玩家。
//
// Usage:
//   sudo client -server 1.2.3.4:4700
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
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
	LastSeen   time.Time
}

// ── Client ─────────────────────────────────────────────────────

type Client struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	tunDev     *tun.Device
	virtualIP  net.IP
	serverIP   net.IP
	subnet     *net.IPNet
	peers      map[string]*Peer // virtualIP string → Peer
	mu         sync.RWMutex
	done       chan struct{}
	username   string
}

func main() {
	serverAddr := flag.String("server", "127.0.0.1:4700", "服务器地址 (host:port)")
	mtu := flag.Int("mtu", 1400, "隧道 MTU")
	flag.Parse()

	// 自动生成用户名（hostname）
	hostname, _ := os.Hostname()
	username := hostname

	// 解析服务器地址
	sAddr, err := net.ResolveUDPAddr("udp4", *serverAddr)
	if err != nil {
		log.Fatalf("服务器地址无效 %q: %v", *serverAddr, err)
	}

	// 绑定本地 UDP
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Fatalf("绑定 UDP 失败: %v", err)
	}

	_, subnet, _ := net.ParseCIDR("10.10.0.0/24")

	c := &Client{
		conn:       conn,
		serverAddr: sAddr,
		username:   username,
		subnet:     subnet,
		peers:      make(map[string]*Peer),
		done:       make(chan struct{}),
	}

	// 注册到服务器
	if err := c.register(); err != nil {
		log.Fatalf("注册失败: %v", err)
	}

	// 创建 TUN 设备
	cfg := tun.Config{
		VirtualIP:  c.virtualIP,
		SubnetMask: net.CIDRMask(24, 24),
		ServerIP:   c.serverIP,
		MTU:        *mtu,
	}
	tunDev, err := tun.New(cfg)
	if err != nil {
		log.Fatalf("创建 TUN 失败: %v", err)
	}
	defer tunDev.Close()
	c.tunDev = tunDev

	fmt.Println("╔═══════════════════════════════════════════╗")
	fmt.Println("║       GameTunnel 已连接                   ║")
	fmt.Println("╠═══════════════════════════════════════════╣")
	fmt.Printf("║  虚拟IP:  %-31s ║\n", c.virtualIP)
	fmt.Printf("║  服务器:  %-31s ║\n", c.serverIP)
	fmt.Printf("║  虚拟网卡: %-30s ║\n", tunDev.Name())
	fmt.Println("╚═══════════════════════════════════════════╝")
	fmt.Println()
	fmt.Println("  ✅ 虚拟局域网已就绪")
	fmt.Println("  ✅ 打开星际争霸1 → Multiplayer → Local Area Network")
	fmt.Println("  ✅ 建主/加入游戏即可，跟真局域网一样")
	fmt.Println()

	// 启动各个协程
	go c.receiveFromServer()
	go c.receiveFromTUN()
	go c.keepaliveLoop()
	go c.peerDiscoveryLoop()

	// 等待退出
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	<-sig
	fmt.Println("\n[client] 断开连接...")
	close(c.done)
}

// ── 注册 ───────────────────────────────────────────────────────

func (c *Client) register() error {
	reg := &protocol.RegisterPayload{
		RoomID:   "starcraft",
		Username: c.username,
	}
	packet := protocol.Encode(protocol.TypeRegister, reg.Marshal())

	c.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer c.conn.SetReadDeadline(time.Time{})

	for attempt := 0; attempt < 3; attempt++ {
		_, err := c.conn.WriteToUDP(packet, c.serverAddr)
		if err != nil {
			return fmt.Errorf("发送注册包失败: %w", err)
		}

		buf := make([]byte, 1500)
		n, _, err := c.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[client] 注册超时，重试 %d/3...", attempt+1)
				continue
			}
			return fmt.Errorf("读取响应失败: %w", err)
		}

		msg, err := protocol.Decode(buf[:n])
		if err != nil {
			return fmt.Errorf("解码响应失败: %w", err)
		}

		switch msg.Type {
		case protocol.TypeAssignIP:
			assign, err := protocol.UnmarshalAssignIP(msg.Payload)
			if err != nil {
				return fmt.Errorf("解析IP分配失败: %w", err)
			}
			c.virtualIP = assign.VirtualIP
			c.serverIP = assign.ServerIP
			return nil
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("被拒绝: %s", kick.Reason)
		}
	}
	return fmt.Errorf("注册失败（重试3次）")
}

// ── 从服务器接收 ────────────────────────────────────────────────

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
		}
	}
}

// ── 对等节点信息 ────────────────────────────────────────────────

func (c *Client) handlePeerInfo(payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	newPeers := make(map[string]*Peer)
	for _, entry := range info.Peers {
		key := entry.VirtualIP.String()
		if existing, ok := c.peers[key]; ok {
			existing.PublicAddr = entry.PublicAddr
			existing.LastSeen = time.Now()
			newPeers[key] = existing
		} else {
			newPeers[key] = &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: entry.PublicAddr,
				LastSeen:   time.Now(),
			}
			log.Printf("[client] 新玩家加入: %s", entry.VirtualIP)
			go c.startHolePunch(entry.VirtualIP)
		}
	}
	c.peers = newPeers
}

// ── 从服务器中转的数据 ──────────────────────────────────────────

func (c *Client) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	if len(dp.Data) > 0 {
		c.tunDev.Write(dp.Data)
	}
}

// ── NAT 打洞 ───────────────────────────────────────────────────

func (c *Client) handleHolePunch(payload []byte) {
	// 收到打洞包，NAT映射已建立
}

func (c *Client) startHolePunch(peerIP net.IP) {
	c.mu.RLock()
	peer, ok := c.peers[peerIP.String()]
	c.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	punchPayload := make([]byte, 4)
	copy(punchPayload, peerIP.To4())
	packet := protocol.Encode(protocol.TypeHolePunch, punchPayload)

	for i := 0; i < 5; i++ {
		c.conn.WriteToUDP(packet, peer.PublicAddr)
		time.Sleep(200 * time.Millisecond)
	}
}

// ── 从 TUN 设备读取（游戏发出的包）────────────────────────────

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
				continue
			}
		}
		if n < 20 {
			continue
		}

		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		srcIP := net.IP(pkt[12:16])
		dstIP := net.IP(pkt[16:20])

		c.routePacket(pkt, srcIP, dstIP)
	}
}

// ── 路由：核心逻辑 ─────────────────────────────────────────────

func (c *Client) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// 广播包 → 转发给所有对等节点（星际1发现游戏的关键）
	if c.isBroadcast(dstIP) {
		c.relayBroadcast(pkt, srcIP)
		return
	}

	// 发给服务器虚拟IP → 直接中转
	if dstIP.Equal(c.serverIP) {
		c.sendToServer(pkt, srcIP, dstIP)
		return
	}

	// 发给某个对等节点 → 尝试直连，否则走服务器中转
	c.mu.RLock()
	peer, ok := c.peers[dstIP.String()]
	c.mu.RUnlock()

	if ok && peer.PublicAddr != nil {
		// 有直连地址，尝试 P2P
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
		c.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		// 走服务器中转
		c.sendToServer(pkt, srcIP, dstIP)
	}
}

// ── 广播转发（星际1局域网发现的核心）────────────────────────────

func (c *Client) relayBroadcast(pkt []byte, srcIP net.IP) {
	// 广播包通过服务器中转给所有玩家
	dp := &protocol.DataPayload{
		SrcIP: srcIP,
		DstIP: net.IPv4(255, 255, 255, 255),
		Data:  pkt,
	}
	encoded := protocol.Encode(protocol.TypeData, dp.Marshal())

	// 发给服务器（服务器会转发给同房间所有人）
	c.conn.WriteToUDP(encoded, c.serverAddr)

	// 也尝试直接发给已知的对等节点
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, peer := range c.peers {
		if peer.PublicAddr != nil {
			c.conn.WriteToUDP(encoded, peer.PublicAddr)
		}
	}
}

// ── 判断是否广播地址 ────────────────────────────────────────────

func (c *Client) isBroadcast(ip net.IP) bool {
	ip4 := ip.To4()
	if ip4 == nil {
		return false
	}
	// 255.255.255.255
	if ip4.Equal(net.IPv4bcast) {
		return true
	}
	// 子网广播: 10.10.0.255
	if c.subnet != nil {
		bcast := make(net.IP, 4)
		for i := 0; i < 4; i++ {
			bcast[i] = ip4[i] | ^c.subnet.Mask[i]
		}
		if ip4.Equal(bcast) {
			return true
		}
	}
	return false
}

// ── 发送到服务器 ────────────────────────────────────────────────

func (c *Client) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	c.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), c.serverAddr)
}

// ── 心跳 ───────────────────────────────────────────────────────

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

// ── 对等节点发现 ────────────────────────────────────────────────

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
