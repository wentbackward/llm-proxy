# Logging and diagnostics

## Startup probes

At startup the proxy probes every configured backend, logs whether it is reachable, and maps upstream models to their virtual names:

```
[probe] backend vllm         OK  upstream models: [Qwen/Qwen3.5-9B]
[probe]   → my-fast                          (real: Qwen/Qwen3.5-9B)
[probe]   → my-coder                         (real: Qwen/Qwen3.5-9B)
[probe] backend anthropic    OK  upstream models: []
[probe]   → claude-sonnet                    (real: claude-sonnet-4-6-20251001)
[probe] backend hf-serverless UNREACHABLE: dial tcp ...: connection refused
```

Cloud APIs (Anthropic, OpenAI, HuggingFace) don't expose `/v1/models` — set `skip_probe: true` on those backends to suppress the 404 noise.

Probe output is always printed regardless of log level.

## Hot reload

Send `SIGHUP` to reload the entire config, log level, and re-probe all backends without restarting:

```bash
docker kill --signal=HUP llm-proxy
```

If the new config fails to parse, the old config is kept and an error is logged. In-flight requests continue with the old config; new requests pick up the new config.

## Log levels

Set `LOG_LEVEL` in the environment (default `0`). Reloaded on `SIGHUP`.

| Level | What is logged |
|---|---|
| `0` | Errors only (default) |
| `1` | One line per request — method, path, virtual model → real model, backend, status, duration |
| `2` | Level 1 + incoming request headers + transformation summary (backend, target URL, model rewrite, auth type, merged params) |
| `3` | Level 2 + first 80 characters of the request body |
| `4` | Level 3 + full message text content (request and response) |

```yaml
# docker-compose.yml
environment:
  LOG_LEVEL: "1"
```

## Interaction ID

Every request is assigned an 8-character hex interaction ID (e.g. `abc12def`) that appears in:

- All log lines: `[req abc12def] POST /v1/chat/completions model=...`
- The `X-Request-ID` response header

This ties request headers, body, transformation summary, and response together for debugging.

### Example log output at L2

```
[hdr abc12def] POST /v1/chat/completions
[hdr abc12def]   Authorization: Bearer sk-***
[hdr abc12def]   Content-Type: application/json
[hdr abc12def] → backend=local target=http://gpu-server:8000 model=my-fast→Qwen/Qwen3.5-9B auth=bearer params=map[temperature:0.7]
[req abc12def] POST /v1/chat/completions model=my-fast→Qwen/Qwen3.5-9B backend=local status=200 dur=1.234s stream=true
```

### Example log output at L4

```
[msg abc12def] role=system | You are a helpful coding assistant.
[msg abc12def] role=user | Write a function that sorts a list of integers.
[resp abc12def] | def sort_list(nums: list[int]) -> list[int]:
    return sorted(nums)
```

Response text is capped at 32KB to prevent memory issues on very long responses.

## Request journal

The journal emits a structured OTel log record for every proxied request. Each record contains:

- **Routing metadata** — virtual model, real model, backend, protocol
- **Message statistics** — message count, character counts, estimated tokens
- **Structural signals** — tool use, code fences, JSON blocks, multimodal content
- **Parameters** — merged sampling params applied to the request

Records flow through OpenTelemetry:

- **Stdout exporter** (default) — JSONL visible in `docker logs`
- **OTLP exporter** (optional) — sends to an OTel collector for storage in Loki, ClickHouse, etc.

```yaml
journal:
  enabled: true
  otlp_endpoint: ""              # e.g. "http://otel-collector:4318"
```

When `otlp_endpoint` is empty, only stdout. When set, records flow to both stdout and the collector.
