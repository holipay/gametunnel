package server

import (
	"context"
	"fmt"
	"log"
	"math/bits"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/holipay/gametunnel/internal/auth"
	"github.com/holipay/gametunnel/internal/i18n"
	"github.com/holipay/gametunnel/internal/protocol"
)

// Room holds all per-room state. Each room has an independent virtual subnet,
// player list, IP allocation, and authentication.
type Room struct {
	roomID     string
	roomPass   string
	subnet     *net.IPNet
	serverIP   net.IP
	maxPlayers int

	// Network
	conn       *net.UDPConn      // UDP connection for sending packets
	bwLimiter  *BandwidthLimiter // per-client outbound bandwidth limiter

	// Per-room client state
	clients    map[[16]byte]*Client // virtualIP → Client
	addrMap    map[rateKey]*Client  // client endpoint → Client
	mu         sync.RWMutex

	// IP allocation (per-room, independent)
	ipBitmap []uint64

	// Auth
	authKeys    sync.Map // map[string][]byte, roomID → derived key
	pendingAuth int
	maxPending  int

	// Registration rate limiting (per-room)
	regMu       sync.Mutex
	regBuf      [2]map[string]int
	regTick     *time.Ticker
	maxRegPerIP int

	// Per-IP connection count (per-room)
	ipConnMu   sync.Mutex
	ipConnCount map[string]int
	maxPerIP    int

	// PeerInfo batching
	peerInfoDirty   atomic.Bool
	peerInfoMu      sync.Mutex
	peerInfoEncoded []byte
	peerInfoCachedAt time.Time

	// Operational metrics (per-room)
	totalRegistrations  atomic.Uint64
	authFailures        atomic.Uint64
	peakPlayers         atomic.Uint32
	totalPacketsRelay   atomic.Uint64
	totalPacketsDropped atomic.Uint64
	totalKicks          atomic.Uint64
	sendErrors          atomic.Int64

	// Send error logging (rate-limited)
	lastSendErrorLog        time.Time
	sendErrorCountSinceLog  atomic.Int64
	sendErrorMu             sync.Mutex

	// Callbacks (set by Server for state persistence)
	onDirty func() // called when room state changes (for persistence)

	// Timestamps
	createdAt time.Time
}

// RoomConfig holds configuration for creating a new room.
type RoomConfig struct {
	RoomID     string
	RoomPass   string
	Subnet     *net.IPNet
	MaxPlayers int
	MaxPerIP   int
	Conn       *net.UDPConn      // UDP connection for sending packets
	BWLimiter  *BandwidthLimiter // per-client outbound bandwidth limiter (optional)
}

// NewRoom creates a new room. The subnet must be /24.
func NewRoom(cfg RoomConfig) (*Room, error) {
	ones, bits := cfg.Subnet.Mask.Size()
	if bits != 32 || ones != 24 {
		return nil, fmt.Errorf("subnet must be /%d", ones)
	}

	serverIP := make(net.IP, 4)
	copy(serverIP, cfg.Subnet.IP.To4())
	serverIP[3] = 1

	maxPerIP := cfg.MaxPerIP
	if maxPerIP <= 0 {
		maxPerIP = 3
	}

	r := &Room{
		roomID:      cfg.RoomID,
		roomPass:    cfg.RoomPass,
		subnet:      cfg.Subnet,
		serverIP:    serverIP,
		maxPlayers:  cfg.MaxPlayers,
		conn:        cfg.Conn,
		bwLimiter:   cfg.BWLimiter,
		clients:     make(map[[16]byte]*Client),
		addrMap:     make(map[rateKey]*Client),
		ipBitmap:    make([]uint64, 4),
		maxPending:  cfg.MaxPlayers * 3,
		regBuf:      [2]map[string]int{make(map[string]int), make(map[string]int)},
		maxRegPerIP: 5,
		ipConnCount: make(map[string]int),
		maxPerIP:    maxPerIP,
		createdAt:   time.Now(),
	}

	// Reserve network, server, and broadcast addresses
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 0))
	r.markIPUsed(serverIP)
	r.markIPUsed(net.IPv4(serverIP[0], serverIP[1], serverIP[2], 255))

	// Start per-room reg rate limiter
	r.regTick = time.NewTicker(time.Second)
	go r.regRateLimitLoop()

	return r, nil
}

