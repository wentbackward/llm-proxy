# hikyaku — Load Balancing Design

Spec for adding multi-backend load balancing to hikyaku, with affinity
preservation aligned to vLLM/SGLang prefix-cache behavior.

## Goals (priority order)

1. **Preserve prefix-cache locality.** Multi-turn coding sessions reuse
   tens of thousands of cached KV tokens per request. A naive round-robin
   would cause full prefix re-prefill on every turn — seconds of wasted
   TTFT per turn at modest context sizes, tens of seconds at large ones.
2. **Distribute load when sessions are independent.** N concurrent sessions
   on M backends should spread roughly N/M per backend.
3. **Bail off an affinity pin when the target is overloaded.** Don't let one
   replica become a hotspot just because affinity says so.
4. **Tolerate one backend going away.** Health check + fallback to peers,
   no blast-radius beyond the connections that were pinned to it.

## Non-goals

- Cost-based or latency-based routing (LiteLLM-style). Useful for some
  multi-vendor setups; orthogonal to the prefix-cache problem we're
  solving here.
- Per-token billing accounting. The proxy already passes upstream usage
  data through; load balancing doesn't change that.
- Cross-backend cache replication. vLLM/SGLang prefix caches are
  per-replica in-memory only; the proxy's job is to route consistently,
  not synchronize state.

## Background: how the cache actually works

Both vLLM and SGLang cache KV at the **block-token level**:

1. The chat template renders `messages[]` to a single prompt string.
2. The string is tokenized.
3. Tokens are grouped into fixed-size blocks (default 16 tokens in vLLM;
   SGLang has a similar block structure).
4. Each block's hash chains from the previous block's hash, so a cache
   hit on block N requires blocks 1..N-1 to match exactly.
5. A new request hits the cache for whatever block-aligned prefix matches
   what's already in memory.

Implications for routing:

- **Same session → strong overlap.** Turn N+1 sends the entire prior
  conversation; turn N's tokens are still cached on whichever backend
  served turn N. Routing turn N+1 elsewhere costs full prefill.
- **Same system prompt across sessions → partial overlap.** Coalescing
  sessions onto fewer backends amortizes the system prompt's KV.
- **Block-aligned alignment is precise but expensive to compute.** The
  proxy approximates with byte-level hashing of a canonical prefix —
  good enough for routing, ~free.

## Architecture

```
┌─────────┐      ┌──────────────────────────┐      ┌────────────┐
│ client  │──┬──▶│  hikyaku               │──┬──▶│ backend A  │
└─────────┘  │   │                          │  │   │ vLLM/SGLang│
             │   │  ┌────────────────────┐  │  │   └────────────┘
             │   │  │ affinity table     │  │  │
             │   │  │  key → backend     │  │  │   ┌────────────┐
             │   │  └────────────────────┘  │  └──▶│ backend B  │
             │   │                          │      │ vLLM/SGLang│
             │   │  ┌────────────────────┐  │      └────────────┘
             │   │  │ backend state      │  │
             │   │  │  health, metrics   │  │      (one entry per
             │   │  └────────────────────┘  │       backend in pool)
             │   │                          │
             │   │  goroutines:             │
             │   │   - health checker      │
             │   │   - metrics scraper     │
             │   │   - affinity sweeper    │
             │   └──────────────────────────┘
```

## Configuration schema (additive)

Extend the existing route config with a `backends` list and routing block:

```yaml
routes:
  gresh-coder:
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    backends:
      - url: http://192.168.1.235:3041
      - url: http://192.168.1.247:3042
      # Optional per-backend overrides:
      # - url: http://...
      #   weight: 2          # bias least-loaded selection (default 1)
      #   max_concurrency: 4 # override route-level value
    strategy: sticky_least_loaded
      # Options:
      #   sticky_least_loaded — primary; pin by affinity, bail when overloaded
      #   least_loaded        — pure load balancing, no affinity (cache-cold)
      #   round_robin         — simple distribution, no affinity (cache-cold)
      #   single              — single backend (existing behavior)
    affinity:
      key: canonical_prefix      # canonical_prefix | header:NAME | none
      prefix_bytes: 1024         # for canonical_prefix only
      ttl_seconds: 3600          # evict idle entries after this
      max_entries: 10000         # LRU cap
    overload:                    # used by sticky_least_loaded to bail off pin
      max_concurrency: 4         # match backend's --max-num-seqs
      kv_cache_pct: 0.85
      stale_metrics_action: pin  # pin | bail — when metrics > stale_threshold
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 2
      unhealthy_after: 3         # consecutive failures before marking down
    metrics_scrape:
      enabled: auto              # auto | true | false
      interval_seconds: 5
      stale_threshold_seconds: 30
      # auto = probe each backend's /metrics on startup/SIGHUP and enable
      # per-backend if the endpoint responds with parseable Prometheus text
```

