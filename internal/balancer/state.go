// Package balancer selects a backend from a load-balanced group,
// preserving prefix-cache affinity.
package balancer

import (
	"sync"
	"sync/atomic"
	"time"
)

// BackendState tracks runtime state for one backend in a group.
type BackendState struct {
	ID     string
	URL    string
	Weight int // from config, default 1

	mu sync.RWMutex

	// Health (protected by mu)
	Healthy             bool
	ConsecutiveFailures int
	LastHealthCheck     time.Time

	// Recovery state (protected by mu)
	RampingUp          bool
	RampUpEnd          time.Time
	UnhealthySince     time.Time
	RecentFailureUntil time.Time // exclusion window after connection-level failure

	// Scraped metrics (protected by mu)
	MetricsAvailable  bool
	RunningReqs       int
	WaitingReqs       int
	KVCachePct        float64
	LastMetricsUpdate time.Time

	// Flow tracking (separate mutex in FlowStats)
	Flow *FlowStats

	// Local fallback (always tracked, even when metrics are disabled)
	InFlight atomic.Int64 // requests currently being proxied
}

// NewBackendState creates a healthy BackendState.
func NewBackendState(id, url string, weight, windowSeconds int) *BackendState {
	return &BackendState{
		ID:      id,
		URL:     url,
		Weight:  weight,
		Healthy: true,
		Flow:    NewFlowStats(windowSeconds),
	}
}

// IsHealthy returns whether the backend is currently marked healthy.
func (b *BackendState) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Healthy
}

// IsRampingUp returns whether the backend is in ramp-up phase.
// During ramp-up, existing affinity pins are honored but new pins
// are declined (prefer other backends for new sessions).
func (b *BackendState) IsRampingUp() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if !b.RampingUp {
		return false
	}
	// Check if ramp-up period has expired
	if time.Now().After(b.RampUpEnd) {
		return false
	}
	return true
}

// FinishRampUp marks the backend as fully recovered (exit ramp-up).
func (b *BackendState) FinishRampUp() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.RampingUp = false
}

// SetHealthy marks the backend as healthy and enters ramp-up phase.
func (b *BackendState) SetHealthy(rampUpSeconds int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Healthy = true
	b.ConsecutiveFailures = 0
	b.LastHealthCheck = time.Now()
	if rampUpSeconds > 0 {
		b.RampingUp = true
		b.RampUpEnd = time.Now().Add(time.Duration(rampUpSeconds) * time.Second)
	} else {
		b.RampingUp = false
	}
}

// RecordFailure increments the failure counter and marks unhealthy
// when consecutive failures reach the threshold.
func (b *BackendState) RecordFailure(failures, threshold int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ConsecutiveFailures = failures
	if threshold == 0 {
		threshold = 3
	}
	if failures >= threshold && b.Healthy {
		b.Healthy = false
		b.UnhealthySince = time.Now()
	}
	b.LastHealthCheck = time.Now()
}

// RecordDispatchFailure marks the backend as having a recent connection-level
// failure, excluding it from selection for a cooldown period (default 10s).
// This bridges the gap until the background alive probe marks it unhealthy.
func (b *BackendState) RecordDispatchFailure(cooldownSeconds int) {
	if cooldownSeconds <= 0 {
		cooldownSeconds = 10
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.RecentFailureUntil = time.Now().Add(time.Duration(cooldownSeconds) * time.Second)
}

// IsFailing returns true if the backend is in a recent-failure cooldown window.
func (b *BackendState) IsFailing() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return time.Now().Before(b.RecentFailureUntil)
}

// RecordRequestOutcome tracks health from real traffic. Success resets
// the failure counter; failure increments it. After unhealthyAfter
// consecutive failures, marks the backend unhealthy. This is the
// ground-truth signal — if a backend handles messages, it's alive.
func (b *BackendState) RecordRequestOutcome(success bool, unhealthyAfter, rampUpSeconds int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.LastHealthCheck = time.Now()

	if success {
		b.ConsecutiveFailures = 0
		if !b.Healthy {
			b.Healthy = true
			if rampUpSeconds > 0 {
				b.RampingUp = true
				b.RampUpEnd = time.Now().Add(time.Duration(rampUpSeconds) * time.Second)
			} else {
				b.RampingUp = false
			}
		} else if b.RampingUp && time.Now().After(b.RampUpEnd) {
			b.RampingUp = false
		}
		return
	}

	b.ConsecutiveFailures++
	if unhealthyAfter <= 0 {
		unhealthyAfter = 3
	}
	if b.ConsecutiveFailures >= unhealthyAfter && b.Healthy {
		b.Healthy = false
		b.UnhealthySince = time.Now()
	}
}

// ShouldRetry returns whether enough time has passed since becoming unhealthy
// to warrant another health check attempt.
func (b *BackendState) ShouldRetry(retryDelaySec int) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	if b.Healthy {
		return true
	}
	if retryDelaySec <= 0 {
		return true
	}
	return time.Since(b.UnhealthySince) >= time.Duration(retryDelaySec)*time.Second
}

// UpdateMetrics stores freshly scraped metrics for this backend.
func (b *BackendState) UpdateMetrics(metricsAvailable bool, running, waiting int, kvCachePct float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.MetricsAvailable = metricsAvailable
	b.RunningReqs = running
	b.WaitingReqs = waiting
	b.KVCachePct = kvCachePct
	b.LastMetricsUpdate = time.Now()
}

// GetEffectiveLoad returns the effective request count for load scoring.
// When metrics are available and fresh (within staleThreshold), uses scraped RunningReqs.
// Otherwise falls back to local InFlight counter.
func (b *BackendState) GetEffectiveLoad(staleThreshold time.Duration) float64 {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if b.MetricsAvailable && time.Since(b.LastMetricsUpdate) < staleThreshold {
		return float64(b.RunningReqs)
	}
	return float64(b.InFlight.Load())
}

// IsOverloadedByMetrics reports whether the scraped KV cache percentage
// exceeds the configured threshold. Only valid when metrics are fresh.
func (b *BackendState) IsOverloadedByMetrics(kvCachePct float64, staleThreshold time.Duration) bool {
	b.mu.RLock()
	defer b.mu.RUnlock()

	if !b.MetricsAvailable || time.Since(b.LastMetricsUpdate) > staleThreshold {
		return false
	}
	return kvCachePct > 0 && b.KVCachePct >= kvCachePct
}
