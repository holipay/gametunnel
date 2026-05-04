// GameTunnel Client — 通用局域网游戏隧道 (Windows)
//
// CLI 模式：直接在终端运行，Ctrl+C 断开。
//
// Usage:
//
//	gtunnel-client.exe -server 1.2.3.4:4700
//	gtunnel-client.exe -server 1.2.3.4:4700 -name Player1 -room myroom
//	gtunnel-client.exe  (使用配置文件)
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

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
	connMu     sync.Mutex // protects WriteToUDP (not safe for concurrent use)
	serverAddr *net.UDPAddr
	tunDev     *tun.Device
	virtualIP  net.IP
	serverIP   net.IP
	subnetMask net.IPMask
	peers      map[string]*Peer
	mu         sync.RWMutex
	username   string
	roomID     string
}

func main() {
	serverFlag := flag.String("server", "", "服务器地址 (host:port)")
	nameFlag := flag.String("name", "", "玩家名称")
	roomFlag := flag.String("room", "", "房间ID")
	mtuFlag := flag.Int("mtu", 1400, "隧道 MTU")
	flag.Parse()

	// Load config (CLI flags override)
	cfg := loadConfig()
	if *serverFlag != "" {
		cfg.ServerAddr = *serverFlag
	}
	if *nameFlag != "" {
		cfg.PlayerName = *nameFlag
	}
	if *roomFlag != "" {
		cfg.RoomID = *roomFlag
	}
	if cfg.ServerAddr == "" {
		cfg.ServerAddr = "127.0.0.1:4700"
		saveConfig(cfg)
		fmt.Fprintf(os.Stderr, "首次运行，已写入默认配置。请指定服务器地址:\n")
		fmt.Fprintf(os.Stderr, "  gtunnel-client.exe -server 你的服务器IP:4700\n")
		os.Exit(1)
	}

	// Setup logging
	logFile := setupLog()
	defer logFile.Close()

	// Setup signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	t := &Tunnel{
		username: cfg.PlayerName,
		roomID:   cfg.RoomID,
		peers:    make(map[string]*Peer),
	}

	// Connect
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n正在断开...")
		cancel()
		t.Disconnect()
	}()

	fmt.Printf("🎮 GameTunnel 客户端\n")
	fmt.Printf("   服务器: %s\n", cfg.ServerAddr)
	fmt.Printf("   玩家:   %s\n", cfg.RoomID)
	fmt.Printf("   房间:   %s\n", cfg.RoomID)
	fmt.Printf("\n正在连接...\n")

	t.Connect(ctx, cfg.ServerAddr, *mtuFlag)
}

// ── Connect / Disconnect ───────────────────────────────────────

func (t *Tunnel) Connect(ctx context.Context, serverAddr string, mtu int) {
	sAddr, err := net.ResolveUDPAddr("udp4", serverAddr)
	if err != nil {
		log.Fatalf("服务器地址无效: %v", err)
	}
	t.serverAddr = sAddr

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		log.Fatalf("绑定 UDP 失败: %v", err)
	}
	t.conn = conn

	if err := t.register(ctx); err != nil {
		log.Fatalf("注册失败: %v", err)
	}

	tunCfg := tun.Config{
		VirtualIP:  t.virtualIP,
		SubnetMask: t.subnetMask,
		ServerIP:   t.serverIP,
		MTU:        mtu,
	}
	tunDev, err := tun.New(tunCfg)
	if err != nil {
		log.Fatalf("创建 TUN 失败: %v", err)
	}
	t.tunDev = tunDev
	defer tunDev.Close()

	fmt.Printf("\n✅ 已连接! 虚拟IP: %s\n", t.virtualIP)
	fmt.Printf("   打开游戏，进入局域网模式即可\n")
	fmt.Printf("   按 Ctrl+C 断开\n\n")

	// Run tunnel loops
	go t.receiveFromServer(ctx)
	go t.receiveFromTUN(ctx)
	go t.keepaliveLoop(ctx)
	go t.peerDiscoveryLoop(ctx)

	<-ctx.Done()
	log.Printf("[tunnel] 断开连接")
	fmt.Println("已断开。")
}

