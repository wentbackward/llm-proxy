# Notes for coding agents

Short reference for agents working in this repo. For full docs see `docs/`.

## Build and test

```
make build     # go build -o bin/llm-proxy ./cmd/llm-proxy
make test      # go test ./... -race -count=1
make lint      # golangci-lint run ./...
make check     # lint + test + build, everything CI runs
```

Run `make check` (not just the package you changed) before declaring work done — tests in `internal/proxy` and `internal/capture` cover cross-package wiring, and `golangci-lint` is wired into CI.

The linter config is tuned for this repo (`.golangci.yml`, v2 schema). Do NOT reintroduce `errcheck.check-type-assertions: true` or `check-blank: true` — the codebase uses `x, _ := m[k].(T)` idiomatically for best-effort JSON parsing, and we've silenced that pattern deliberately.

## Releases ship by tag, NOT by merge

**Pushing to `main` does not release anything.** CI (`ci.yml`) runs tests only.

The Docker image at `ghcr.io/wentbackward/llm-proxy:latest` and the GitHub release only update when a `v*` tag is pushed — that triggers `release.yml`.

After a feature or fix lands on `main`, if the user wants it in prod, you need to tag a release:

```bash
git tag v0.x.y
git push origin v0.x.y
```

Do NOT tag without explicit user confirmation. Never say "the pipeline will pick it up" about a plain push — that's only tests. Be precise: "pushed to main, tests will run" vs "tagged v0.x.y, full release workflow running."

Versioning convention: patch bumps for features and fixes alike in the pre-1.0 series.

## Hardened build

Two build variants gated by the `hardened` Go build tag:

- **Default (`make build`)**: all features, including SIGUSR1 message capture, log levels 3-4, and the prompt text fields in journal entries.
- **Hardened (`make build-hardened`, `go build -tags hardened`)**: above three features are compiled out — not runtime-disabled, removed from the binary. Structural telemetry stays.

When adding new code that touches any of those three feature areas, split across build-tag files using the existing pattern (see `internal/capture/new_debug.go` + `new_hardened.go`, `internal/logger/level_*.go`, `internal/journal/text_*.go`, `cmd/llm-proxy/buildmode_*.go`). The `hardened` file always has the stripped/no-op implementation. Run `make check-hardened` to verify the hardened variant still builds and lints cleanly.

## Protocol rule

The proxy does NOT translate between protocols. Three lanes, each isolated:

- OpenAI (`/v1/chat/completions`, `/v1/completions`, `/v1/embeddings`) → `type: openai` backends only
- Anthropic (`/v1/messages`) → `type: anthropic` backends only
- Ollama native (`/api/chat`, `/api/generate`, `/api/embed`, `/api/embeddings`, `/api/tags`) → `type: ollama` backends only

Do not add bidirectional translation without explicit scope from the user. Ollama's sampling params are nested under `body["options"]`; the proxyRequest pipeline flattens them before router merge and re-nests before send — see `internal/proxy/server.go` for the pattern.

## Logging and debug capture

- `LOG_LEVEL` env var or `server.log_level` in config.yaml (env wins). Levels 0–4.
- `sig_message_capture.enabled: true` + `output_folder` path + SIGUSR1 to arm a bounded capture window of full request/response bodies. No default folder by design — bodies only land where explicitly configured. Never suggest defaulting `output_folder` to `/tmp` or similar.
