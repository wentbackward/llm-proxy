package balancer

import (
	"testing"
	"time"
)

func TestStore_GetSet(t *testing.T) {
	s := NewInMemoryStore(1*time.Hour, 1000)
	s.Set("key1", AffinityEntry{BackendID: "b1"})
	entry, ok := s.Get("key1")
	if !ok {
		t.Fatal("expected entry")
	}
	if entry.BackendID != "b1" {
		t.Errorf("BackendID: got %q, want b1", entry.BackendID)
	}
}

func TestStore_GetMiss(t *testing.T) {
	s := NewInMemoryStore(1*time.Hour, 1000)
	_, ok := s.Get("nonexistent")
	if ok {
		t.Error("expected miss")
	}
}

func TestStore_TTLExpiration(t *testing.T) {
	s := NewInMemoryStore(10*time.Millisecond, 1000)
	s.Set("key1", AffinityEntry{BackendID: "b1"})
	time.Sleep(20 * time.Millisecond)
	_, ok := s.Get("key1")
	if ok {
		t.Error("entry should be expired")
	}
}

func TestStore_LRUEviction(t *testing.T) {
	s := NewInMemoryStore(1*time.Hour, 2)
	s.Set("a", AffinityEntry{BackendID: "b1"})
	s.Set("b", AffinityEntry{BackendID: "b2"})
	// Access "a" to make it MRU
	s.Touch("a")
	// Add "c" — should evict "b" (LRU)
	s.Set("c", AffinityEntry{BackendID: "b3"})
	_, okA := s.Get("a")
	_, okB := s.Get("b")
	_, okC := s.Get("c")
	if !okA {
		t.Error("a should survive (MRU)")
	}
	if okB {
		t.Error("b should be evicted (LRU)")
	}
	if !okC {
		t.Error("c should exist")
	}
}

func TestStore_EvictExpired(t *testing.T) {
	s := NewInMemoryStore(50*time.Millisecond, 1000)
	s.Set("exp", AffinityEntry{BackendID: "b1"})
	time.Sleep(10 * time.Millisecond)
	s.Set("live", AffinityEntry{BackendID: "b2"})
	time.Sleep(45 * time.Millisecond)
	s.EvictExpired(time.Now())
	_, ok := s.Get("exp")
	if ok {
		t.Error("expired entry should be evicted")
	}
	_, ok = s.Get("live")
	if !ok {
		t.Error("live entry should survive")
	}
}

func TestStore_MigrateKeepsValidBackends(t *testing.T) {
	s := NewInMemoryStore(1*time.Hour, 1000)
	s.Set("session1", AffinityEntry{BackendID: "b1"})
	s.Set("session2", AffinityEntry{BackendID: "b2"})
	s.Migrate(map[string]struct{}{"b1": {}})
	_, ok := s.Get("session1")
	if !ok {
		t.Error("session1 pinned to kept backend should survive")
	}
	_, ok = s.Get("session2")
	if ok {
		t.Error("session2 pinned to removed backend should be evicted")
	}
}
