package balancer

import (
	"errors"
)

// ErrNoHealthyBackend is returned when all backends in a group are down.
var ErrNoHealthyBackend = errors.New("no healthy backend in group")

// Selector chooses one backend from a healthy pool.
type Selector interface {
	Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error)
}

// pickLeastLoaded returns the backend with the lowest effective load score.
// Lower is better. Uses InFlight as the load signal.
// Weight divides the score (higher weight → lower effective load).
// Deterministic tiebreak by ID hash to avoid oscillation.
func pickLeastLoaded(pool []*BackendState) *BackendState {
	best := pool[0]
	bestScore := loadScore(pool[0])
	for _, b := range pool[1:] {
		score := loadScore(b)
		if score < bestScore {
			best = b
			bestScore = score
		}
	}
	return best
}

func loadScore(b *BackendState) float64 {
	inflight := float64(b.InFlight.Load())
	if b.Weight > 0 {
		inflight /= float64(b.Weight)
	}
	// Deterministic tiebreak: hash the ID to [0, 1)
	tieBreak := float64(fnv64a([]byte(b.ID))%1000) * 1e-6
	return inflight + tieBreak
}

// isOverloaded reports whether a backend should be avoided.
// Phase 1: uses InFlight count vs configured max concurrency.
// Phase 2: incorporates scraped KV cache percentage.
func isOverloaded(b *BackendState, maxConcurrency int, kvCachePct float64) bool {
	_ = kvCachePct // reserved for Phase 2: scraped KV cache percentage
	if maxConcurrency > 0 && int(b.InFlight.Load()) >= maxConcurrency {
		return true
	}
	// Phase 2 (future): incorporate scraped KV cache percentage.
	// if kvCachePct > 0 && b.KVCachePct >= kvCachePct {
	// 	return true
	// }
	return false
}
