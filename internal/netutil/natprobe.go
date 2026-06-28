// Package netutil provides network utility functions for NAT detection,
// port prediction, and TCP transport.
package netutil

import (
	"fmt"
	"log"
	"net"
	"time"

	"github.com/holipay/gametunnel/internal/protocol"
)

// NATType classifies the client's NAT behavior.
// Mirrors protocol.NATType but lives in netutil for client-side logic.
type NATType = protocol.NATType

const (
	NATUnknown        = protocol.NATTypeUnknown
	NATFullCone       = protocol.NATTypeFullCone
	NATRestrictedCone = protocol.NATTypeRestrictedCone
	NATPortRestricted = protocol.NATTypePortRestricted
	NATSymmetric      = protocol.NATTypeSymmetric
	NATNoNAT          = protocol.NATTypeNoNAT
)

// NATProbeResult holds the result of NAT type detection.
type NATProbeResult struct {
	Type        NATType
	ExternalIP  net.IP
	ExternalPort int
	AltIP       net.IP      // address observed from alt port (nil if same)
	AltPort     int
	RTT         time.Duration // probe round-trip time
}

// NATProber performs NAT type detection by sending probe packets to the server.
type NATProber struct {
	conn       *net.UDPConn
	serverAddr *net.UDPAddr
}

// NewNATProber creates a prober that will send probes through the given connection.
func NewNATProber(conn *net.UDPConn, serverAddr *net.UDPAddr) *NATProber {
	return &NATProber{
		conn:       conn,
		serverAddr: serverAddr,
	}
}

// Probe sends a NAT probe to the server and waits for the response.
// The server responds with the client's observed external address and a
// classification hint based on whether the address changes across probes.
//
// probeID is a sequence number (0, 1, 2...) that the server uses to track
// multiple probes from the same client.
func (np *NATProber) Probe(probeID byte, timeout time.Duration) (*NATProbeResult, error) {
	req := &protocol.NATProbePayload{ProbeID: probeID}
	packet := protocol.EncodeChecked(protocol.TypeNATProbe, req.Marshal())

	start := time.Now()
	if _, err := np.conn.WriteToUDP(packet, np.serverAddr); err != nil {
		return nil, fmt.Errorf("send probe: %w", err)
	}

	np.conn.SetReadDeadline(time.Now().Add(timeout))
	defer np.conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 1500)
	for {
		n, _, err := np.conn.ReadFromUDP(buf)
		if err != nil {
			return nil, fmt.Errorf("read probe response: %w", err)
		}

		msg, err := protocol.DecodeChecked(buf[:n])
		if err != nil {
			continue
		}
		if msg.Type != protocol.TypeNATResponse {
			continue // skip non-response packets
		}

		resp, err := protocol.UnmarshalNATResponse(msg.Payload)
		if err != nil {
			return nil, fmt.Errorf("unmarshal NAT response: %w", err)
		}
		if resp.ProbeID != probeID {
			continue // not our probe
		}

		rtt := time.Since(start)
		result := &NATProbeResult{
			Type: resp.NATType,
			RTT:  rtt,
		}
		if resp.ObservedAddr != nil {
			result.ExternalIP = resp.ObservedAddr.IP
			result.ExternalPort = resp.ObservedAddr.Port
		}
		if resp.AltAddr != nil {
			result.AltIP = resp.AltAddr.IP
			result.AltPort = resp.AltAddr.Port
		}
		return result, nil
	}
}

// ProbeNATType performs the full NAT type detection sequence:
// 1. Send 3 probes and analyze the responses to classify NAT type.
// 2. Compare external addresses across probes for consistency.
//
// The server responds to each probe with the client's observed address
// from its main port and an alternate port. By comparing these addresses
// across multiple probes, we can classify the NAT type:
//   - Same IP+Port across probes, same from alt port → Full Cone or No NAT
//   - Same IP, different port from alt → Restricted Cone or Port Restricted
//   - Different IP across probes → Symmetric NAT (carrier-grade)
func ProbeNATType(conn *net.UDPConn, serverAddr *net.UDPAddr) (*NATProbeResult, error) {
	prober := NewNATProber(conn, serverAddr)

	const (
		numProbes     = 3
		probeTimeout  = 3 * time.Second
		probeInterval = 200 * time.Millisecond
	)

	results := make([]*NATProbeResult, 0, numProbes)
	for i := 0; i < numProbes; i++ {
		if i > 0 {
			time.Sleep(probeInterval)
		}
		result, err := prober.Probe(byte(i), probeTimeout)
		if err != nil {
			log.Printf("[nat-probe] probe %d failed: %v", i, err)
			continue
		}
		results = append(results, result)
	}

	if len(results) == 0 {
		return &NATProbeResult{Type: NATUnknown}, fmt.Errorf("all probes failed")
	}

	// Use the first successful result as the base
	base := results[0]

	// Analyze consistency across probes
	base.classifyNATType(results)

	return base, nil
}

// classifyNATType determines the NAT type based on multiple probe results.
// Modifies r.Type in place.
func (r *NATProbeResult) classifyNATType(allResults []*NATProbeResult) {
	if len(allResults) < 2 {
		// Single probe — use server-provided hint
		return
	}

	// Check if external IP is consistent across probes
	sameIP := true
	samePort := true
	for _, other := range allResults[1:] {
		if !r.ExternalIP.Equal(other.ExternalIP) {
			sameIP = false
		}
		if r.ExternalPort != other.ExternalPort {
			samePort = false
		}
	}

	// Check if alt address differs from primary
	altDiffers := false
	if r.AltIP != nil && r.ExternalIP != nil {
		if !r.AltIP.Equal(r.ExternalIP) || r.AltPort != r.ExternalPort {
			altDiffers = true
		}
	}

	switch {
	case !sameIP:
		// Different external IP across probes → Symmetric NAT (carrier-grade)
		r.Type = NATSymmetric
	case sameIP && !samePort:
		// Same IP but different port → Symmetric NAT (port-dependent)
		r.Type = NATSymmetric
	case sameIP && samePort && altDiffers:
		// Same mapping, but alt port sees different → Port Restricted Cone
		r.Type = NATPortRestricted
	case sameIP && samePort && !altDiffers:
		// Consistent mapping, alt port same → Full Cone or No NAT
		// The server already distinguishes these in its response
		if r.Type == NATUnknown {
			r.Type = NATFullCone
		}
	}
}

// HolePunchStrategy returns the recommended hole-punching strategy based on NAT type.
type HolePunchStrategy int

const (
	StrategyDirect    HolePunchStrategy = 0 // direct hole punch, high success rate
	StrategyExtended  HolePunchStrategy = 1 // extended hole punch with more attempts
	StrategyRelay     HolePunchStrategy = 2 // use server relay, skip hole punch
)

// GetHolePunchStrategy returns the recommended strategy for punching between
// two peers based on their NAT types.
func GetHolePunchStrategy(local, remote NATType) HolePunchStrategy {
	// If either side is No NAT or Full Cone, direct punch almost always works
	if local == NATNoNAT || local == NATFullCone ||
		remote == NATNoNAT || remote == NATFullCone {
		return StrategyDirect
	}

	// Symmetric + Symmetric = impossible, use relay
	if local == NATSymmetric && remote == NATSymmetric {
		return StrategyRelay
	}

	// Symmetric + Cone = possible but needs port prediction
	if local == NATSymmetric || remote == NATSymmetric {
		return StrategyExtended
	}

	// Cone + Cone = standard punch works well
	return StrategyDirect
}
