# Development

## Prerequisites

- Go 1.25+
- Docker (for container builds)

## Building

```bash
go mod tidy
make build        # produces bin/llm-proxy
make test         # runs all tests with race detector
make run          # runs against config.example.yaml
```

## Project structure

```
cmd/llm-proxy/          main.go — startup, signal handling, probes
internal/
  config/               YAML config loader, validation, env expansion
  proxy/                HTTP handlers, reverse proxy, SSE parser, idle timeout
  router/               Virtual model resolution, parameter merge, auto-routing
  logger/               Levelled logging (L0–L4), atomic runtime reload
  journal/              OTel log emitter, message analysis
  telemetry/            OTel metrics with Prometheus exporter
docs/                   Documentation
```

## Testing

```bash
make test             # go test ./... -v -race -count=1
make test-short       # go test ./... -short
make lint             # go vet ./...
```

Tests cover config loading/validation, routing/parameter merge, SSE parsing (OpenAI + Anthropic), message analysis, and logger level thresholds.

## Docker

```bash
make docker-build     # docker build -t llm-proxy .
```

The Dockerfile uses a two-stage build: Go builder on `golang:1.25-alpine`, then copies the static binary to `scratch`. Final image is ~7 MB.

Multi-arch images (linux/amd64, linux/arm64) are built automatically by the release workflow on tag push.

## Release process

1. Commit and push to `main` — CI runs tests
2. Tag a version: `git tag v0.x.y && git push --tags`
3. Release workflow runs tests, builds binaries (linux/darwin/windows × amd64/arm64), creates a GitHub release, and pushes Docker images to `ghcr.io/wentbackward/llm-proxy`
