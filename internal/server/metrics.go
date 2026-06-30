package server

import (
	"context"
	"sync"
	"time"
)

const (
	// metricsInterval is how often we take a snapshot.
	metricsInterval = 1 * time.Minute

	// metricsSlots is the number of slots in the ring buffer.
	// Ring covers 1 hour at 1 minute intervals.
	metricsSlots = 60
)

// MetricsSample is a single point-in-time snapshot of server metrics.
type MetricsSample struct {
	Timestamp    int64   `json:"t"`  // unix timestamp (seconds)
	Players      int     `json:"p"`  // online player count
	AvgRTT       float64 `json:"r"`  // average RTT in ms (0 = no data)
	AvgLoss      float64 `json:"l"`  // average loss rate 0.0-1.0
	RelayPkts    uint64  `json:"rp"` // relay packets delta since last sample
	DroppedPkts  uint64  `json:"dp"` // dropped packets delta since last sample
	Kicks        uint64  `json:"k"`  // kicks delta since last sample
	Registrations uint64 `json:"rg"` // registrations delta since last sample
	SendErrors   uint64  `json:"se"` // send errors delta since last sample
}

// MetricsTimeSeries holds the ring buffer of metrics samples.
type MetricsTimeSeries struct {
	mu      sync.RWMutex
	samples [metricsSlots]MetricsSample
	writeIdx int       // next write position
	count    int       // number of samples written (capped at metricsSlots)
	full     bool      // true once we've wrapped around at least once
}

// NewMetricsTimeSeries creates a new time-series ring buffer.
func NewMetricsTimeSeries() *MetricsTimeSeries {
	return &MetricsTimeSeries{}
}

// subUint64 returns a-b, clamped to 0 on underflow.
// Used for metric deltas where counters may decrease (e.g. room recreated).
func subUint64(a, b uint64) uint64 {
	if a > b {
		return a - b
	}
	return 0
}

// Append adds a sample to the ring buffer.
func (ts *MetricsTimeSeries) Append(sample MetricsSample) {
	ts.mu.Lock()
	defer ts.mu.Unlock()

	ts.samples[ts.writeIdx] = sample
	ts.writeIdx = (ts.writeIdx + 1) % metricsSlots
	if ts.count < metricsSlots {
		ts.count++
	} else {
		ts.full = true
	}
}

// Snapshot returns all samples in chronological order (oldest first).
func (ts *MetricsTimeSeries) Snapshot() []MetricsSample {
	ts.mu.RLock()
	defer ts.mu.RUnlock()

	if ts.count == 0 {
		return nil
	}

	result := make([]MetricsSample, ts.count)
	if ts.full {
		// Ring has wrapped: read from writeIdx to writeIdx-1
		for i := 0; i < metricsSlots; i++ {
			result[i] = ts.samples[(ts.writeIdx+i)%metricsSlots]
		}
	} else {
		// Not wrapped: read from 0 to count-1
		for i := 0; i < ts.count; i++ {
			result[i] = ts.samples[i]
		}
	}
	return result
}

// metricsLoop periodically samples server metrics and appends to the ring buffer.
func (s *Server) metricsLoop(ctx context.Context) {
	ticker := time.NewTicker(metricsInterval)
	defer ticker.Stop()

	// Initialize "previous" counters for delta calculation
	prevDropped := s.totalPacketsDropped.Load()
	prevSendErr := s.sendErrors.Load()
	var prevRelay, prevKicks, prevRegs uint64

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		now := time.Now()

		// Collect current values
		dropped := s.totalPacketsDropped.Load()
		sendErr := s.sendErrors.Load()

		// Aggregate relay, kicks, registrations and player stats from all rooms
		var relay uint64
		var kicks uint64
		var regs uint64
		var playerCount int
		var totalRTT time.Duration
		var rttCount int
		var totalLoss float64
		var lossCount int

		s.roomMu.RLock()
		for _, room := range s.rooms {
			relay += room.totalPacketsRelay.Load()
			kicks += room.totalKicks.Load()
			regs += room.totalRegistrations.Load()
			room.mu.RLock()
			playerCount += len(room.clients)
			for _, c := range room.clients {
				if c.RTT > 0 {
					totalRTT += c.RTT
					rttCount++
				}
				if c.pingIdx > 0 {
					loss, _ := c.PingStats()
					totalLoss += loss
					lossCount++
				}
			}
			room.mu.RUnlock()
		}
		s.roomMu.RUnlock()

		var avgRTT float64
		if rttCount > 0 {
			avgRTT = float64(totalRTT.Milliseconds()) / float64(rttCount)
		}
		var avgLoss float64
		if lossCount > 0 {
			avgLoss = totalLoss / float64(lossCount)
		}

		sample := MetricsSample{
			Timestamp:    now.Unix(),
			Players:      playerCount,
			AvgRTT:       avgRTT,
			AvgLoss:      avgLoss,
			RelayPkts:    subUint64(relay, prevRelay),
			DroppedPkts:  subUint64(dropped, prevDropped),
			Kicks:        subUint64(kicks, prevKicks),
			Registrations: subUint64(regs, prevRegs),
			SendErrors:   subUint64(uint64(sendErr), uint64(prevSendErr)),
		}
		s.metricsTS.Append(sample)

		// Update previous counters
		prevRelay = relay
		prevDropped = dropped
		prevKicks = kicks
		prevRegs = regs
		prevSendErr = sendErr
	}
}
