# Phase 2 — Telemetry-Aware Load Balancing

## Scope
Background metrics scraping from backend `/metrics` endpoints (vLLM/SGLang Prometheus format). Feeds scraped data into load scoring and overload detection. Falls back to local InFlight tracking when metrics are stale or unavailable.

### 1. Config
- `metrics_scrape` on groups: `enabled` (bool|"auto"), `interval_seconds`, `path` (default `/metrics`)
- `stale_threshold_seconds` (default 30) — metrics older than this are considered stale
- `stale_metrics_action`: `pin` (use stale metrics) vs `bail` (fall back to local InFlight)

### 2. Scraper
- Probes `/metrics` at startup to detect availability
- Parses Prometheus text format (both vLLM and SGLang variants)
- Extracts: `num_requests_running`, `num_requests_waiting`, `kv_cache_usage_perc`

### 3. BackendState extension
- Adds: `RunningReqs`, `WaitingReqs`, `KVCachePct`, `LastMetricsUpdate`, `MetricsAvailable`
- Protected by existing mutex

### 4. Integration
- When metrics available + fresh: use scraped `RunningReqs` + `KVCachePct` in `loadScore` and `isOverloaded`
- When stale: honor `stale_metrics_action` (pin = use stale data, bail = fall back to InFlight)
- When unavailable: use local InFlight (current behavior)

## Tasks

- [ ] Step 1: Add metrics_scrape config to GroupConfig
- [ ] Step 2: Extend BackendState with scraped metrics fields
- [ ] Step 3: Implement Prometheus parser (vLLM + SGLang)
- [ ] Step 4: Integrate scraper into balancer (startup probe + background loop)
- [ ] Step 5: Update loadScore and isOverloaded to use scraped metrics
- [ ] Step 6: Tests (parser + integration)
- [ ] Step 7: make check
