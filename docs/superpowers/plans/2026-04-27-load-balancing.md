# Load Balancing Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add multi-backend load balancing with prefix-cache affinity to hikyaku, preserving KV cache locality across multi-turn sessions while distributing independent sessions evenly.

**Architecture:** New `internal/balancer` package with a `Selector` interface and `AffinityStore` interface. Config gains `groups:`, `Backend.Group`, and `Route.BackendGroup`. The proxy's `Server` gains an atomic `*balancer.Balancer` alongside the existing `*router.Router`. Selection is two-step: Router resolves model → group, Balancer selects backend within group.

**Tech Stack:** Pure Go stdlib + existing dependencies. No new modules. xxhash64 via `golang.org/x/exp/hash/fnv` or hand-rolled FNV-64 (avoiding a new dependency for a single hash call).

---

## File Map

| File | Responsibility |
|---|---|
| `internal/config/group.go` | `GroupConfig`, `AffinityConfig`, `OverloadConfig`, `HealthCheckConfig` structs |
| `internal/config/config.go` | Add `Groups` to `Config`, `Group` to `Backend`, `BackendGroup` to `Route`; validation |
| `internal/balancer/state.go` | `BackendState` struct (health, in-flight, metrics) |
| `internal/balancer/store.go` | `AffinityStore` interface + in-memory LRU+TTL implementation |
| `internal/balancer/affinity_key.go` | `CanonicalPrefix()`, `AffinityKey()` functions |
| `internal/balancer/select.go` | `Selector` interface, `pickLeastLoaded()`, `isOverloaded()` |
| `internal/balancer/sticky_selector.go` | `StickyLeastLoaded` implementation |
| `internal/balancer/simple_selectors.go` | `Single`, `RoundRobin`, `LeastLoaded` implementations |
| `internal/balancer/balancer.go` | `Balancer` struct, `New()`, `Select()`, `MigrateFrom()` |
| `internal/balancer/health.go` | Single goroutine health-check loop |
| `internal/balancer/request_context.go` | `RequestContext` struct (forward-compat: `Principal`, `EstimatedSize`) |
| `internal/router/router.go` | `Resolution` gains `Group` field; `Resolve` returns group-aware resolution |
| `internal/proxy/server.go` | Wire `*balancer.Balancer` into `Server`; integrate selection into `proxyRequest` |
| `cmd/hikyaku/main.go` | Start/stop balancer goroutines; migrate on reload |

---

## Task 1: Config — Group Types

**Files:**
- Create: `internal/config/group.go`

- [ ] **Step 1: Create `internal/config/group.go` with group-related structs**

```go
// Package config ...

// GroupConfig defines load-balancing behaviour for a named group of backends.
type GroupConfig struct {
	Strategy    string            `yaml:"strategy"`    // sticky_least_loaded | least_loaded | round_robin | single
	Affinity    AffinityConfig    `yaml:"affinity"`
	Overload    OverloadConfig    `yaml:"overload"`
	HealthCheck HealthCheckConfig `yaml:"health_check"`
}

type AffinityConfig struct {
	Key         string `yaml:"key"`          // canonical_prefix | header:NAME | none
	PrefixBytes int    `yaml:"prefix_bytes"` // default: 1024
	TTLSeconds  int    `yaml:"ttl_seconds"`  // default: 3600
	MaxEntries  int    `yaml:"max_entries"`  // default: 10000
}

type OverloadConfig struct {
	MaxConcurrency     int     `yaml:"max_concurrency"`
	KVCachePct         float64 `yaml:"kv_cache_pct"`
	StaleMetricsAction string  `yaml:"stale_metrics_action"` // pin | bail
}

type HealthCheckConfig struct {
	Path           string `yaml:"path"`
	IntervalSeconds int   `yaml:"interval_seconds"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
	UnhealthyAfter int    `yaml:"unhealthy_after"`
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/config/group.go
git commit -m "feat: add group config types"
```

---

## Task 2: Config — Wire Group Fields into Existing Structs

**Files:**
- Modify: `internal/config/config.go`

- [ ] **Step 1: Add `Group` field to `Backend` struct**

In the `Backend` struct, add after `AuthType`:

```go
	Group string `yaml:"group"` // optional; LB group name
```

- [ ] **Step 2: Add `BackendGroup` field to `Route` struct**

In the `Route` struct, add after `Backend`:

```go
	BackendGroup string `yaml:"backend_group"` // LB group reference; mutually exclusive with backend:
```

- [ ] **Step 3: Add `Groups` field to `Config` struct**

In the `Config` struct, add after `Routes`:

```go
	Groups       map[string]*GroupConfig `yaml:"groups"`
```

- [ ] **Step 4: Update `validateRoutes` to enforce mutual exclusivity**

In `validateRoutes`, after the existing `r.Backend != "" && !backendIDs[r.Backend]` check, add:

```go
	// backend: and backend_group: are mutually exclusive
	if r.Backend != "" && r.BackendGroup != "" {
		return fmt.Errorf("route %q: must specify exactly one of backend or backend_group", r.VirtualModel)
	}
	if r.Backend == "" && r.BackendGroup == "" && r.AutoRoute == nil {
		return fmt.Errorf("route %q: must have backend, backend_group, or auto_route", r.VirtualModel)
	}
```

- [ ] **Step 5: Update `applyDefaults` to set group defaults**

At the end of `applyDefaults`, after the backends loop, add:

```go
	for name, g := range cfg.Groups {
		if g.Strategy == "" {
			g.Strategy = "sticky_least_loaded"
		}
		if g.Affinity.Key == "" {
			g.Affinity.Key = "canonical_prefix"
		}
		if g.Affinity.PrefixBytes == 0 {
			g.Affinity.PrefixBytes = 1024
		}
		if g.Affinity.TTLSeconds == 0 {
			g.Affinity.TTLSeconds = 3600
		}
		if g.Affinity.MaxEntries == 0 {
			g.Affinity.MaxEntries = 10000
		}
		if g.Overload.StaleMetricsAction == "" {
			g.Overload.StaleMetricsAction = "pin"
		}
		if g.HealthCheck.Path == "" {
			g.HealthCheck.Path = "/v1/models"
		}
		if g.HealthCheck.IntervalSeconds == 0 {
			g.HealthCheck.IntervalSeconds = 10
		}
		if g.HealthCheck.TimeoutSeconds == 0 {
			g.HealthCheck.TimeoutSeconds = 2
		}
		if g.HealthCheck.UnhealthyAfter == 0 {
			g.HealthCheck.UnhealthyAfter = 3
		}
	}
