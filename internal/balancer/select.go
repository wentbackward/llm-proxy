package balancer

import (
	"errors"
	"time"
)

// ErrNoHealthyBackend is returned when all backends in a group are down.
var ErrNoHealthyBackend = errors.New("no healthy backend in group")

// Selector chooses one backend from a healthy pool.
type Selector interface {
	Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error)
}

// pickLeastLoaded returns the backend with the lowest effective load score.
// Lower is better. Uses scraped metrics when available, falls back to InFlight.
func pickLeastLoaded(pool []*BackendState, staleThreshold time.Duration) *BackendState {
	best := pool[0]
	bestScore := loadScore(pool[0], staleThreshold)
	for _, b := range pool[1:] {
		score := loadScore(b, staleThreshold)
		if score < bestScore {
			best = b
			bestScore = score
		}
	}
	return best
}

// loadScore computes the effective load for a backend.
// When metrics are fresh: uses scraped RunningReqs + KVCachePct weighting.
// When stale/unavailable: falls back to local InFlight counter.
// Weight divides the score (higher weight → lower effective load).
// Deterministic tiebreak by ID hash to avoid oscillation.
func loadScore(b *BackendState, staleThreshold time.Duration) float64 {
	load := b.GetEffectiveLoad(staleThreshold)

	// Add KV cache pressure when metrics are available and fresh
	if b.MetricsAvailable {
		b.mu.RLock()
		cachePct := b.KVCachePct
		fresh := time.Since(b.LastMetricsUpdate) < staleThreshold
		b.mu.RUnlock()
		if fresh && cachePct > 0 {
			load += cachePct * 2.0
		}
	}

	if b.Weight > 0 {
		load /= float64(b.Weight)
	}
	// Deterministic tiebreak: hash the ID to [0, 1)
	tieBreak := float64(fnv64a([]byte(b.ID))%1000) * 1e-6
	return load + tieBreak
}

// isOverloaded reports whether a backend should be avoided.
// Uses local InFlight vs maxConcurrency, plus scraped KV cache percentage.
func isOverloaded(b *BackendState, maxConcurrency int, kvCachePct float64, staleThreshold time.Duration) bool {
	if maxConcurrency > 0 && int(b.InFlight.Load()) >= maxConcurrency {
		return true
	}
	// Scraped KV cache percentage check
	if kvCachePct > 0 && b.IsOverloadedByMetrics(kvCachePct, staleThreshold) {
		return true
	}
	return false
}
