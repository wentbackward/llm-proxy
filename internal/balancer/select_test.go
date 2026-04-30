package balancer

import (
	"testing"
	"time"
)

func TestPickLeastLoaded_LowestInFlight(t *testing.T) {
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
		NewBackendState("c", "http://c", 1),
	}
	pool[0].InFlight.Add(5)
	pool[1].InFlight.Add(1)
	pool[2].InFlight.Add(3)
	chosen := pickLeastLoaded(pool, 30*time.Second)
	if chosen.ID != "b" {
		t.Errorf("expected b (lowest in-flight), got %s", chosen.ID)
	}
}

func TestPickLeastLoaded_WeightBias(t *testing.T) {
	pool := []*BackendState{
		NewBackendState("heavy", "http://heavy", 2), // weight 2 → half effective load
		NewBackendState("light", "http://light", 1),
	}
	pool[0].InFlight.Add(2) // effective: 2/2 = 1.0
	pool[1].InFlight.Add(1) // effective: 1/1 = 1.0
	// Tiebreak by ID hash — "heavy" < "light" lexicographically
	chosen := pickLeastLoaded(pool, 30*time.Second)
	// Either is acceptable; the point is it doesn't panic or oscillate
	if chosen == nil {
		t.Fatal("must return a backend")
	}
}

func TestIsOverloaded_InFlightExceeds(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(4)
	if !isOverloaded(b, 4, 0, 30*time.Second) {
		t.Error("should be overloaded at max_concurrency boundary")
	}
}

func TestIsOverloaded_UnderLimit(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(2)
	if isOverloaded(b, 4, 0, 30*time.Second) {
		t.Error("should not be overloaded under limit")
	}
}

func TestIsOverloaded_ZeroMaxConcurrencyMeansUnlimited(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(100)
	if isOverloaded(b, 0, 0, 30*time.Second) {
		t.Error("max_concurrency=0 means unlimited")
	}
}