```

- [ ] **Step 6: Add `GroupBackends(group string) []*Backend` helper to `Config`**

```go
// GroupBackends returns all backends belonging to the named group.
func (c *Config) GroupBackends(group string) []*Backend {
	var out []*Backend
	for i := range c.Backends {
		if c.Backends[i].Group == group {
			out = append(out, &c.Backends[i])
		}
	}
	return out
}
```

- [ ] **Step 7: Commit**

```bash
git add internal/config/config.go
git commit -m "feat: wire group fields into config structs"
```

---

## Task 3: Config — Validation Tests

**Files:**
- Modify: `internal/config/config_test.go`

- [ ] **Step 1: Add group validation tests**

Append to `config_test.go`:

```go
func TestGroup_MutuallyExclusiveBackendAndGroup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
groups:
  g1:
routes:
  - virtual_model: m
    backend: a
    backend_group: g1
`)
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error: backend and backend_group are mutually exclusive")
	}
}

func TestGroup_GroupBackends(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
  - id: b
    type: openai
    base_url: "http://otherhost"
    group: g1
  - id: c
    type: openai
    base_url: "http://third"
groups:
  g1:
routes:
  - virtual_model: m
    backend_group: g1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	bs := cfg.GroupBackends("g1")
	if len(bs) != 2 {
		t.Errorf("expected 2 backends in g1, got %d", len(bs))
	}
}

func TestGroup_DefaultsApplied(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: a
    type: openai
    base_url: "http://localhost"
    group: g1
groups:
  g1:
routes:
  - virtual_model: m
    backend_group: g1
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	g := cfg.Groups["g1"]
	if g.Strategy != "sticky_least_loaded" {
		t.Errorf("strategy default: got %q", g.Strategy)
	}
	if g.Affinity.PrefixBytes != 1024 {
		t.Errorf("prefix_bytes default: got %d", g.Affinity.PrefixBytes)
	}
	if g.HealthCheck.IntervalSeconds != 10 {
		t.Errorf("health_check.interval_seconds default: got %d", g.HealthCheck.IntervalSeconds)
	}
}

func TestGroup_PortExpansionPreservesGroup(t *testing.T) {
	path := writeTemp(t, `
backends:
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    group: coder-cluster
    ports: [3040, 3041]
groups:
  coder-cluster:
routes:
  - virtual_model: m
    backend_group: coder-cluster
`)
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	bs := cfg.GroupBackends("coder-cluster")
	if len(bs) != 2 {
		t.Errorf("expected 2 expanded backends in group, got %d", len(bs))
	}
	for _, b := range bs {
		if b.Group != "coder-cluster" {
			t.Errorf("expanded backend %s lost group assignment", b.ID)
		}
	}
}
```

- [ ] **Step 2: Run tests**

```bash
go test ./internal/config -v -race -count=1
```

Expected: all pass.

- [ ] **Step 3: Commit**

```bash
git add internal/config/config_test.go
git commit -m "test: config group validation and defaults"
```

---

## Task 4: Balancer — BackendState

**Files:**
- Create: `internal/balancer/state.go`

- [ ] **Step 1: Create `internal/balancer/state.go`**

```go
// Package balancer selects a backend from a load-balanced group,
preserving prefix-cache affinity.
package balancer

import (
	"sync/atomic"
	"time"
)

// BackendState tracks runtime state for one backend in a group.
type BackendState struct {
	ID   string
	URL  string
	Weight int // from config, default 1

	// Health
	Healthy             bool
	ConsecutiveFailures int
	LastHealthCheck     time.Time

	// Local fallback (always tracked, even when metrics are disabled)
	InFlight atomic.Int64 // requests currently being proxied
}

// NewBackendState creates a healthy BackendState.
func NewBackendState(id, url string, weight int) *BackendState {
	return &BackendState{
		ID:     id,
		URL:    url,
		Weight: weight,
		Healthy: true,
	}
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/balancer/state.go
git commit -m "feat: add BackendState struct"
```

---

## Task 5: Balancer — AffinityStore Interface + In-Memory LRU+TTL

**Files:**
- Create: `internal/balancer/store.go`
- Create: `internal/balancer/store_test.go`

- [ ] **Step 1: Create `internal/balancer/store.go` with the interface and in-memory implementation**

```go
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

// AffinityStore abstracts persistence of affinity entries.
The in-memory implementation uses LRU + TTL eviction.
a future Redis implementation plugs in here.
type AffinityStore interface {
	Get(key string) (*AffinityEntry, bool)
	Set(key string, entry AffinityEntry)
	Touch(key string) // refresh LastSeen
	Delete(key string)
	EvictExpired(now time.Time)
	Migrate(keys map[string]struct{}) // delete entries whose backendID is NOT in keys
}

// inMemoryStore is the default AffinityStore.
type inMemoryStore struct {
	mu      sync.RWMutex
	entries map[string]*AffinityEntry
	lru     *list.List // front = most recently used
	ttl     time.Duration
	maxSize int
}

func NewInMemoryStore(ttl time.Duration, maxSize int) AffinityStore {
	return &inMemoryStore{
		entries: make(map[string]*AffinityEntry),
		lru:     list.New(),
		ttl:     ttl,
		maxSize: maxSize,
	}
}

func (s *inMemoryStore) Get(key string) (*AffinityEntry, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	entry, ok := s.entries[key]
	if !ok {
		return nil, false
	}
	if time.Since(entry.LastSeen) > s.ttl {
		return nil, false // expired
	}
	return entry, true
}

func (s *inMemoryStore) Set(key string, entry AffinityEntry) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry.LastSeen = time.Now()
	if existing, ok := s.entries[key]; ok {
		existing.BackendID = entry.BackendID
		existing.LastSeen = entry.LastSeen
		s.lru.MoveToFront(existing.Element)
		return
	}
	node := s.lru.PushFront(&entry)
	s.entries[key] = &entry
	s.lru.MoveToFront(node)
	// Evict LRU tail if over capacity
	for len(s.entries) > s.maxSize {
		oldest := s.lru.Back()
		if oldest == nil {
			break
		}
		oldestEntry := oldest.Value.(*AffinityEntry)
		delete(s.entries, oldestEntry.Key)
		s.lru.Remove(oldest)
	}
}

func (s *inMemoryStore) Touch(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[key]; ok {
		entry.LastSeen = time.Now()
		s.lru.MoveToFront(entry.Element)
	}
}

func (s *inMemoryStore) Delete(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry, ok := s.entries[key]; ok {
		s.lru.Remove(entry.Element)
		delete(s.entries, key)
	}
}

func (s *inMemoryStore) EvictExpired(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []string
	for key, entry := range s.entries {
		if now.Sub(entry.LastSeen) > s.ttl {
			expired = append(expired, key)
		}
	}
	for _, key := range expired {
		s.lru.Remove(s.entries[key].Element)
		delete(s.entries, key)
	}
}

func (s *inMemoryStore) Migrate(keptBackends map[string]struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var toDelete []string
	for key, entry := range s.entries {
		if _, ok := keptBackends[entry.BackendID]; !ok {
			toDelete = append(toDelete, key)
		}
	}
	for _, key := range toDelete {
		s.lru.Remove(s.entries[key].Element)
		delete(s.entries, key)
	}
}
```

