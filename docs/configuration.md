# Configuration

All configuration lives in a single YAML file (default: `config.yaml`). Secrets use `${ENV_VAR}` syntax and are resolved at startup from the environment.

## Server

```yaml
server:
  host: "0.0.0.0"
  port: 4000
  api_key: "${PROXY_API_KEY}"   # clients must send: Authorization: Bearer <key>
  tls:
    cert: ""                     # path to TLS certificate
    key:  ""                     # path to TLS private key
```

### TLS with Tailscale

If you're running on a Tailscale network, provision a cert for your node:

```bash
tailscale cert spark-01.your-tailnet.ts.net
```

Then in config:

```yaml
server:
  api_key: "${PROXY_API_KEY}"
  tls:
    cert: /certs/spark-01.your-tailnet.ts.net.crt
    key:  /certs/spark-01.your-tailnet.ts.net.key
```

Clients connect to `https://spark-01.your-tailnet.ts.net:4000/v1`.

## Backends

Each backend is an upstream LLM provider. The proxy supports `openai` and `anthropic` protocol types.

```yaml
backends:
  - id: my-vllm
    type: openai
    base_url: "http://gpu-server:8000"
    api_key: "${VLLM_API_KEY}"
    timeout_seconds: 300          # idle timeout — cancel if no bytes for this long

  - id: anthropic
    type: anthropic
    base_url: "https://api.anthropic.com"
    api_key: "${ANTHROPIC_API_KEY}"
    skip_probe: true              # cloud APIs don't expose /v1/models
```

- **`type`** — `openai` or `anthropic`. Determines auth header format and protocol handling.
- **`base_url`** — scheme + host. Trailing `/v1` is stripped automatically at load time.
- **`timeout_seconds`** — idle timeout per request. If no bytes flow for this duration, the request is cancelled. Default: 300.
- **`skip_probe`** — skip the startup `/v1/models` health check. Set `true` for cloud APIs.

## Routes

Routes map virtual model names to backends with optional parameter profiles. Omit routes entirely to run as a pure transparent proxy.

```yaml
routes:
  - virtual_model: my-fast
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    context_length: 131072        # reported to clients; 0 = pass through
    defaults:
      temperature: 0.7
      max_tokens: 4096
      enable_thinking: true
    clamp:
      enable_thinking: true       # caller cannot override this
```

### Virtual models

A virtual model is a named personality layered over a real model. Multiple virtual models can point to the same underlying model with different parameter profiles:

```yaml
routes:
  - virtual_model: my-creative
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"
    defaults:
      temperature: 0.9
      enable_thinking: false
      max_tokens: 8192

  - virtual_model: my-coder
    backend: my-vllm
    real_model: "Qwen/Qwen3.5-9B"    # same model, different behaviour
    defaults:
      temperature: 0.2
      enable_thinking: true
      max_tokens: 16384
    clamp:
      enable_thinking: true
```

Both appear in `/v1/models`. Switching models in your client UI changes behaviour instantly.

### Parameter merge order

Parameters are applied in three layers: `defaults < caller < clamp`.

1. **Defaults** — applied if the caller doesn't specify a value
2. **Caller** — values from the request body (only sampling keys: `temperature`, `top_p`, `top_k`, `max_tokens`, `presence_penalty`, `frequency_penalty`, `seed`, `stop`, `enable_thinking`, `thinking_budget`)
3. **Clamp** — always wins; caller cannot override these

### Auto-routing

Route based on message content without the client choosing a model:

```yaml
  - virtual_model: my-auto
    auto_route:
      text: my-fast               # plain text messages
      vision: my-vision           # images, video, documents
```

The proxy inspects message content parts. If any non-text content is found (image_url, video, document, file), it routes to the vision target. Otherwise, text.

### `enable_thinking` translation

The proxy translates `enable_thinking` and `thinking_budget` to the correct backend format:

| Backend type | Translation |
|---|---|
| `openai` (vLLM/Qwen) | `chat_template_kwargs.enable_thinking` |
| `anthropic` | `thinking.type: "enabled"` + `thinking.budget_tokens` |

## Telemetry

```yaml
telemetry:
  prometheus:
    enabled: true
    port: 9091
    path: /metrics
```

See [Metrics](metrics.md) for the full metrics reference.

## Journal

```yaml
journal:
  enabled: true
  otlp_endpoint: ""              # optional — e.g. "http://otel-collector:4318"
```

See [Logging](logging.md) for details.

## Environment variables

Any `${VAR_NAME}` in the config file is expanded from the environment at load time. Unset variables expand to empty strings.

```yaml
api_key: "${MY_SECRET}"          # resolved from environment
```
