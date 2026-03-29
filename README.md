# llm-proxy

[![CI](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml)
[![Release](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml)
[![Docker](https://img.shields.io/badge/ghcr.io-wentbackward%2Fllm--proxy-blue?logo=docker)](https://github.com/wentbackward/llm-proxy/pkgs/container/llm-proxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/wentbackward/llm-proxy)](https://goreportcard.com/report/github.com/wentbackward/llm-proxy)
[![Go Version](https://img.shields.io/badge/go-1.22+-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A transparent, high-performance reverse proxy for LLM APIs that speaks OpenAI and Anthropic natively.

| 📡 OpenTelemetry | 🎛 Inject Params |
|---|---|
| TTFT, tokens, latency — out of the box, for any client and any backend, with zero code changes | Set defaults, enforce limits, or clamp values per model. Take control of every call without touching your app |

| 🏷️ Virtual Models | ⚙️ Simple Admin |
|---|---|
| Uniquely name the same underlying model with different temperature, thinking budget, and token limits — clients just switch models to change behaviour, on the fly | Swap models or backends for every client at once with a single config change — no redeployments, no coordination |

Written in Go — a single, self-contained binary with no runtime dependencies. Handles high request volume with minimal overhead. Docker images available for Linux, macOS and Windows across amd64 and arm64.

## Features

- **Protocol-aware passthrough** — understands OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) natively; forwards each in its own format with no translation required
- **OpenTelemetry metrics** — TTFT, end-to-end duration, prompt/completion tokens, active requests; exported as Prometheus by default, OTLP-ready
- **Zero-copy streaming** — SSE responses flow directly to the client; the proxy parses metrics from the byte stream without buffering
- **Virtual model aliases** — map friendly names (`gresh-flash`, `claude-sonnet`) to real model IDs on any backend
- **Parameter profiles** — set defaults and clamp values per model; caller params slot in between (`defaults < caller < clamp`)
- **Content-based auto-routing** — `gresh-auto` inspects message content and routes text to a fast model, images/video/documents to a vision model. With no route configured, requests pass through as-is — capability errors (e.g. sending multimodal content to a text-only model) are the backend's to return, not the proxy's to prevent
- **`enable_thinking` abstraction** — a single flag translated to the right backend: `chat_template_kwargs` for vLLM/Qwen3, `thinking` block for Anthropic
- **Config-driven** — all hosts, ports, model names and secrets in one YAML file; `${ENV_VAR}` templating keeps secrets out of config files
- **Single static binary** — no runtime, no dependencies; ~7 MB Docker image built on `scratch`

## Quick start

```bash
cp config.example.yaml config.yaml
# Edit config.yaml — set your backends and API keys
docker compose up -d
```

Point your OpenAI SDK at `http://localhost:4000/v1` or your Anthropic SDK at `http://localhost:4000`. Everything else stays the same.

Metrics are available at `http://localhost:9091/metrics`.

### TLS with Tailscale

If you're running on a Tailscale network, provision a cert for your node and point the proxy at it:

```bash
tailscale cert spark-01.your-tailnet.ts.net
```

Then in `config.yaml`:

```yaml
server:
  api_key: "${PROXY_API_KEY}"
  tls:
    cert: /certs/spark-01.your-tailnet.ts.net.crt
    key:  /certs/spark-01.your-tailnet.ts.net.key
```

Clients then connect to `https://spark-01.your-tailnet.ts.net:4000/v1` with `Authorization: Bearer <key>`.

## Configuration

```yaml
server:
  host: "0.0.0.0"
  port: 4000

telemetry:
  prometheus:
    enabled: true
    port: 9091
    path: /metrics

backends:
  - id: my-vllm
    type: openai                          # openai | anthropic
    base_url: "http://gpu-server:8000/v1"
    api_key: "${VLLM_API_KEY}"            # ${ENV_VAR} expanded at startup
    timeout_seconds: 300

  - id: anthropic
    type: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"

routes:                                   # optional — omit to run as pure proxy
  - virtual_model: my-fast
    backend: my-vllm
    real_model: "Qwen/Qwen2.5-7B-Instruct"
    defaults:
      temperature: 0.7
      enable_thinking: true
    clamp:
      enable_thinking: true               # caller cannot disable this

  - virtual_model: my-auto
    auto_route:
      text: my-fast                       # plain text → fast model
      vision: my-vision                   # images/video/docs → vision model
```

All fields under `server`, `backends`, and `routes` — including ports, model IDs, and URLs — live in this file. Secrets use `${ENV_VAR}` syntax and are never read from the config file directly.

## Virtual models

A virtual model is a named personality layered over a real model. Multiple virtual models can point to the same underlying model with different parameter profiles — clients simply switch models in their UI to change behaviour.

```yaml
routes:
  # Relaxed, creative — high temperature, long replies
  - virtual_model: my-fast
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    defaults:
      temperature: 0.8
      max_tokens: 8192

  # Precise, focused — lower temperature, thinking forced on
  - virtual_model: my-coder
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"        # same model, different behaviour
    defaults:
      temperature: 0.2
      max_tokens: 4096
    clamp:
      enable_thinking: true               # caller cannot disable this
      presence_penalty: 0.0
```

Both models appear in `/v1/models` and are selectable from any client. Switching from `my-fast` to `my-coder` in the UI is all that's needed to test a different parameter set — no API changes, no redeployment, no secret sharing with clients.

Use `context_length` to override what clients see in the model card, preventing them from requesting more context than your GPU can actually serve:

```yaml
  - virtual_model: my-fast
    context_length: 131072               # reported to clients; 0 = pass through from upstream
```

## Logging and diagnostics

At startup the proxy probes every configured backend, logs whether it is reachable, lists the models it reports, and maps each one to its virtual model names:

```
[probe] backend vllm         OK  upstream models: [Qwen/Qwen3.5-9B]
[probe]   → my-fast                          (real: Qwen/Qwen3.5-9B)
[probe]   → my-coder                         (real: Qwen/Qwen3.5-9B)
[probe] backend anthropic    OK  upstream models: []
[probe]   → claude-sonnet                    (real: claude-sonnet-4-6-20251001)
[probe] backend hf-serverless UNREACHABLE: dial tcp ...: connection refused
```

Cloud APIs (Anthropic, OpenAI, HuggingFace) don't expose `/v1/models` — set `skip_probe: true` on those backends to suppress the 404 noise:

```yaml
backends:
  - id: hf-serverless
    type: openai
    base_url: "https://router.huggingface.co/..."
    api_key: "${HF_API_KEY}"
    skip_probe: true
```

Probe output is always printed regardless of log level. Send `SIGHUP` to reload the entire config, log level, and re-probe all backends — no restart needed:

```bash
docker kill --signal=HUP llm-proxy
```

This picks up changes to backends, routes, API keys, and log level. If the new config fails to parse, the old config is kept and an error is logged.

### Log levels

Set `LOG_LEVEL` in the environment (default `0`). `SIGHUP` also reloads this value at runtime.

| Level | What is logged |
|---|---|
| `0` | Errors only (default) |
| `1` | One line per request — method, path, virtual model → real model, backend, status, duration |
| `2` | Level 1 + all incoming request headers |
| `3` | Level 2 + first 80 characters of the request body |

```yaml
# docker-compose.yml
environment:
  LOG_LEVEL: "1"
```

## Metrics reference

| Metric | Type | Labels |
|---|---|---|
| `llm_request_duration_seconds` | Histogram | `backend`, `model`, `status` |
| `llm_time_to_first_token_seconds` | Histogram | `backend`, `model` |
| `llm_prompt_tokens_total` | Counter | `backend`, `model` |
| `llm_completion_tokens_total` | Counter | `backend`, `model` |
| `llm_active_requests` | Gauge | `backend`, `model` |
| `llm_requests_total` | Counter | `backend`, `model`, `status` |
| `llm_generation_tokens_per_second` | Gauge | `backend`, `model` |
| `llm_think_content_ratio` | Histogram | `backend`, `model` |
| `llm_prompt_tokens_per_request` | Histogram | `backend`, `model` |

## Development

```bash
go mod tidy
make test       # runs all tests with race detector
make build      # produces bin/llm-proxy
make run        # runs against config.example.yaml
```

## License

MIT — Copyright (c) 2026 Paul Gresham Advisory LLC
