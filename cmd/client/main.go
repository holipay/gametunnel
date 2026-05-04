// GameTunnel Client — 通用局域网游戏隧道 (Windows)
//
// GUI 模式：系统托盘图标，右键菜单操作。
//
// Usage:
//
//	gtunnel-client.exe
//	gtunnel-client.exe -server 1.2.3.4:4700
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/holipay/gametunnel/internal/gui"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
	"github.com/holipay/gametunnel/internal/tun"
)

// ── Peer State ─────────────────────────────────────────────────

type Peer struct {
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	Username   string
}

// ── Tunnel ─────────────────────────────────────────────────────

type Tunnel struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
	tunDev     *tun.Device
	virtualIP  net.IP
	serverIP   net.IP
	subnetMask net.IPMask
	peers      map[string]*Peer
	mu         sync.RWMutex
	done       chan struct{}
	stopped    chan struct{}
	username   string
	roomID     string
	gui        *gui.GUI
}

func main() {
	// CLI flags (override config)
	serverFlag := flag.String("server", "", "服务器地址 (host:port)")
	nameFlag := flag.String("name", "", "玩家名称")
	roomFlag := flag.String("room", "", "房间ID")
	flag.Parse()

	// Load config
	cfg := gui.LoadConfig()

	// CLI flags override config
	if *serverFlag != "" {
		cfg.ServerAddr = *serverFlag
	}
	if *nameFlag != "" {
		cfg.PlayerName = *nameFlag
	}
	if *roomFlag != "" {
		cfg.RoomID = *roomFlag
	}

	// If no server configured, save default
	firstRun := false
	if cfg.ServerAddr == "" {
		cfg.ServerAddr = "127.0.0.1:4700"
		gui.SaveConfig(cfg)
		log.Printf("[tunnel] 首次运行，已写入默认配置: %s", gui.ConfigPath())
		firstRun = true
	}

	// Set up logging to file (since we're a GUI app)
	logFile := setupLog()
	defer logFile.Close()

	// Create GUI
	g := gui.New(cfg)

	// Show first-run notice in GUI (user may never see log file)
	if firstRun {
		go func() {
			time.Sleep(500 * time.Millisecond)
			g.SetNotice(fmt.Sprintf("首次运行，请在设置中配置服务器地址 (默认: %s)", cfg.ServerAddr))
		}()
	}

	// Create tunnel
	t := &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		peers:    make(map[string]*Peer),
		done:     make(chan struct{}),
		stopped:  make(chan struct{}),
		gui:      g,
	}

	// Wire up callbacks
	g.SetCallbacks(
		func() { go t.Connect(cfg.ServerAddr) },
		func() { t.Disconnect() },
	)

	// Run GUI (blocks until quit)
	g.Run()
}

// ── Connect / Disconnect ───────────────────────────────────────

func (t *Tunnel) Connect(serverAddr string) {
	// Clean up previous connection if any
	select {
	case <-t.done:
		// already stopped
	default:
		close(t.done)
		<-t.stopped
	}

	t.done = make(chan struct{})
	t.stopped = make(chan struct{})
	defer close(t.stopped)

	t.gui.UpdateState(gui.StateConnecting)

	// Parse server address
	sAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		log.Printf("[tunnel] 服务器地址无效: %v", err)
		t.gui.UpdateState(gui.StateDisconnected)
		return
	}

	// Bind UDP
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Printf("[tunnel] 绑定 UDP 失败: %v", err)
		t.gui.UpdateState(gui.StateDisconnected)
		return
	}
	t.conn = conn
	t.serverAddr = sAddr

	// Register
	if err := t.register(); err != nil {
		log.Printf("[tunnel] 注册失败: %v", err)
		conn.Close()
		t.gui.UpdateState(gui.StateDisconnected)
		return
	}

	// Create TUN
	cfg := tun.Config{
		VirtualIP:  t.virtualIP,
		SubnetMask: t.subnetMask,
		ServerIP:   t.serverIP,
		MTU:        1400,
	}
	tunDev, err := tun.New(cfg)
	if err != nil {
		log.Printf("[tunnel] 创建 TUN 失败: %v", err)
		conn.Close()
		t.gui.UpdateState(gui.StateDisconnected)
		return
	}
	t.tunDev = tunDev
	defer tunDev.Close()

	// Update GUI
	t.gui.SetIP(t.virtualIP.String())
	t.gui.UpdateState(gui.StateConnected)
	log.Printf("[tunnel] 已连接: %s → %s", t.virtualIP, serverAddr)

	// Run tunnel loops
	go t.receiveFromServer()
	go t.receiveFromTUN()
	go t.keepaliveLoop()
	go t.peerDiscoveryLoop()

	// Wait for disconnect
	<-t.done
	log.Printf("[tunnel] 断开连接")
	t.gui.UpdateState(gui.StateDisconnected)
}

func (t *Tunnel) Disconnect() {
	select {
	case <-t.done:
		return // already stopped
	default:
		close(t.done)
	}
	if t.conn != nil {
		t.conn.Close()
	}
}

// ── Registration ───────────────────────────────────────────────

