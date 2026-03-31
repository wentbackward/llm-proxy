package config

import (
	"fmt"
	"os"
	"path/filepath"
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
