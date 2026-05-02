# hikyaku — Current Features

## Core Proxy
- **Single URL abstraction** — clients point at one endpoint; hikyaku routes to any backend
- **Multi-protocol** — OpenAI (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`), Anthropic (`/v1/messages`), Ollama native (`/api/chat`, `/api/generate`, `/api/embed`, `/api/tags`)
- **Protocol isolation** — no translation between protocols. Each lane isolated to its backend type
- **SSE passthrough** — zero-copy streaming, metrics parsed from byte stream without buffering
- **RFC 3986 URL resolution** — all URLs via `url.ResolveReference`, no string concatenation

## Routing & Virtual Models
- **Virtual models** — same underlying model, multiple names with different parameter profiles
- **Three-layer param merge** — `defaults < caller < clamp`
- **Auto-routing** — inspect request body to pick text vs vision sub-route
- **Passthrough unrouted** — forward unknown model names to default backend
- **System prompt ops** — prepend/append/replace on system message per route
- **Body injection** — deep-merge arbitrary JSON into request body per route
- **Header manipulation** — add/remove/rename outbound headers per route and per backend

## Load Balancing
- **Four strategies** — `sticky_least_loaded`, `least_loaded`, `round_robin`, `single`
- **Hash-based tiebreak** — deterministic even distribution across backends using affinity key hash
- **Config syntax** — list and map formats supported, mixed-format detection with clear errors

## Health & Recovery
- **Three-layer health model**:
  - **Alive** — OR-based probes (lightweight_chat or HTTP GET)
  - **Quality** — rolling window of TTFT, error rate, p95
  - **Capacity** — scraped metrics (KV cache %, running/waiting requests) + in-flight
- **Ground-truth health** — real request outcomes drive health state (3 strikes → unhealthy)
- **Innocent until proven guilty** — backends with in-flight requests immune to probe demotion
- **Graduated recovery** — ramp-up phase on recovery, affinity-aware (no new pins during warm-up)
- **Failover migration** — pin invalidated on connection error or 5xx, backend excluded for 10s cooldown
- **Timeout tolerance** — timeouts don't trigger cooldown or health penalty (backend busy, not dead)
- **Passive recovery** — when all probes disabled, periodic re-admission after cooldown
- **Probe knobs** — `health_check.enabled` and `alive.enabled` (true/false), fully zero-probe capable

## Observability
- **OpenTelemetry + Prometheus** — TTFT, duration, token counts, active requests, generation speed
- **New gauges** — `hikyaku_active_requests`, `hikyaku_affinity_cache_entries`, `hikyaku_request_body_bytes_buffered`
- **Request journal** — structured log of every request (configurable depth)
- **SIGUSR1 capture** — bounded window capturing full request/response bodies to disk
- **Log levels 0-4** — from silent to full content inspection

## Defenders
- **Loop detection** — catches repeated identical requests from agent retry loops
- **Zero-content detection** — rejects trivial user content wrapped in massive system/tool defs
- **Drop empty content** — strips malformed messages with empty content

## Security
- **TLS enforcement** — refuses to start without TLS unless explicitly opted in
- **Constant-time auth** — bearer token comparison
- **Secret resolution** — `${ENV_VAR}` syntax, never stored in config
- **Non-root container** — `FROM scratch`, USER 65532, no shell, no libc (~7MB)
- **Hardened build** — `-tags hardened` compiles out capture, verbose logging, and prompt text
- **Per-backend auth** — api_key, tls_client_cert, bearer_token

## Operations
- **Hot reload** — SIGHUP updates config, log level, probes without restart
- **Per-backend concurrency limits** — semaphore-based, config-driven
- **Per-backend timeouts** — independent timeout per backend
- **Config map syntax** — YAML map keys auto-become `id`/`virtual_model`
