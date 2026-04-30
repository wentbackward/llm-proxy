# Health & Recovery Design Override (Rev 2)

## Problems with the original spec

1. **Permanent startup decision** — if `/metrics` fails at boot (cold start, network blip), the backend is stuck in `/models` mode forever. No recovery path.
2. **Scrape failure = backend death** — `/metrics` can be flaky (GC stalls, prometheus handler bugs) while the engine still processes requests perfectly fine.
3. **No message-flow telemetry** — we dispatch requests but don't track outcomes per backend. We don't know success rates, timeouts, or completion patterns.
4. **Affinity traps** — a user pinned to an overloaded/dead backend waits forever; the pin doesn't expire gracefully.
5. **No graduated recovery** — a backend that recomes online gets hammered immediately with all pending affinity traffic.
6. **Probe timeouts coupled to request timeouts** — a 600s request timeout means a 600s probe hang.

## Core principle: Separate "Alive" from "Ready"

Three independent signals, each answering a different question:

| Signal | Question | Source | Frequency |
|--------|----------|--------|-----------|
| **Alive** | Can the backend accept HTTP at all? | Lightweight probe OR `/health` | Slow (60s) |
| **Quality** | How well is it performing? | Scraped `/metrics` + message flow stats | Fast (5s) |
| **Capacity** | Should we send it more work right now? | Queue depth, KV cache, in-flight count | Real-time |

## Signal 1: Alive Check (OR-based)

A backend is "alive" if **either** of these succeeds:

```
alive = (lightweight_probe_succeeds) OR (health_endpoint_succeeds)
```

**Lightweight probe**: POST to `/v1/chat/completions` with `max_tokens: 1`, a 3-character message, and an empty system prompt. Minimal token cost, validates the full inference pipeline (tokenize → schedule → decode 1 token → return). Timeout: 5s (independent of request timeout).

**Health endpoint**: GET `/health` (vLLM) or `/v1/models` (generic). Validates HTTP stack. Timeout: 2s (independent of request timeout).

If **neither** succeeds → backend is truly dead. Exclude from pool.
If **either** succeeds → backend is alive. Proceed to quality assessment.

## Signal 2: Quality Assessment (rolling window)

Track per-backend message flow statistics over a **per-group** rolling window:

Collected per backend:
- `dispatched_count` — requests sent to this backend in the window
- `completed_count` — requests that returned (regardless of outcome)
- `success_count` — HTTP 200 completions
- `failure_count` — HTTP 4xx/5xx responses
- `timeout_count` — requests that exceeded the configured timeout
- `avg_ttft_ms` — mean time-to-first-token in the window
- `stalled_count` — requests dispatched but not yet completed (overflowing the window)

Derived metrics:
- `success_rate = success_count / completed_count` (0.0 – 1.0)
- `throughput = dispatched_count / window_duration` (req/s)
- `staleness_factor = time_since_last_success / window_duration` (0.0 = fresh, 1.0 = cold)

These are computed lazily on `Select()` — no background computation, just counting.

### Window length tuning

Different workloads need different observation windows:

| Workload | Typical request length | Recommended window | Rationale |
|----------|----------------------|-------------------|-----------|
| Chat (bursty) | Short, fast | 60–120s | Quick reaction to transient failures |
| Coding (heavy) | Long, slow | 300–600s | Smooth out variance from long generations |
| Mixed | Variable | 120–300s | Balance responsiveness and noise filtering |

Expressed as a **multiplier of the typical request timeout** for automatic scaling:

```yaml
flow_tracking:
  window_mode: multiplier   # multiplier | fixed
  window_multiplier: 2.0    # window = timeout * multiplier (default 2x)
  window_fixed_seconds: 300 # used when mode=fixed
```

Or as an absolute duration:

```yaml
flow_tracking:
  window_mode: fixed
  window_fixed_seconds: 300
```

## Signal 3: Capacity (real-time)

Combines scraped metrics (when available) with local in-flight tracking:

```
effective_load = weighted_sum(
    scraped_running_reqs * 1.0,      # from /metrics
    scraped_kv_cache_pct * 2.0,      # from /metrics (penalty)
    local_in_flight * 1.0,           # always available
    stalled_count * 3.0,             # from quality window (strong penalty)
) / weight
```

When scraped metrics are stale (older than `stale_threshold`):
- `stale_metrics_action: pin` → use last-known scraped values (assume steady state)
- `stale_metrics_action: bail` → fall back to local in-flight only

## Self-healing: graduated recovery

When a backend transitions to **unhealthy**:

1. **Mark unhealthy** — remove from healthy pool immediately
2. **Retry delay** — wait `retry_delay_seconds` (default 30s) before next alive check
3. **Graduated ramp** — on recovery, don't restore full affinity immediately:
   - Phase 1 (first `ramp_up_seconds`, default 60s): accept new requests but prefer other backends for new affinity pins. Existing pins honored.
   - Phase 2: fully restored, eligible for new affinity pins.

## Routing decision: composite score

Replace the simple `pickLeastLoaded` with a composite scorer:

```
score = effective_load * (1.0 + staleness_penalty) * (1.0 + failure_penalty)

where:
  staleness_penalty = (1.0 - success_rate) * 2.0   # 0.0 if 100% success, up to 2.0
  failure_penalty   = timeout_count / max(dispatched_count, 1) * 3.0
```

Lower score = preferred. Affinity still applies as the first-pass filter (try pinned backend, bail if overloaded).

