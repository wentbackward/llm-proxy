# llm-proxy

[![CI](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml)
[![Release](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml)
[![Docker](https://img.shields.io/badge/ghcr.io-wentbackward%2Fllm--proxy-blue?logo=docker)](https://github.com/wentbackward/llm-proxy/pkgs/container/llm-proxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/wentbackward/llm-proxy)](https://goreportcard.com/report/github.com/wentbackward/llm-proxy)
[![Go Version](https://img.shields.io/badge/go-1.25+-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

Virtualize models from any provider — local or cloud — and switch between them in your client UI.

```
client → llm-proxy:4000/v1 → vLLM (local)
                             → OpenAI (cloud)
                             → HuggingFace (cloud)
                             → any OpenAI-compatible API
```

## What it does

**Unify your backends.** Point your client at one URL. The proxy forwards requests transparently — model resolution, auth headers, and parameter profiles are applied, but the request format is never translated. Supports `/v1/chat/completions` and `/v1/completions` (code completion / FIM).

**Virtual models.** Name the same underlying model multiple times with different parameter profiles. A `coder` with low temperature and thinking enabled, a `creative` with high temperature and thinking off — same model, different behaviour. Clients just switch the model name.

```yaml
routes:
  - virtual_model: coder
    backend: local
    real_model: "Qwen/Qwen3.5-35B-A3B-FP8"
    defaults: { temperature: 0.2, enable_thinking: true, max_tokens: 16384 }
    clamp: { enable_thinking: true }

  - virtual_model: creative
    backend: local
    real_model: "Qwen/Qwen3.5-35B-A3B-FP8"
    defaults: { temperature: 0.9, enable_thinking: false, max_tokens: 8192 }
```

**Parameter control.** Three-layer merge: `defaults < caller < clamp`. Set sensible defaults, let callers override what you allow, lock down what they can't.

**Observability.** OpenTelemetry metrics out of the box — TTFT, request duration, token counts, active requests, generation speed. Prometheus exporter, ready for Grafana. Request journal logs structured data about every request for analysis.

**Zero overhead.** SSE streams flow directly to the client. Metrics are parsed from the byte stream without buffering. Single static Go binary, ~7 MB Docker image on `scratch`.

## Quick start

```bash
cp config.example.yaml config.yaml
# Edit config.yaml — set your backends and API keys
docker compose up -d
```

Point your client at `http://localhost:4000/v1`. Metrics at `http://localhost:9091/metrics`.

## Configuration

```yaml
backends:
  - id: local
    type: openai
    base_url: "http://gpu-server:8000"
    timeout_seconds: 300

  - id: hf
    type: openai
    base_url: "https://router.huggingface.co"
    api_key: "${HF_TOKEN}"
    skip_probe: true
```

Secrets use `${ENV_VAR}` syntax — resolved at startup, never stored in config. Hot-reload with `SIGHUP` — config, log level, and backend probes update without restart.

See the [full configuration reference](docs/configuration.md) for details on auth types, TLS, auto-routing, and parameter profiles.

## Documentation

- **[Configuration](docs/configuration.md)** — backends, routes, virtual models, parameter profiles, TLS
- **[Logging and diagnostics](docs/logging.md)** — log levels, interaction IDs, request journal, hot reload
- **[Metrics](docs/metrics.md)** — OTel/Prometheus metrics reference
- **[Development](docs/development.md)** — building, testing, project structure

## License

MIT — Copyright (c) 2026 Paul Gresham Advisory LLC