// ── IP Bitmap ──────────────────────────────────────────────────

func (r *Room) markIPUsed(ip net.IP) {
	octet := ip.To4()[3]
	r.ipBitmap[octet/64] |= 1 << (octet % 64)
}

func (r *Room) markIPFree(ip net.IP) {
	octet := ip.To4()[3]
	r.ipBitmap[octet/64] &^= 1 << (octet % 64)
}

func (r *Room) nextAvailableIP() net.IP {
	base := r.subnet.IP.To4()
	for i, word := range r.ipBitmap {
		if word == ^uint64(0) {
			continue
		}
		free := ^word
		for free != 0 {
			bit := bits.TrailingZeros64(free)
			octet := i*64 + bit
			if octet >= 2 && octet < 255 {
				return net.IPv4(base[0], base[1], base[2], byte(octet))
			}
			free &= free - 1 // clear lowest set bit
		}
	}
	return nil
}

// ── Registration Rate Limiting ─────────────────────────────────

func (r *Room) checkRegRate(ip string) bool {
	r.regMu.Lock()
	r.regBuf[0][ip]++
	ok := r.regBuf[0][ip] <= r.maxRegPerIP
	r.regMu.Unlock()
	return ok
}

func (r *Room) regRateLimitLoop() {
	for range r.regTick.C {
		r.regMu.Lock()
		r.regBuf[0], r.regBuf[1] = r.regBuf[1], r.regBuf[0]
		for k := range r.regBuf[1] {
			delete(r.regBuf[1], k)
		}
		r.regMu.Unlock()
	}
}

// ── Auth ───────────────────────────────────────────────────────

func (r *Room) getAuthKey(roomID string) []byte {
	if v, ok := r.authKeys.Load(roomID); ok {
		return v.([]byte)
	}
	key := auth.DeriveKey(r.roomPass, roomID)
	if key != nil {
		r.authKeys.Store(roomID, key)
	}
	return key
}

// ── Packet Handling ────────────────────────────────────────────

// HandlePacket dispatches a packet within this room.
func (r *Room) HandlePacket(msgType byte, payload []byte, from *net.UDPAddr) {
	switch msgType {
	case protocol.TypeRegister:
		r.handleRegister(payload, from)
	case protocol.TypeAuthResponse:
		r.handleAuthResponse(payload, from)
	case protocol.TypeKeepAlive:
		r.handleKeepAlive(from)
	case protocol.TypePeerRequest:
		r.handlePeerRequest(from)
	case protocol.TypeData:
		r.handleRelay(payload, from)
	case protocol.TypeHolePunch:
		r.handleHolePunch(payload, from)
	case protocol.TypeDisconnect:
		r.handleDisconnect(from)
	case protocol.TypePong:
		r.handlePong(payload, from)
	}
}

// ── Register ───────────────────────────────────────────────────

func (r *Room) handleRegister(payload []byte, from *net.UDPAddr) {
	reg, err := protocol.UnmarshalRegister(payload)
	if err != nil {
		return
	}

	t := i18n.T()

	if len(reg.Username) == 0 || len(reg.Username) > maxUsernameLen {
		r.sendKick(from, t.KickInvalidName)
		return
	}
	if len(reg.RoomID) == 0 || len(reg.RoomID) > maxRoomIDLen {
		r.sendKick(from, t.KickInvalidRoom)
		return
	}

	clientIP := from.IP.String()
	if !r.checkRegRate(clientIP) {
		r.sendKick(from, t.KickRateLimit)
		return
	}

	r.mu.Lock()
	fromKey := addrToRateKey(from)

	if existing := r.addrMap[fromKey]; existing != nil && existing.auth == authChallengeSent {
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthPending)
		return
	}

	if existing := r.addrMap[fromKey]; existing != nil {
		existing.LastSeen = time.Now()
		selfIP := existing.VirtualIP
		r.mu.Unlock()
		r.sendAssignIP(selfIP, from)
		r.sendPeerInfoToClient(from)
		return
	}

	r.ipConnMu.Lock()
	ipCount := r.ipConnCount[clientIP]
	r.ipConnMu.Unlock()
	if ipCount >= r.maxPerIP {
		r.mu.Unlock()
		r.sendKick(from, t.KickIPLimit)
		return
	}

	if len(r.clients) >= r.maxPlayers {
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	}

	for _, c := range r.clients {
		if c.auth == authNone || c.auth == authChallengeSent {
			continue
		}
		if c.authRoomID == reg.RoomID && c.Username == reg.Username {
			r.mu.Unlock()
			r.sendKick(from, t.KickDuplicateName)
			return
		}
	}

	if r.roomPass == "" {
		r.registerClientLocked(reg, from)
		return
	}

	if r.pendingAuth >= r.maxPending {
		r.mu.Unlock()
		r.sendKick(from, t.KickServerBusy)
		return
	}

	r.sendAuthChallengeLocked(reg, from)
}

