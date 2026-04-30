# Phase 1 — Affinity fix + Health checking

- [x] Step 1: Config — rename `prefix_bytes` → `max_content_bytes`, `canonical_prefix` → `first_user_message`
- [x] Step 2: Balancer — rewrite `CanonicalPrefix` → `FirstUserMessageKey` (skips system/assistant/tool, hashes first user content)
- [x] Step 3: Balancer — real health checking (per-backend goroutine, HTTP probe, consecutive failure tracking)
- [x] Step 4: Tests — updated affinity_key_test.go, balancer_test.go, config_test.go, lb_test.go
- [x] Step 5: `make check`