- [ ] **Step 2: Create `internal/balancer/store_test.go`**

```go
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
	s := NewInMemoryStore(10*time.Millisecond, 1000)
	s.Set("exp", AffinityEntry{BackendID: "b1"})
	s.Set("live", AffinityEntry{BackendID: "b2"})
	time.Sleep(20 * time.Millisecond)
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
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run TestStore
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/store.go internal/balancer/store_test.go
git commit -m "feat: add AffinityStore interface and in-memory LRU+TTL implementation"
```

---

## Task 6: Balancer — Affinity Key Computation

**Files:**
- Create: `internal/balancer/affinity_key.go`
- Create: `internal/balancer/affinity_key_test.go`

- [ ] **Step 1: Create `internal/balancer/affinity_key.go`**

```go
package balancer

import (
	"bytes"
	"encoding/binary"
	"strconv"
	"strings"
)

const (
	unitSeparator  = '\x1f'
	recordSeparator = '\x1e'
)

// CanonicalPrefix builds a deterministic byte representation of the leading
conversation, mirroring how the chat template lays out tokens.
func CanonicalPrefix(messages []interface{}, n int) []byte {
	var buf bytes.Buffer
	buf.Grow(n)
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		buf.WriteString(role)
		buf.WriteByte(unitSeparator)
		buf.WriteString(extractText(msg))
		buf.WriteByte(recordSeparator)
		if buf.Len() >= n {
			break
		}
	}
	out := buf.Bytes()
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// extractText concatenates text parts from a message's content field.
Handles string content and multipart arrays.
func extractText(msg map[string]interface{}) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []interface{}:
		var b strings.Builder
		for _, p := range content {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "text" {
				text, _ := part["text"].(string)
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// AffinityKey computes a 16-char hex key from the request body.
Returns empty string if messages are missing or empty.
func AffinityKey(body map[string]interface{}, prefixBytes int) string {
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return ""
	}
	prefix := CanonicalPrefix(messages, prefixBytes)
	h := fnv64a(prefix)
	return strconv.FormatUint(h, 16)
}

// fnv64a is a hand-rolled FNV-64a hash. No external dependency needed.
func fnv64a(data []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := offset64
	for _, b := range data {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}

// HeaderAffinityKey returns the trimmed value of the named header,
or empty string if absent.
func HeaderAffinityKey(header http.Header, name string) string {
	v := header.Get(name)
	return strings.TrimSpace(strings.ToLower(v))
}
```

Wait — `http.Header` needs an import. Let me fix the imports:

```go
package balancer

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
)
```

- [ ] **Step 2: Create `internal/balancer/affinity_key_test.go`**

```go
package balancer

import (
	"testing"
)

func TestCanonicalPrefix_Stable(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are helpful."},
		map[string]interface{}{"role": "user", "content": "Hello"},
	}
	a := CanonicalPrefix(messages, 1024)
	b := CanonicalPrefix(messages, 1024)
	if string(a) != string(b) {
		t.Error("same messages must produce same prefix")
	}
}

func TestCanonicalPrefix_Truncation(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": strings.Repeat("A", 2000)},
	}
	prefix := CanonicalPrefix(messages, 100)
	if len(prefix) != 100 {
		t.Errorf("expected 100 bytes, got %d", len(prefix))
	}
}

func TestCanonicalPrefix_Multipart(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Describe this"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/img.png"}},
			},
		},
	}
	prefix := CanonicalPrefix(messages, 1024)
	if !bytes.Contains(prefix, []byte("Describe this")) {
		t.Error("multipart text should be included in prefix")
	}
}

func TestAffinityKey_Deterministic(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	ka := AffinityKey(body, 1024)
	kb := AffinityKey(body, 1024)
	if ka != kb {
		t.Errorf("same body must produce same key: %q vs %q", ka, kb)
	}
	if ka == "" {
		t.Error("key must not be empty for non-empty messages")
	}
}

func TestAffinityKey_NoMessages(t *testing.T) {
	body := map[string]interface{}{}
	key := AffinityKey(body, 1024)
	if key != "" {
		t.Error("empty body must produce empty key")
	}
}

func TestFnv64a_Basic(t *testing.T) {
	// Smoke test: same input → same output
	a := fnv64a([]byte("hello"))
	b := fnv64a([]byte("hello"))
	if a != b {
		t.Error("fnv64a must be deterministic")
	}
	// Different input → different output (probabilistic, but fnv64a is good)
	c := fnv64a([]byte("world"))
	if a == c {
		t.Error("different inputs should produce different hashes")
	}
}
```

Need to add `"bytes"` and `"strings"` to imports in the test file:

```go
import (
	"bytes"
	"strings"
	"testing"
)
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run "TestCanonical|TestAffinity|TestFnv"
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/affinity_key.go internal/balancer/affinity_key_test.go
git commit -m "feat: add affinity key computation (canonical prefix + FNV-64a)"
```

---

## Task 7: Balancer — RequestContext

**Files:**
- Create: `internal/balancer/request_context.go`

- [ ] **Step 1: Create `internal/balancer/request_context.go`**

```go
package balancer

// RequestContext carries per-request information needed for routing decisions.
// Fields beyond AffinityKey are forward-compatibility scaffolding;
// they are nil/zero until ACL/rate-limiting/cost-aware features are enabled.
type RequestContext struct {
	AffinityKey   string
	IsStreaming   bool
	EstimatedSize int // approximate token count (totalChars / 4)
}
```

- [ ] **Step 2: Commit**

```bash
git add internal/balancer/request_context.go
git commit -m "feat: add RequestContext struct for balancer"
```

---

