// Package config loads and validates the proxy YAML configuration and provides
// lookup helpers for backends and routes.
package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// ── Schema ────────────────────────────────────────────────────────────────────

type TLSConfig struct {
	Cert string `yaml:"cert"` // path to certificate file
	Key  string `yaml:"key"`  // path to private key file
}

type TransportConfig struct {
	MaxIdleConns        int `yaml:"max_idle_conns"`          // total idle connections across all backends (default: 100)
	MaxIdleConnsPerHost int `yaml:"max_idle_conns_per_host"` // idle connections per backend (default: 20)
	IdleConnTimeout     int `yaml:"idle_conn_timeout"`       // seconds before idle connections are closed (default: 120)
}

type ServerConfig struct {
	Host                string          `yaml:"host"`
	Port                int             `yaml:"port"`
	APIKey              string          `yaml:"api_key"`              // required bearer token for inbound requests
	PassthroughUnrouted bool            `yaml:"passthrough_unrouted"` // false = reject unknown models; true = forward to first backend
	LogLevel            *int            `yaml:"log_level"`            // 0-4 (see internal/logger); LOG_LEVEL env wins when set
	MaxRequestBodyMB    int             `yaml:"max_request_body_mb"`  // hard cap per request body; default 50, 0 means use default
	TLS                 TLSConfig       `yaml:"tls"`
	Transport           TransportConfig `yaml:"transport"`
}

type PrometheusConfig struct {
	Enabled bool   `yaml:"enabled"`
	Host    string `yaml:"host"` // bind address; default 127.0.0.1 (localhost-only, no auth on metrics)
	Port    int    `yaml:"port"`
	Path    string `yaml:"path"`
}

type TelemetryConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
}

// PortRange holds one or more port numbers, parsed from YAML as:
//   - single int:    ports: 3040
//   - list of ints:  ports: [3040, 3042, 3044]
//   - range string:  ports: "3040-3045"  (inclusive)
type PortRange []int

func (p *PortRange) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		// Try as int first
		if n, err := strconv.Atoi(value.Value); err == nil {
			*p = PortRange{n}
			return nil
		}
		// Try as range "lo-hi"
		parts := strings.SplitN(value.Value, "-", 2)
		if len(parts) != 2 {
			return fmt.Errorf("ports: invalid range %q (expected \"lo-hi\")", value.Value)
		}
		lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
		hi, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err1 != nil || err2 != nil || lo > hi {
			return fmt.Errorf("ports: invalid range %q", value.Value)
		}
		for i := lo; i <= hi; i++ {
			*p = append(*p, i)
		}
		return nil
	case yaml.SequenceNode:
		for _, child := range value.Content {
			n, err := strconv.Atoi(child.Value)
			if err != nil {
				return fmt.Errorf("ports: non-integer value %q", child.Value)
			}
			*p = append(*p, n)
		}
		return nil
	default:
		return fmt.Errorf("ports: expected number, list, or range string")
	}
}

type Backend struct {
	ID             string    `yaml:"id"`
	Type           string    `yaml:"type"` // openai | anthropic
	BaseURL        string    `yaml:"base_url"`
	APIKey         string    `yaml:"api_key"`
	AuthType       string    `yaml:"auth_type"` // bearer | x-api-key | "" (auto: bearer for openai, x-api-key for anthropic)
	TimeoutSeconds int       `yaml:"timeout_seconds"`
	MaxConcurrency int       `yaml:"max_concurrency"` // 0 = unlimited; limits in-flight requests to this backend
	SkipProbe      bool      `yaml:"skip_probe"`      // skip /v1/models health check at startup/SIGHUP
	Default        bool      `yaml:"default"`         // target for passthrough_unrouted and /v1/models; at most one backend may set this
	Ports          PortRange `yaml:"ports"`           // expand this backend into one per port; use {port} in id and base_url
}

type AutoRoute struct {
	Text   string `yaml:"text"`
	Vision string `yaml:"vision"`
}

type Route struct {
	VirtualModel  string                 `yaml:"virtual_model"`
	Backend       string                 `yaml:"backend"`
	RealModel     string                 `yaml:"real_model"`
	ContextLength int                    `yaml:"context_length"` // overrides upstream value in /v1/models; 0 = pass through
	Defaults      map[string]interface{} `yaml:"defaults"`
	Clamp         map[string]interface{} `yaml:"clamp"`
	AutoRoute     *AutoRoute             `yaml:"auto_route"`
}

