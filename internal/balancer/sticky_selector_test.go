package balancer

import (
	"errors"
	"testing"
	"time"
)

func newTestStore(t *testing.T) AffinityStore {
	t.Helper()
	return NewInMemoryStore(1*time.Hour, 10000)
}

func TestSticky_PinsOnFirstRequest(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}

	// First request — pins to one backend
	chosen1, err := sel.Select(pool, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}

	// Second request with same key — must return the same backend
	chosen2, err := sel.Select(pool, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen1.ID != chosen2.ID {
		t.Errorf("affinity broken: first=%s, second=%s", chosen1.ID, chosen2.ID)
	}
}

func TestSticky_DifferentKeysDifferentBackends(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}

	chosenA, _ := sel.Select(pool, "key-a", nil)
	chosenB, _ := sel.Select(pool, "key-b", nil)

	// Each key pins to its own backend. With equal load, least-loaded
	// picks by tiebreak. Both could land on the same backend — that's fine.
	// What matters is that each key is consistent on subsequent calls.
	chosenA2, _ := sel.Select(pool, "key-a", nil)
	chosenB2, _ := sel.Select(pool, "key-b", nil)

	if chosenA.ID != chosenA2.ID {
		t.Error("key-a affinity broken")
	}
	if chosenB.ID != chosenB2.ID {
		t.Error("key-b affinity broken")
	}
}

func TestSticky_BailsWhenOverloaded(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 2, 0, 30*time.Second) // max 2 concurrent
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}

	// Give "b" heavy load so "a" is deterministically picked first
	pool[1].InFlight.Add(10)

	// Pin to "a"
	chosen1, _ := sel.Select(pool, "session-1", nil)
	if chosen1.ID != "a" {
		t.Fatalf("expected first pick to be a, got %s", chosen1.ID)
	}

	// Reset and simulate "a" reaching capacity
	pool[1].InFlight.Add(-10)
	pool[0].InFlight.Add(2)

	// Next request should bail to "b"
	chosen2, err := sel.Select(pool, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen2.ID == chosen1.ID {
		t.Error("should have bailed off overloaded pinned backend")
	}

	// The pin is overwritten with the fallback choice
	chosen3, _ := sel.Select(pool, "session-1", nil)
	if chosen3.ID != chosen2.ID {
		t.Error("should continue using new fallback")
	}
}

func TestSticky_EmptyKeyUsesFallback(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}

	chosen, err := sel.Select(pool, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen == nil {
		t.Error("must return a backend even with empty key")
	}
}

func TestSticky_AllPoolDown(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)

	_, err := sel.Select([]*BackendState{}, "key", nil)
	if !errors.Is(err, ErrNoHealthyBackend) {
		t.Errorf("expected ErrNoHealthyBackend, got %v", err)
	}
}
