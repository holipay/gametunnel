// GameTunnel Client — 通用局域网游戏隧道 (Windows)
//
// CLI 模式：直接在终端运行，Ctrl+C 断开。
//
// Usage:
//
//	gtunnel-client.exe -server 1.2.3.4:4700
//	gtunnel-client.exe -server 1.2.3.4:4700 -name Player1 -room myroom -password secret
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

// Version is set at build time via -ldflags.
var Version = "dev"

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
	roomPass   string
}

func main() {
	serverFlag := flag.String("server", "", "服务器地址 (host:port)")
	nameFlag := flag.String("name", "", "玩家名称")
	roomFlag := flag.String("room", "", "房间ID")
	passFlag := flag.String("password", "", "房间密码")
	mtuFlag := flag.Int("mtu", 1400, "隧道 MTU")
	versionFlag := flag.Bool("version", false, "显示版本")
	flag.Parse()

	if *versionFlag {
		fmt.Printf("gtunnel-client %s\n", Version)
		os.Exit(0)
	}

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
	if *passFlag != "" {
		cfg.RoomPassword = *passFlag
	}
	if cfg.ServerAddr == "" {
		cfg.ServerAddr = "127.0.0.1:4700"
		saveConfig(cfg)
		fmt.Fprintf(os.Stderr, "首次运行，已写入默认配置。请指定服务器地址:\n")
		fmt.Fprintf(os.Stderr, "  gtunnel-client.exe -server 你的服务器IP:4700\n")
		os.Exit(1)
	}

	// Setup logging (file + stderr)
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
		roomPass: cfg.RoomPassword,
		peers:    make(map[string]*Peer),
	}

	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "\n正在断开...")
		cancel()
		t.Disconnect()
	}()

	fmt.Printf("🎮 GameTunnel 客户端 %s\n", Version)
	fmt.Printf("   服务器: %s\n", cfg.ServerAddr)
	fmt.Printf("   玩家:   %s\n", cfg.PlayerName)
	fmt.Printf("   房间:   %s\n", cfg.RoomID)
	if cfg.RoomPassword != "" {
		fmt.Printf("   认证:   HMAC 密码验证\n")
	} else {
		fmt.Printf("   认证:   无\n")
	}
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

// ── Registration (with HMAC auth support) ──────────────────────

func (t *Tunnel) register(ctx context.Context) error {
	reg := &protocol.RegisterPayload{
		RoomID:   t.roomID,
		Username: t.username,
	}
	packet := protocol.EncodeChecked(protocol.TypeRegister, reg.Marshal())

	t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer t.conn.SetReadDeadline(time.Time{})

	for attempt := 0; attempt < 3; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		t.sendUDP(packet, t.serverAddr)

		// Wait for response (AssignIP, AuthChallenge, or Kick)
		msg, err := t.readResponse(ctx)
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				log.Printf("[tunnel] 注册超时，重试 %d/3...", attempt+1)
				continue
			}
			return err
		}

		switch msg.Type {
		case protocol.TypeAssignIP:
			return t.handleAssignIP(msg.Payload)
		case protocol.TypeAuthChallenge:
			if err := t.handleAuthChallenge(msg.Payload); err != nil {
				return err
			}
			// Auth response sent, wait for AssignIP.
			// Reset deadline but DON'T consume an attempt — the server
			// needs time to verify, and retrying the register would be wrong.
			t.conn.SetReadDeadline(time.Now().Add(10 * time.Second))
			attempt-- // don't count auth round as a retry
			continue
		case protocol.TypeKick:
			kick, _ := protocol.UnmarshalKick(msg.Payload)
			return fmt.Errorf("被拒绝: %s", kick.Reason)
		}
	}
	return fmt.Errorf("注册失败（重试3次）")
}

// readResponse reads and decodes one protocol message from the server.
func (t *Tunnel) readResponse(ctx context.Context) (*protocol.Message, error) {
	for {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		buf := make([]byte, 1500)
		n, _, err := t.conn.ReadFromUDP(buf)
		if err != nil {
			return nil, err
		}

		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			return nil, fmt.Errorf("解码响应失败: %w", err)
		}
		return msg, nil
	}
}

func (t *Tunnel) handleAssignIP(payload []byte) error {
	assign, err := protocol.UnmarshalAssignIP(payload)
	if err != nil {
		return fmt.Errorf("解析IP分配失败: %w", err)
	}
	t.virtualIP = assign.VirtualIP
	t.serverIP = assign.ServerIP
	t.subnetMask = net.IPMask(assign.SubnetMask)
	return nil
}

