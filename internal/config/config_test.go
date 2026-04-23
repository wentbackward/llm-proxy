package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTemp(t *testing.T, content string) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(content); err != nil {
		t.Fatal(err)
	}
	f.Close()
	return f.Name()
}

func TestLoad_Minimal(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000/v1"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.Server.Port != 4000 {
		t.Errorf("default port: got %d, want 4000", cfg.Server.Port)
	}
	if cfg.Telemetry.Prometheus.Path != "/metrics" {
		t.Errorf("default metrics path: got %q", cfg.Telemetry.Prometheus.Path)
	}
}

func TestLoad_EnvExpansion(t *testing.T) {
	t.Setenv("TEST_API_KEY", "secret-key-123")
	path := writeTemp(t, `
backends:
  - id: remote
    type: openai
    base_url: "https://api.example.com/v1"
    api_key: "${TEST_API_KEY}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	b, ok := cfg.Backend("remote")
	if !ok {
		t.Fatal("backend not found")
	}
	if b.APIKey != "secret-key-123" {
		t.Errorf("api key: got %q, want %q", b.APIKey, "secret-key-123")
	}
}

func TestLoad_UnsetEnvVar(t *testing.T) {
	os.Unsetenv("MISSING_KEY")
	path := writeTemp(t, `
backends:
  - id: x
    type: openai
    base_url: "http://localhost"
    api_key: "${MISSING_KEY}"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("should not error on missing env var: %v", err)
	}
	b, _ := cfg.Backend("x")
	if b.APIKey != "" {
		t.Errorf("expected empty api_key for unset env var, got %q", b.APIKey)
	}
}

func TestLoad_InvalidBackendType(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: bad
    type: grpc
    base_url: "http://localhost"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for invalid backend type")
	}
}

func TestLoad_MissingBackendID(t *testing.T) {
	path := writeTemp(t, `
backends:
  - type: openai
    base_url: "http://localhost"
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for missing backend id")
	}
}

func TestLoad_RouteUnknownBackend(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: real
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: my-model
    backend: nonexistent
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for unknown backend in route")
	}
}

func TestLoad_RouteNeitherBackendNorAutoRoute(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: real
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: orphan
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for route with no backend and no auto_route")
	}
}

func TestLoad_DuplicateVirtualModel(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: b1
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: dup
    backend: b1
  - virtual_model: dup
    backend: b1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for duplicate virtual_model")
	}
}

func TestLoad_AutoRouteRequiresBothFields(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: b1
    type: openai
    base_url: "http://localhost"
routes:
  - virtual_model: auto
    auto_route:
      text: fast
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for incomplete auto_route")
	}
}

func TestLoad_FullConfig(t *testing.T) {
	path := filepath.Join("..", "..", "config.example.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("config.example.yaml not found")
	}
	// Should parse without error (env vars may be empty, that's fine)
	_, err := Load(path)
	if err != nil {
		t.Fatalf("example config failed: %v", err)
	}
}

func TestBackendLookup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
  - id: b
    type: anthropic
    base_url: "https://api.anthropic.com"
`)
	cfg, _ := Load(path)

	if _, ok := cfg.Backend("a"); !ok {
		t.Error("backend a not found")
	}
	if _, ok := cfg.Backend("b"); !ok {
		t.Error("backend b not found")
	}
	if _, ok := cfg.Backend("c"); ok {
		t.Error("backend c should not exist")
	}
}

// ── ValidateListenPolicy ─────────────────────────────────────────────────────

func TestListenPolicy_PlaintextGatewayRejectedByDefault(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err == nil {
		t.Fatal("plaintext gateway without allow_plaintext should be rejected")
	}
}

func TestListenPolicy_PlaintextGatewayAllowedWithOptIn(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("allow_plaintext: true should permit plaintext: %v", err)
	}
}

