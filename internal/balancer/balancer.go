package balancer

import (
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Balancer coordinates backend state, health checking, and selection
// for all groups in a config. One Balancer per Server.
type Balancer struct {
	groups map[string]*Group
	hc     *hcClient
	done   chan struct{}
	wg     sync.WaitGroup
}

// Group holds the state for one load-balanced backend group.
type Group struct {
	Cfg      *config.GroupConfig
	Selector Selector
	States   map[string]*BackendState // keyed by backend ID
}

// hcJob describes one health-check probe target.
type hcJob struct {
	id       string
	url      string
	interval time.Duration
	timeout  time.Duration
	path     string
	failures int
}

// New creates a Balancer from the config and starts background goroutines.
func New(cfg *config.Config) *Balancer {
	b := &Balancer{
		groups: make(map[string]*Group, len(cfg.Groups)),
		hc:     newHCClient(cfg),
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

	b.startHealthChecks(cfg)
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

// startHealthChecks launches one probe goroutine per unique backend.
func (b *Balancer) startHealthChecks(cfg *config.Config) {
	jobs := make(map[string]*hcJob)

	for name, grpCfg := range cfg.Groups {
		for _, be := range cfg.GroupBackends(name) {
			job := jobs[be.ID]
			if job == nil {
				job = &hcJob{id: be.ID, url: be.BaseURL}
				jobs[be.ID] = job
			}
			if job.interval == 0 {
				job.interval = time.Duration(grpCfg.HealthCheck.IntervalSeconds) * time.Second
			}
			if job.timeout == 0 {
				job.timeout = time.Duration(grpCfg.HealthCheck.TimeoutSeconds) * time.Second
			}
			if job.path == "" {
				job.path = grpCfg.HealthCheck.Path
			}
		}
	}

	for _, job := range jobs {
		b.wg.Add(1)
		go b.runHealthCheck(job)
	}
}

// runHealthCheck probes one backend on a timer.
func (b *Balancer) runHealthCheck(job *hcJob) {
	defer b.wg.Done()

	probeURL := job.url + job.path
	interval := job.interval
	if interval == 0 {
		interval = 10 * time.Second
	}
	timeout := job.timeout
	if timeout == 0 {
		timeout = 2 * time.Second
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			status, err := b.hc.probe(probeURL, timeout)
			if err != nil || status != http.StatusOK {
				job.failures++
				b.markUnhealthy(job.id, job.failures)
			} else {
				job.failures = 0
				b.markHealthy(job.id)
			}
		}
	}
}

func (b *Balancer) markHealthy(id string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			st.Healthy = true
			st.ConsecutiveFailures = 0
			st.LastHealthCheck = time.Now()
		}
	}
}

func (b *Balancer) markUnhealthy(id string, failures int) {
	for _, grp := range b.groups {
		st, ok := grp.States[id]
		if !ok {
			continue
		}
		st.ConsecutiveFailures = failures
		threshold := grp.Cfg.HealthCheck.UnhealthyAfter
		if threshold == 0 {
			threshold = 3
		}
		if failures >= threshold {
			st.Healthy = false
		}
		st.LastHealthCheck = time.Now()
	}
}
