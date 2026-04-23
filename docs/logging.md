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

Set in `config.yaml` under `server.log_level`, or via the `LOG_LEVEL` environment variable. Default is `0`. Reloaded on `SIGHUP`.

| Level | What is logged |
|---|---|
| `0` | Errors only (default) |
| `1` | One line per request — method, path, virtual model → real model, backend, status, duration |
| `2` | Level 1 + incoming request headers + transformation summary (backend, target URL, model rewrite, auth type, merged params) |
| `3` | Level 2 + first 80 characters of the request body |
| `4` | Level 3 + full message text content (request and response) |

```yaml
# config.yaml
server:
  log_level: 2
```

```yaml
# docker-compose.yml — env wins over config when both are set, useful for ops overrides
environment:
  LOG_LEVEL: "1"
```

The startup line `[logger] log level N (config|env|default)` shows which source won.

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

## Full-body message capture (SIGUSR1)

The journal records metadata (character counts, structural signals, first 8KB of the last user message) — good for observability, not enough when you need to inspect the *exact* bytes being sent upstream. For that, the proxy has a SIGUSR1-armed capture mode that dumps full request and response bodies to disk for a bounded number of requests.

Typical uses:

- **Prompt cache debugging** — diff the `resolved` body across consecutive requests to confirm the tool list and system prompt are byte-stable.
- **Context-size investigations** — see exactly what the 24K characters are, not just the count.
- **Response shape verification** — inspect MTP/thinking output fields that the metrics pipeline collapses.

### Configuration

```yaml
sig_message_capture:
  enabled: false             # off by default
  output_folder: ""          # REQUIRED when enabled — no default location
  max_messages: 5            # capture window size (default 5)
```

Both `enabled: true` and a non-empty `output_folder` are required for the feature to activate. An unset `output_folder` with `enabled: true` is treated as disabled rather than defaulting to a directory — bodies only land where you explicitly send them.

### Arming a capture window

Send `SIGUSR1` to the proxy process:

```bash
docker kill --signal=USR1 llm-proxy
```

The next `max_messages` proxied requests are each written as one JSON file to the output folder, then the window closes automatically. Send `SIGUSR1` again to re-arm. If the feature is disabled in config, the signal is logged and ignored.

### Output format

One file per request: `{YYYYMMDDTHHMMSS.sssZ}-{request_id}.json`, mode `0600`.

```json
{
  "request_id": "abc12345",
  "timestamp": "2026-04-20T18:30:45.123Z",
  "request": {
    "method": "POST",
    "path": "/v1/chat/completions",
    "virtual_model": "gresh-general",
    "real_model": "Qwen/Qwen3.5-35B",
    "backend": "vllm",
    "protocol": "openai",
    "streaming": true,
    "headers": { "Authorization": "[redacted]", "Content-Type": "application/json" },
    "incoming": { "model": "gresh-general", "messages": [ ... ], "tools": [ ... ] },
    "resolved": { "model": "Qwen/Qwen3.5-35B", "messages": [ ... ], "tools": [ ... ], "temperature": 0.7 }
  },
  "response": {
    "status_code": 200,
    "sse": "data: {...}\n\ndata: [DONE]\n\n"
  },
  "timing": { "started_at": "2026-04-20T18:30:45.123Z", "duration_ms": 1234.5 }
}
```

- `incoming` is the body as received from the client.
- `resolved` is the body as sent to the backend (after defaults, caller overrides, clamp, and protocol-specific translation).
- Streaming responses are captured as raw SSE bytes under `response.sse`; non-streaming responses are captured under `response.body`.
- Per-response capture is capped at 5 MB; over-cap bodies set `response.truncated: true`.
- `Authorization`, `x-api-key`, `Cookie`, `Set-Cookie`, and `Proxy-Authorization` headers are redacted.

### Notes

- Errors (semaphore rejection, upstream failures) also write a capture file with `response.error` set — the slot isn't wasted on failed requests.
- `SIGHUP` rebuilds the capture handle from config. An armed window is cleared by reload.
- The feature is deliberately explicit: no env var, no default folder, logs the capture location at startup so its presence is visible in `docker logs`.
