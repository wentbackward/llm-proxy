# Configuration

All configuration lives in a single YAML file (default: `config.yaml`). Secrets use `${ENV_VAR}` syntax and are resolved at startup from the environment.

## Server

```yaml
server:
  host: "0.0.0.0"
  port: 4000
  api_key: "${PROXY_API_KEY}"   # clients must send: Authorization: Bearer <key>
  passthrough_unrouted: false    # reject unknown models with 404 (default)
  tls:
    cert: ""                     # path to TLS certificate
    key:  ""                     # path to TLS private key
  transport:
    max_idle_conns: 100          # total idle connections across all backends (default: 100)
    max_idle_conns_per_host: 20  # idle connections per backend (default: 20)
    idle_conn_timeout: 120       # seconds before idle connections are closed (default: 120)
```

- **`passthrough_unrouted`** — when `false` (default), requests for unknown model names are rejected with a 404 that lists available virtual models. When `true`, unknown models are forwarded to the first configured backend as-is.
- **`transport.max_idle_conns`** — total number of idle (keep-alive) connections across all backends. Default: 100.
- **`transport.max_idle_conns_per_host`** — idle connections retained per backend host. Increase this if you have few backends with high concurrency. Default: 20.
- **`transport.idle_conn_timeout`** — seconds an idle connection sits unused before being closed. Default: 120.

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

Each backend is an upstream LLM provider. The `type` determines auth header format and SSE parsing — `openai` for OpenAI-compatible APIs, `anthropic` for Anthropic's Messages API.

**Important:** The proxy does not translate between protocols. A client sending OpenAI format (`/v1/chat/completions`) can only route to an `openai`-type backend. A client sending Anthropic format (`/v1/messages`) can only route to an `anthropic`-type backend. Most clients (OpenWebUI, Continue, Cline, etc.) speak OpenAI format.

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

- **`type`** — `openai` or `anthropic`. Determines auth header format, SSE event parsing, and `enable_thinking` translation. Must match the protocol the client speaks.
- **`base_url`** — scheme + host. Trailing `/v1` is stripped automatically at load time.
- **`api_key`** — static key or OAuth token. Sent to the backend using the auth header format determined by `auth_type`. If empty, the client's original auth headers pass through to the backend.
- **`auth_type`** — `bearer` or `x-api-key`. Controls which HTTP header carries the API key. Default: `bearer` for `openai` backends, `x-api-key` for `anthropic` backends. Override to `bearer` when using OAuth tokens with Anthropic.
- **`timeout_seconds`** — idle timeout per request. If no bytes flow for this duration, the request is cancelled. Default: 300.
- **`max_concurrency`** — maximum number of in-flight requests to this backend. When the limit is reached, new requests queue until a slot opens or the client disconnects. `0` (default) means unlimited.
- **`skip_probe`** — skip the startup `/v1/models` health check. Set `true` for cloud APIs.
- **`ports`** — expand a single backend definition into one backend per port. Use `{port}` as a placeholder in `id` and `base_url`. Accepts a single integer, a YAML list, or a `"lo-hi"` range string (inclusive). All other fields are copied to each expanded backend.

```yaml
  # Generates backends vllm-4000, vllm-4001, vllm-4002
  - id: vllm-{port}
    type: openai
    base_url: "http://127.0.0.1:{port}"
    ports: "4000-4002"
    # also: ports: 4000           # single port
    # also: ports: [4000, 4005]   # explicit list
```

### Authentication

The default auth header format matches each provider's convention:

| Backend type | Default `auth_type` | Header sent |
|---|---|---|
| `openai` | `bearer` | `Authorization: Bearer <key>` |
| `anthropic` | `x-api-key` | `x-api-key: <key>` |

Override `auth_type` when the default doesn't match your provider's expectations:

```yaml
  - id: custom-provider
    type: openai
    base_url: "https://api.example.com"
    api_key: "${PROVIDER_KEY}"
    auth_type: x-api-key           # send key as x-api-key instead of Bearer
```

If `api_key` is empty, the proxy does not set any auth headers, and the client's original `Authorization` or `x-api-key` header passes through to the backend unchanged.

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

## Endpoints

The proxy serves the following endpoints, all using the same reverse-proxy pipeline:

| Endpoint | Protocol | Routes to |
|---|---|---|
| `/v1/chat/completions` | OpenAI | `openai`-type backends |
| `/v1/completions` | OpenAI | `openai`-type backends (code completion / FIM) |
| `/v1/messages` | Anthropic | `anthropic`-type backends |
| `/v1/models` | OpenAI | Lists virtual models (rewrites upstream response) |
| `/health` | — | Health check (unauthenticated) |

Each endpoint forwards requests in the client's format — no protocol translation. A request to `/v1/chat/completions` must route to an `openai`-type backend; `/v1/messages` must route to an `anthropic`-type backend.

`/v1/chat/completions` and `/v1/completions` share the same code path — both get streaming support, SSE parsing, metrics, idle timeout, and logging. The only difference is `/v1/completions` forces the backend path to `/v1/completions` (for base models that support fill-in-the-middle).

## Environment variables

Any `${VAR_NAME}` in the config file is expanded from the environment at load time. Unset variables expand to empty strings.

```yaml
api_key: "${MY_SECRET}"          # resolved from environment
```
