package config

// GroupConfig defines load-balancing behavior for a named group of backends.
type GroupConfig struct {
	Strategy    string            `yaml:"strategy"` // sticky_least_loaded | least_loaded | round_robin | single
	Affinity    AffinityConfig    `yaml:"affinity"`
	Overload    OverloadConfig    `yaml:"overload"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
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
