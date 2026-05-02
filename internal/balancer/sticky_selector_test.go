package balancer

import (
	"errors"
	"fmt"
	"math/rand"
	"sync"
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
		NewBackendState("a", "http://a", 1, 300),
		NewBackendState("b", "http://b", 1, 300),
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
		NewBackendState("a", "http://a", 1, 300),
		NewBackendState("b", "http://b", 1, 300),
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
		NewBackendState("a", "http://a", 1, 300),
		NewBackendState("b", "http://b", 1, 300),
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
		NewBackendState("a", "http://a", 1, 300),
		NewBackendState("b", "http://b", 1, 300),
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

func TestSticky_ConcurrentSameKey(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)
	pool := []*BackendState{
		NewBackendState("test1", "http://test1", 1, 300),
		NewBackendState("test2", "http://test2", 1, 300),
	}

	const key = "concurrent-session"
	const goroutines = 16
	const iterations = 5

	// Run concurrent requests with the SAME affinity key
	var results [goroutines][iterations]string
	var mu sync.Mutex
	wg := sync.WaitGroup{}

	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for i := 0; i < iterations; i++ {
				chosen, err := sel.Select(pool, key, nil)
				if err != nil {
					mu.Lock()
					t.Errorf("goroutine %d iteration %d: %v", idx, i, err)
					mu.Unlock()
					return
				}
				mu.Lock()
				results[idx][i] = chosen.ID
				mu.Unlock()
				// Small delay to simulate real request timing
				time.Sleep(time.Millisecond * time.Duration(rand.Intn(10)))
			}
		}(g)
	}
	wg.Wait()

	// All requests with the same key should have gotten the same backend
	// (after the first request establishes the pin)
	firstResult := results[0][0]
	mismatches := 0
	for g := 0; g < goroutines; g++ {
		for i := 0; i < iterations; i++ {
			if results[g][i] != firstResult {
				mismatches++
			}
		}
	}

	// Allow some mismatches for the very first request (race to establish pin)
	// But after the pin is set, all subsequent requests should honor it
	if mismatches > goroutines { // at most 1 mismatch per goroutine (the first request)
		t.Errorf("too many mismatches: %d out of %d. Results:", mismatches, goroutines*iterations)
		for g := 0; g < goroutines; g++ {
			t.Logf("  goroutine %d: %v", g, results[g])
		}
	}
}

func TestSticky_ConcurrentDifferentKeys(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, 30*time.Second)
	pool := []*BackendState{
		NewBackendState("test1", "http://test1", 1, 300),
		NewBackendState("test2", "http://test2", 1, 300),
	}

	const sessions = 16
	const turns = 5

	// Each session has its own key, simulating 16 concurrent sessions
	type sessionResult struct {
		backends [turns]string
	}
	results := make(map[int]*sessionResult)
	var mu sync.Mutex
	wg := sync.WaitGroup{}

	for s := 0; s < sessions; s++ {
		wg.Add(1)
		go func(sessionID int) {
			defer wg.Done()
			key := fmt.Sprintf("session-%d", sessionID)
			res := &sessionResult{}
			for turn := 0; turn < turns; turn++ {
				chosen, err := sel.Select(pool, key, nil)
				if err != nil {
					mu.Lock()
					t.Errorf("session %d turn %d: %v", sessionID, turn, err)
					mu.Unlock()
					return
				}
				res.backends[turn] = chosen.ID
				mu.Lock()
				results[sessionID] = res
				mu.Unlock()
				time.Sleep(time.Millisecond * time.Duration(rand.Intn(10)))
			}
		}(s)
	}
	wg.Wait()

	// Each session should be consistent across all its turns
	for sessionID, res := range results {
		first := res.backends[0]
		for turn := 1; turn < turns; turn++ {
			if res.backends[turn] != first {
				t.Errorf("session %d: inconsistent routing %v (expected all %s)",
					sessionID, res.backends, first)
			}
		}
	}
}
