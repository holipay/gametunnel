package client

import (
	"net"
	"sync"

	"github.com/holipay/gametunnel/internal/crypto"
	"github.com/holipay/gametunnel/internal/nat"
	"github.com/holipay/gametunnel/internal/netutil"
	"github.com/holipay/gametunnel/internal/protocol"
)

// session holds connection session state: virtual IP, server identity,
// and authentication context. Grouped together because they're all set
// during registration and read together in the hot path.
type session struct {
	virtualIP    net.IP
	serverIP     net.IP
	serverIPKey  any // atomic.Value stores [16]byte — use accessor
	subnetMask   net.IPMask
	cachedSubnet any // atomic.Pointer[net.IPNet] — use accessor
	sessionToken [16]byte
	serverVersion any // atomic.Uint32 — use accessor
	username     string
	roomID       string
	roomPass     string
}

// cryptoState holds end-to-end encryption state. Grouped together because
// all ciphers are created during registration and cleared on disconnect.
type cryptoState struct {
	encCipher     *crypto.Cipher // client→server (relay send)
	decCipher     *crypto.Cipher // server→client (relay receive)
	p2pCipher     *crypto.Cipher // client↔client (P2P direct)
	ecdhSessionKey []byte        // from ECDH negotiation (nil if not used)
}

// natState holds NAT detection results and hole punch optimization state.
type natState struct {
	probeResult    *nat.NATProbeResult // NAT type from probe (nil if not probed)
	portPredictor  *nat.PortPredictor  // port prediction for hole punching
	cachedPunchPacket any              // atomic.Value stores []byte — use accessor
}

// Tunnel is the GameTunnel client. Sub-structures group related fields:
//   - session: connection identity and credentials
//   - crypto: encryption ciphers
//   - nat: NAT detection and hole punch optimization
//
// Atomic fields (serverAddr, tunDev, etc.) remain on the top level
// because Go atomic types cannot be embedded in sub-structs.
type Tunnel struct {
	// Sub-structures
	session session
	crypto  cryptoState
	nat     natState

	// Network I/O
	conn       *net.UDPConn
	sendCh     chan sendJob
	ctrlCh     chan sendJob
	tunCh      chan tunJob
	serverAddr atomic.Pointer[net.UDPAddr] // snapshot at Connect time

	// TUN device
	tunDev atomic.Value // stores TunDevice

	// Peers
	peers map[[16]byte]*Peer
	mu    sync.RWMutex

	// Lifecycle
	disconnectOnce atomic.Pointer[sync.Once]
	closeTUNOnce   sync.Once
	runCancel      context.CancelFunc
	runDone        chan struct{}
	runWg          sync.WaitGroup
	holePunchWg    sync.WaitGroup

	// Rate limiting & liveness
	sendLimiter      *clientSendLimiter
	lastServerResponse atomic.Int64
	sendErrors       atomic.Int64
	cancelKicks      atomic.Bool

	// TCP fallback
	tcpTransport *netutil.TCPTransport

	// Rebind
	rebindAckCh chan *protocol.RebindAckPayload

	// TUN reuse state
	lastAssignedIP net.IP
	lastMTU        int
	newTUNFunc     func(TunConfig) (TunDevice, error)
}
