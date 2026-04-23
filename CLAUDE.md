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

## Protocol rule

The proxy does NOT translate between OpenAI and Anthropic protocols. A client speaking OpenAI chat completions can only reach `openai`-type backends; Anthropic messages clients can only reach `anthropic`-type backends. Do not add bidirectional translation without explicit scope from the user.

## Logging and debug capture

- `LOG_LEVEL` env var or `server.log_level` in config.yaml (env wins). Levels 0–4.
- `sig_message_capture.enabled: true` + `output_folder` path + SIGUSR1 to arm a bounded capture window of full request/response bodies. No default folder by design — bodies only land where explicitly configured. Never suggest defaulting `output_folder` to `/tmp` or similar.