## Backend capability probing

On proxy startup and on `SIGHUP` (config reload), probe each backend's
`/metrics` endpoint:

```
GET /metrics
Accept: text/plain

Decision tree:
  200 OK + parseable Prometheus text   → enable metrics for this backend
                                         attempt to detect engine type
                                         (look for vllm:* vs sglang:* names)
  any other response                   → disable metrics for this backend
                                         routing falls back to running-count
                                         from in-flight tracking only
```

Engine-specific metric mappings (proxy must support both):

| Backend metric            | vLLM                          | SGLang                        |
|---------------------------|-------------------------------|-------------------------------|
| In-flight requests        | `vllm:num_requests_running`   | `sglang:num_running_reqs`     |
| Queued requests           | `vllm:num_requests_waiting`   | `sglang:num_queue_reqs`       |
| KV cache utilization      | `vllm:gpu_cache_usage_perc`   | `sglang:token_usage`          |
| (optional) prompt tokens  | `vllm:prompt_tokens_total`    | `sglang:prompt_tokens_total`  |
| (optional) gen tokens     | `vllm:generation_tokens_total`| `sglang:gen_tokens_total`     |

(The exact SGLang metric names should be verified by probing a live
instance — the proxy's parser should be defensive: look for any metric
name containing `running`, `queue`/`waiting`, and `cache`/`usage`/`token`,
and prefer the lowest-cardinality gauge.)

If `/metrics` is unavailable or returns a parse error after retries,
the proxy must still route correctly using only its own in-flight tracking
(count of requests currently being proxied to each backend). Metrics
enrich the routing decision; they aren't required for correctness.

## Backend state model

Per backend, in memory:

```go
type Backend struct {
    URL                  string
    Weight               int           // from config, default 1

    // Health
    Healthy              bool
    ConsecutiveFailures  int
    LastHealthCheck      time.Time

    // Engine type (detected at startup)
    EngineType           string        // "vllm" | "sglang" | "unknown"
    MetricsEnabled       bool          // /metrics probe succeeded

    // Metrics-derived (nil/zero when MetricsEnabled = false)
    RunningReqs          int
    WaitingReqs          int
    KVCachePct           float64
    LastMetricsUpdate    time.Time

    // Local fallback (always tracked, even when metrics enabled)
    InFlightLocal        atomic.Int64  // requests currently being proxied
}
```

`InFlightLocal` is incremented when the proxy dispatches to this backend,
decremented when the response completes (or errors). It's the routing
decision's source of truth when metrics are stale or disabled.

## Affinity table

```go
type AffinityEntry struct {
    BackendURL string
    LastSeen   time.Time
}

type AffinityTable struct {
    mu      sync.RWMutex
    entries map[string]AffinityEntry
    lru     *list.List              // for max_entries enforcement
    ttl     time.Duration
    maxSize int
}

// Periodic sweeper (every 60s):
//   evict entries where time.Since(LastSeen) > ttl
//   evict LRU tail until len(entries) <= maxSize
```

## Affinity key computation

For `key: canonical_prefix`, build a deterministic byte representation of
the leading conversation, hash with xxhash64:

```go
const (
    UnitSeparator    = '\x1f'
    RecordSeparator  = '\x1e'
)

func canonicalPrefix(messages []Message, n int) []byte {
    var buf bytes.Buffer
    buf.Grow(n)
    for _, m := range messages {
        buf.WriteString(m.Role)
        buf.WriteByte(UnitSeparator)
        // Content can be a string or a multipart array of parts.
        // For multipart, concatenate the text parts in order.
        buf.WriteString(stringifyContent(m.Content))
        buf.WriteByte(RecordSeparator)
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

func affinityKey(req Request) string {
    if len(req.Messages) == 0 {
        return ""  // no key → fall through to least-loaded
    }
    prefix := canonicalPrefix(req.Messages, cfg.PrefixBytes)
    h := xxhash.Sum64(prefix)
    return strconv.FormatUint(h, 16)  // 16-char hex key
}
```

### Why this fingerprint

- **Walks `messages[]` in order**, mirroring how the chat template lays
  out tokens. Same session always produces the same leading bytes
  regardless of how many turns deep it is.
- **Captures both system prompt and first user turn** in the typical
  1024-byte window. Sessions sharing a system prompt naturally coalesce
  to the same backend, amortizing the system-prompt KV.
- **Distinguishes sessions by their opening user prompt** once they've
  diverged past the system prompt.
- **Resilient to:** missing system prompt (just hashes the user message),
  multiple system prompts, multipart content (text parts concatenated).
- **Vulnerable to:** intentional history truncation (CLI drops oldest
  turns when context grows). When the leading bytes shift, affinity
  re-keys and may flip backend. By that point the session is many turns
  in, prefix cache is hot on the original backend, and a flip is costly.
  Mitigation: optional second fingerprint (last user message hash) for
  re-pinning on long sessions; defer to Phase 2 unless observed.

