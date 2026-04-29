package balancer

import (
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

func testConfigWithGroup(t *testing.T) *config.Config {
	t.Helper()
	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "a", Type: "openai", BaseURL: "http://a", Group: "g1"},
			{ID: "b", Type: "openai", BaseURL: "http://b", Group: "g1"},
		},
		Groups: map[string]*config.GroupConfig{
			"g1": {
				Strategy: "sticky_least_loaded",
				Affinity: config.AffinityConfig{
					Key:         "canonical_prefix",
					PrefixBytes: 1024,
					TTLSeconds:  3600,
					MaxEntries:  10000,
				},
				Overload: config.OverloadConfig{
					MaxConcurrency:     4,
					StaleMetricsAction: "pin",
				},
			},
		},
	}
	return cfg
}

func TestBalancer_SelectReturnsBackend(t *testing.T) {
	cfg := testConfigWithGroup(t)
	b := New(cfg)
	defer b.Stop()

	chosen, err := b.Select("g1", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen == nil {
		t.Fatal("expected a backend")
	}
	if chosen.ID != "a" && chosen.ID != "b" {
		t.Errorf("unexpected backend ID: %s", chosen.ID)
	}
}

func TestBalancer_AffinityPersistence(t *testing.T) {
	cfg := testConfigWithGroup(t)
	b := New(cfg)
	defer b.Stop()

	// Two requests with same key should hit the same backend
	c1, _ := b.Select("g1", "sess-1", nil)
	c2, _ := b.Select("g1", "sess-1", nil)
	if c1.ID != c2.ID {
		t.Errorf("affinity broken: %s vs %s", c1.ID, c2.ID)
	}
}

func TestBalancer_IncrDecr(t *testing.T) {
	cfg := testConfigWithGroup(t)
	b := New(cfg)
	defer b.Stop()

	b.Incr("a")
	b.Incr("a")
	b.Incr("b")

	grp := b.groups["g1"]
	if v := grp.States["a"].InFlight.Load(); v != 2 {
		t.Errorf("backend a in-flight: got %d, want 2", v)
	}
	if v := grp.States["b"].InFlight.Load(); v != 1 {
		t.Errorf("backend b in-flight: got %d, want 1", v)
	}

	b.Decr("a")
	if v := grp.States["a"].InFlight.Load(); v != 1 {
		t.Errorf("backend a after decr: got %d, want 1", v)
	}
}

func TestBalancer_UnknownGroup(t *testing.T) {
	cfg := testConfigWithGroup(t)
	b := New(cfg)
	defer b.Stop()

	_, err := b.Select("nonexistent", "", nil)
	if err == nil {
		t.Error("expected error for unknown group")
	}
}

func TestBalancer_NoGroups(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "a", Type: "openai", BaseURL: "http://a"},
		},
	}
	b := New(cfg)
	defer b.Stop()

	_, err := b.Select("missing", "", nil)
	if err == nil {
		t.Error("expected error when no groups configured")
	}
}