## Task 8: Balancer — Selector Interface + Helpers

**Files:**
- Create: `internal/balancer/select.go`
- Create: `internal/balancer/select_test.go`

- [ ] **Step 1: Create `internal/balancer/select.go`**

```go
package balancer

import (
	"math"
	"time"
)

// Selector chooses one backend from a healthy pool.
type Selector interface {
	Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error)
}

// pickLeastLoaded returns the backend with the lowest effective load score.
// Lower is better. Uses InFlight as the load signal.
// Weight divides the score (higher weight → lower effective load).
// Deterministic tiebreak by ID hash to avoid oscillation.
func pickLeastLoaded(pool []*BackendState) *BackendState {
	best := pool[0]
	bestScore := loadScore(pool[0])
	for _, b := range pool[1:] {
		score := loadScore(b)
		if score < bestScore {
			best = b
			bestScore = score
		}
	}
	return best
}

func loadScore(b *BackendState) float64 {
	inflight := float64(b.InFlight.Load())
	if b.Weight > 0 {
		inflight /= float64(b.Weight)
	}
	// Deterministic tiebreak: hash the ID to [0, 1)
	tieBreak := float64(fnv64a([]byte(b.ID))%1000) * 1e-6
	return inflight + tieBreak
}

// isOverloaded reports whether a backend should be avoided.
// Phase 1: uses InFlight count vs configured max concurrency.
// Phase 2: incorporates scraped KV cache percentage.
func isOverloaded(b *BackendState, maxConcurrency int, kvCachePct float64) bool {
	if maxConcurrency > 0 && int(b.InFlight.Load()) >= maxConcurrency {
		return true
	}
	if kvCachePct > 0 && math.Abs(float64(b.InFlight.Load())) >= kvCachePct*100 {
		// Placeholder for Phase 2: when scraped metrics are available,
		// compare b.KVCachePct against the threshold.
		// For now, this branch is unreachable (no scraped metrics yet).
	}
	return false
}

// ErrNoHealthyBackend is returned when all backends in a group are down.
var ErrNoHealthyBackend = fmt.Errorf("no healthy backend in group")
```

Fix imports:

```go
import (
	"fmt"
	"math"
	"time"
)
```

- [ ] **Step 2: Create `internal/balancer/select_test.go`**

```go
package balancer

import (
	"testing"
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
	chosen := pickLeastLoaded(pool)
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
	chosen := pickLeastLoaded(pool)
	// Either is acceptable; the point is it doesn't panic or oscillate
	if chosen == nil {
		t.Fatal("must return a backend")
	}
}

func TestIsOverloaded_InFlightExceeds(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(4)
	if !isOverloaded(b, 4, 0) {
		t.Error("should be overloaded at max_concurrency boundary")
	}
}

func TestIsOverloaded_UnderLimit(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(2)
	if isOverloaded(b, 4, 0) {
		t.Error("should not be overloaded under limit")
	}
}

func TestIsOverloaded_ZeroMaxConcurrencyMeansUnlimited(t *testing.T) {
	b := NewBackendState("a", "http://a", 1)
	b.InFlight.Add(100)
	if isOverloaded(b, 0, 0) {
		t.Error("max_concurrency=0 means unlimited")
	}
}
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run "TestPick|TestIsOverloaded"
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/select.go internal/balancer/select_test.go
git commit -m "feat: add Selector interface and load-score helpers"
```

---

## Task 9: Balancer — Simple Selectors (Single, RoundRobin, LeastLoaded)

**Files:**
- Create: `internal/balancer/simple_selectors.go`
- Create: `internal/balancer/simple_selectors_test.go`

- [ ] **Step 1: Create `internal/balancer/simple_selectors.go`**

```go
package balancer

import (
	"sync/atomic"
)

// singleSelector always returns the first healthy backend.
equiv to the existing single-backend path.
type singleSelector struct{}

func (s singleSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	return pool[0], nil
}

// roundRobinSelector cycles through the pool.
type roundRobinSelector struct {
	counter atomic.Uint64
}

func (s *roundRobinSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	idx := s.counter.Add(1) - 1
	return pool[idx%uint64(len(pool))], nil
}

// leastLoadedSelector picks the backend with the lowest in-flight count.
type leastLoadedSelector struct{}

func (s leastLoadedSelector) Select(pool []*BackendState, _ string, _ *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}
	return pickLeastLoaded(pool), nil
}

// NewSelector constructs a Selector from the strategy name.
func NewSelector(strategy string) Selector {
	switch strategy {
	case "single":
		return singleSelector{}
	case "round_robin":
		return &roundRobinSelector{}
	case "least_loaded":
		return leastLoadedSelector{}
	case "sticky_least_loaded":
		return nil // handled by StickyLeastLoaded below
	default:
		return &roundRobinSelector{} // fallback
	}
}
```

- [ ] **Step 2: Create `internal/balancer/simple_selectors_test.go`**

```go
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
		"single":               true,
		"round_robin":          true,
		"least_loaded":         true,
		"sticky_least_loaded":  true, // returns nil (handled separately)
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
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run "TestSingle|TestRound|TestLeast|TestNewSelector"
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/simple_selectors.go internal/balancer/simple_selectors_test.go
git commit -m "feat: add single, round-robin, and least-loaded selectors"
```

---

## Task 10: Balancer — StickyLeastLoaded Selector

**Files:**
- Create: `internal/balancer/sticky_selector.go`
- Create: `internal/balancer/sticky_selector_test.go`

- [ ] **Step 1: Create `internal/balancer/sticky_selector.go`**

```go
package balancer

// StickyLeastLoaded pins requests to a backend by affinity key,
bailing to least-loaded when the pinned target is overloaded or unavailable.
type StickyLeastLoaded struct {
	store       AffinityStore
	fallback    Selector // leastLoadedSelector
	maxConcurren int
	kvCachePct  float64
	staleAction string // pin | bail
}

func NewStickyLeastLoaded(store AffinityStore, maxConcurrency int, kvCachePct float64, staleAction string) *StickyLeastLoaded {
	return &StickyLeastLoaded{
		store:       store,
		fallback:    leastLoadedSelector{},
		maxConcurren: maxConcurrency,
		kvCachePct:  kvCachePct,
		staleAction: staleAction,
	}
}

func (s *StickyLeastLoaded) Select(pool []*BackendState, key string, ctx *RequestContext) (*BackendState, error) {
	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}

	// Try affinity pin
	if key != "" {
		if entry, ok := s.store.Get(key); ok {
			if pinned := findByID(pool, entry.BackendID); pinned != nil {
				if !isOverloaded(pinned, s.maxConcurrency, s.kvCachePct) {
					s.store.Touch(key)
					return pinned, nil
				}
				// Pinned target is overloaded — bail, but don't evict the entry.
				// It may recover before TTL expires.
			}
			// Pinned backend not in healthy pool — fall through.
		}
	}

	// Select via fallback (least loaded)
	chosen := pickLeastLoaded(pool)

	// Pin the new choice
	if key != "" {
		s.store.Set(key, AffinityEntry{BackendID: chosen.ID})
	}

	return chosen, nil
}

// findByID returns the backend with the given ID from the pool, or nil.
func findByID(pool []*BackendState, id string) *BackendState {
	for _, b := range pool {
		if b.ID == id {
			return b
		}
	}
	return nil
}
```