type JournalConfig struct {
	Enabled      bool   `yaml:"enabled"`
	OTLPEndpoint string `yaml:"otlp_endpoint"` // optional — e.g. "http://otel-collector:4318"
}

// SigMessageCaptureConfig configures the SIGUSR1-armed full-body capture mode.
// Disabled unless Enabled is true AND OutputFolder is set. There is no default
// folder — bodies only land where explicitly configured.
type SigMessageCaptureConfig struct {
	Enabled      bool   `yaml:"enabled"`
	OutputFolder string `yaml:"output_folder"` // required when Enabled; unset = disabled
	MaxMessages  int    `yaml:"max_messages"`  // default 5 (see capture.DefaultMaxMessages)
}

type Config struct {
	Server            ServerConfig            `yaml:"server"`
	Telemetry         TelemetryConfig         `yaml:"telemetry"`
	Journal           JournalConfig           `yaml:"journal"`
	SigMessageCapture SigMessageCaptureConfig `yaml:"sig_message_capture"`
	Backends          []Backend               `yaml:"backends"`
	Routes            []Route                 `yaml:"routes"`

	backendByID  map[string]*Backend
	routeByModel map[string]*Route
}

// ── Loader ────────────────────────────────────────────────────────────────────

func Load(path string) (*Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	// Expand ${ENV_VAR} before parsing
	expanded := expandEnvVars(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	cfg.Backends, err = expandPorts(cfg.Backends)
	if err != nil {
		return nil, err
	}

	applyDefaults(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}

	cfg.backendByID = make(map[string]*Backend, len(cfg.Backends))
	for i := range cfg.Backends {
		cfg.backendByID[cfg.Backends[i].ID] = &cfg.Backends[i]
	}

	cfg.routeByModel = make(map[string]*Route, len(cfg.Routes))
	for i := range cfg.Routes {
		r := &cfg.Routes[i]
		if r.Defaults == nil {
			r.Defaults = make(map[string]interface{})
		}
		if r.Clamp == nil {
			r.Clamp = make(map[string]interface{})
		}
		cfg.routeByModel[r.VirtualModel] = r
	}

	return &cfg, nil
}

func expandEnvVars(s string) string {
	return envVarRe.ReplaceAllStringFunc(s, func(m string) string {
		return os.Getenv(m[2 : len(m)-1])
	})
}

// expandPorts replaces any backend that has a ports field with one concrete
// backend per port, substituting {port} in id and base_url.
func expandPorts(backends []Backend) ([]Backend, error) {
	var out []Backend
	for i := range backends {
		b := &backends[i]
		if len(b.Ports) == 0 {
			out = append(out, *b)
			continue
		}
		if !strings.Contains(b.ID, "{port}") {
			return nil, fmt.Errorf("backend %q: id must contain {port} when ports is set", b.ID)
		}
		if !strings.Contains(b.BaseURL, "{port}") {
			return nil, fmt.Errorf("backend %q: base_url must contain {port} when ports is set", b.ID)
		}
		for _, port := range b.Ports {
			ps := strconv.Itoa(port)
			expanded := *b // copy
			expanded.ID = strings.ReplaceAll(b.ID, "{port}", ps)
			expanded.BaseURL = strings.ReplaceAll(b.BaseURL, "{port}", ps)
			expanded.Ports = nil
			out = append(out, expanded)
		}
	}
	return out, nil
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 4000
	}
	if cfg.Server.Transport.MaxIdleConns == 0 {
		cfg.Server.Transport.MaxIdleConns = 100
	}
	if cfg.Server.Transport.MaxIdleConnsPerHost == 0 {
		cfg.Server.Transport.MaxIdleConnsPerHost = 20
	}
	if cfg.Server.Transport.IdleConnTimeout == 0 {
		cfg.Server.Transport.IdleConnTimeout = 120
	}
	if cfg.Server.MaxRequestBodyMB == 0 {
		cfg.Server.MaxRequestBodyMB = 50
	}
	if cfg.Telemetry.Prometheus.Port == 0 {
		cfg.Telemetry.Prometheus.Port = 9091
	}
	if cfg.Telemetry.Prometheus.Path == "" {
		cfg.Telemetry.Prometheus.Path = "/metrics"
	}
	if cfg.Telemetry.Prometheus.Host == "" {
		// Default to localhost. Metrics have no auth; explicit opt-in required
		// for network exposure.
		cfg.Telemetry.Prometheus.Host = "127.0.0.1"
	}
	for i := range cfg.Backends {
		if cfg.Backends[i].TimeoutSeconds == 0 {
			cfg.Backends[i].TimeoutSeconds = 300
		}
		if u, err := url.Parse(cfg.Backends[i].BaseURL); err == nil {
			u.Path = strings.TrimSuffix(u.Path, "/v1")
			cfg.Backends[i].BaseURL = u.String()
		}
	}
}