func TestListenPolicy_TLSGatewaySatisfiesPolicy(t *testing.T) {
	path := writeTemp(t, `
server:
  tls:
    cert: /certs/a.crt
    key: /certs/a.key
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("configured TLS should satisfy policy: %v", err)
	}
}

func TestListenPolicy_MetricsLoopbackIsFine(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 127.0.0.1
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("loopback metrics should pass without opt-in: %v", err)
	}
}

func TestListenPolicy_MetricsNonLoopbackRejectedByDefault(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 0.0.0.0
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err == nil {
		t.Fatal("plaintext metrics on 0.0.0.0 without TLS or opt-in should be rejected")
	}
}

func TestListenPolicy_MetricsTLSSatisfies(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: true
    host: 0.0.0.0
    tls:
      cert: /certs/m.crt
      key: /certs/m.key
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("metrics TLS should satisfy policy: %v", err)
	}
}

func TestListenPolicy_DisabledMetricsIgnored(t *testing.T) {
	path := writeTemp(t, `
server:
  allow_plaintext: true
telemetry:
  prometheus:
    enabled: false
    host: 0.0.0.0   # would be rejected if enabled, but it isn't
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if err := cfg.ValidateListenPolicy(); err != nil {
		t.Errorf("disabled metrics should never fail policy: %v", err)
	}
}

func TestDefault_MaxRequestBodyMB(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Server.MaxRequestBodyMB != 50 {
		t.Errorf("default max_request_body_mb: got %d, want 50", cfg.Server.MaxRequestBodyMB)
	}
}

func TestDefault_PrometheusHostLocalhost(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Telemetry.Prometheus.Host != "127.0.0.1" {
		t.Errorf("prometheus.host default: got %q, want 127.0.0.1 (metrics have no auth; localhost-only by default)", cfg.Telemetry.Prometheus.Host)
	}
}

func TestExplicit_PrometheusHostOverride(t *testing.T) {
	path := writeTemp(t, `
telemetry:
  prometheus:
    host: "0.0.0.0"
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	if cfg.Telemetry.Prometheus.Host != "0.0.0.0" {
		t.Errorf("explicit host should override default, got %q", cfg.Telemetry.Prometheus.Host)
	}
}

func TestDefaultTimeout(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
`)
	cfg, _ := Load(path)
	b, _ := cfg.Backend("a")
	if b.TimeoutSeconds != 300 {
		t.Errorf("default timeout: got %d, want 300", b.TimeoutSeconds)
	}
}

func TestDefaultBackend_ExplicitDefault(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: first
    type: openai
    base_url: "http://a"
  - id: chosen
    type: openai
    base_url: "http://b"
    default: true
  - id: third
    type: openai
    base_url: "http://c"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.HasExplicitDefault() {
		t.Error("HasExplicitDefault should be true")
	}
	def := cfg.DefaultBackend()
	if def == nil || def.ID != "chosen" {
		t.Errorf("DefaultBackend: got %v, want chosen", def)
	}
}

func TestDefaultBackend_FallbackToFirst(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: first
    type: openai
    base_url: "http://a"
  - id: second
    type: openai
    base_url: "http://b"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.HasExplicitDefault() {
		t.Error("HasExplicitDefault should be false")
	}
	def := cfg.DefaultBackend()
	if def == nil || def.ID != "first" {
		t.Errorf("DefaultBackend fallback: got %v, want first", def)
	}
}

func TestDefaultBackend_NoBackends(t *testing.T) {
	cfg := &Config{}
	if def := cfg.DefaultBackend(); def != nil {
		t.Errorf("DefaultBackend with no backends: got %v, want nil", def)
	}
}

func TestDefaultBackend_MultipleDefaultsRejected(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://a"
    default: true
  - id: b
    type: openai
    base_url: "http://b"
    default: true
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error for two default backends")
	}
	if !strings.Contains(err.Error(), "default") {
		t.Errorf("error should mention default, got: %v", err)
	}
}

