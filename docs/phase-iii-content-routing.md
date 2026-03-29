# Phase III — Content-based routing

## Problem

Route requests to different models based on the nature of the task — coding, creative writing, planning, tool use — without the client needing to pick the right model.

## Mid-stream re-routing

You can't re-route mid-stream. Once tokens are flowing, the request is committed to one backend. In practice, the input context contains enough signal for accurate routing before generation starts. For the rare mid-stream pivot (model decides to write code during a planning task), two established patterns handle it:

- **Tool calling**: the model on the fast path calls a `run_code` tool, which internally routes to a coding model. The proxy doesn't re-route; the model delegates.
- **Multi-turn re-routing**: each conversation turn is routed independently. Turn 1 goes to the planner; turn 2 (now containing code context) goes to the coder.

## Tier 1 — Heuristic routing (zero added latency)

Pattern match on the request body before forwarding. No model involved. This extends the existing `auto_route` from two categories (text/vision) to N categories.

Reliable signals:

| Signal | Source | Routes to |
|---|---|---|
| `tools` or `tool_choice` in request body | JSON field | tool-capable model |
| Code fences, file paths, language keywords | Last user message | coding model |
| System prompt keywords ("code assistant", "developer") | System message | coding model |
| "plan", "design", "architect", "strategy" | Last user message | planning model |
| Image/video/document attachments | Message content | vision model |
| None of the above | — | default/fast model |

### Config

```yaml
- virtual_model: gresh-auto
  auto_route:
    vision: gresh-vision       # images/video/documents
    coding: gresh-coder        # code fences, file paths, tools present
    planning: gresh-agent      # long-form reasoning, design, architecture
    default: gresh-fast        # fallback for everything else
```

### Implementation

Extend `router.go` to inspect message content when `auto_route` has more than `text`/`vision` keys. The classification function checks signals in priority order and returns the first match. All pattern matching happens in-process — no network calls, no added latency.

## Tier 2 — Local classifier (sub-100ms)

A small model (fine-tuned DistilBERT or similar) that takes the system prompt + last user message and returns a category. Runs on CPU alongside the proxy, no GPU needed. Fixed vocabulary: `creative`, `coding`, `planning`, `tool_use`, `general`.

This could be an optional sidecar or compiled into the binary via ONNX Runtime. Config would reference a `classifier` endpoint:

```yaml
- virtual_model: gresh-auto
  auto_route:
    classifier: http://localhost:8090/classify
    coding: gresh-coder
    planning: gresh-agent
    creative: gresh-fast
    default: gresh-fast
```

The proxy POSTs `{system_prompt, last_message}` to the classifier, gets back a category string, and routes accordingly. Timeout on the classifier should be aggressive (100ms) with fallback to `default`.

## Tier 3 — LLM triage (200-500ms)

Send a condensed prompt to the fastest available model:

> Classify this request as one of: coding, creative, planning, tool_use, general.
> System: {first 100 chars of system prompt}
> User: {first 200 chars of last message}
> Category:

Most accurate but adds latency to every request. Only worth it if the cost difference between models is significant (e.g. routing between a cheap local model and an expensive cloud API).

Config:

```yaml
- virtual_model: gresh-auto
  auto_route:
    classifier_model: gresh-fast   # use this model to classify
    classifier_budget_ms: 500      # max latency for classification
    coding: gresh-coder
    planning: gresh-agent
    default: gresh-fast
```

## Recommendation

Start with Tier 1. It covers the common cases, adds zero latency, and builds on the existing `auto_route` mechanism. Tier 2/3 can be layered on later if heuristic accuracy isn't sufficient.

The mid-stream pivot is a non-problem in practice — let the assigned model handle it. The input context is almost always sufficient for correct routing.
