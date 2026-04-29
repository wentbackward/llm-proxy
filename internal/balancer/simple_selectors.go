package balancer

import (
	"sync/atomic"
)

// singleSelector always returns the first healthy backend.
// Equivalent to the existing single-backend path.
type singleSelector struct{}

func (s singleSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	return pool[0], nil
}

// roundRobinSelector cycles through the pool.
type roundRobinSelector struct {
	counter atomic.Uint64
}

func (s *roundRobinSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	idx := s.counter.Add(1) - 1
	return pool[idx%uint64(len(pool))], nil
}

// leastLoadedSelector picks the backend with the lowest in-flight count.
type leastLoadedSelector struct{}

func (s leastLoadedSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	return pickLeastLoaded(pool), nil
}

// NewSelector constructs a Selector from the strategy name.
func NewSelector(strategy string) Selector {
	switch strategy {
	case "single":
		return singleSelector{}
	case "round_robin":
		return &roundRobinSelector{}
	case "least_loaded":
		return leastLoadedSelector{}
	case "sticky_least_loaded":
		return nil // handled by StickyLeastLoaded below
	default:
		return &roundRobinSelector{} // fallback
	}
}
