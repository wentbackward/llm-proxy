package balancer

import (
	"sync"
	"time"
)

// FlowStats tracks message flow statistics for a backend over a rolling window.
type FlowStats struct {
	mu             sync.Mutex
	windowSeconds  int
	dispatched     int
	completed      int
	success        int
	failure        int
	timeout        int
	sumTTFTMs      float64
	lastDispatched time.Time
	lastCompleted  time.Time
	lastSuccess    time.Time
}

// NewFlowStats creates a new FlowStats tracker with the given window duration.
func NewFlowStats(windowSeconds int) *FlowStats {
	return &FlowStats{
		windowSeconds: windowSeconds,
	}
}

// Dispatch records that a request was sent to this backend.
func (fs *FlowStats) Dispatch() {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.dispatched++
	fs.lastDispatched = time.Now()
}

// Complete records that a request completed (regardless of outcome).
func (fs *FlowStats) Complete(success, timedOut bool, ttftMs float64) {
	fs.mu.Lock()
	defer fs.mu.Unlock()
	fs.completed++
	if timedOut {
		fs.timeout++
	} else if success {
		fs.success++
		fs.lastSuccess = time.Now()
	} else {
		fs.failure++
	}
	fs.sumTTFTMs += ttftMs
	fs.lastCompleted = time.Now()
}

// GetStats returns a snapshot of the current flow statistics.
// Computes derived metrics over the rolling window.
func (fs *FlowStats) GetStats() (stats FlowSnapshot) {
	fs.mu.Lock()
	defer fs.mu.Unlock()

	now := time.Now()
	windowDur := time.Duration(fs.windowSeconds) * time.Second

	// Calculate window-relative counts
	// For simplicity, we use total counts but factor in staleness
	stats.Dispatched = fs.dispatched
	stats.Completed = fs.completed
	stats.Success = fs.success
	stats.Failure = fs.failure
	stats.Timeout = fs.timeout
	stats.Stalled = fs.dispatched - fs.completed
	if stats.Stalled < 0 {
		stats.Stalled = 0
	}

	// Average TTFT
	if fs.completed > 0 {
		stats.AvgTTFTMs = fs.sumTTFTMs / float64(fs.completed)
	}

	// Success rate
	if fs.completed > 0 {
		stats.SuccessRate = float64(fs.success) / float64(fs.completed)
	} else {
		stats.SuccessRate = 1.0
	}

	// Throughput (requests per second over window)
	if fs.windowSeconds > 0 {
		stats.Throughput = float64(fs.dispatched) / float64(fs.windowSeconds)
	}

	// Staleness factor: how long since last success (0.0 = fresh, 1.0 = cold)
	if !fs.lastSuccess.IsZero() {
		sinceLastSuccess := now.Sub(fs.lastSuccess)
		if sinceLastSuccess < windowDur {
			stats.StalenessFactor = sinceLastSuccess.Seconds() / windowDur.Seconds()
		} else {
			stats.StalenessFactor = 1.0
		}
	} else {
		stats.StalenessFactor = 1.0
	}

	return
}

// FlowSnapshot is an immutable snapshot of flow statistics.
type FlowSnapshot struct {
	Dispatched      int
	Completed       int
	Success         int
	Failure         int
	Timeout         int
	Stalled         int
	AvgTTFTMs       float64
	SuccessRate     float64
	Throughput      float64
	StalenessFactor float64
}
