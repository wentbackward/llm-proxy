# llm-proxy

[![CI](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml)
[![Go Report Card](https://goreportcard.com/badge/github.com/wentbackward/llm-proxy)](https://goreportcard.com/report/github.com/wentbackward/llm-proxy)
[![Go Version](https://img.shields.io/badge/go-1.22+-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

A transparent, high-performance reverse proxy for LLM APIs that speaks OpenAI and Anthropic natively. Drop it between your app and any LLM provider to get structured observability — TTFT, token counts, request duration — with zero changes to your application code.

Route all your clients through a single proxy and swap the underlying model for everyone with one config change — no redeployments, no client updates, no coordination.

## Features

- **Protocol-aware passthrough** — understands OpenAI (`/v1/chat/completions`) and Anthropic (`/v1/messages`) natively; forwards each in its own format with no translation required
- **OpenTelemetry metrics** — TTFT, end-to-end duration, prompt/completion tokens, active requests; exported as Prometheus by default, OTLP-ready
- **Zero-copy streaming** — SSE responses flow directly to the client; the proxy parses metrics from the byte stream without buffering
- **Virtual model aliases** — map friendly names (`gresh-flash`, `claude-sonnet`) to real model IDs on any backend
- **Parameter profiles** — set defaults and hard locks per model; caller params slot in between (`defaults < caller < locked`)
- **Content-based auto-routing** — `gresh-auto` inspects message content and routes text to a fast model, images/video/documents to a vision model
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
    locked:
      enable_thinking: true               # caller cannot disable this

  - virtual_model: my-auto
    auto_route:
      text: my-fast                       # plain text → fast model
      vision: my-vision                   # images/video/docs → vision model
```

All fields under `server`, `backends`, and `routes` — including ports, model IDs, and URLs — live in this file. Secrets use `${ENV_VAR}` syntax and are never read from the config file directly.

## Metrics reference

| Metric | Type | Labels |
|---|---|---|
| `llm_request_duration_seconds` | Histogram | `backend`, `model`, `status` |
| `llm_time_to_first_token_seconds` | Histogram | `backend`, `model` |
| `llm_prompt_tokens_total` | Counter | `backend`, `model` |
| `llm_completion_tokens_total` | Counter | `backend`, `model` |
| `llm_active_requests` | Gauge | `backend`, `model` |
| `llm_requests_total` | Counter | `backend`, `model`, `status` |

## Development

```bash
go mod tidy
make test       # runs all tests with race detector
make build      # produces bin/llm-proxy
make run        # runs against config.example.yaml
```

## License

MIT — Copyright (c) 2026 Paul Gresham Advisory LLC
