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
// Incorporates flow statistics for quality-aware routing.
// When multiple backends tie, uses keyHash to deterministically spread
// assignments (0 means no spread — picks first in pool order).
func pickLeastLoaded(pool []*BackendState, staleThreshold time.Duration, keyHash uint64) *BackendState {
	bestScore := loadScore(pool[0], staleThreshold)
	tied := []*BackendState{pool[0]}

	for _, b := range pool[1:] {
		score := loadScore(b, staleThreshold)
		if score < bestScore {
			bestScore = score
			tied = []*BackendState{b}
		} else if score == bestScore {
			tied = append(tied, b)
		}
	}

	if len(tied) == 1 || keyHash == 0 {
		return tied[0]
	}
	// Hash-based spread among equally-loaded backends
	return tied[keyHash%uint64(len(tied))]
}

// loadScore computes the composite load score for a backend.
// Combines: effective load (scraped or local), KV cache pressure, and flow quality penalties.
// Formula: score = effective_load * (1.0 + staleness_penalty) * (1.0 + failure_penalty) / weight
// Lower score = preferred backend.
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

	// Apply flow quality penalties
	if b.Flow != nil {
		stats := b.Flow.GetStats()

		// Staleness penalty: penalize backends with low success rates
		// 0.0 if 100% success, up to 2.0 if 0% success
		stalenessPenalty := (1.0 - stats.SuccessRate) * 2.0

		// Failure penalty: penalize backends with high timeout rates
		// 0.0 if no timeouts, up to 3.0 if all requests timeout
		var failurePenalty float64
		if stats.Dispatched > 0 {
			failurePenalty = float64(stats.Timeout) / float64(stats.Dispatched) * 3.0
		}

		// Stall penalty: penalize backends with many stalled (incomplete) requests
		stallPenalty := float64(stats.Stalled) * 3.0

		// Composite: multiply load by quality factors
		load = load*(1.0+stalenessPenalty)*(1.0+failurePenalty) + stallPenalty
	}

	if b.Weight > 0 {
		load /= float64(b.Weight)
	}
	// Deterministic tiebreak: hash the ID to [0, 1)
	tieBreak := float64(fnv64a([]byte(b.ID))%1000) * 1e-6
	return load + tieBreak
}

// isOverloaded reports whether a backend should be avoided.
// Uses local InFlight vs maxConcurrency, scraped KV cache percentage,
// and flow quality (high stall count indicates trouble).
func isOverloaded(b *BackendState, maxConcurrency int, kvCachePct float64, staleThreshold time.Duration) bool {
	if maxConcurrency > 0 && int(b.InFlight.Load()) >= maxConcurrency {
		return true
	}
	// Scraped KV cache percentage check
	if kvCachePct > 0 && b.IsOverloadedByMetrics(kvCachePct, staleThreshold) {
		return true
	}
	// Flow-based overload: if too many stalled requests, the backend is struggling
	if b.Flow != nil {
		stats := b.Flow.GetStats()
		if stats.Stalled > 10 { // configurable threshold; 10 is conservative
			return true
		}
	}
	return false
}