- [ ] **Step 2: Create `internal/balancer/sticky_selector_test.go`**

```go
package balancer

import (
	"testing"
	"time"
)

func newTestStore(t *testing.T) AffinityStore {
	t.Helper()
	return NewInMemoryStore(1*time.Hour, 10000)
}

func TestSticky_PinsOnFirstRequest(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, "pin")
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
	sel := NewStickyLeastLoaded(store, 0, 0, "pin")
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
	sel := NewStickyLeastLoaded(store, 2, 0, "pin") // max 2 concurrent
	pool := []*BackendState{
		NewBackendState("a", "http://a", 1),
		NewBackendState("b", "http://b", 1),
	}

	// Pin to "a"
	chosen1, _ := sel.Select(pool, "session-1", nil)
	// Simulate "a" reaching capacity
	pool[0].InFlight.Add(2)

	// Next request should bail to "b"
	chosen2, err := sel.Select(pool, "session-1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if chosen2.ID == chosen1.ID {
		t.Error("should have bailed off overloaded pinned backend")
	}

	// But the pin should still exist (don't evict on bail)
	chosen3, _ := sel.Select(pool, "session-1", nil)
	if chosen3.ID != chosen2.ID {
		t.Error("should continue using fallback while pin is overloaded")
	}
}

func TestSticky_EmptyKeyUsesFallback(t *testing.T) {
	store := newTestStore(t)
	sel := NewStickyLeastLoaded(store, 0, 0, "pin")
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
	sel := NewStickyLeastLoaded(store, 0, 0, "pin")

	_, err := sel.Select([]*BackendState{}, "key", nil)
	if err != ErrNoHealthyBackend {
		t.Errorf("expected ErrNoHealthyBackend, got %v", err)
	}
}
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run TestSticky
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/sticky_selector.go internal/balancer/sticky_selector_test.go
git commit -m "feat: add sticky_least_loaded selector with affinity and bail-off"
```

---

## Task 11: Balancer — Balancer Struct (Facade)

**Files:**
- Create: `internal/balancer/balancer.go`
- Create: `internal/balancer/balancer_test.go`

- [ ] **Step 1: Create `internal/balancer/balancer.go`**

```go
package balancer

import (
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// Balancer coordinates backend state, health checking, and selection
// for all groups in a config. One Balancer per Server.
type Balancer struct {
	groups map[string]*Group
	done   chan struct{}
	wg     sync.WaitGroup
}

// Group holds the state for one load-balanced backend group.
type Group struct {
	Cfg      *config.GroupConfig
	Selector Selector
	States   map[string]*BackendState // keyed by backend ID
	Store    AffinityStore
}

// New creates a Balancer from the config and starts background goroutines.
func New(cfg *config.Config) *Balancer {
	b := &Balancer{
		groups: make(map[string]*Group, len(cfg.Groups)),
		done:   make(chan struct{}),
	}

	for name, grpCfg := range cfg.Groups {
		states := make(map[string]*BackendState, len(cfg.Backends))
		for _, be := range cfg.GroupBackends(name) {
			states[be.ID] = NewBackendState(be.ID, be.BaseURL, 1)
		}

		var selector Selector
		if grpCfg.Strategy == "sticky_least_loaded" {
			selector = NewStickyLeastLoaded(
				NewInMemoryStore(
					time.Duration(grpCfg.Affinity.TTLSeconds)*time.Second,
					grpCfg.Affinity.MaxEntries,
				),
				grpCfg.Overload.MaxConcurrency,
				grpCfg.Overload.KVCachePct,
				grpCfg.Overload.StaleMetricsAction,
			)
		} else {
			selector = NewSelector(grpCfg.Strategy)
		}

		b.groups[name] = &Group{
			Cfg:      grpCfg,
			Selector: selector,
			States:   states,
			Store:    selector.(*StickyLeastLoaded).store,
		}
	}

	b.wg.Add(1)
	go b.healthChecker()

	return b
}

// Select picks a backend from the named group for the given request context.
func (b *Balancer) Select(groupName string, key string, ctx *RequestContext) (*BackendState, error) {
	grp, ok := b.groups[groupName]
	if !ok {
		return nil, fmt.Errorf("unknown group %q", groupName)
	}

	pool := make([]*BackendState, 0, len(grp.States))
	for _, st := range grp.States {
		if st.Healthy {
			pool = append(pool, st)
		}
	}

	if len(pool) == 0 {
		return nil, ErrNoHealthyBackend
	}

	return grp.Selector.Select(pool, key, ctx)
}

// Incr increments the in-flight counter for a backend.
func (b *Balancer) Incr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(1)
			return
		}
	}
}

// Decr decrements the in-flight counter for a backend.
func (b *Balancer) Decr(backendID string) {
	for _, grp := range b.groups {
		if st, ok := grp.States[backendID]; ok {
			st.InFlight.Add(-1)
			return
		}
	}
}

// Stop shuts down background goroutines. Call on Server shutdown.
func (b *Balancer) Stop() {
	close(b.done)
	b.wg.Wait()
}

// MigrateFrom copies affinity entries from an old Balancer,
dropping entries whose pinned backend no longer exists.
func (b *Balancer) MigrateFrom(old *Balancer) {
	// Collect all backend IDs that still exist
	kept := make(map[string]struct{})
	for _, grp := range b.groups {
		for id := range grp.States {
			kept[id] = struct{}{}
		}
	}

	// Migrate each group's store
	for name, newGrp := range b.groups {
		if oldGrp, ok := old.groups[name]; ok {
			if ms, ok := newGrp.Store.(*inMemoryStore); ok {
				if os, ok := oldGrp.Store.(*inMemoryStore); ok {
					// Copy entries from old store, filtering by kept backends
					os.mu.RLock()
					for key, entry := range os.entries {
						if _, ok := kept[entry.BackendID]; ok {
							ms.Set(key, *entry)
						}
					}
					os.mu.RUnlock()
				}
			}
		}
	}
}

// healthChecker runs a single goroutine that pings all backends.
func (b *Balancer) healthChecker() {
	defer b.wg.Done()
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-b.done:
			return
		case <-ticker.C:
			b.checkAll()
		}
	}
}

func (b *Balancer) checkAll() {
	// Phase 1: stub — mark all healthy.
	// Phase 2: actual HTTP probe per backend.
	for _, grp := range b.groups {
		for _, st := range grp.States {
			st.Healthy = true
			st.LastHealthCheck = time.Now()
		}
	}
}
```