func (t *Tunnel) Disconnect() {
	if t.conn != nil {
		t.conn.Close()
	}
}

// ── Registration ───────────────────────────────────────────────

func (t *Tunnel) register(ctx context.Context) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.roomID,
		Username: t.username,
	}
	packet := protocol.Encode(protocol.TypeRegister, reg.Marshal())

	t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer t.conn.SetReadDeadline(time.Time{})

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

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

func (t *Tunnel) receiveFromServer(ctx context.Context) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			select {
			case <-ctx.Done():
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

	newPeers := make(map[string]*Peer, len(info.Peers))
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
}

// ── Data from Server ───────────────────────────────────────────

func (t *Tunnel) handleDataFromServer(payload []byte) {
	dp, err := protocol.UnmarshalData(payload)
	if err != nil {
		return
	}
	if len(dp.Data) > 0 && t.tunDev != nil {
		// Copy to avoid buffer reuse issues
		pkt := make([]byte, len(dp.Data))
		copy(pkt, dp.Data)
		t.tunDev.Write(pkt)
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
		t.sendUDP(packet, peer.PublicAddr)
		time.Sleep(200 * time.Millisecond)
	}
}

// ── TUN Reader ─────────────────────────────────────────────────

func (t *Tunnel) receiveFromTUN(ctx context.Context) {
	buf := make([]byte, 65535)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		n, err := t.tunDev.Read(buf)
		if err != nil {
			select {
			case <-ctx.Done():
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
		t.sendUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
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
	t.sendUDP(encoded, t.serverAddr)

	t.mu.RLock()
	defer t.mu.RUnlock()
	for _, peer := range t.peers {
		if peer.PublicAddr != nil {
			t.sendUDP(encoded, peer.PublicAddr)
		}
	}
}

func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	t.sendUDP(protocol.Encode(protocol.TypeData, dp.Marshal()), t.serverAddr)
}

// ── sendUDP — thread-safe UDP write ────────────────────────────

func (t *Tunnel) sendUDP(data []byte, addr *net.UDPAddr) {
	t.connMu.Lock()
	defer t.connMu.Unlock()
	if t.conn != nil {
		t.conn.WriteToUDP(data, addr)
	}
}

// ── Keepalive ──────────────────────────────────────────────────

func (t *Tunnel) keepaliveLoop(ctx context.Context) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	packet := protocol.Encode(protocol.TypeKeepAlive, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}

// ── Peer Discovery ─────────────────────────────────────────────

func (t *Tunnel) peerDiscoveryLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	packet := protocol.Encode(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}

// ── Config (simplified, no GUI dependency) ─────────────────────

type Config struct {
	ServerAddr string `json:"server_addr"`
	PlayerName string `json:"player_name"`
	RoomID     string `json:"room_id"`
}

func configPath() string {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	return filepath.Join(appData, "GameTunnel", "config.json")
}

func loadConfig() *Config {
	hostname, _ := os.Hostname()
	cfg := &Config{
		ServerAddr: "",
		PlayerName: hostname,
		RoomID:     "default",
	}
	data, err := os.ReadFile(configPath())
	if err != nil {
		return cfg
	}
	// Best-effort parse, ignore fields we don't know
	type raw struct {
		ServerAddr  string `json:"server_addr"`
		PlayerName  string `json:"player_name"`
		RoomID      string `json:"room_id"`
		AutoConnect *bool  `json:"auto_connect,omitempty"` // ignored
	}
	var r raw
	if err := json.Unmarshal(data, &r); err == nil {
		if r.ServerAddr != "" {
			cfg.ServerAddr = r.ServerAddr
		}
		if r.PlayerName != "" {
			cfg.PlayerName = r.PlayerName
		}
		if r.RoomID != "" {
			cfg.RoomID = r.RoomID
		}
	}
	return cfg
}

func saveConfig(cfg *Config) error {
	path := configPath()
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0644)
}

// ── Logging ────────────────────────────────────────────────────

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
		return os.Stderr
	}
	log.SetOutput(f)
	log.Printf("=== GameTunnel 启动 ===")
	return f
}