func (r *Room) registerClientLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	vip := r.nextAvailableIP()
	if vip == nil {
		r.mu.Unlock()
		r.sendKick(from, t.KickIPExhausted)
		return
	}

	r.markIPUsed(vip)

	c := &Client{
		Username:   reg.Username,
		VirtualIP:  vip,
		PublicAddr: from,
		LastSeen:   time.Now(),
		auth:       authNone,
	}
	r.clients[ipKey(vip)] = c
	r.addrMap[addrToRateKey(from)] = c

	clientIP := from.IP.String()
	r.ipConnMu.Lock()
	r.ipConnCount[clientIP]++
	r.ipConnMu.Unlock()

	log.Printf(t.LogPlayerJoin, reg.Username, from, vip, len(r.clients))

	r.totalRegistrations.Add(1)
	if cur := uint32(len(r.clients)); cur > r.peakPlayers.Load() {
		r.peakPlayers.Store(cur)
	}

	selfIP := vip
	r.mu.Unlock()

	r.sendAssignIP(selfIP, from)
	r.sendPeerInfoToClient(from)
	r.peerInfoDirty.Store(true)
	r.markDirty()
}

func (r *Room) sendAuthChallengeLocked(reg *protocol.RegisterPayload, from *net.UDPAddr) {
	t := i18n.T()
	challenge, err := auth.GenerateChallenge()
	if err != nil {
		r.mu.Unlock()
		log.Printf(t.LogChallengeFail, err)
		r.sendKick(from, t.KickInternalError)
		return
	}

	c := &Client{
		Username:    reg.Username,
		PublicAddr:  from,
		LastSeen:    time.Now(),
		auth:        authChallengeSent,
		challenge:   challenge,
		challengeAt: time.Now(),
		authRoomID:  reg.RoomID,
	}
	r.addrMap[addrToRateKey(from)] = c
	r.pendingAuth++
	r.mu.Unlock()

	acp := &protocol.AuthChallengePayload{Challenge: challenge, ClientAddr: from.String()}
	r.sendChecked(protocol.TypeAuthChallenge, acp.Marshal(), from)
}

func (r *Room) handleAuthResponse(payload []byte, from *net.UDPAddr) {
	resp, err := protocol.UnmarshalAuthResponse(payload)
	if err != nil {
		return
	}
	if len(resp.HMAC) != auth.HMACSize {
		return
	}

	t := i18n.T()
	r.mu.Lock()
	fromKey := addrToRateKey(from)
	c := r.addrMap[fromKey]

	if c == nil || c.auth != authChallengeSent {
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthAbnormal)
		return
	}

	if time.Since(c.challengeAt) > 15*time.Second {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickAuthTimeout)
		return
	}

	authKey := r.getAuthKey(c.authRoomID)
	if authKey == nil {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		r.mu.Unlock()
		r.sendKick(from, t.KickInternalError)
		return
	}

	if !auth.VerifyHMAC(authKey, resp.HMAC, c.challenge, resp.RoomID, resp.Username, from) {
		delete(r.addrMap, fromKey)
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
		r.mu.Unlock()
		log.Printf(t.LogAuthFail, resp.Username, from)
		r.authFailures.Add(1)
		r.sendKick(from, t.KickWrongPassword)
		return
	}

	log.Printf(t.LogAuthPass, resp.Username, from)
	delete(r.addrMap, fromKey)
	if r.pendingAuth > 0 {
		r.pendingAuth--
	}

	if len(r.clients) >= r.maxPlayers {
		r.mu.Unlock()
		r.sendKick(from, t.KickRoomFull)
		return
	}

	for _, existing := range r.clients {
		if existing.auth == authNone || existing.auth == authChallengeSent {
			continue
		}
		if existing.authRoomID == resp.RoomID && existing.Username == resp.Username {
			r.mu.Unlock()
			r.sendKick(from, t.KickDuplicateName)
			return
		}
	}

	reg := &protocol.RegisterPayload{RoomID: resp.RoomID, Username: resp.Username}
	r.registerClientLocked(reg, from)
}