func (t *Tunnel) register() error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.roomID,
		Username: t.username,
	}
	packet := protocol.Encode(protocol.TypeRegister, reg.Marshal())

	t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer t.conn.SetReadDeadline(time.Time{})

	for attempt := 0; attempt < 3; attempt++ {
		_, err := t.conn.WriteToUDP(packet, t.serverAddr)
		if err != nil {
			return fmt.Errorf("发送注册包失败: %w", err)
		}

		buf := make([]byte, 1500)
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[tunnel] 注册超时，重试 %d/3...", attempt+1)
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
			t.virtualIP = assign.VirtualIP
			t.serverIP = assign.ServerIP
			t.subnetMask = net.IPMask(assign.SubnetMask)
			return nil
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("被拒绝: %s", kick.Reason)
		}
	}
	return fmt.Errorf("注册失败（重试3次）")
}

// ── Server Receiver ────────────────────────────────────────────

func (t *Tunnel) receiveFromServer() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-t.done:
			return
		default:
		}

		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-t.done:
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
			t.handlePeerInfo(msg.Payload)
		case protocol.TypeData:
			t.handleDataFromServer(msg.Payload)
		case protocol.TypeHolePunch:
			// NAT mapping established
		}
	}
}

// ── Peer Info ──────────────────────────────────────────────────

func (t *Tunnel) handlePeerInfo(payload []byte) {
	info, err := protocol.UnmarshalPeerInfo(payload)
	if err != nil {
		return
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	newPeers := make(map[string]*Peer)
	for _, entry := range info.Peers {
		key := entry.VirtualIP.String()
		if existing, ok := t.peers[key]; ok {
			existing.PublicAddr = entry.PublicAddr
			existing.Username = entry.Username
			newPeers[key] = existing
		} else {
			newPeers[key] = &Peer{
				VirtualIP:  entry.VirtualIP,
				PublicAddr: entry.PublicAddr,
				Username:   entry.Username,
			}
			log.Printf("[tunnel] 新玩家: %s (%s)", entry.Username, entry.VirtualIP)
			go t.startHolePunch(entry.VirtualIP)
		}
	}
	t.peers = newPeers

	// Update GUI player count
	t.gui.SetPlayers(len(t.peers))
}

// ── Data from Server ───────────────────────────────────────────

func (t *Tunnel) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	if len(dp.Data) > 0 && t.tunDev != nil {
		t.tunDev.Write(dp.Data)
	}
}

// ── NAT Hole Punch ─────────────────────────────────────────────

func (t *Tunnel) startHolePunch(peerIP net.IP) {
	t.mu.RLock()
	peer, ok := t.peers[peerIP.String()]
	t.mu.RUnlock()
	if !ok || peer.PublicAddr == nil {
		return
	}

	punchPayload := make([]byte, 4)
	copy(punchPayload, peerIP.To4())
	packet := protocol.Encode(protocol.TypeHolePunch, punchPayload)

	for i := 0; i < 5; i++ {
		select {
		case <-t.done:
			return
		default:
		}
		t.conn.WriteToUDP(packet, peer.PublicAddr)
		time.Sleep(200 * time.Millisecond)
	}
}

// ── TUN Reader ─────────────────────────────────────────────────

func (t *Tunnel) receiveFromTUN() {
	buf := make([]byte, 65535)
	for {
		select {
		case <-t.done:
			return
		default:
		}

		n, err := t.tunDev.Read(buf)
		if err != nil {
			select {
			case <-t.done:
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
		t.routePacket(pkt, srcIP, dstIP)
	}
}

// ── Routing ────────────────────────────────────────────────────

func (t *Tunnel) routePacket(pkt []byte, srcIP, dstIP net.IP) {
	// Build subnet for broadcast check
	subnet := &net.IPNet{
		IP:   t.virtualIP.Mask(t.subnetMask),
		Mask: t.subnetMask,
	}
	if netutil.IsBroadcast(dstIP, subnet) {
		t.relayBroadcast(pkt, srcIP)
		return
	}
	if dstIP.Equal(t.serverIP) {
		t.sendToServer(pkt, srcIP, dstIP)
		return
	}

	t.mu.RLock()
	peer, ok := t.peers[dstIP.String()]
	t.mu.RUnlock()

	if ok && peer.PublicAddr != nil {
		dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
		t.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
	} else {
		t.sendToServer(pkt, srcIP, dstIP)
	}
}

func (t *Tunnel) relayBroadcast(pkt []byte, srcIP net.IP) {
	dp := &protocol.DataPayload{
		SrcIP: srcIP,
		DstIP: net.IPv4(255, 255, 255, 255),
		Data:  pkt,
	}
	encoded := protocol.Encode(protocol.TypeData, dp.Marshal())
	t.conn.WriteToUDP(encoded, t.serverAddr)

	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, peer := range t.peers {
		if peer.PublicAddr != nil {
			t.conn.WriteToUDP(encoded, peer.PublicAddr)
		}
	}
}

func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	t.conn.WriteToUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), t.serverAddr)
}

// ── Keepalive ──────────────────────────────────────────────────

func (t *Tunnel) keepaliveLoop() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.Encode(protocol.TypeKeepAlive, nil)
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.conn.WriteToUDP(packet, t.serverAddr)
		}
	}
}

// ── Peer Discovery ─────────────────────────────────────────────

func (t *Tunnel) peerDiscoveryLoop() {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	packet := protocol.Encode(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-t.done:
			return
		case <-ticker.C:
			t.conn.WriteToUDP(packet, t.serverAddr)
		}
	}
}

// ── Helpers ────────────────────────────────────────────────────

func setupLog() *os.File {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	logDir := filepath.Join(appData, "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
	if err != nil {
		// Fall back to stderr
		return os.Stderr
	}
	log.SetOutput(f)
	log.Printf("=== GameTunnel 启动 ===")
	return f
}
