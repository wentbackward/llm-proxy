package config

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

var envVarRe = regexp.MustCompile(`\$\{([^}]+)\}`)

// ── Schema ────────────────────────────────────────────────────────────────────

type TLSConfig struct {
	Cert string `yaml:"cert"` // path to certificate file
	Key  string `yaml:"key"`  // path to private key file
}

type ServerConfig struct {
	Host   string    `yaml:"host"`
	Port   int       `yaml:"port"`
	APIKey string    `yaml:"api_key"` // required bearer token for inbound requests
	TLS    TLSConfig `yaml:"tls"`
}

type PrometheusConfig struct {
	Enabled bool   `yaml:"enabled"`
	Port    int    `yaml:"port"`
	Path    string `yaml:"path"`
}

type TelemetryConfig struct {
	Prometheus PrometheusConfig `yaml:"prometheus"`
}

type Backend struct {
	ID             string `yaml:"id"`
	Type           string `yaml:"type"` // openai | anthropic
	BaseURL        string `yaml:"base_url"`
	APIKey         string `yaml:"api_key"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	SkipProbe      bool   `yaml:"skip_probe"` // skip /v1/models health check at startup/SIGHUP
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

type Config struct {
	Server    ServerConfig    `yaml:"server"`
	Telemetry TelemetryConfig `yaml:"telemetry"`
	Journal   JournalConfig   `yaml:"journal"`
	Backends  []Backend       `yaml:"backends"`
	Routes    []Route         `yaml:"routes"`

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

func applyDefaults(cfg *Config) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = "0.0.0.0"
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = 4000
	}
	if cfg.Telemetry.Prometheus.Port == 0 {
		cfg.Telemetry.Prometheus.Port = 9091
	}
	if cfg.Telemetry.Prometheus.Path == "" {
		cfg.Telemetry.Prometheus.Path = "/metrics"
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
	for _, b := range backends {
		if b.ID == "" {
			return nil, fmt.Errorf("backend missing id")
		}
		if b.BaseURL == "" {
			return nil, fmt.Errorf("backend %q: base_url required", b.ID)
		}
		if b.Type != "openai" && b.Type != "anthropic" {
			return nil, fmt.Errorf("backend %q: type must be openai or anthropic, got %q", b.ID, b.Type)
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
