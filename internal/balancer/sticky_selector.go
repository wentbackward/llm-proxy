package balancer

import "time"

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
			if pinned := findByID(pool, entry.BackendID); pinned != nil {
				if !isOverloaded(pinned, s.maxConcurrency, s.kvCachePct, staleThreshold) {
					s.store.Touch(key)
					return pinned, nil
				}
				// Pinned target is overloaded — bail, but don't evict the entry.
				// It may recover before TTL expires.
			}
			// Pinned backend not in healthy pool — fall through.
		}
	}

	// Select via fallback (least loaded)
	chosen := pickLeastLoaded(pool, staleThreshold)

	// Pin the new choice
	if key != "" {
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
