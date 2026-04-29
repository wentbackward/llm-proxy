// Package balancer selects a backend from a load-balanced group,
// preserving prefix-cache affinity.
package balancer

import (
	"sync/atomic"
	"time"
)

// BackendState tracks runtime state for one backend in a group.
type BackendState struct {
	ID     string
	URL    string
	Weight int // from config, default 1

	// Health
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
