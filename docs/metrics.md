# Metrics reference

All metrics are exported via OpenTelemetry with a Prometheus exporter. Default endpoint: `http://localhost:9091/metrics`.

| Metric | Type | Labels | Description |
|---|---|---|---|
| `llm_request_duration_seconds` | Histogram | `backend`, `model`, `status` | End-to-end request duration |
| `llm_time_to_first_token_seconds` | Histogram | `backend`, `model` | Time from request start to first streamed content token |
| `llm_prompt_tokens_total` | Counter | `backend`, `model` | Cumulative prompt/input tokens |
| `llm_completion_tokens_total` | Counter | `backend`, `model` | Cumulative completion/output tokens |
| `llm_active_requests` | Gauge | `backend`, `model` | Requests currently in flight |
| `llm_requests_total` | Counter | `backend`, `model`, `status` | Total requests by backend, model, and HTTP status |
| `llm_generation_tokens_per_second` | Gauge | `backend`, `model` | Output token generation speed for the last completed request |
| `llm_think_content_ratio` | Histogram | `backend`, `model` | Fraction of response that is thinking/reasoning vs content |
| `llm_prompt_tokens_per_request` | Histogram | `backend`, `model` | Prompt token count per request |

## Labels

- **`backend`** — the backend ID from config (e.g. `my-vllm`, `anthropic`)
- **`model`** — the real model name sent upstream (e.g. `Qwen/Qwen3.5-9B`)
- **`status`** — HTTP status code or `error` for upstream failures

## Histogram buckets

| Metric | Buckets |
|---|---|
| `llm_request_duration_seconds` | 0.5, 1, 2, 5, 10, 30, 60, 120, 300 |
| `llm_time_to_first_token_seconds` | 0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300 |
| `llm_think_content_ratio` | 0.0 – 1.0 in 0.1 increments |
| `llm_prompt_tokens_per_request` | 128, 256, 512, 1K, 2K, 4K, 8K, 16K, 32K, 64K, 128K |

## Grafana

Add a Prometheus data source pointing at `http://hikyaku:9091` (or wherever your metrics endpoint is). All metrics use the `llm_` prefix for easy discovery.