// ── KeepAlive / Disconnect / Pong ──────────────────────────────

func (r *Room) handleKeepAlive(from *net.UDPAddr) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if c := r.addrMap[addrToRateKey(from)]; c != nil {
		c.LastSeen = time.Now()
	}
}

func (r *Room) handleDisconnect(from *net.UDPAddr) {
	fromKey := addrToRateKey(from)
	r.mu.Lock()
	c := r.addrMap[fromKey]
	if c == nil {
		r.mu.Unlock()
		return
	}
	log.Printf(i18n.T().LogPlayerLeave, c.Username, c.VirtualIP)
	if c.auth == authChallengeSent {
		if r.pendingAuth > 0 {
			r.pendingAuth--
		}
	} else {
		r.markIPFree(c.VirtualIP)
		delete(r.clients, ipKey(c.VirtualIP))
		ip := c.PublicAddr.IP.String()
		r.ipConnMu.Lock()
		r.ipConnCount[ip]--
		if r.ipConnCount[ip] <= 0 {
			delete(r.ipConnCount, ip)
		}
		r.ipConnMu.Unlock()
	}
	delete(r.addrMap, fromKey)
	r.mu.Unlock()

	if r.bwLimiter != nil {
		r.bwLimiter.Remove(from)
	}
	r.peerInfoDirty.Store(true)
	r.markDirty()
}

func (r *Room) handlePong(payload []byte, from *net.UDPAddr) {
	ping, err := protocol.UnmarshalPing(payload)
	if err != nil {
		return
	}
	rtt := time.Since(time.Unix(0, ping.Timestamp))
	if rtt < 0 || rtt > 10*time.Second {
		return
	}
	r.mu.Lock()
	if c := r.addrMap[addrToRateKey(from)]; c != nil {
		c.RTT = rtt
		c.pingHistory[c.pingIdx%pingHistorySize] = rtt
		c.pingIdx++
	}
	r.mu.Unlock()
}

// ── Relay ──────────────────────────────────────────────────────

func (r *Room) handleRelay(payload []byte, from *net.UDPAddr) {
	if len(payload) < 8 {
		return
	}

	srcIP := net.IP(payload[0:4])
	dstIP := net.IP(payload[4:8])

	r.mu.RLock()
	sender := r.addrMap[addrToRateKey(from)]
	if sender == nil {
		r.mu.RUnlock()
		return
	}

	if !srcIP.Equal(sender.VirtualIP) {
		r.mu.RUnlock()
		return
	}

	isBroadcast := protocol.IsRelayTarget(dstIP, r.subnet)

	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]

	if isBroadcast {
		for _, c := range r.clients {
			if c != sender {
				targets = append(targets, c.PublicAddr)
			}
		}
	} else {
		if dst, ok := r.clients[ipKey(dstIP)]; ok {
			targets = append(targets, dst.PublicAddr)
		}
	}
	r.mu.RUnlock()

	if len(targets) == 0 {
		return
	}
	encoded := protocol.EncodeChecked(protocol.TypeData, payload)
	packetSize := len(encoded)
	for _, addr := range targets {
		if r.bwLimiter == nil || r.bwLimiter.Allow(addr, packetSize) {
			r.sendCheckedRaw(encoded, addr)
		}
	}
	r.totalPacketsRelay.Add(1)
}

func (r *Room) handleHolePunch(payload []byte, from *net.UDPAddr) {
	if len(payload) < 4 {
		return
	}
	dstIP := net.IP(payload[:4])

	r.mu.RLock()
	src, ok1 := r.addrMap[addrToRateKey(from)]
	dst, ok2 := r.clients[ipKey(dstIP)]
	r.mu.RUnlock()

	if !ok1 || !ok2 {
		return
	}

	if src.VirtualIP == nil {
		return
	}

	addrStr := from.String()
	punchData := make([]byte, 4+len(addrStr))
	copy(punchData[:4], src.VirtualIP.To4())
	copy(punchData[4:], []byte(addrStr))
	r.sendChecked(protocol.TypeHolePunch, punchData, dst.PublicAddr)
}

