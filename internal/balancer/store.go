package balancer

import (
	"container/list"
	"sync"
	"time"
)

// AffinityEntry maps an affinity key to a pinned backend ID.
type AffinityEntry struct {
	BackendID string
	LastSeen  time.Time
}

// lruNode wraps an entry with its key for efficient LRU eviction.
type lruNode struct {
	key     string
	entry   *AffinityEntry
	element *list.Element
}

// AffinityStore abstracts persistence of affinity entries.
// The in-memory implementation uses LRU + TTL eviction;
// a future Redis implementation plugs in here.
type AffinityStore interface {
	Get(key string) (*AffinityEntry, bool)
	Set(key string, entry AffinityEntry)
	Touch(key string) // refresh LastSeen
	Delete(key string)
	EvictExpired(now time.Time)
	Migrate(keptBackends map[string]struct{}) // delete entries whose backendID is NOT in keys
}

// inMemoryStore is the default AffinityStore.
type inMemoryStore struct {
	mu      sync.RWMutex
	entries map[string]*lruNode
	lru     *list.List // front = most recently used
	ttl     time.Duration
	maxSize int
}

func NewInMemoryStore(ttl time.Duration, maxSize int) AffinityStore {
	return &inMemoryStore{
		entries: make(map[string]*lruNode),
		lru:     list.New(),
		ttl:     ttl,
		maxSize: maxSize,
	}
}

func (s *inMemoryStore) Get(key string) (*AffinityEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	node, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(node.entry.LastSeen) > s.ttl {
		return nil, false // expired
	}
	return node.entry, true
}

func (s *inMemoryStore) Set(key string, entry AffinityEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.LastSeen = time.Now()
	if existing, ok := s.entries[key]; ok {
		existing.entry.BackendID = entry.BackendID
		existing.entry.LastSeen = entry.LastSeen
		s.lru.MoveToFront(existing.element)
		return
	}
	node := &lruNode{key: key, entry: &entry}
	node.element = s.lru.PushFront(node)
	s.entries[key] = node
	// Evict LRU tail if over capacity
	for len(s.entries) > s.maxSize {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		oldestNode := oldest.Value.(*lruNode)
		delete(s.entries, oldestNode.key)
		s.lru.Remove(oldest)
	}
}

func (s *inMemoryStore) Touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node, ok := s.entries[key]; ok {
		node.entry.LastSeen = time.Now()
		s.lru.MoveToFront(node.element)
	}
}

func (s *inMemoryStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if node, ok := s.entries[key]; ok {
		s.lru.Remove(node.element)
		delete(s.entries, key)
	}
}

func (s *inMemoryStore) EvictExpired(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []string
	for key, node := range s.entries {
		if now.Sub(node.entry.LastSeen) > s.ttl {
			expired = append(expired, key)
		}
	}
	for _, key := range expired {
		node := s.entries[key]
		s.lru.Remove(node.element)
		delete(s.entries, key)
	}
}

func (s *inMemoryStore) Migrate(keptBackends map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var toDelete []string
	for key, node := range s.entries {
		if _, ok := keptBackends[node.entry.BackendID]; !ok {
			toDelete = append(toDelete, key)
		}
	}
	for _, key := range toDelete {
		node := s.entries[key]
		s.lru.Remove(node.element)
		delete(s.entries, key)
	}
}