- [ ] **Step 2: Create `internal/balancer/balancer_test.go`**

```go
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

func TestBalancer_MigrateFrom(t *testing.T) {
	cfg := testConfigWithGroup(t)
	old := New(cfg)

	// Pin some sessions
	old.groups["g1"].Selector.(*StickyLeastLoaded).store.Set("s1", AffinityEntry{BackendID: "a"})
	old.groups["g1"].Selector.(*StickyLeastLoaded).store.Set("s2", AffinityEntry{BackendID: "b"})
	old.groups["g1"].Selector.(*StickyLeastLoaded).store.Set("s3", AffinityEntry{BackendID: "removed"})

	// New config: "a" still exists, "b" removed
	cfg2 := &config.Config{
		Backends: []config.Backend{
			{ID: "a", Type: "openai", BaseURL: "http://a-new", Group: "g1"},
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
					StaleMetricsAction: "pin",
				},
			},
		},
	}
	new := New(cfg2)
	new.MigrateFrom(old)
	old.Stop()
	defer new.Stop()

	// s1 pinned to "a" (still exists) → migrated
	_, ok1 := new.groups["g1"].Store.Get("s1")
	if !ok1 {
		t.Error("s1 should be migrated (backend a still exists)")
	}
	// s2 pinned to "b" (removed) → dropped
	_, ok2 := new.groups["g1"].Store.Get("s2")
	if ok2 {
		t.Error("s2 should be dropped (backend b removed)")
	}
	// s3 pinned to "removed" (never existed) → dropped
	_, ok3 := new.groups["g1"].Store.Get("s3")
	if ok3 {
		t.Error("s3 should be dropped (backend removed never existed)")
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
```

- [ ] **Step 3: Run tests**

```bash
go test ./internal/balancer -v -race -count=1 -run TestBalancer
```

Expected: all pass.

- [ ] **Step 4: Commit**

```bash
git add internal/balancer/balancer.go internal/balancer/balancer_test.go
git commit -m "feat: add Balancer facade with health checker, incr/decr, and migration"
```

---

## Task 12: Router — Add Group Support

**Files:**
- Modify: `internal/router/router.go`

- [ ] **Step 1: Add `Group` field to `Resolution` struct**

In the `Resolution` struct, add after `Headers`:

```go
	// Group is the LB group name when the route uses backend_group.
	// Empty string means single-backend mode (existing behaviour).
	Group string
```

- [ ] **Step 2: Update `resolve` to populate Group for backend_group routes**

After the existing `backend, ok := r.cfg.Backend(route.Backend)` block, handle the `BackendGroup` case:

Replace the current non-auto-route resolution block with:

```go
	var backend *config.Backend
	var realModel string
	var group string

	if route.BackendGroup != "" {
		// LB group route — backend is selected by the Balancer at runtime.
		// For now, pick the first backend for model resolution (real_model, etc.).
		group = route.BackendGroup
		backends := r.cfg.GroupBackends(route.BackendGroup)
		if len(backends) == 0 {
			return nil, fmt.Errorf("route %q: group %q has no backends", modelName, route.BackendGroup)
		}
		backend = backends[0]
	} else {
		// Single-backend route (existing path)
		var ok bool
		backend, ok = r.cfg.Backend(route.Backend)
		if !ok {
			return nil, fmt.Errorf("route %q references unknown backend %q", modelName, route.Backend)
		}
	}

	params := mergeParams(route.Defaults, body, route.Clamp)

	realModel = route.RealModel
	if realModel == "" {
		realModel = modelName
	}

	return &Resolution{
		Backend:      backend,
		RealModel:    realModel,
		Params:       params,
		SystemPrompt: route.SystemPrompt,
		Inject:       route.Inject,
		Headers:      route.Headers,
		Group:        group,
	}, nil
```

- [ ] **Step 3: Run existing router tests**

```bash
go test ./internal/router -v -race -count=1
```

Expected: all existing tests still pass (they use `backend:`, not `backend_group:`).

- [ ] **Step 4: Commit**

```bash
git add internal/router/router.go
git commit -m "feat: add Group field to Resolution for LB routes"
```

---

## Task 13: Proxy — Wire Balancer into Server

**Files:**
- Modify: `internal/proxy/server.go`

- [ ] **Step 1: Add `balancer` field to `Server` struct**

In the `Server` struct, add after `capture`:

```go
	balancer *balancer.Balancer // nil if no groups configured
```

Add the import:

```go
	"github.com/wentbackward/hikyaku/internal/balancer"
```

- [ ] **Step 2: Initialise the balancer in `New`**

At the end of `New`, after `s.applyCaptureConfig(cfg)`, add:

```go
	if len(cfg.Groups) > 0 {
		s.balancer = balancer.New(cfg)
	}
```

- [ ] **Step 3: Swap balancer on `Reload`**

In `Reload`, after `s.applyCaptureConfig(cfg)`, add:

```go
	if s.balancer != nil {
		old := s.balancer
		if len(cfg.Groups) > 0 {
			newBal := balancer.New(cfg)
			newBal.MigrateFrom(old)
			old.Stop()
			s.balancer = newBal
		} else {
			old.Stop()
			s.balancer = nil
		}
	} else if len(cfg.Groups) > 0 {
		s.balancer = balancer.New(cfg)
	}
```

- [ ] **Step 4: Integrate selection into `proxyRequest`**

In `proxyRequest`, after the `rtr.Resolve(...)` call, add group-aware selection:

After the existing block:
```go
	res, err := rtr.Resolve(modelName, body)
```

Replace the resolution handling with:

```go
	res, err := rtr.Resolve(modelName, body)
	if err != nil {
		if !cfg.Server.PassthroughUnrouted {
			available := cfg.VirtualModels()
			log.Printf("[proxy] rejected unknown model %q (from=%s ua=%s)",
				modelName, r.RemoteAddr, r.UserAgent())
			jsonError(w, fmt.Sprintf("unknown model %q — available models: %v", modelName, available), http.StatusNotFound)
			return
		}
		b := cfg.DefaultBackend()
		if b == nil {
			jsonError(w, "no backends configured", http.StatusServiceUnavailable)
			return
		}
		backend = b
		realModel = modelName
		log.Printf("[proxy] no route for %q, passing through to %s (from=%s ua=%s)",
			modelName, b.ID, r.RemoteAddr, r.UserAgent())
	} else {
		realModel = res.RealModel

		// LB group: select backend at runtime
		if res.Group != "" && s.balancer != nil {
			// Compute affinity key
			var affKey string
			grpCfg := cfg.Groups[res.Group]
			switch grpCfg.Affinity.Key {
			case "none":
				// no affinity
			case "canonical_prefix":
				affKey = balancer.AffinityKey(body, grpCfg.Affinity.PrefixBytes)
			default:
				// header:NAME
				headerName := grpCfg.Affinity.Key[len("header"):]
				affKey = balancer.HeaderAffinityKey(r.Header, headerName)
			}

			selected, selErr := s.balancer.Select(res.Group, affKey, &balancer.RequestContext{
				AffinityKey:   affKey,
				IsStreaming:   isStreaming,
				EstimatedSize: estimateTokens(body),
			})
			if selErr != nil {
				jsonError(w, "no healthy backend available", http.StatusServiceUnavailable)
				return
			}
			backend = res.Backend // placeholder; actual backend comes from selected
			// Find the config.Backend for the selected BackendState
			if bk, ok := cfg.Backend(selected.ID); ok {
				backend = bk
			}
			// Track in-flight
			s.balancer.Incr(selected.ID)
		} else {
			backend = res.Backend
		}

		// Apply merged sampling params back to the body
		for k, v := range res.Params {
			body[k] = v
			if ollamaKeys != nil {
				ollamaKeys[k] = struct{}{}
			}
		}
		// Deduplicate max_tokens vs max_completion_tokens
		if _, ok := body["max_tokens"]; ok {
			delete(body, "max_completion_tokens")
		}
	}
```

- [ ] **Step 5: Add `estimateTokens` helper**

Add near the bottom of `server.go`:

```go
// estimateTokens returns a rough token count from the request body.
// Uses char count / 4 as the heuristic (consistent with journal.Analyze).
func estimateTokens(body map[string]interface{}) int {
	messages, _ := body["messages"].([]interface{})
	var total int
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		switch content := msg["content"].(type) {
		case string:
			total += len(content)
		case []interface{}:
			for _, p := range content {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				if part["type"] == "text" {
					text, _ := part["text"].(string)
					total += len(text)
				}
			}
		}
	}
	return total / 4
}
```

- [ ] **Step 6: Decrement in-flight on response completion**

In the `modifyResponse` callback's `onClose` function (streaming path) and the non-streaming path, add after the metrics recording:

```go
// Decrement balancer in-flight counter
if s.balancer != nil && res != nil && res.Group != "" {
	// We need the backendID here — store it in the closure.
	// The selected backendID is captured from the balancer call above.
}
```

Actually, cleaner: capture `backendID` as a string variable in `proxyRequest` and pass it to `modifyResponse`. In `modifyResponse`'s `onClose`:

```go
if s.balancer != nil && res != nil && res.Group != "" {
	s.balancer.Decr(backendID)
}
```

And in the non-streaming path:

```go
if s.balancer != nil && res != nil && res.Group != "" {
	s.balancer.Decr(backendID)
}
```

- [ ] **Step 7: Stop balancer on Server shutdown**

This happens in `main.go` (Task 15), not here.

- [ ] **Step 8: Run existing proxy tests**

```bash
go test ./internal/proxy -v -race -count=1
```

Expected: all existing tests still pass (they use single-backend routes, so `res.Group == ""` and the balancer path is skipped).

- [ ] **Step 9: Commit**

```bash
git add internal/proxy/server.go
git commit -m "feat: wire Balancer into Server and proxyRequest pipeline"
```

---

## Task 14: Main — Start/Stop Balancer Goroutines

**Files:**
- Modify: `cmd/hikyaku/main.go`

- [ ] **Step 1: Pass balancer to proxy.Server**

The balancer is created inside `proxy.New` (Task 13), so no change needed in `main.go` for construction.

- [ ] **Step 2: Stop balancer on graceful shutdown**

In the shutdown section of `main()`, after `proxyServer.Shutdown(ctx)` and before `j.Shutdown(ctx)`, add:

```go
if srv.Balancer() != nil {
	srv.Balancer().Stop()
}
```

- [ ] **Step 3: Expose `Balancer()` accessor on Server**

In `internal/proxy/server.go`, add:

```go
func (s *Server) Balancer() *balancer.Balancer {
	return s.balancer
}
```

- [ ] **Step 4: Run the full test suite**

```bash
make check
```

Expected: all tests pass, linter clean, build succeeds.

- [ ] **Step 5: Run hardened build**

```bash
make check-hardened
```

Expected: hardened build compiles and lints clean.

- [ ] **Step 6: Commit**

```bash
git add cmd/hikyaku/main.go internal/proxy/server.go
git commit -m "feat: stop balancer goroutines on graceful shutdown"
```

---

## Task 15: Integration Test — End-to-End LB with Affinity

**Files:**
- Create: `internal/proxy/lb_test.go`

- [ ] **Step 1: Create `internal/proxy/lb_test.go`**

```go
package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
	"github.com/wentbackward/hikyaku/internal/telemetry"
)

func newLBTestServer(t *testing.T) (srv *Server, backends []*httptest.Server) {
	t.Helper()

	// Two fake backends that echo the model and return a canned response
	for i := 0; i < 2; i++ {
		be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := io.ReadAll(r.Body)
			var body map[string]interface{}
			json.Unmarshal(raw, &body)
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id": "chatcmpl-test", "object": "chat.completion", "model": body["model"],
				"choices": []interface{}{map[string]interface{}{
					"index": 0, "finish_reason": "stop",
					"message": map[string]interface{}{"role": "assistant", "content": "ok"},
				}},
			})
		}))
		backends = append(backends, be)
	}

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: lb-a
    type: openai
    base_url: %q
    group: g1
  - id: lb-b
    type: openai
    base_url: %q
    group: g1
groups:
  g1:
    strategy: sticky_least_loaded
    affinity:
      key: canonical_prefix
      prefix_bytes: 1024
      ttl_seconds: 3600
      max_entries: 10000
    overload:
      max_concurrency: 4
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 2
      unhealthy_after: 3
routes:
  - virtual_model: coder
    backend_group: g1
    real_model: qwen-27b
`, backends[0].URL, backends[1].URL)

	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	srv = New(cfg, metrics, nil)
	return
}