func TestLogLevel_Unset(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel != nil {
		t.Errorf("log_level should be nil when omitted, got %v", *cfg.Server.LogLevel)
	}
}

func TestLogLevel_ExplicitZero(t *testing.T) {
	path := writeTemp(t, `
server:
  log_level: 0
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel == nil {
		t.Fatal("log_level: 0 should round-trip as non-nil pointer (distinguishes from unset)")
	}
	if *cfg.Server.LogLevel != 0 {
		t.Errorf("log_level: got %d, want 0", *cfg.Server.LogLevel)
	}
}

func TestLogLevel_ParsesNonZero(t *testing.T) {
	path := writeTemp(t, `
server:
  log_level: 3
backends:
  - id: local
    type: openai
    base_url: "http://localhost:8000"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.LogLevel == nil || *cfg.Server.LogLevel != 3 {
		t.Errorf("log_level: got %v, want 3", cfg.Server.LogLevel)
	}
}

func TestPorts_Single(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: 3040
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 1 {
		t.Fatalf("expected 1 backend, got %d", len(cfg.Backends))
	}
	b := cfg.Backends[0]
	if b.ID != "vllm-3040" {
		t.Errorf("id: got %q, want %q", b.ID, "vllm-3040")
	}
	if b.BaseURL != "http://127.0.0.1:3040" {
		t.Errorf("base_url: got %q, want %q", b.BaseURL, "http://127.0.0.1:3040")
	}
}

func TestPorts_List(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: [3040, 3042, 3044]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 3 {
		t.Fatalf("expected 3 backends, got %d", len(cfg.Backends))
	}
	want := []struct{ id, url string }{
		{"vllm-3040", "http://127.0.0.1:3040"},
		{"vllm-3042", "http://127.0.0.1:3042"},
		{"vllm-3044", "http://127.0.0.1:3044"},
	}
	for i, w := range want {
		if cfg.Backends[i].ID != w.id {
			t.Errorf("[%d] id: got %q, want %q", i, cfg.Backends[i].ID, w.id)
		}
		if cfg.Backends[i].BaseURL != w.url {
			t.Errorf("[%d] base_url: got %q, want %q", i, cfg.Backends[i].BaseURL, w.url)
		}
	}
}

func TestPorts_Range(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: "3040-3043"
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(cfg.Backends) != 4 {
		t.Fatalf("expected 4 backends, got %d", len(cfg.Backends))
	}
	for i, port := range []int{3040, 3041, 3042, 3043} {
		wantID := fmt.Sprintf("vllm-%d", port)
		if cfg.Backends[i].ID != wantID {
			t.Errorf("[%d] id: got %q, want %q", i, cfg.Backends[i].ID, wantID)
		}
	}
}

func TestPorts_MissingPlaceholder(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-static
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: 3040
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error when id lacks {port} placeholder")
	}
}

func TestPorts_PropertiesPreserved(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    api_key: "secret"
    timeout_seconds: 120
    skip_probe: true
    ports: [3040, 3041]
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range cfg.Backends {
		if b.APIKey != "secret" {
			t.Errorf("%s: api_key not preserved", b.ID)
		}
		if b.TimeoutSeconds != 120 {
			t.Errorf("%s: timeout_seconds not preserved", b.ID)
		}
		if !b.SkipProbe {
			t.Errorf("%s: skip_probe not preserved", b.ID)
		}
		if len(b.Ports) != 0 {
			t.Errorf("%s: ports should be cleared after expansion", b.ID)
		}
	}
}

func TestPorts_RoutesReferenceExpanded(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: [3040, 3041]
routes:
  - virtual_model: model-a
    backend: vllm-3040
  - virtual_model: model-b
    backend: vllm-3041
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, ok := cfg.Backend("vllm-3040"); !ok {
		t.Error("expanded backend vllm-3040 not found")
	}
	if _, ok := cfg.Backend("vllm-3041"); !ok {
		t.Error("expanded backend vllm-3041 not found")
	}
}
