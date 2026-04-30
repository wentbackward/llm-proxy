package config

// GroupConfig defines load-balancing behavior for a named group of backends.
type GroupConfig struct {
	Strategy      string              `yaml:"strategy"` // sticky_least_loaded | least_loaded | round_robin | single
	Affinity      AffinityConfig      `yaml:"affinity"`
	Overload      OverloadConfig      `yaml:"overload"`
	HealthCheck   HealthCheckConfig   `yaml:"health_check"`
	MetricsScrape MetricsScrapeConfig `yaml:"metrics_scrape"`
}

// AffinityConfig controls prefix-cache affinity within a group.
type AffinityConfig struct {
	Key             string `yaml:"key"`               // first_user_message | header:NAME | none
	MaxContentBytes int    `yaml:"max_content_bytes"` // default: 2048
	TTLSeconds      int    `yaml:"ttl_seconds"`       // default: 3600
	MaxEntries      int    `yaml:"max_entries"`       // default: 10000
}

// OverloadConfig defines thresholds for backing off from a busy backend.
type OverloadConfig struct {
	MaxConcurrency     int     `yaml:"max_concurrency"`
	KVCachePct         float64 `yaml:"kv_cache_pct"`
	StaleMetricsAction string  `yaml:"stale_metrics_action"` // pin | bail
}

// HealthCheckConfig defines periodic probing of backends within a group.
type HealthCheckConfig struct {
	Path            string `yaml:"path"`
	IntervalSeconds int    `yaml:"interval_seconds"`
	TimeoutSeconds  int    `yaml:"timeout_seconds"`
	UnhealthyAfter  int    `yaml:"unhealthy_after"`
}

// MetricsScrapeConfig controls background scraping of backend /metrics endpoints.
type MetricsScrapeConfig struct {
	Enabled               string `yaml:"enabled"`                 // true | false | auto (default: false)
	IntervalSeconds       int    `yaml:"interval_seconds"`        // default: 5
	Path                  string `yaml:"path"`                    // default: /metrics
	StaleThresholdSeconds int    `yaml:"stale_threshold_seconds"` // default: 30
}

// GetStaleThreshold returns the stale threshold in seconds for a group.
// Default: 30s.
func (g *GroupConfig) GetStaleThreshold() int {
	if g.MetricsScrape.StaleThresholdSeconds > 0 {
		return g.MetricsScrape.StaleThresholdSeconds
	}
	return 30
}

// GetScrapeInterval returns the scrape interval in seconds for a group.
// Default: 5s.
func (g *GroupConfig) GetScrapeInterval() int {
	if g.MetricsScrape.IntervalSeconds > 0 {
		return g.MetricsScrape.IntervalSeconds
	}
	return 5
}

// ScrapeEnabled returns whether metrics scraping should be attempted.
// "auto" means probe at startup and use if available.
// "true" means force-enable (will fail silently if not available).
// "false" or empty means disable.
func (g *GroupConfig) ScrapeEnabled() bool {
	switch g.MetricsScrape.Enabled {
	case "true", "auto":
		return true
	default:
		return false
	}
}

// ScrapeAuto returns true if scraping should be probed at startup.
func (g *GroupConfig) ScrapeAuto() bool {
	return g.MetricsScrape.Enabled == "auto"
}

// GetScrapePath returns the metrics path. Default: /metrics.
func (g *GroupConfig) GetScrapePath() string {
	if g.MetricsScrape.Path != "" {
		return g.MetricsScrape.Path
	}
	return "/metrics"
}
