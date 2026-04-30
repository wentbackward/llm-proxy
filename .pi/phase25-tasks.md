# Phase 2.5 — Request Defenders

## Scope
Two pre-routing checks that short-circuit pathological requests.

### 1. Zero-content detection
- **Signal**: last user message content < `min_user_content_chars` (default 10) AND estimated total input >= `min_total_input_tokens` (default 2000)
- **Action**: `refuse_400` (default) or `inject_minimal_response`
- **Placement**: before routing, after body parse

### 2. Loop detection
- **Signal**: same `(affinity_key, hash(last_user_message.content))` arrives ≥ `consecutive_threshold` (default 3) within `window_seconds` (default 60)
- **Action**: `inject_forcing_message` (default), escalate to `refuse_429` after `escalate_after` further repeats
- **Placement**: before routing, after body parse

### 3. Config schema
```yaml
defenders:
  loop_detection:
    enabled: true
    consecutive_threshold: 3
    window_seconds: 60
    action: inject_forcing_message
    escalate_after: 2
    escalate_action: refuse_429
  zero_content_detection:
    enabled: true
    min_user_content_chars: 10
    min_total_input_tokens: 2000
    action: refuse_400
```
Per-route override: `routes[].defenders.loop_detection` / `.zero_content_detection`

### 4. Observability
- Prometheus counters: `loop_detection_triggered_total`, `loop_detection_escalated_total`, `zero_content_blocked_total`
- Response header: `X-hikyaku-defender: <action>`

## Tasks

- [x] Step 1: Add defenders config schema (global + per-route)
- [x] Step 2: Implement zero-content detection (validateLastUserContent + checkTotalInputSize)
- [x] Step 3: Implement loop detection (LoopDetector with LRU/TTL map)
- [x] Step 4: Wire both into proxyRequest (before routing)
- [x] Step 5: Add metrics + response headers
- [x] Step 6: Tests (unit + integration)
- [x] Step 7: `make check`
