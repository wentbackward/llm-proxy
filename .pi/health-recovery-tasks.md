# Health & Recovery Implementation Plan

## Phase 2.1 — Monitoring Config
- [ ] Step 1: Add `load_balancing` top-level config structs (AliveConfig, MetricsConfig, FlowTrackingConfig, RecoveryConfig)
- [ ] Step 2: Add per-group override support in GroupConfig
- [ ] Step 3: Add resolution helpers: `GetAliveConfig()`, `GetMetricsConfig()`, `GetFlowTracking()`, `GetRecovery()`
- [ ] Step 4: Tests for config resolution cascade
- [ ] Step 5: `make check`

## Phase 2.2 — Flow Tracker
- [ ] Step 6: Add `FlowStats` struct to `BackendState` (dispatched, completed, success, failure, timeout, avg_ttft_ms, stalled)
- [ ] Step 7: Rolling window bucket implementation (sliding window, not fixed-interval)
- [ ] Step 8: Wire flow tracking hooks into proxy (on dispatch, on completion)
- [ ] Step 9: Tests for flow tracking accuracy
- [ ] Step 10: `make check`

## Phase 2.3 — Composite Score
- [ ] Step 11: Replace `loadScore` with composite scorer incorporating flow stats
- [ ] Step 12: Update `isOverloaded` to use composite threshold
- [ ] Step 13: Tests for composite scoring
- [ ] Step 14: `make check`

## Phase 2.4 — Alive Checker
- [ ] Step 15: Lightweight probe implementation (max_tokens: 1)
- [ ] Step 16: OR-based alive logic (lightweight OR http_get)
- [ ] Step 17: Separate alive checker goroutine from scrape loop
- [ ] Step 18: Tests for alive checker
- [ ] Step 19: `make check`

## Phase 2.5 — Graduated Recovery
- [ ] Step 20: Retry delay on unhealthy transition
- [ ] Step 21: Ramp-up phase on recovery
- [ ] Step 22: Affinity awareness during ramp-up
- [ ] Step 23: Tests for recovery behavior
- [ ] Step 24: `make check`

## Phase 2.6 — Metrics Retry
- [ ] Step 25: Periodic `/metrics` retry in HealthMode
- [ ] Step 26: Seamless transition to ScrapeMode on success
- [ ] Step 27: Tests for metrics retry
- [ ] Step 28: `make check`
