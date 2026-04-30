package balancer

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Balancer coordinates backend state, health checking, and selection
// for all groups in a config. One Balancer per Server.
type Balancer struct {
	groups map[string]*Group
	hc     *hcClient
	ac     *aliveChecker
	done   chan struct{}
	wg     sync.WaitGroup
}

// Group holds the state for one load-balanced backend group.
type Group struct {
	Cfg      *config.GroupConfig
	Selector Selector
	States   map[string]*BackendState // keyed by backend ID
}

// scrapeJob describes one metrics scrape target.
type scrapeJob struct {
	id             string
	url            string
	path           string
	interval       time.Duration
	timeout        time.Duration
	staleThreshold time.Duration
	engine         EngineType
	failures       int
}

// healthJob describes one health-check probe target (fallback when metrics unavailable).
type healthJob struct {
	id       string
	url      string
	path     string
	interval time.Duration
	timeout  time.Duration
	failures int
}

// aliveJob describes one alive-check probe target.
type aliveJob struct {
	id             string
	url            string
	interval       time.Duration
	unhealthyAfter int
	probes         []config.AliveProbe
	failures       int
}

// New creates a Balancer from the config and starts background goroutines.
func New(cfg *config.Config) *Balancer {
	b := &Balancer{
		groups: make(map[string]*Group, len(cfg.Groups)),
		hc:     newHCClient(cfg),
		ac:     newAliveChecker(),
		done:   make(chan struct{}),
	}

	for name, grpCfg := range cfg.Groups {
		states := make(map[string]*BackendState, len(cfg.Backends))
		for _, be := range cfg.GroupBackends(name) {
			windowSec := cfg.GetFlowWindowDuration(grpCfg)
			states[be.ID] = NewBackendState(be.ID, be.BaseURL, 1, windowSec)
		}

		var selector Selector
		if grpCfg.Strategy == "sticky_least_loaded" {
			staleThresh := time.Duration(grpCfg.GetStaleThreshold()) * time.Second
			if staleThresh == 0 {
				staleThresh = 30 * time.Second
			}
			selector = NewStickyLeastLoaded(
				NewInMemoryStore(
					time.Duration(grpCfg.Affinity.TTLSeconds)*time.Second,
					grpCfg.Affinity.MaxEntries,
				),
				grpCfg.Overload.MaxConcurrency,
				grpCfg.Overload.KVCachePct,
				staleThresh,
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

	b.startMonitoring(cfg)
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
		if st.IsHealthy() {
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

// startMonitoring launches monitoring goroutines for all backends.
func (b *Balancer) startMonitoring(cfg *config.Config) {
	type backendInfo struct {
		id     string
		url    string
		groups []*config.GroupConfig
	}
	backendMap := make(map[string]*backendInfo)

	for name, grpCfg := range cfg.Groups {
		for _, be := range cfg.GroupBackends(name) {
			info, exists := backendMap[be.ID]
			if !exists {
				info = &backendInfo{id: be.ID, url: be.BaseURL}
				backendMap[be.ID] = info
			}
			info.groups = append(info.groups, grpCfg)
		}
	}

	for _, info := range backendMap {
		var scrapeCfg *config.GroupConfig
		healthCfg := info.groups[0]

		for _, gc := range info.groups {
			if gc.ScrapeEnabled() {
				scrapeCfg = gc
				break
			}
		}

		if scrapeCfg != nil {
			scrapeURL := info.url + scrapeCfg.GetScrapePath()
			timeout := 3 * time.Second
			body, err := b.hc.scrapeMetrics(scrapeURL, timeout)

			if err == nil && len(body) > 0 {
				job := &scrapeJob{
					id:             info.id,
					url:            info.url,
					path:           scrapeCfg.GetScrapePath(),
					interval:       time.Duration(scrapeCfg.GetScrapeInterval()) * time.Second,
					timeout:        timeout,
					staleThreshold: time.Duration(scrapeCfg.GetStaleThreshold()) * time.Second,
				}
				if job.interval == 0 {
					job.interval = 5 * time.Second
				}
				b.wg.Add(1)
				go b.runScrapeAndHealth(job)
				continue
			}
		}

		// Metrics not available - launch health-only loop
		job := &healthJob{
			id:       info.id,
			url:      info.url,
			path:     healthCfg.HealthCheck.Path,
			interval: time.Duration(healthCfg.HealthCheck.IntervalSeconds) * time.Second,
			timeout:  time.Duration(healthCfg.HealthCheck.TimeoutSeconds) * time.Second,
		}
		if job.interval == 0 {
			job.interval = 10 * time.Second
		}
		if job.timeout == 0 {
			job.timeout = 2 * time.Second
		}
		b.wg.Add(1)
		go b.runHealthCheck(job)
	}

	// Start alive checker for all backends
	b.startAliveChecks(cfg)
}

// startAliveChecks launches alive check goroutines for all backends.
func (b *Balancer) startAliveChecks(cfg *config.Config) {
	type backendInfo struct {
		id     string
		url    string
		groups []*config.GroupConfig
	}
	backendMap := make(map[string]*backendInfo)

	for name, grpCfg := range cfg.Groups {
		for _, be := range cfg.GroupBackends(name) {
			info, exists := backendMap[be.ID]
			if !exists {
				info = &backendInfo{id: be.ID, url: be.BaseURL}
				backendMap[be.ID] = info
			}
			info.groups = append(info.groups, grpCfg)
		}
	}

	for _, info := range backendMap {
		aliveCfg := cfg.GetAliveConfig(info.groups[0])
		job := &aliveJob{
			id:             info.id,
			url:            info.url,
			interval:       time.Duration(aliveCfg.IntervalSeconds) * time.Second,
			unhealthyAfter: aliveCfg.UnhealthyAfter,
			probes:         aliveCfg.Probes,
		}
		if job.interval == 0 {
			job.interval = 60 * time.Second
		}
		if job.unhealthyAfter == 0 {
			job.unhealthyAfter = 3
		}
		b.wg.Add(1)
		go b.runAliveCheck(job)
	}
}

// runAliveCheck performs periodic OR-based alive checks.
func (b *Balancer) runAliveCheck(job *aliveJob) {
	defer b.wg.Done()

	// Check immediately on startup
	if len(job.probes) > 0 {
		if alive := b.ac.checkAlive(job.url, job.probes); !alive {
			job.failures++
			b.markUnhealthy(job.id, job.failures)
		} else {
			job.failures = 0
			b.markHealthy(job.id)
		}
	}

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			if len(job.probes) > 0 {
				if alive := b.ac.checkAlive(job.url, job.probes); !alive {
					job.failures++
					b.markUnhealthy(job.id, job.failures)
				} else {
					job.failures = 0
					b.markHealthy(job.id)
				}
			}
		}
	}
}

// runScrapeAndHealth scrapes /metrics and derives health from scrape success/failure.
func (b *Balancer) runScrapeAndHealth(job *scrapeJob) {
	defer b.wg.Done()

	// Scrape immediately on startup
	b.doScrape(job)

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.doScrape(job)
		}
	}
}

// doScrape performs one scrape cycle and updates backend state.
func (b *Balancer) doScrape(job *scrapeJob) {
	body, err := b.hc.scrapeMetrics(job.url+job.path, job.timeout)
	if err != nil {
		job.failures++
		b.markUnhealthy(job.id, job.failures)
		return
	}

	result := parsePrometheusMetrics(strings.NewReader(string(body)), job.engine)

	if !result.Parsed {
		job.failures = 0
		b.markHealthy(job.id)
		b.updateMetrics(job.id, false, 0, 0, 0)
		return
	}

	job.failures = 0
	b.markHealthy(job.id)
	b.updateMetrics(job.id, true, result.RunningReqs, result.WaitingReqs, result.KVCachePct)
}

// runHealthCheck probes one backend on a timer (health-only, no metrics).
func (b *Balancer) runHealthCheck(job *healthJob) {
	defer b.wg.Done()

	probeURL := job.url + job.path

	// Probe immediately so backends are marked before any requests arrive.
	status, err := b.hc.probe(probeURL, job.timeout)
	if err != nil || status != http.StatusOK {
		job.failures++
		b.markUnhealthy(job.id, job.failures)
	} else {
		job.failures = 0
		b.markHealthy(job.id)
	}

	ticker := time.NewTicker(job.interval)
	defer ticker.Stop()

	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			status, err := b.hc.probe(probeURL, job.timeout)
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
			st.SetHealthy()
		}
	}
}

