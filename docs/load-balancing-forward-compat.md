# Forward-Compatibility Analysis — Load Balancing

**Date:** 2026-04-27  
**Author:** Coding Agent  
**Parent Spec:** `load-balancing-design.md`

---

## Purpose

Document the design decisions made during the load balancing spec review that protect against future requirements: multi-instance proxy clustering, per-key ACL/rate limiting/accounting, and intelligent cost-vs-latency routing.

---

## 1. Multi-Instance Proxy Clustering (Redundancy / Failover)

### Problem statement

Two or more hikyaku instances behind a front-end load balancer. Instance A pins session X to backend 3. Session X's next request hits Instance B. Instance B has no affinity entry → sends to backend 1 → full prefill on every other turn.

### Design decisions

#### 1.1 `AffinityStore` interface (not concrete type)

The in-memory LRU table is the Phase 1 implementation. A Redis-backed store plugs in later without touching the selector or balancer.

```go
type AffinityStore interface {
    Get(key string) (*AffinityEntry, bool)
    Set(key string, entry AffinityEntry)
    Delete(key string)
}
```

Effort: ~10 lines. Payoff: zero-touch migration to shared affinity.

#### 1.2 Pin by `backendID`, not `backendURL`

Backends are identified by stable logical IDs (`vllm-3040`), not URLs. URLs change with IPs, ports, DNS rotation. The affinity table stores `entry.BackendID string`.

This is a one-field choice. Retrofitting it later means migrating stored entries and updating the selector.

#### 1.3 Health check divergence is accepted (Phase 1)

Instance A marks backend X unhealthy. Instance B hasn't noticed. Requests from B to X fail and the client retries. Wasteful but not incorrect. If clustering is needed later, introduce a gossip mechanism or shared health store. Don't over-engineer for a scenario that hasn't materialised.

---

## 2. Per-Key Authentication, ACL, Rate Limiting, Accounting

### Problem statement

Current model: one `server.api_key` gates everything. Desired: per-key model access control, per-key rate limits, per-key token accounting for billing/allocation.

### Design decisions

#### 2.1 `RequestContext.Principal`

The `RequestContext` passed to `Balancer.Select` carries the authenticated identity:

```go
type RequestContext struct {
    Principal     *AuthenticatedKey
    AffinityKey   string
    IsStreaming   bool
    EstimatedSize int
}
```

Without this, ACL enforcement has no home — it bolts onto the auth middleware and leaks into the proxy handler. With it, the balancer can refuse a selection ("this key can't access this group") and accounting hooks attribute token counts correctly.

#### 2.2 Rate limiter sits in the proxy handler chain, not the balancer

Rate limiting is a pre-selection gate, orthogonal to routing:

```
bearerAuth → rateLimit → systemPrompt → inject → translateParams → selectBackend → proxy
```

The rate limiter reads from `Principal` and consults a per-key counter. In-memory sliding window or token bucket for Phase 1. External store for clustered deployments.

#### 2.3 Accounting is a label dimension, not a new subsystem

The proxy already extracts `prompt_tokens` and `completion_tokens` from every response and records them in Prometheus counters. Add a `key` label:

```
llm_completion_tokens_total{backend="vllm-3040", model="real-model", key="sk-dev-abc"} += 1234
```

Zero new plumbing — one extra attribute on existing measurements. Default label value is `"global"` for the legacy global key.

#### 2.4 Config shape (additive, non-breaking)

```yaml
server:
  api_key: sk-global-fallback    # existing; still supported
  keys:                           # NEW
    - id: dev-team
      token: sk-dev-xxxxxxxxxxxx
      allowed_groups: [coder-cluster]
      rate_limit:
        requests_per_minute: 100
        tokens_per_hour: 500000
    - id: prod-service
      token: sk-prod-yyyyyyyyyyyy
      allowed_groups: [coder-cluster, general-cluster]
      rate_limit:
        requests_per_minute: 1000
        tokens_per_hour: 10000000
```

Validation: if `allowed_groups` is specified, requests to routes whose group is not listed return 403. If omitted, the key accesses everything (like the current global key).

---

## 3. Intelligent Routing (Cost vs Cache Trade-off)

### Problem statement

Sometimes it's cheaper to take a cache miss than to block a heavily loaded server. Example: backend 1 has 8 queued requests, backend 2 has 0. Session X is pinned to backend 1. Should we bail to backend 2 despite the prefill cost?

### Design decisions

#### 3.1 `RequestContext.EstimatedSize` enables the trade-off

Carrying approximate token count (`totalChars / 4`) in the request context gives the selector the information needed to weigh prefill cost against queue depth:

```
evictionPressure = f(runningReqs, waitingReqs, kvCachePct, sessionTokenCount)
if evictionPressure > threshold { bail }
```

This is an enhancement to `sticky_least_loaded`'s bail-off logic, not a new strategy. The `Selector` interface doesn't change.

#### 3.2 Strategy taxonomy (phased)

| Category | Strategies | Phase |
|---|---|---|
| Baseline | `single`, `round_robin`, `least_loaded` | 1 |
| Locality-aware | `sticky_least_loaded`, `sticky_round_robin` | 1 |
| Cost-aware | `cheapest`, `cost_cap` | Future |
| Latency-aware | `fastest`, `latency_target` | Future |

Future strategies read new fields from `RequestContext` (e.g., `BudgetPerToken`, `TargetTTFT`). The interface is stable.

---

## 4. Consolidated Changes Required Now

| Change | Effort | Enables |
|---|---|---|
| `AffinityStore` interface (not concrete type) | ~10 lines | Redis-backed affinity for multi-instance clusters |
| Pin by `backendID`, not `backendURL` | 1 field rename | Stable affinity across IP/port changes |
| `RequestContext.Principal` field | 1 field + plumbing | ACL, rate limiting, per-key accounting |
| `server.keys:` config section | New struct + validation | Per-key model access, quotas, billing |
| Accounting metrics carry `key` label | 1 attribute | Per-key token attribution in Prometheus |

None add meaningful complexity to Phase 1. All are additive scaffolding — interfaces with one implementation, fields that are `nil` until configured, labels that default to `"global"`.

---

## 5. Known Deferrals

- **Redis-backed affinity store** — deferred until multi-instance deployment is needed
- **Per-key rate limiter middleware** — deferred until per-key auth is needed
- **Drain mode** — deferred until rolling deploy automation is needed
- **Prometheus metrics emitted by the proxy** (affinity hit rate, per-backend dispatch counts) — deferred to polish phase
- **Cross-proxy gossip for health state** — deferred until clustering is needed