func validate(cfg *Config) error {
	ids, err := validateBackends(cfg.Backends)
	if err != nil {
		return err
	}
	return validateRoutes(cfg.Routes, ids)
}

func validateBackends(backends []Backend) (map[string]bool, error) {
	ids := make(map[string]bool, len(backends))
	var defaultID string
	for i := range backends {
		b := &backends[i]
		if b.ID == "" {
			return nil, fmt.Errorf("backend missing id")
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("backend %q: base_url required", b.ID)
		}
		if b.Type != "openai" && b.Type != "anthropic" {
			return nil, fmt.Errorf("backend %q: type must be openai or anthropic, got %q", b.ID, b.Type)
		}
		if b.Default {
			if defaultID != "" {
				return nil, fmt.Errorf("multiple backends marked default: %q and %q", defaultID, b.ID)
			}
			defaultID = b.ID
		}
		ids[b.ID] = true
	}
	return ids, nil
}

func validateRoutes(routes []Route, backendIDs map[string]bool) error {
	seen := make(map[string]bool, len(routes))
	for _, r := range routes {
		if r.VirtualModel == "" {
			return fmt.Errorf("route missing virtual_model")
		}
		if seen[r.VirtualModel] {
			return fmt.Errorf("duplicate virtual_model %q", r.VirtualModel)
		}
		seen[r.VirtualModel] = true

		if r.AutoRoute == nil && r.Backend == "" {
			return fmt.Errorf("route %q: must have backend or auto_route", r.VirtualModel)
		}
		if r.Backend != "" && !backendIDs[r.Backend] {
			return fmt.Errorf("route %q: unknown backend %q", r.VirtualModel, r.Backend)
		}
		if r.AutoRoute != nil && (r.AutoRoute.Text == "" || r.AutoRoute.Vision == "") {
			return fmt.Errorf("route %q: auto_route requires text and vision", r.VirtualModel)
		}
	}
	return nil
}

// ── Lookups ───────────────────────────────────────────────────────────────────

func (c *Config) Backend(id string) (*Backend, bool) {
	b, ok := c.backendByID[id]
	return b, ok
}

func (c *Config) Route(model string) (*Route, bool) {
	r, ok := c.routeByModel[model]
	return r, ok
}

// DefaultBackend returns the backend marked default: true, falling back to
// the first configured backend when no default is set. Returns nil if no
// backends are configured.
//
// Used as the target for passthrough_unrouted requests and /v1/models.
// The implicit "first backend" fallback keeps older configs working; new
// configs should set default: true on exactly one backend to be explicit.
func (c *Config) DefaultBackend() *Backend {
	for i := range c.Backends {
		if c.Backends[i].Default {
			return &c.Backends[i]
		}
	}
	if len(c.Backends) > 0 {
		return &c.Backends[0]
	}
	return nil
}

// HasExplicitDefault reports whether any backend has default: true set.
// Used at startup to decide whether to log the implicit-fallback warning.
func (c *Config) HasExplicitDefault() bool {
	for i := range c.Backends {
		if c.Backends[i].Default {
			return true
		}
	}
	return false
}

// VirtualModels returns all configured virtual model names.
func (c *Config) VirtualModels() []string {
	names := make([]string, 0, len(c.Routes))
	for _, r := range c.Routes {
		names = append(names, r.VirtualModel)
	}
	return names
}
