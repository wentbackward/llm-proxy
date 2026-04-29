package balancer

import (
	"fmt"
	"sync"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
)

// Balancer coordinates backend state, health checking, and selection
// for all groups in a config. One Balancer per Server.
type Balancer struct {
	groups map[string]*Group
	done   chan struct{}
	wg     sync.WaitGroup
}

// Group holds the state for one load-balanced backend group.
type Group struct {
	Cfg      *config.GroupConfig
	Selector Selector
	States   map[string]*BackendState // keyed by backend ID
}

// New creates a Balancer from the config and starts background goroutines.
func New(cfg *config.Config) *Balancer {
	b := &Balancer{
		groups: make(map[string]*Group, len(cfg.Groups)),
		done:   make(chan struct{}),
	}

	for name, grpCfg := range cfg.Groups {
		states := make(map[string]*BackendState, len(cfg.Backends))
		for _, be := range cfg.GroupBackends(name) {
			states[be.ID] = NewBackendState(be.ID, be.BaseURL, 1)
		}

		var selector Selector
		if grpCfg.Strategy == "sticky_least_loaded" {
			selector = NewStickyLeastLoaded(
				NewInMemoryStore(
					time.Duration(grpCfg.Affinity.TTLSeconds)*time.Second,
					grpCfg.Affinity.MaxEntries,
				),
				grpCfg.Overload.MaxConcurrency,
				grpCfg.Overload.KVCachePct,
			)
		} else {
			selector = NewSelector(grpCfg.Strategy)
		}

		b.groups[name] = &Group{
			Cfg:      grpCfg,
			Selector: selector,
			States:   states,
		}
	}

	b.wg.Add(1)
	go b.healthChecker()

	return b
}

// Select picks a backend from the named group for the given request context.
func (b *Balancer) Select(groupName, key string, ctx *RequestContext) (*BackendState, error) {
	grp, ok := b.groups[groupName]
	if !ok {
		return nil, fmt.Errorf("unknown group %q", groupName)
	}

	pool := make([]*BackendState, 0, len(grp.States))
	for _, st := range grp.States {
		if st.Healthy {
			pool = append(pool, st)
		}
	}

	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}

	return grp.Selector.Select(pool, key, ctx)
}

// Incr increments the in-flight counter for a backend.
func (b *Balancer) Incr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(1)
			return
		}
	}
}

// Decr decrements the in-flight counter for a backend.
func (b *Balancer) Decr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(-1)
			return
		}
	}
}

// Stop shuts down background goroutines. Call on Server shutdown.
func (b *Balancer) Stop() {
	close(b.done)
	b.wg.Wait()
}

// healthChecker runs a single goroutine that pings all backends.
func (b *Balancer) healthChecker() {
	defer b.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.checkAll()
		}
	}
}

func (b *Balancer) checkAll() {
	// Phase 1: stub — mark all healthy.
	// Phase 2: actual HTTP probe per backend.
	for _, grp := range b.groups {
		for _, st := range grp.States {
			st.Healthy = true
			st.LastHealthCheck = time.Now()
		}
	}
}
