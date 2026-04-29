# hikyaku — Code & Build Pipeline Assessment

**Review Date:** 2026-04-27  
**Reviewer:** Coding Agent  
**Scope:** Full repository audit — architecture, tests, CI/CD, security, code quality  

---

## Repository Overview

An LLM reverse proxy in Go (~25 source files, 6 packages) that sits between clients and backends (OpenAI, Anthropic, Ollama). It provides model aliasing, parameter merging, hot-reloading via SIGHUP, Prometheus metrics, OTel journaling, and a SIGUSR1-based debug capture feature. Two build variants: default (debug) and hardened (strips prompt-exposing features at compile time).

---

## 1. Architecture & Code Quality

### Strengths

**Clear separation of concerns.** Six focused packages:

| Package | Responsibility |
|---|---|
| `config` | YAML loading, env-var expansion, validation, port-range expansion |
| `router` | Model resolution, param merge (defaults → caller → clamp), multimodal detection |
| `proxy` | HTTP handlers, reverse proxy pipeline, streaming interception |
| `capture` | SIGUSR1-armed bounded request/response capture |
| `logger` | Leveled logging (0–4) with runtime switching |
| `journal` | Structured OTel log emission with request analysis |
| `telemetry` | Prometheus/OpenTelemetry metrics initialization |

**Hardened build tag discipline.** Four pairs of build-tag-guarded files properly strip features at compile time (not runtime):

- `capture/new_debug.go` / `new_hardened.go`
- `logger/level_debug.go` / `level_hardened.go`
- `journal/text_debug.go` / `text_hardened.go`
- `cmd/hikyaku/buildmode_debug.go` / `buildmode_hardened.go`

This is the correct approach — `go build -tags hardened` produces a smaller binary with no dead code for those features.

**Atomic config reload.** Uses `atomic.Pointer` for both config and router. In-flight requests use the old config; new requests pick up the new one immediately. Semaphores are rebuilt on reload. Clean.

**Constant-time auth comparison.** `subtle.ConstantTimeCompare` for bearer token matching. Good security hygiene.

**Well-thought-out proxyRequest pipeline.** The ordered transformation sequence (flatten Ollama options → resolve → system_prompt → inject → translateParams → re-nest Ollama) is correctly sequenced and well-commented.

**SSE interception without full buffering.** The `interceptedBody` wrapper feeds bytes through the parser in read-order while streaming directly to the client. Zero-copy for the happy path.

### Concerns

- **`handleOllamaTags` uses `http.DefaultClient`** instead of the shared `s.transport`. All other proxy paths use the configured transport with idle/connection tuning. Minor inconsistency — the tags endpoint gets no connection pooling benefit.

- **`/v1/models` upstream fetch also uses `http.DefaultClient`** (server.go ~line 180). Same issue. These are low-frequency calls so the impact is small, but worth noting for consistency.

- **No graceful shutdown timeout differentiation.** Both proxy and metrics servers share a single 30-second deadline. For long-running streams this is generous, but a hung metrics scrape also waits 30s. Not a blocker, just not differentiated.

- **`detectProtocol` is simplistic.** It only checks the URL path suffix and `anthropic-version` header. An OpenAI client hitting `/v1/messages` would be misidentified. Works today because the route table isolates protocols, but fragile if someone adds a cross-protocol route later.

---

## 2. Test Coverage

### Strengths

**~100 tests across 5 packages**, all passing with `-race`:

| Package | Count | What's Covered |
|---|---|---|
| `capture` | 10 | New, Arm/Reserve lifecycle, concurrent safety, file permissions, CappedBuffer |
| `config` | 30 | Env expansion, validation edge cases, listen policy, port ranges, defaults, system_prompt mutual exclusion |
| `journal` | 20 | Analysis of messages, multimodal, tools, code fences, JSON blocks, truncation |
| `logger` | 10 | Level gating, env vs YAML precedence, nil handling |
| `proxy` | 50+ | Completions, embeddings, streaming SSE, bearer auth, body size caps, capture windows, semaphore concurrency, reload, headers ops, inject deep merge, system prompt mutations, Ollama endpoints |
| `router` | 14 | Param merge layers, multimodal detection, auto-routing, unknown model rejection |

**Race detector enabled** on all tests (`-race -count=1`). The semaphore concurrency test is particularly valuable — it actually verifies the channel-based limiting under contention.

### Gaps

- **`telemetry` has no tests.** The metrics package is thin (mostly constructor boilerplate), but there's no verification that histograms/counters emit the expected labels. Low risk given the simplicity.

- **No integration/E2E test.** Everything uses `httptest.Server` mocks. There's no test that exercises the full `main()` lifecycle (startup → probe → serve → SIGHUP reload → SIGTERM shutdown). Acceptable for a library-like proxy, but an E2E smoke test would catch config-loading regressions.

