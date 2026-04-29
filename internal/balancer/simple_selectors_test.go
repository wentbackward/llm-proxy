package balancer

import (
	"testing"
)

func TestSingleSelector(t *testing.T) {
	sel := singleSelector{}
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}
	chosen, err := sel.Select(pool, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.ID != "a" {
		t.Errorf("expected first backend, got %s", chosen.ID)
	}
}

func TestRoundRobinSelector(t *testing.T) {
	sel := &roundRobinSelector{}
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}
	order := []string{}
	for i := 0; i < 6; i++ {
		chosen, err := sel.Select(pool, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		order = append(order, chosen.ID)
	}
	want := []string{"a", "b", "a", "b", "a", "b"}
	for i, got := range order {
		if got != want[i] {
			t.Errorf("[%d] got %q, want %q", i, got, want[i])
		}
	}
}

func TestLeastLoadedSelector(t *testing.T) {
	sel := leastLoadedSelector{}
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}
	pool[0].InFlight.Add(10)
	chosen, err := sel.Select(pool, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen.ID != "b" {
		t.Errorf("expected less-loaded backend b, got %s", chosen.ID)
	}
}

func TestNewSelector_Strategies(t *testing.T) {
	cases := map[string]bool{
		"single":              true,
		"round_robin":         true,
		"least_loaded":        true,
		"sticky_least_loaded": true, // returns nil (handled separately)
	}
	for strategy := range cases {
		sel := NewSelector(strategy)
		if strategy == "sticky_least_loaded" {
			if sel != nil {
				t.Errorf("sticky_least_loaded should return nil from NewSelector, got %T", sel)
			}
		} else {
			if sel == nil {
				t.Errorf("strategy %q returned nil", strategy)
			}
		}
	}
}
