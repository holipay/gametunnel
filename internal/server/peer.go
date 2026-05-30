package server

import "time"

const (
	// peerInfoInterval is how often the batch PeerInfo broadcast runs.
	// 50ms coalesces up to ~20 join/leave events per broadcast, acceptable latency for LAN games.
	peerInfoInterval = 50 * time.Millisecond
	peerInfoCacheTTL = peerInfoInterval
)

// pingInterval is how often the server pings clients for RTT measurement.
const pingInterval = 5 * time.Second