// ── Peer Info ──────────────────────────────────────────────────

func (r *Room) handlePeerRequest(from *net.UDPAddr) {
	r.mu.RLock()
	c := r.addrMap[addrToRateKey(from)]
	r.mu.RUnlock()
	if c == nil {
		return
	}
	r.sendPeerInfoToClient(from)
}

func (r *Room) sendPeerInfoToClient(target *net.UDPAddr) {
	encoded := r.getEncodedPeerInfo()
	r.sendCheckedRaw(encoded, target)
}

func (r *Room) sendPeerInfoBroadcast() {
	r.mu.RLock()
	if len(r.clients) == 0 {
		r.mu.RUnlock()
		return
	}

	// Use stack-allocated array for small rooms to avoid heap allocation
	var stackTargets [maxInlineTargets]*net.UDPAddr
	targets := stackTargets[:0]
	for _, c := range r.clients {
		targets = append(targets, c.PublicAddr)
	}
	r.mu.RUnlock()

	encoded := r.getEncodedPeerInfo()
	for _, addr := range targets {
		r.sendCheckedRaw(encoded, addr)
	}
}

func (r *Room) getEncodedPeerInfo() []byte {
	now := time.Now()
	r.peerInfoMu.Lock()
	if r.peerInfoEncoded != nil && now.Sub(r.peerInfoCachedAt) < peerInfoCacheTTL {
		encoded := r.peerInfoEncoded
		r.peerInfoMu.Unlock()
		return encoded
	}

	r.mu.RLock()
	peers := protocol.GetPeerInfoPayload()
	peers.Peers = peers.Peers[:0] // reset slice but keep capacity
	for _, c := range r.clients {
		peers.Peers = append(peers.Peers, protocol.PeerInfoEntry{
			VirtualIP:  c.VirtualIP,
			PublicAddr: c.PublicAddr,
			Username:   c.Username,
		})
	}
	r.mu.RUnlock()

	encoded := protocol.EncodeChecked(protocol.TypePeerInfo, peers.Marshal())
	protocol.PutPeerInfoPayload(peers)
	r.peerInfoEncoded = encoded
	r.peerInfoCachedAt = now
	r.peerInfoMu.Unlock()
	return encoded
}

// ── Send Helpers ───────────────────────────────────────────────

// sendErrorLogInterval is how often to log send errors (rate limiting).
const sendErrorLogInterval = 1 * time.Minute

func (r *Room) sendChecked(typ byte, payload []byte, to *net.UDPAddr) {
	data := protocol.EncodeChecked(typ, payload)
	if _, err := r.conn.WriteToUDP(data, to); err != nil {
		r.sendErrors.Add(1)
		r.logSendError(err)
	}
}

func (r *Room) sendCheckedRaw(data []byte, to *net.UDPAddr) {
	if _, err := r.conn.WriteToUDP(data, to); err != nil {
		r.sendErrors.Add(1)
		r.logSendError(err)
	}
}

// logSendError logs send errors with rate limiting.
// Instead of logging every error (or only powers of 2), it logs
// a summary every sendErrorLogInterval when errors are occurring.
func (r *Room) logSendError(err error) {
	r.sendErrorCountSinceLog.Add(1)

	r.sendErrorMu.Lock()
	defer r.sendErrorMu.Unlock()

	now := time.Now()
	if now.Sub(r.lastSendErrorLog) < sendErrorLogInterval {
		return
	}

	count := r.sendErrorCountSinceLog.Swap(0)
	if count > 0 {
		r.lastSendErrorLog = now
		log.Printf(i18n.T().ServerSendFail, r.sendErrors.Load(), 
			fmt.Sprintf("(%d errors in last minute, last: %v)", count, err))
	}
}

func (r *Room) sendKick(to *net.UDPAddr, reason string) {
	kick := &protocol.KickPayload{Reason: reason}
	r.sendChecked(protocol.TypeKick, kick.Marshal(), to)
	r.totalKicks.Add(1)
}

func (r *Room) sendAssignIP(vip net.IP, to *net.UDPAddr) {
	assign := &protocol.AssignIPPayload{
		VirtualIP:  vip,
		SubnetMask: r.subnet.Mask,
		ServerIP:   r.serverIP,
	}
	r.sendChecked(protocol.TypeAssignIP, assign.Marshal(), to)
}