- **`modifyResponse` error path partially untested.** The `ErrorHandler` on the ReverseProxy is tested indirectly via `TestCompletions_BackendUnreachable`, but the non-streaming `io.ReadAll` error branch in `modifyResponse` (where the upstream body is unreadable mid-stream) isn't explicitly exercised.

- **SSE parser edge cases.** The chunked line delivery test is good, but there's no test for extremely large single-SSE-chunks (multi-MB deltas that could blow the `lineBuf`).

---

## 3. Build Pipeline Assessment

### CI (`ci.yml`)

**Solid dual-lane setup:**

- **test lane:** `go mod tidy` → `go mod download` → `go vet` → `go test -race` → `go build`
- **lint lane:** `golangci-lint v2.11.4` on both default and hardened build tags, plus a standalone hardened build

**Minor gap:** CI doesn't run `make check-hardened` (which combines hardened lint + build). The lint lane does hardened lint separately, and the test lane builds default only. A missed syntax error in a `_hardened.go` file could slip through if it only manifests during a combined hardened build. In practice, the separate `--build-tags hardened` lint action catches this, so the risk is low.

**`go mod tidy` in CI** ensures `go.mod`/`go.sum` drift is caught early. Good.

### Release (`release.yml`)

**Tag-triggered only** (matches the protocol rule in CLAUDE.md). Multi-platform build matrix:

- `linux/amd64`, `linux/arm64`, `darwin/arm64`, `darwin/amd64`
- Windows intentionally excluded (POSIX-only SIGUSR1)

**Docker image** pushed to GHCR with semver + latest tagging, multi-platform (`linux/amd64` + `linux/arm64`), with buildkit caching.

**Good practice:** Test gate before binaries and docker jobs (`needs: test`).

### Dockerfile

**Scratch-based final image.** Correct: `golang:1.25-alpine` builder → `scratch` runtime. Copies CA certs. Runs as non-root UID 65532.

**Issue:** The Dockerfile comment says "Pre-create /capture so the default sig_message_capture.output_folder works" but there is **no default output folder** by design (the CLAUDE.md explicitly states "No default folder by design"). The `/rootfs/capture` directory is created but never referenced as a default anywhere in the code. Dead artifact. Also, the hardcoded UID `65532` is Docker's default non-root user, which is fine, but if someone runs with a different `--user` the directory ownership won't match.

### Dependabot

Weekly updates for `gomod`, `github-actions`, and `docker`. Well-configured with PR limits and labels.

---

## 4. Security Posture

**Strong points:**

- TLS-by-default gateway (refuses plaintext without `allow_plaintext: true`)
- Metrics default to loopback-only (`127.0.0.1`) with explicit opt-in for network binding
- Constant-time bearer token comparison
- Request body size cap (default 50 MB, configurable)
- Capture files written with `0o600` permissions, atomic write-then-rename
- Sensitive headers redacted in capture payloads
- Hardened build strips all prompt-bearing surfaces at compile time

**Areas to watch:**

- Environment variable expansion (`${VAR}`) is eager and unconditional — `${NONEXISTENT}` expands to empty string silently. This is documented behaviour but could lead to subtle misconfiguration (empty API key that "works" until the backend rejects it).
- The `handleOllamaTags` and `/v1/models` upstream fetches don't use the shared transport, so they bypass `DialContext` timeout (10s) and use `http.DefaultClient`'s defaults. Minor.

---

## 5. Summary Verdict

| Dimension | Rating | Notes |
|---|---|---|
| **Architecture** | Strong | Clean package boundaries, atomic reload, correct build-tag pattern |
| **Test Coverage** | Good | ~100 tests, race-detected, solid unit coverage; no E2E |
| **CI/CD** | Solid | Dual lint/test lanes, tag-gated releases, multi-platform Docker |
| **Security** | Mature | Secure defaults, hardened build, proper file permissions |
| **Code Quality** | High | Consistent naming, extensive comments, thoughtful error handling |
| **Documentation** | Good | CLAUDE.md is excellent for engineers; inline docs are thorough |

**Overall:** This is a well-engineered, production-ready proxy. The codebase shows mature Go patterns (atomic pointers for reload, channel-based semaphores, `sync.Once` for cleanup, build tags for feature stripping). The test suite is comprehensive for a service of this size. The few gaps noted are all low-severity and easily addressed.

---

## Items to Address Later

- [ ] Replace `http.DefaultClient` in `handleOllamaTags` and `/v1/models` with the shared transport
- [ ] Clarify/remove the stale `/rootfs/capture` directory creation in the Dockerfile
- [ ] Consider adding a lightweight E2E smoke test covering the full `main()` lifecycle
- [ ] Add basic tests for the `telemetry` package
- [ ] Differentiate graceful shutdown timeouts for proxy vs metrics server (low priority)
- [ ] Consider more robust `detectProtocol` logic if cross-protocol routes are ever added