## Startup behavior: resilient probing

At startup, probe each backend's `/metrics` with a retry budget:

```
startup_probe_retries: 3
startup_probe_backoff: 5s (fixed)
```

After 3 failures, fall back to health-only mode BUT **keep trying** `/metrics` periodically (every `metrics_retry_interval`, default 120s). On success, transition to scrape mode seamlessly.

## Configuration model

### Top-level: `load_balancing` block (global defaults)

```yaml
load_balancing:
  # Alive checks
  alive:
    interval_seconds: 60
    unhealthy_after: 3
    probes:
      - type: lightweight_chat
        path: /v1/chat/completions
        timeout_seconds: 5
      - type: http_get
        path: /health
        timeout_seconds: 2

  # Metrics scraping
  metrics:
    startup_retries: 3
    startup_backoff_seconds: 5
    retry_interval_seconds: 120  # how often to retry /metrics in HealthMode
    scrape_timeout_seconds: 3

  # Flow tracking
  flow_tracking:
    window_mode: multiplier
    window_multiplier: 2.0
    window_fixed_seconds: 300

  # Recovery
  recovery:
    retry_delay_seconds: 30
    ramp_up_seconds: 60
```

### Per-group: override any subset

```yaml
groups:
  coding-group:
    strategy: sticky_least_loaded
    # ...
    load_balancing:
      flow_tracking:
        window_mode: fixed
        window_fixed_seconds: 600  # long window for heavy coding workloads
      alive:
        interval_seconds: 120      # slower alive checks (less noisy)
        probes:
          - type: lightweight_chat
            timeout_seconds: 10    # heavier probe for complex models
          - type: http_get
            timeout_seconds: 3

  chat-group:
    strategy: sticky_least_loaded
    # ...
    load_balancing:
      flow_tracking:
        window_mode: fixed
        window_fixed_seconds: 90   # quick reaction for bursty chat
      alive:
        interval_seconds: 30       # faster detection
```

### Resolution rule

```
per-group.load_balancing.<key> > global.load_balancing.<key> > hardcoded defaults
```

Any key absent at the group level inherits from global. Any key absent globally uses the hardcoded default. Per-group `probes: []` (empty list) means "use global probes."

### Why not per-backend?

Parameters like `window_length` and `alive_interval` describe the **workload profile**, not the machine. All backends in a group serve the same route/model, hence the same workload. Per-group is the right granularity. If an operator needs per-backend tuning, they put it in its own group.

### Why independent probe timeouts?

Probes must be fast. A 600s request timeout reflects a long generation, not a health concern. Probe timeouts are bounded separately:
- Lightweight probe: 5s default (enough for tokenize + 1 token decode)
- HTTP GET: 2s default (enough for a small response)
- Metrics scrape: 3s default (enough for Prometheus serialization)

These are orders of magnitude smaller than typical request timeouts and don't interfere with ongoing work.

## Architecture diagram

```
┌─────────────────────────────────────────────────┐
│              Backend Lifecycle                   │
│                                                  │
│  STARTUP                                         │
│    ├── Probe /metrics (3 retries, 5s apart)     │
│    │   ├── Success → ScrapeMode                 │
│    │   └── Failure → HealthMode + RetryTimer    │
│    └── Probe alive (OR: lightweight + /health)  │
│        ├── Any success → Healthy                │
│        └── All fail → Unhealthy                 │
│                                                  │
│  RUNNING                                         │
│    ├── AliveChecker (every 60s, group-configured)│
│    │   └── OR-based: lightweight OR /health     │
│    ├── Scraper (every 5s, if ScrapeMode)        │
│    │   └── Updates: running, waiting, kv_cache  │
│    ├── FlowTracker (on every request complete)  │
│    │   └── Updates: dispatched, success, fail   │
│    └── RetryTimer (if HealthMode)               │
│        └── Every 120s: try /metrics again       │
│                                                  │
│  RECOVERY                                        │
│    ├── Unhealthy → wait retry_delay (30s)       │
│    ├── Alive check passes → RampUp phase        │
│    ├── RampUp (60s): honor pins, decline new    │
│    └── Fully restored → normal operation        │
└─────────────────────────────────────────────────┘
```

## Implementation order

### Phase 2.1 — Monitoring Config (~half day)
- `load_balancing` top-level config block
- Per-group override support
- Resolution helpers: `GetAliveConfig()`, `GetFlowTracking()`, `GetRecovery()`

### Phase 2.2 — Flow Tracker (~half day)
- Per-backend counters: dispatched, completed, success, failure, timeout
- Rolling window bucket (sliding, not fixed-interval)
- Expose via `BackendState.FlowStats`
- Thread-safe (atomic counters + mutex for window management)

### Phase 2.3 — Composite Score (~half day)
- Replace `loadScore` with composite scorer
- Incorporate flow stats into scoring
- `isOverloaded` uses composite threshold

### Phase 2.4 — Alive Checker (~half day)
- Lightweight probe implementation (max_tokens: 1)
- OR-based alive logic
- Separate from scrape loop

### Phase 2.5 — Graduated Recovery (~half day)
- Retry delay on unhealthy transition
- Ramp-up phase on recovery
- Affinity awareness during ramp-up

### Phase 2.6 — Metrics Retry (~quarter day)
- Periodic `/metrics` retry in HealthMode
- Seamless transition to ScrapeMode on success

## Total: ~2.5 days
