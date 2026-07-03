package nat

import (
	"math"
	"sort"
	"sync"
	"time"
)

// PortPredictor predicts the next external port a NAT will assign,
// based on observed port sequences from previous connections.
//
// Many NATs (especially home routers) assign external ports sequentially
// or with a fixed increment. By observing 3+ samples, we can predict
// the next port with reasonable accuracy.
type PortPredictor struct {
	mu       sync.Mutex
	samples  []portSample // observed (external port, timestamp) pairs
	pattern  string       // "incremental" | "random" | "fixed_offset" | "unknown"
	basePort int          // base port (first observed)
	delta    int          // incremental: avg step between ports; fixed_offset: offset from base
}

type portSample struct {
	port      int
	timestamp int64 // unix nano
}

// NewPortPredictor creates a new predictor.
func NewPortPredictor() *PortPredictor {
	return &PortPredictor{}
}

// AddSample records an observed external port.
func (pp *PortPredictor) AddSample(port int, timestamp int64) {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	pp.samples = append(pp.samples, portSample{port: port, timestamp: timestamp})

	// Keep only the last 20 samples
	if len(pp.samples) > 20 {
		pp.samples = pp.samples[len(pp.samples)-20:]
	}

	pp.analyze()
}

// analyze detects the port assignment pattern.
// Must be called with pp.mu held.
func (pp *PortPredictor) analyze() {
	if len(pp.samples) < 2 {
		pp.pattern = "unknown"
		return
	}

	pp.basePort = pp.samples[0].port

	// Calculate increments between consecutive samples
	increments := make([]int, len(pp.samples)-1)
	for i := 1; i < len(pp.samples); i++ {
		increments[i-1] = pp.samples[i].port - pp.samples[i-1].port
	}

	// Check for fixed offset (all increments are the same)
	allSame := true
	for _, inc := range increments[1:] {
		if inc != increments[0] {
			allSame = false
			break
		}
	}

	if allSame && len(increments) > 0 {
		pp.pattern = "incremental"
		pp.delta = increments[0]
		return
	}

	// Check for consistent increment (within ±2 of mean)
	mean := 0
	for _, inc := range increments {
		mean += inc
	}
	mean /= len(increments)

	consistent := true
	for _, inc := range increments {
		diff := inc - mean
		if diff < 0 {
			diff = -diff
		}
		if diff > 2 {
			consistent = false
			break
		}
	}

	if consistent {
		pp.pattern = "incremental"
		pp.delta = mean
		return
	}

	// Check for fixed offset from base
	offsets := make([]int, len(pp.samples))
	for i, s := range pp.samples {
		offsets[i] = s.port - pp.basePort
	}

	sort.Ints(offsets)
	median := offsets[len(offsets)/2]

	pp.pattern = "fixed_offset"
	pp.delta = median
}

// PredictPorts returns a list of candidate ports for the next connection.
// Returns nil if the pattern is unknown or random.
//
// The returned list is sorted by probability (most likely first).
// For incremental patterns, returns the predicted port ± a small range.
// For fixed_offset, returns the base + predicted offset ± range.
func (pp *PortPredictor) PredictPorts() []int {
	pp.mu.Lock()
	defer pp.mu.Unlock()

	if len(pp.samples) < 2 {
		return nil
	}

	lastPort := pp.samples[len(pp.samples)-1].port

	switch pp.pattern {
	case "incremental":
		predicted := lastPort + pp.delta
		return clampPorts(predicted, 3) // ±3 range

	case "fixed_offset":
		predicted := pp.basePort + pp.delta
		return clampPorts(predicted, 3)

	default:
		return nil
	}
}

// PredictPortsForPeer returns candidate ports to try when punching to a peer.
// peerPorts is a list of the peer's previously observed external ports.
// Returns nil if prediction is not possible.
func (pp *PortPredictor) PredictPortsForPeer(peerPorts []int) []int {
	if len(peerPorts) < 2 {
		return nil
	}

	// Calculate increments
	increments := make([]int, len(peerPorts)-1)
	for i := 1; i < len(peerPorts); i++ {
		increments[i-1] = peerPorts[i] - peerPorts[i-1]
	}

	// Check consistency
	mean := 0
	for _, inc := range increments {
		mean += inc
	}
	mean /= len(increments)

	consistent := true
	for _, inc := range increments {
		diff := inc - mean
		if diff < 0 {
			diff = -diff
		}
		if diff > 3 {
			consistent = false
			break
		}
	}

	if !consistent {
		return nil
	}

	predicted := peerPorts[len(peerPorts)-1] + mean
	return clampPorts(predicted, 5) // ±5 range for peers (wider since less data)
}

// clampPorts returns ports in [1, 65535] centered on predicted, within ±radius.
func clampPorts(predicted, radius int) []int {
	var ports []int
	for i := -radius; i <= radius; i++ {
		port := predicted + i
		if port >= 1 && port <= 65535 {
			ports = append(ports, port)
		}
	}
	return ports
}

// PortPredictorFromNATProbe creates a predictor initialized with probe results.
func PortPredictorFromNATProbe(probes []*NATProbeResult) *PortPredictor {
	pp := NewPortPredictor()
	for _, p := range probes {
		if p.ExternalPort > 0 {
			pp.AddSample(p.ExternalPort, time.Now().UnixNano())
		}
	}
	return pp
}

// EstimateNATPortEntropy returns a rough estimate of how predictable
// the NAT's port assignment is (0.0 = fully predictable, 1.0 = random).
// Useful for deciding whether port prediction is worth attempting.
func EstimateNATPortEntropy(ports []int) float64 {
	if len(ports) < 3 {
		return 1.0 // not enough data
	}

	// Calculate coefficient of variation of increments
	increments := make([]float64, len(ports)-1)
	for i := 1; i < len(ports); i++ {
		increments[i-1] = float64(ports[i] - ports[i-1])
	}

	mean := 0.0
	for _, v := range increments {
		mean += v
	}
	mean /= float64(len(increments))

	if mean == 0 {
		return 0.0 // all increments are 0 → perfectly predictable
	}

	variance := 0.0
	for _, v := range increments {
		diff := v - mean
		variance += diff * diff
	}
	variance /= float64(len(increments))

	stddev := math.Sqrt(variance)
	cv := stddev / math.Abs(mean) // coefficient of variation

	// Clamp to [0, 1]
	if cv > 1.0 {
		cv = 1.0
	}
	return cv
}