// markDirty notifies the server that room state has changed (for persistence).
func (r *Room) markDirty() {
	if r.onDirty != nil {
		r.onDirty()
	}
}

// ── Keepalive Cleanup ──────────────────────────────────────────

// CleanupStale removes clients that haven't been seen for 30s.
// Returns true if any clients were removed.
func (r *Room) CleanupStale() bool {
	now := time.Now()

	r.mu.RLock()
	type staleClient struct {
		key [16]byte
		c   *Client
	}
	type staleAuth struct {
		key rateKey
		c   *Client
	}
	var staleClients []staleClient
	var staleAuths []staleAuth
	for key, c := range r.clients {
		if now.Sub(c.LastSeen) > 30*time.Second {
			staleClients = append(staleClients, staleClient{key: key, c: c})
		}
	}
	for addrKey, c := range r.addrMap {
		if c.auth == authChallengeSent && now.Sub(c.challengeAt) > 30*time.Second {
			staleAuths = append(staleAuths, staleAuth{key: addrKey, c: c})
		}
	}
	r.mu.RUnlock()

	if len(staleClients) == 0 && len(staleAuths) == 0 {
		return false
	}

	r.mu.Lock()
	changed := false
	for _, sc := range staleClients {
		if cur, ok := r.clients[sc.key]; ok && cur == sc.c {
			log.Printf("%s", i18n.Format(i18n.T().ServerPeerLeave, sc.c.Username, sc.c.VirtualIP))
			r.markIPFree(sc.c.VirtualIP)
			delete(r.clients, sc.key)
			delete(r.addrMap, addrToRateKey(sc.c.PublicAddr))
			ip := sc.c.PublicAddr.IP.String()
			r.ipConnMu.Lock()
			r.ipConnCount[ip]--
			if r.ipConnCount[ip] <= 0 {
				delete(r.ipConnCount, ip)
			}
			r.ipConnMu.Unlock()
			changed = true
		}
	}
	for _, sa := range staleAuths {
		if cur, ok := r.addrMap[sa.key]; ok && cur == sa.c {
			delete(r.addrMap, sa.key)
			r.pendingAuth--
		}
	}
	r.mu.Unlock()

	if changed {
		r.markDirty()
	}
	return changed
}

// ── Status Info ────────────────────────────────────────────────

// RoomStatusInfo holds per-room status for the status page.
type RoomStatusInfo struct {
	RoomID      string           `json:"room_id"`
	Players     int              `json:"players"`
	MaxPlayers  int              `json:"max_players"`
	HasAuth     bool             `json:"has_auth"`
	Connections []ConnectionInfo `json:"connections,omitempty"`

	TotalRegistrations  uint64 `json:"total_registrations"`
	AuthFailures        uint64 `json:"auth_failures"`
	PeakPlayers         uint32 `json:"peak_players"`
	TotalPacketsRelay   uint64 `json:"total_packets_relay"`
	TotalPacketsDropped uint64 `json:"total_packets_dropped"`
	TotalKicks          uint64 `json:"total_kicks"`
	SendErrors          int64  `json:"send_errors"`
}

// BuildRoomStatus creates a RoomStatusInfo snapshot.
func (r *Room) BuildRoomStatus() RoomStatusInfo {
	t := i18n.T()
	now := time.Now()

	r.mu.RLock()
	conns := make([]ConnectionInfo, 0, len(r.clients))
	for _, c := range r.clients {
		idle := now.Sub(c.LastSeen)
		idleStr := t.StatusJustNow
		if idle > time.Second {
			idleStr = fmt.Sprintf(t.StatusSecAgo, int(idle.Seconds()))
		}
		pubAddr := ""
		if c.PublicAddr != nil {
			pubAddr = c.PublicAddr.String()
		}
		pingStr := "--"
		if c.RTT > 0 {
			pingStr = fmt.Sprintf("%dms", c.RTT.Milliseconds())
		}
		lossRate, jitter := c.PingStats()
		lossStr := "--"
		if c.pingIdx > 0 {
			lossStr = fmt.Sprintf("%.0f%%", lossRate*100)
		}
		jitterStr := "--"
		if jitter > 0 {
			jitterStr = fmt.Sprintf("%dms", jitter.Milliseconds())
		}
		conns = append(conns, ConnectionInfo{
			Username:   c.Username,
			VirtualIP:  c.VirtualIP.String(),
			PublicAddr: pubAddr,
			Idle:       idleStr,
			Ping:       pingStr,
			Loss:       lossStr,
			Jitter:     jitterStr,
		})
	}
	r.mu.RUnlock()

	return RoomStatusInfo{
		RoomID:              r.roomID,
		Players:             len(conns),
		MaxPlayers:          r.maxPlayers,
		HasAuth:             r.roomPass != "",
		Connections:         conns,
		TotalRegistrations:  r.totalRegistrations.Load(),
		AuthFailures:        r.authFailures.Load(),
		PeakPlayers:         r.peakPlayers.Load(),
		TotalPacketsRelay:   r.totalPacketsRelay.Load(),
		TotalPacketsDropped: r.totalPacketsDropped.Load(),
		TotalKicks:          r.totalKicks.Load(),
		SendErrors:          r.sendErrors.Load(),
	}
}