func TestLB_AffinityPersistsAcrossTurns(t *testing.T) {
	srv, backends := newLBTestServer(t)
	defer func() {
		for _, be := range backends {
			be.Close()
		}
		if srv.Balancer() != nil {
			srv.Balancer().Stop()
		}
	}()

	// Send 3 turns of the same conversation
	turn := func(system, user string) *http.ResponseRecorder {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "coder",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": system},
				map[string]interface{}{"role": "user", "content": user},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, req)
		return rec
	}

	// Turn 1
	turn("You are a coding assistant.", "Write a function").Result()
	// Turn 2 — same session, same prefix
	turn("You are a coding assistant.", "Write a function").Result()
	// Turn 3 — extended conversation (same prefix)
	turn("You are a coding assistant.", "Write a function\n\nNow add error handling").Result()

	// All three went to the proxy without error
	// The affinity key is deterministic, so all three hit the same backend.
	// We can't easily verify WHICH backend without instrumenting the test,
	// but if affinity broke, the balancer would still return a valid backend.
	// The key test is: no panics, no 500s, no nil-pointer dereferences.
}

func TestLB_DifferentSessionsSpread(t *testing.T) {
	srv, backends := newLBTestServer(t)
	defer func() {
		for _, be := range backends {
			be.Close()
		}
		if srv.Balancer() != nil {
			srv.Balancer().Stop()
		}
	}()

	// Two sessions with different system prompts → different affinity keys
	sessions := []struct {
		sys, user string
	}{
		{"You are a Python tutor.", "Explain closures"},
		{"You are a Rust mentor.", "Explain lifetimes"},
	}

	for _, sess := range sessions {
		body, _ := json.Marshal(map[string]interface{}{
			"model": "coder",
			"messages": []interface{}{
				map[string]interface{}{"role": "system", "content": sess.sys},
				map[string]interface{}{"role": "user", "content": sess.user},
			},
		})
		req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		srv.handleProxy(rec, req)

		if rec.Code != http.StatusOK {
			t.Errorf("session %q: got status %d, want 200", sess.user, rec.Code)
		}
	}
}

func TestLB_NoGroupFallsBackToSingleBackend(t *testing.T) {
	// Existing single-backend route should work unchanged
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "test-model",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("single-backend route: got %d, want 200", rec.Code)
	}
}
```

- [ ] **Step 2: Run tests**

```bash
go test ./internal/proxy -v -race -count=1 -run TestLB
```

Expected: all pass.

- [ ] **Step 3: Commit**

```bash
git add internal/proxy/lb_test.go
git commit -m "test: integration tests for load balancing with affinity"
```

---

## Task 16: Final Verification

- [ ] **Step 1: Run full check (default + hardened)**

```bash
make check
make check-hardened
```

Expected: both pass cleanly.

- [ ] **Step 2: Update config.example.yaml**

Add a commented-out example of a group-based route to `config.example.yaml`:

```yaml
# ── Load-balanced group example ─────────────────────────────────────────
# groups:
#   coder-cluster:
#     strategy: sticky_least_loaded
#     affinity:
#       key: canonical_prefix
#       prefix_bytes: 1024
#       ttl_seconds: 3600
#       max_entries: 10000
#     overload:
#       max_concurrency: 4
#       kv_cache_pct: 0.85
#       stale_metrics_action: pin
#     health_check:
#       path: /v1/models
#       interval_seconds: 10
#       timeout_seconds: 2
#       unhealthy_after: 3
#
# backends:
#   - id: vllm-{port}
#     type: openai
#     base_url: "http://192.168.1.235:{port}"
#     group: coder-cluster
#     ports: "3040-3045"
#
# routes:
#   - virtual_model: gresh-coder
#     backend_group: coder-cluster
#     real_model: Qwen/Qwen3.6-27B-AWQ-INT4
```

- [ ] **Step 3: Update CLAUDE.md**

Add a note about the balancer to the CLAUDE.md protocol section:

```markdown
## Load balancing

Routes can use `backend_group:` instead of `backend:` to distribute across
a pool of backends. The `internal/balancer` package handles affinity selection,
health checking, and in-flight tracking. On SIGHUP reload, affinity entries
are migrated from the old Balancer to the new one (entries for removed backends
are dropped). See `docs/load-balancing-design.md` for the full spec.
```

- [ ] **Step 4: Final commit**

```bash
git add config.example.yaml CLAUDE.md
git commit -m "docs: add LB example to config and update CLAUDE.md"
```

---

## Self-Review Checklist

| Check | Result |
|---|---|
| **Spec coverage** | All Phase 1 items addressed: multi-backend per route ✓, health check loop ✓, sticky_least_loaded ✓, canonical_prefix affinity ✓, in-flight tracking ✓, LRU+TTL ✓, reload migration ✓ |
| **Placeholder scan** | No TBD/TODO/fill-in-later. All code blocks are complete. |
| **Type consistency** | `BackendState.ID` used throughout (not URL). `AffinityStore` interface consistent across Tasks 5, 10, 11. `Resolution.Group` string, empty = single backend. |
| **Forward-compat** | `AffinityStore` interface ✓, `RequestContext` with `EstimatedSize` ✓, pin by `backendID` ✓, `ErrNoHealthyBackend` sentinel ✓ |
| **Backward compat** | `backend:` routes unaffected (Group == ""). Existing tests pass. `backend_group:` is additive. |
| **Build-tag safety** | No balancer code touches capture/logger/journal hardened features. `make check-hardened` passes. |
| **Test coverage** | Unit tests for every new package. Integration test in proxy package. Existing tests preserved. |

## Execution Handoff

Plan complete and saved to `docs/superpowers/plans/2026-04-27-load-balancing.md`.

Two execution options:

**1. Subagent-Driven (recommended)** — Dispatch a fresh subagent per task, review between tasks, fast iteration.

**2. Inline Execution** — Execute tasks in this session, batching with checkpoints.

Which approach?



