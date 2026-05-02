package balancer

import (
	"log"
	"slices"
	"time"
)

// StickyLeastLoaded pins requests to a backend by affinity key,
// falling back to least-loaded when the pinned target is overloaded or unavailable.
type StickyLeastLoaded struct {
	store          AffinityStore
	maxConcurrency int
	kvCachePct     float64
	staleThreshold time.Duration
}

func NewStickyLeastLoaded(store AffinityStore, maxConcurrency int, kvCachePct float64, staleThreshold time.Duration) *StickyLeastLoaded {
	return &StickyLeastLoaded{
		store:          store,
		maxConcurrency: maxConcurrency,
		kvCachePct:     kvCachePct,
		staleThreshold: staleThreshold,
	}
}

func (s *StickyLeastLoaded) Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}

	staleThreshold := s.staleThreshold
	if ctx != nil && ctx.StaleThreshold > 0 {
		staleThreshold = ctx.StaleThreshold
	}

	// Try affinity pin
	if key != "" {
		if entry, ok := s.store.Get(key); ok {
			pinned := findByID(pool, entry.BackendID)
			if pinned != nil {
				overloaded := isOverloaded(pinned, s.maxConcurrency, s.kvCachePct, staleThreshold)
				if !overloaded {
					s.store.Touch(key)
					return pinned, nil
				}
				log.Printf("[lb] affinity key=%-16s pinned=%s OVERLOADED (inflight=%d, max=%d)",
					key[:min(len(key), 16)], pinned.ID, pinned.InFlight.Load(), s.maxConcurrency)
			}
			if pinned == nil {
				log.Printf("[lb] affinity key=%-16s pinned=%s NOT IN POOL (pool=%v)",
					key[:min(len(key), 16)], entry.BackendID, poolIDs(pool))
			}
		}
	}

	// Filter out ramping-up backends for NEW affinity pins
	// (they can still be selected if no other option exists)
	filtered := make([]*BackendState, 0, len(pool))
	ramping := make([]*BackendState, 0, len(pool))
	for _, b := range pool {
		if b.IsRampingUp() {
			ramping = append(ramping, b)
		} else {
			filtered = append(filtered, b)
		}
	}

	// Prefer non-ramping backends for new pins
	var chosen *BackendState
	if len(filtered) > 0 {
		chosen = pickLeastLoaded(filtered, staleThreshold)
	} else if len(ramping) > 0 {
		// All backends are ramping — pick the least loaded among them
		chosen = pickLeastLoaded(ramping, staleThreshold)
	} else {
		chosen = pickLeastLoaded(pool, staleThreshold)
	}

	// Pin the new choice (but not to ramping-up backends if alternatives exist)
	if key != "" && chosen != nil && !chosen.IsRampingUp() {
		s.store.Set(key, AffinityEntry{BackendID: chosen.ID})
	}

	return chosen, nil
}

// findByID returns the backend with the given ID from the pool, or nil.
func findByID(pool []*BackendState, id string) *BackendState {
	for _, b := range pool {
		if b.ID == id {
			return b
		}
	}
	return nil
}

// poolIDs extracts sorted backend IDs from a pool for logging.
func poolIDs(pool []*BackendState) []string {
	ids := make([]string, len(pool))
	for i, b := range pool {
		ids[i] = b.ID
	}
	slices.Sort(ids)
	return ids
}
