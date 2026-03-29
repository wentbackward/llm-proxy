# llm-proxy

[![CI](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/ci.yml)
[![Release](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml/badge.svg)](https://github.com/wentbackward/llm-proxy/actions/workflows/release.yml)
[![Docker](https://img.shields.io/badge/ghcr.io-wentbackward%2Fllm--proxy-blue?logo=docker)](https://github.com/wentbackward/llm-proxy/pkgs/container/llm-proxy)
[![Go Report Card](https://goreportcard.com/badge/github.com/wentbackward/llm-proxy)](https://goreportcard.com/report/github.com/wentbackward/llm-proxy)
[![Go Version](https://img.shields.io/badge/go-1.25+-blue)](https://go.dev)
[![License: MIT](https://img.shields.io/badge/license-MIT-green)](LICENSE)

One endpoint for all your LLMs. Point your client at llm-proxy and let config decide which backend answers.

```yaml
backends:
  - id: anthropic
    type: anthropic
    base_url: https://api.anthropic.com
    api_key: ${ANTHROPIC_API_KEY}

  - id: gemini
    type: openai
    base_url: https://generativelanguage.googleapis.com/v1beta/openai
    api_key: ${GEMINI_API_KEY}

  - id: local
    type: openai
    base_url: http://localhost:11434   # ollama, vLLM, anything OpenAI-compatible

routes:
  - virtual_model: coder
    backend: anthropic
    real_model: claude-sonnet-4-6
    defaults: { temperature: 0.2, enable_thinking: true }

  - virtual_model: researcher
    backend: gemini
    real_model: gemini-2.5-pro
    defaults: { temperature: 0.5, max_tokens: 16384 }

  - virtual_model: fast
    backend: local
    real_model: qwen3.5:latest
    defaults: { temperature: 0.7, enable_thinking: false }

  - virtual_model: auto
    auto_route:
      text: fast
      vision: researcher
```

Your client connects to `http://localhost:4000/v1` and picks a virtual model. The proxy handles the rest — rewrites the model name, sets auth headers, applies parameter profiles, and forwards to the right backend. Switch models in your UI to change behaviour. Swap backends in config to change providers. No code changes, no redeployment.

## Why

- You have accounts with multiple LLM providers and want one API endpoint
- You're running local models (vLLM, ollama) alongside cloud APIs
- You want to control temperature, thinking, and token limits per use-case without touching your app
- You're serving multiple users and want to minimise cloud spend by routing to local models first

## Features

- **Speaks OpenAI and Anthropic natively** — forwards each in its own format, no translation
- **Virtual models** — named personalities over real models with parameter profiles (defaults/clamp)
- **Content-based auto-routing** — text to one model, images to another, more categories coming
- **OpenTelemetry metrics** — TTFT, duration, tokens, active requests; Prometheus by default
- **Request journal** — structured OTel log records for every request, ready for Loki/ClickHouse
- **Zero-copy streaming** — SSE responses flow directly to the client; metrics parsed from the byte stream
- **`enable_thinking` abstraction** — one flag, translated per backend (vLLM, Anthropic, etc.)
- **Hot reload** — `SIGHUP` reloads config, re-probes backends, no restart needed
- **Single static binary** — no runtime, no dependencies; ~7 MB Docker image on `scratch`

## Quick start

```bash
cp config.example.yaml config.yaml
# Edit config.yaml — set your backends and API keys
docker compose up -d
```

Point your client at `http://localhost:4000/v1`. Metrics at `http://localhost:9091/metrics`.

## Documentation

- **[Configuration](docs/configuration.md)** — backends, routes, virtual models, parameter profiles, TLS, environment variables
- **[Logging and diagnostics](docs/logging.md)** — log levels, interaction IDs, request journal, startup probes, SIGHUP reload
- **[Metrics](docs/metrics.md)** — OTel/Prometheus metrics reference
- **[Development](docs/development.md)** — building, testing, project structure

## License

MIT — Copyright (c) 2026 Paul Gresham Advisory LLC