func (t *Tunnel) handleAuthChallenge(payload []byte) error {
	if t.roomPass == "" {
		return fmt.Errorf("服务器需要房间密码，请用 -password 参数指定")
	}

	acp, err := protocol.UnmarshalAuthChallenge(payload)
	if err != nil {
		return fmt.Errorf("解析认证请求失败: %w", err)
	}

	key := protocol.DeriveKey(t.roomPass, t.roomID)
	if key == nil {
		return fmt.Errorf("无法派生认证密钥")
	}

	hmacVal := protocol.ComputeAuthHMAC(key, acp.Challenge, t.roomID, t.username, t.serverAddr)

	resp := &protocol.AuthResponsePayload{
		RoomID:   t.roomID,
		Username: t.username,
		HMAC:     hmacVal,
	}

	packet := protocol.EncodeChecked(protocol.TypeAuthResponse, resp.Marshal())
	t.sendUDP(packet, t.serverAddr)

	log.Printf("[tunnel] 已发送认证响应，等待服务器确认...")
	return nil
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

		// Copy to avoid buffer reuse issues
		pkt := make([]byte, n)
		copy(pkt, buf[:n])

		msg, err := protocol.DecodeChecked(pkt)
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
		t.tunDev.Write(dp.Data) // UnmarshalData already copies Data into a new slice
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
	packet := protocol.EncodeChecked(protocol.TypeHolePunch, punchPayload)

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

		// Validate IPv4 header: version must be 4, total length must match
		if pkt[0]>>4 != 4 {
			continue // not an IPv4 packet
		}
		ihl := int(pkt[0]&0x0F) * 4
		if ihl < 20 || n < ihl {
			continue // invalid header length
		}

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
		t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), peer.PublicAddr)
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
	encoded := protocol.EncodeChecked(protocol.TypeData, dp.Marshal())

	// Send to server only — server will forward to all peers in the room.
	// Do NOT also send directly to P2P peers, as that causes duplicate
	// broadcast delivery (peers would receive the same packet twice:
	// once from P2P direct, once from server relay).
	t.sendUDP(encoded, t.serverAddr)
}

func (t *Tunnel) sendToServer(pkt []byte, srcIP, dstIP net.IP) {
	dp := &protocol.DataPayload{SrcIP: srcIP, DstIP: dstIP, Data: pkt}
	t.sendUDP(protocol.EncodeChecked(protocol.TypeData, dp.Marshal()), t.serverAddr)
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
	packet := protocol.EncodeChecked(protocol.TypeKeepAlive, nil)
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
	packet := protocol.EncodeChecked(protocol.TypePeerRequest, nil)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			t.sendUDP(packet, t.serverAddr)
		}
	}
}

// ── Config ─────────────────────────────────────────────────────

type Config struct {
	ServerAddr   string `json:"server_addr"`
	PlayerName   string `json:"player_name"`
	RoomID       string `json:"room_id"`
	RoomPassword string `json:"room_password,omitempty"`
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
	// Backward-compatible: ignore unknown fields
	type raw struct {
		ServerAddr   string `json:"server_addr"`
		PlayerName   string `json:"player_name"`
		RoomID       string `json:"room_id"`
		RoomPassword string `json:"room_password,omitempty"`
		AutoConnect  *bool  `json:"auto_connect,omitempty"` // ignored
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
		cfg.RoomPassword = r.RoomPassword
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
	return os.WriteFile(path, data, 0600) // 0600: owner-only, protect password
}

// ── Logging (file + stderr) ────────────────────────────────────

func setupLog() *os.File {
	appData := os.Getenv("APPDATA")
	if appData == "" {
		appData = filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming")
	}
	logDir := filepath.Join(appData, "GameTunnel")
	os.MkdirAll(logDir, 0755)
	logPath := filepath.Join(logDir, "gametunnel.log")

	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600) // 0600: owner-only
	if err != nil {
		log.SetOutput(os.Stderr)
		return os.Stderr
	}
	// Write to both file and stderr (tee)
	log.SetOutput(newTeeWriter(f, os.Stderr))
	log.Printf("=== GameTunnel 启动 ===")
	return f
}

// teeWriter writes to two writers (for log → file + stderr).
type teeWriter struct {
	a, b *os.File
}

func newTeeWriter(a, b *os.File) *teeWriter {
	return &teeWriter{a: a, b: b}
}

func (t *teeWriter) Write(p []byte) (n int, err error) {
	// Write to file first; if it fails, still write to stderr but return the error.
	n1, err1 := t.a.Write(p)
	n2, err2 := t.b.Write(p)
	if err1 != nil {
		return n1, err1
	}
	if err2 != nil {
		return n2, err2
	}
	return len(p), nil
}