func (b *Balancer) markUnhealthy(id string, failures int) {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			threshold := grp.Cfg.HealthCheck.UnhealthyAfter
			if threshold == 0 {
				threshold = 3
			}
			st.RecordFailure(failures, threshold)
		}
	}
}

// updateMetrics updates the scraped metrics for a backend.
func (b *Balancer) updateMetrics(id string, available bool, running, waiting int, kvCachePct float64) {
	for _, grp := range b.groups {
		if st, ok := grp.States[id]; ok {
			st.UpdateMetrics(available, running, waiting, kvCachePct)
		}
	}
}

// Dispatch records that a request was sent to this backend.
func (b *Balancer) Dispatch(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			if st.Flow != nil {
				st.Flow.Dispatch()
			}
			return
		}
	}
}

// Complete records that a request completed.
func (b *Balancer) Complete(backendID string, success, timedOut bool, ttftMs float64) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			if st.Flow != nil {
				st.Flow.Complete(success, timedOut, ttftMs)
			}
			return
		}
	}
}

// CompleteAndDecr completes a request and decrements the in-flight counter.
func (b *Balancer) CompleteAndDecr(backendID string, success, timedOut bool, ttftMs float64) {
	b.Complete(backendID, success, timedOut, ttftMs)
	b.Decr(backendID)
}
