# Security

llm-proxy is designed for personal or small-team use, typically behind a trusted network boundary (Tailscale, VPN, or a private subnet). This document covers the explicit trade-offs, the features that can expose prompt content, and how to strip those features at compile time for deployments that demand it.

## Threat model

Assumed:

- Operators are trusted. They supply config.yaml, backend credentials, and decide what to log.
- Clients are authenticated by a shared bearer token (`server.api_key`). Anyone with the token is assumed to be trusted to see model lists, issue requests, and — at higher log levels — have their prompts recorded.
- Network between clients and the proxy is either (a) encrypted at the wire by Tailscale/WireGuard, (b) wrapped in TLS via `server.tls`, or (c) on a private subnet that the operator controls.
- Outbound links to upstream providers use HTTPS where the `base_url` says so; the proxy uses Go's standard TLS and CA bundle for verification.

Explicitly **not** addressed:

- Per-client token rotation, quota, or ACLs. There's one shared key.
- Rate limiting at the proxy. Backends' own limits apply.
- PII review of prompts. Anything a user types may be logged or captured depending on configuration.

## Transport

### Between clients and the proxy

Two options for wire encryption:

1. **Tailscale / WireGuard.** If clients and the proxy share a tailnet, all IP traffic between them is encrypted at the WireGuard layer. Running the proxy on plain HTTP inside that tunnel is equivalent in practice to HTTPS over the open internet.
2. **Gateway TLS.** Set `server.tls.cert` and `server.tls.key` in config. The proxy serves HTTPS directly. Use this when exposing the proxy outside a trusted network or when you want defense-in-depth on top of Tailscale.

When using Tailscale, you can provision a per-node cert with `tailscale cert <host>.your-tailnet.ts.net` and point `server.tls` at it — clients then connect to `https://<host>.your-tailnet.ts.net:4000/v1` with certificate verification.

### Between the proxy and upstream providers

Standard Go `net/http` transport with system CA bundle. Any backend `base_url` starting with `https://` uses TLS and verifies the server certificate. No additional configuration needed for Anthropic, OpenAI, HuggingFace, or any other HTTPS-speaking backend.

### Metrics endpoint

The Prometheus `/metrics` endpoint has **no authentication**. It binds to `127.0.0.1:9091` by default — localhost-only, no external access. To expose it on your network for a scraper, set `telemetry.prometheus.host: "0.0.0.0"` explicitly in config. Do this only on a trusted network; anyone who can reach the port can read backend IDs, model names, request volumes, token counts, and error rates.

## Authentication

A single shared bearer token in `server.api_key`. Clients send `Authorization: Bearer <token>`. Comparison is constant-time (`crypto/subtle.ConstantTimeCompare`) to avoid leaking the token via response timing.

If `api_key` is empty, authentication is disabled entirely — any request is accepted. Only use this on a loopback-only bind or inside a fully trusted network.

The token is static. To rotate, update config.yaml and send SIGHUP; in-flight requests finish under the old key.

## Features that can expose prompt contents

These are the features you need to know about — they exist for legitimate debugging reasons but they cause prompts to be written outside the application's process memory.

### Log level 3

`LOG_LEVEL=3` causes the proxy to log the first 80 bytes of every incoming request body to stdout. With typical chat JSON framing, that's enough to see the start of the user's message. All of stdout goes to `docker logs` and anywhere they're shipped.

### Log level 4

`LOG_LEVEL=4` logs full request and response message content — system prompts, user messages, assistant replies, reasoning traces. Response text is capped at 32KB per response but otherwise unfiltered.

### Request journal

When `journal.enabled: true`, the journal emits a structured OTel log record per request containing up to **2KB of the system prompt** and **8KB of the last user message**, regardless of log level. These land on stdout (always) and on the configured OTLP endpoint (if set).

### SIGUSR1 message capture

When `sig_message_capture.enabled: true` and `sig_message_capture.output_folder` is set, the operator can send SIGUSR1 to arm a bounded capture window. The next *N* requests are each written as one JSON file containing the full incoming body, the resolved body as sent to the backend, and the response body (or raw SSE stream). Headers like Authorization are redacted; message content is preserved verbatim. Files are written mode 0600 in the configured folder.

This is a deliberate diagnostic feature — typical use is "something is broken, capture 5 requests, `docker cp` them out, inspect, move on." It's disabled by default and never writes without an explicit folder configured. See [docs/logging.md](logging.md#full-body-message-capture-sigusr1) for the full reference.

### Body size cap

Incoming request bodies are capped at `server.max_request_body_mb` (default 50 MB). Requests above the cap receive HTTP 413. This is a resource-exhaustion guard rather than a privacy control.

## Hardened build

For deployments where any of the above features are a concern, build with `-tags hardened`:

```bash
make build-hardened
# produces bin/llm-proxy-hardened

# or directly:
go build -tags hardened -o llm-proxy ./cmd/llm-proxy
```

The hardened tag **compiles out** (not just disables) the following:

| Feature | Debug build | Hardened build |
|---|---|---|
| SIGUSR1 message capture | Available when configured | Compiled out — signal handler not registered, capture package stubbed |
| LOG_LEVEL 3 (body preview) | Active at level 3+ | `Body()` is a no-op; level clamped at 2 |
| LOG_LEVEL 4 (full content) | Active at level 4 | `Content()` is a no-op; level clamped at 2 |
| Journal `system_text` | Up to 2KB per request | Empty |
| Journal `last_user_text` | Up to 8KB per request | Empty |

The structural signals — message counts, char counts, code-fence counts, multimodal flag, routing params — remain in the journal. You keep useful telemetry; prompt content is never recorded.

Everything else is identical: routing, metrics, parameter merge, streaming, backend auth, reload behaviour.

### Runtime identification

Every build prints a startup banner identifying which mode it's running in:

```
[llm-proxy] hardened build (SIGUSR1 capture, log levels 3-4, and journal prompt text are stripped)
```

or, for the default debug build:

```
===============================================================================
  llm-proxy DEBUG BUILD — includes features that can expose prompt contents:
    * SIGUSR1 writes full request/response bodies to disk (when enabled)
    * LOG_LEVEL=3 logs 80 bytes of request bodies
    * LOG_LEVEL=4 logs full request and response message text
    * The request journal records up to 2KB of system + 8KB of last user text

  For production use, build with:  go build -tags hardened ./cmd/llm-proxy
  See docs/security.md for details.
===============================================================================
```

The banner is grep-able so operators can verify the running mode without re-reading config.

## Container hardening

- `FROM scratch` — no shell, no package manager, no libc.
- `USER 65532:65532` — runs as non-root, so a process exploit can't write `/etc` or `/root` (neither exists anyway).
- Pre-created `/capture` owned by that UID so the default capture path works inside the container without host-mount permission games.
- No writable filesystem outside `/capture` (which contains only JSON files) unless the operator mounts additional volumes.

## Dependencies

`.github/dependabot.yml` watches `go.mod`, GitHub Actions, and the Docker base image, opening weekly PRs for updates. OTel and Prometheus client libraries change frequently; merging promptly keeps CVE exposure low.

## Reporting security issues

This is a personal project. For anything sensitive, open a private security advisory on GitHub (**Security → Advisories → New draft**) rather than a public issue.