### `key: header:NAME`

If a client cooperates by sending a stable session ID header, prefer it:

```yaml
affinity:
  key: header:x-session-id
```

Look up `req.Headers["X-Session-Id"]`, lowercase trim, use as the affinity
key directly. Falls through to `canonical_prefix` if the header is absent.

### `key: none`

Disables affinity entirely. Useful for stateless workloads where no
multi-turn benefit is expected.

## Selection algorithm (sticky_least_loaded)

```go
func selectBackend(req Request, route Route) (*Backend, error) {
    pool := healthyBackends(route)
    if len(pool) == 0 {
        return nil, ErrNoHealthyBackend
    }

    var key string
    if route.Affinity.Key != "none" {
        key = affinityKey(req)  // empty string if can't compute
    }

    if key != "" {
        if pinned := route.AffinityTable.Get(key); pinned != nil {
            backend := lookupBackend(pool, pinned.BackendURL)
            if backend != nil && !isOverloaded(backend, route.Overload) {
                route.AffinityTable.Touch(key)
                return backend, nil
            }
            // Pinned target unavailable or overloaded — fall through.
            // Don't evict the entry yet; it may recover before TTL.
        }
    }

    chosen := pickLeastLoaded(pool)
    if key != "" {
        route.AffinityTable.Set(key, chosen.URL)
    }
    return chosen, nil
}

func isOverloaded(b *Backend, o Overload) bool {
    if !b.MetricsEnabled || time.Since(b.LastMetricsUpdate) > cfg.StaleThreshold {
        // Metrics stale or disabled. Use action policy.
        switch o.StaleMetricsAction {
        case "pin":   return false                           // trust the pin
        case "bail":  return b.InFlightLocal.Load() >= o.MaxConcurrency
        default:      return false
        }
    }
    return b.RunningReqs >= o.MaxConcurrency ||
           b.KVCachePct  >= o.KVCachePct
}

func pickLeastLoaded(pool []*Backend) *Backend {
    return argmin(pool, func(b *Backend) float64 {
        // Lower is better. Use metrics if available, fall back to local.
        var load float64
        if b.MetricsEnabled && time.Since(b.LastMetricsUpdate) < cfg.StaleThreshold {
            load = float64(b.RunningReqs) + b.KVCachePct*2.0
        } else {
            load = float64(b.InFlightLocal.Load())
        }
        // Weight inversely (higher weight → lower effective load)
        load /= float64(b.Weight)
        // Deterministic tiebreak by URL hash to avoid oscillation
        load += float64(stringHash(b.URL)%1000) * 1e-6
        return load
    })
}
```

## Health checking

A simple loop, one goroutine per backend:

```go
func healthLoop(b *Backend, route Route) {
    ticker := time.NewTicker(route.HealthCheck.Interval)
    defer ticker.Stop()
    for range ticker.C {
        ctx, cancel := context.WithTimeout(context.Background(), route.HealthCheck.Timeout)
        resp, err := http.Get(b.URL + route.HealthCheck.Path)
        cancel()

        if err == nil && resp.StatusCode == 200 {
            b.Healthy = true
            b.ConsecutiveFailures = 0
        } else {
            b.ConsecutiveFailures++
            if b.ConsecutiveFailures >= route.HealthCheck.UnhealthyAfter {
                b.Healthy = false
                log.Printf("backend %s marked unhealthy: %v", b.URL, err)
            }
        }
        if resp != nil {
            resp.Body.Close()
        }
    }
}
```

When a backend transitions unhealthy → healthy, do **not** flush its
affinity entries — clients with that pin can resume using the warm KV
cache once it's available again.

## Metrics scraping (when enabled)

```go
func metricsLoop(b *Backend, route Route) {
    ticker := time.NewTicker(route.MetricsScrape.Interval)
    defer ticker.Stop()
    for range ticker.C {
        text, err := scrapeMetrics(b.URL + route.MetricsScrape.Path)
        if err != nil {
            // Don't disable; just leave LastMetricsUpdate stale.
            // Stale-threshold logic in isOverloaded handles it.
            continue
        }
        running, waiting, kvPct, ok := parseMetrics(text, b.EngineType)
        if !ok {
            continue
        }
        b.RunningReqs = running
        b.WaitingReqs = waiting
        b.KVCachePct = kvPct
        b.LastMetricsUpdate = time.Now()
    }
}
```

`parseMetrics` is a simple line scanner over Prometheus text format; no
need for a full Prometheus client library. Match by metric name (per the
mapping table above), pick the gauge value.

## Failure modes and behavior

