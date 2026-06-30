package server

import (
	"crypto/ecdh"
	"crypto/rand"
	"log"
	"net"
	"sync/atomic"
	"time"
)

// ── Auth State ─────────────────────────────────────────────────

type authState int

const (
	authNone          authState = iota // no password required, or already authenticated
	authChallengeSent                  // challenge sent, waiting for response
)

// ── Client State ───────────────────────────────────────────────

// pingHistorySize is the number of recent ping results kept per client
// for loss rate and jitter calculation.
const pingHistorySize = 12

// Client represents a connected player.
type Client struct {
	Username   string
	VirtualIP  net.IP
	PublicAddr *net.UDPAddr
	lastSeen   atomic.Int64 // unix nano, use GetLastSeen/SetLastSeen
	RTT        time.Duration // latest round-trip latency

	// Session token (v1.7+): 16-byte random token for anti-spoofing.
	// Clients with version >= 0x0107 include this in relay packets.
	SessionToken [16]byte

	// Ping quality stats (ring buffer of recent RTTs, 0 = missed)
	pingHistory  [pingHistorySize]time.Duration
	pingIdx      int       // next write position in pingHistory
	pingSeq      uint32    // monotonic ping sequence (for timeout detection)
	lastPingSent time.Time // when the last ping was sent
	lastPingSeq  uint32    // sequence of the last ping sent

	// Auth state (only used when server has a room password)
	auth        authState
	challenge   []byte    // 16-byte nonce
	challengeAt time.Time // for expiry
	authRoomID  string    // room ID from register request (for key derivation)

	// Client version from Register (0 = old client without version)
	clientVersion uint16

	// ECDH state (forward secrecy): ephemeral keypair for session key negotiation.
	// Populated after HMAC auth success, used during ECDH handshake.
	ecdhPriv    *ecdh.PrivateKey // server's ephemeral private key
	ecdhPub     []byte           // server's ephemeral public key (32 bytes)
	ecdhPending bool             // true if waiting for client's ECDHConfirm

	// Session key derived from ECDH shared secret (nil if ECDH not used).
	// Used to create encryption ciphers for this client's session.
	SessionKey []byte
}

func (c *Client) GetLastSeen() time.Time {
	return time.Unix(0, c.lastSeen.Load())
}

func (c *Client) SetLastSeen(t time.Time) {
	c.lastSeen.Store(t.UnixNano())
}

// GenerateSessionToken fills the client's SessionToken with 16 random bytes.
func (c *Client) GenerateSessionToken() {
	if _, err := rand.Read(c.SessionToken[:]); err != nil {
		log.Printf("failed to generate session token: %v", err)
	}
}

// HasSessionToken returns true if the client has a non-zero session token.
func (c *Client) HasSessionToken() bool {
	return c.SessionToken != [16]byte{}
}

// ValidateSessionToken checks if the given token matches the client's stored token.
func (c *Client) ValidateSessionToken(token []byte) bool {
	return len(token) == 16 && c.SessionToken == [16]byte(token)
}

// PingStats returns loss rate (0.0-1.0) and jitter from recent ping history.
func (c *Client) PingStats() (lossRate float64, jitter time.Duration) {
	total := c.pingIdx
	if total == 0 {
		return 0, 0
	}
	n := total
	if n > pingHistorySize {
		n = pingHistorySize
	}

	var received int
	var prevRTT time.Duration
	var jitterSum time.Duration
	var jitterCount int

	// Read from the ring buffer in chronological order: oldest entry first.
	// When pingIdx >= pingHistorySize, the oldest entry is at pingIdx % pingHistorySize.
	start := 0
	if total > pingHistorySize {
		start = total % pingHistorySize
	}
	for i := 0; i < n; i++ {
		rtt := c.pingHistory[(start+i)%pingHistorySize]
		if rtt == 0 {
			continue // missed
		}
		received++
		if prevRTT > 0 {
			diff := rtt - prevRTT
			if diff < 0 {
				diff = -diff
			}
			jitterSum += diff
			jitterCount++
		}
		prevRTT = rtt
	}

	lossRate = 1.0 - float64(received)/float64(n)
	if jitterCount > 0 {
		jitter = jitterSum / time.Duration(jitterCount)
	}
	return
}