// ClientCount returns the number of authenticated clients in the room.
func (r *Room) ClientCount() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.clients)
}

// ── Room Lifecycle Loops ─────────────────────────────────────

// peerInfoLoop periodically checks the dirty flag and broadcasts PeerInfo.
// This coalesces rapid join/leave events into a single broadcast per interval.
func (r *Room) peerInfoLoop(ctx context.Context) {
	ticker := time.NewTicker(peerInfoInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		if r.peerInfoDirty.CompareAndSwap(true, false) {
			r.sendPeerInfoBroadcast()
		}
	}
}

// pingLoop periodically sends TypePing to all authenticated clients
// and tracks timeout (missed pong) for loss rate calculation.
func (r *Room) pingLoop(ctx context.Context) {
	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()

		r.mu.Lock()
		if len(r.clients) == 0 {
			r.mu.Unlock()
			continue
		}

		// Mark previous pings as missed if no pong received within 2*interval.
		for _, c := range r.clients {
			if !c.lastPingSent.IsZero() && now.Sub(c.lastPingSent) > 2*pingInterval {
				c.pingHistory[c.pingIdx%pingHistorySize] = 0 // missed
				c.pingIdx++
			}
		}

		// Send pings and record sequence/time.
		ts := now.UnixNano()
		ping := &protocol.PingPayload{Timestamp: ts}
		encoded := protocol.EncodeChecked(protocol.TypePing, ping.Marshal())
		for _, c := range r.clients {
			c.pingSeq++
			c.lastPingSent = now
			c.lastPingSeq = c.pingSeq
			r.sendCheckedRaw(encoded, c.PublicAddr)
		}
		r.mu.Unlock()
	}
}

// ── State Persistence ────────────────────────────────────────

// SnapshotState creates a RoomState from the current in-memory state.
func (r *Room) SnapshotState() RoomState {
	r.mu.RLock()
	defer r.mu.RUnlock()

	clients := make(map[string]ClientEntry, len(r.clients))
	for _, c := range r.clients {
		// Skip clients still in auth challenge (not fully registered)
		if c.auth == authChallengeSent {
			continue
		}
		ipStr := c.VirtualIP.String()
		clients[ipStr] = ClientEntry{
			Username:  c.Username,
			VirtualIP: ipStr,
			LastSeen:  c.LastSeen,
		}
	}

	return RoomState{
		Version:   stateVersion,
		Subnet:    r.subnet.String(),
		UpdatedAt: time.Now(),
		IPBitmap:  r.ipBitmap,
		Clients:   clients,
	}
}

// resolveRestoredClient handles a client that was restored from persisted state.
// When a client reconnects and its virtual IP was pre-reserved, we attach the
// real PublicAddr and return the existing IP.
// Returns the restored client if matched, nil otherwise.
// MUST be called with r.mu held.
func (r *Room) resolveRestoredClient(username string, roomID string, from *net.UDPAddr) *Client {
	// Look for a placeholder client with matching username and no PublicAddr
	for _, c := range r.clients {
		if c.Username == username && c.PublicAddr == nil && c.auth == authNone {
			// Attach the real address
			c.PublicAddr = from
			c.LastSeen = time.Now()
			r.addrMap[addrToRateKey(from)] = c

			// Track per-IP connection count
			clientIP := from.IP.String()
			r.ipConnMu.Lock()
			r.ipConnCount[clientIP]++
			r.ipConnMu.Unlock()

			return c
		}
	}
	return nil
}