| Scenario | Behavior |
|---|---|
| All backends unhealthy | Return 503 to client. Don't queue. |
| Pinned backend dies mid-session | Next request from that affinity key re-pins to a healthy peer. Cache miss costs one prefill. |
| Pinned backend healthy but overloaded (`isOverloaded`) | Bail to least-loaded peer for this request. Don't update the affinity entry — it remains pinned for when the original recovers. |
| All backends overloaded | Pick least-bad (smallest `load_score`). Don't 503; the queue at the backend is faster than tearing down and retrying. |
| Metrics endpoint flaps | Use last-known values until `stale_threshold_seconds`, then defer to local in-flight count. |
| New backend added via SIGHUP reload | Probe metrics, start health-check loop, mark healthy after first OK probe. Affinity entries pinned to *removed* backends should be evicted on reload. |
| Burst of new sessions arriving simultaneously | Each gets a fresh affinity key (by canonical_prefix); least-loaded distributes them. Affinity entries created in a tight window can briefly imbalance — this is fine, settles within a few seconds. |
| Two sessions with identical opening prompts | Same affinity key → both pin to same backend. Cache locality across sessions is actually a *feature* here. Slight contention; not catastrophic. |

## Phasing

### Phase 1 — MVP (~1 day)

- Multi-backend per route in config
- Health check loop
- `sticky_least_loaded` strategy with `canonical_prefix` affinity
- Local in-flight tracking (`InFlightLocal`) for overload detection
- No metrics scraping yet — `MetricsEnabled = false` for all backends
- LRU + TTL on affinity table

This alone fixes the "4 sessions, 1 backend" hotspot problem and
preserves cache locality.

### Phase 2 — Telemetry-aware (~1 day)

- Probe `/metrics` on startup + SIGHUP per backend
- Background scrape loop, parse vLLM and SGLang Prometheus formats
- Switch `pickLeastLoaded` and `isOverloaded` to use scraped metrics
- `stale_metrics_action` config

### Phase 3 — Polish (optional)

- Drain mode (mark a backend "draining" before stopping it; existing
  affinities stick, no new affinities created)
- Per-route concurrency caps with proxy-side queuing
- Connection-stickiness as a fast-path optimization (skip body parsing
  if source port matches a recent affinity entry — unlikely to help in
  measured CLI behavior, but cheap to add later)
- Prometheus metrics exposed *by* the proxy itself: routing decisions,
  affinity hit rate, per-backend dispatch counts

## Configuration example (full)

```yaml
listen: 0.0.0.0:4000

routes:
  gresh-coder:
    real_model: Qwen/Qwen3.6-27B-AWQ-INT4
    backends:
      - url: http://192.168.1.235:3041
      - url: http://192.168.1.247:3042
    strategy: sticky_least_loaded
    affinity:
      key: canonical_prefix
      prefix_bytes: 1024
      ttl_seconds: 3600
      max_entries: 10000
    overload:
      max_concurrency: 4
      kv_cache_pct: 0.85
      stale_metrics_action: pin
    health_check:
      path: /v1/models
      interval_seconds: 10
      timeout_seconds: 2
      unhealthy_after: 3
    metrics_scrape:
      enabled: auto
      interval_seconds: 5
      stale_threshold_seconds: 30

  gresh-general:
    # 35B currently single-replica — strategy: single is the existing path
    real_model: Qwen/Qwen3.6-35B-A3B-FP8
    backends:
      - url: http://192.168.1.247:3040
    strategy: single
```

## Test plan

1. **Unit**: `canonicalPrefix` produces stable bytes across messages
   re-ordering of same content (it shouldn't — same content in same
   order = same bytes; different order = different bytes; document this).
2. **Unit**: `affinityKey` is deterministic for same input.
3. **Unit**: `pickLeastLoaded` ties broken deterministically.
4. **Integration**: 4 concurrent sessions to one route with 2 backends.
   Verify each session pins to one backend across turns and the two
   backends each get ~2 sessions.
5. **Integration**: kill one backend mid-session. Verify next request
   re-pins, returns successfully, and no 5xx leaks to client.
6. **Integration**: simulate `/metrics` returning 404. Verify routing
   still works using local in-flight tracking.
7. **Integration**: SIGHUP with config change (add a backend). Verify
   probe runs, new backend picks up new sessions, existing pins remain.
8. **Load**: 100 sessions, 5 backends, mixed turn lengths. Distribution
   within ±10% of perfect, affinity hit rate above 95%.

## Open questions for implementation

- Should affinity hits / misses be observable as proxy metrics? (Phase 3)
- Should the proxy report `X-hikyaku-backend` header on responses for
  client-side debugging? Useful but trivial to add.
- Should there be a `force_backend` query param / header for admin
  override of routing during debugging?
- For SGLang, verify the actual metric names and update the mapping
  table. Test against a live SGLang instance during Phase 2.
