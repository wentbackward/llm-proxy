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

	// Local fallback (always tracked, even when metrics are disabled)
	InFlight atomic.Int64 // requests currently being proxied
}

// NewBackendState creates a healthy BackendState.
func NewBackendState(id, url string, weight int) *BackendState {
	return &BackendState{
		ID:      id,
		URL:     url,
		Weight:  weight,
		Healthy: true,
	}
}

// IsHealthy returns whether the backend is currently marked healthy.
func (b *BackendState) IsHealthy() bool {
	b.mu.RLock()
	defer b.mu.RUnlock()
	return b.Healthy
}

// SetHealthy marks the backend as healthy and resets failure count.
func (b *BackendState) SetHealthy() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.Healthy = true
	b.ConsecutiveFailures = 0
	b.LastHealthCheck = time.Now()
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
	if failures >= threshold {
		b.Healthy = false
	}
	b.LastHealthCheck = time.Now()
}
