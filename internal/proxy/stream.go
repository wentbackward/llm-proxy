package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"time"

	"github.com/yourusername/llm-proxy/internal/telemetry"
)

// ── SSE parser ────────────────────────────────────────────────────────────────

// sseParser extracts telemetry from a streaming SSE response without buffering
// the full body. It is fed raw bytes as they arrive and maintains minimal state.
type sseParser struct {
	backendID   string
	model       string
	backendType string // "openai" | "anthropic"
	t0          time.Time
	metrics     *telemetry.Metrics
	ctx         context.Context

	ttftDone bool
	lineBuf  []byte // holds a partial line between Read calls

	// Anthropic accumulates token counts across events
	anthropicInputTokens  int64
	anthropicOutputTokens int64
}

func newSSEParser(backendID, model, backendType string, t0 time.Time, m *telemetry.Metrics, ctx context.Context) *sseParser {
	return &sseParser{
		backendID:   backendID,
		model:       model,
		backendType: backendType,
		t0:          t0,
		metrics:     m,
		ctx:         ctx,
	}
}

// feed processes a raw chunk of bytes from the upstream SSE stream.
// It must be called in the order bytes arrive.
func (p *sseParser) feed(data []byte) {
	// Prepend any leftover from the previous call
	if len(p.lineBuf) > 0 {
		data = append(p.lineBuf, data...)
		p.lineBuf = nil
	}

	for {
		idx := bytes.IndexByte(data, '\n')
		if idx == -1 {
			// Incomplete line — save for next call
			if len(data) > 0 {
				p.lineBuf = append([]byte(nil), data...)
			}
			return
		}
		line := bytes.TrimRight(data[:idx], "\r")
		data = data[idx+1:]

		if !bytes.HasPrefix(line, []byte("data: ")) {
			continue
		}
		payload := line[6:]
		if bytes.Equal(payload, []byte("[DONE]")) {
			if p.backendType == "anthropic" {
				p.recordAnthropicTokens()
			}
			continue
		}

		var evt map[string]interface{}
		if err := json.Unmarshal(payload, &evt); err != nil {
			continue
		}

		if p.backendType == "anthropic" {
			p.handleAnthropicEvent(evt)
		} else {
			p.handleOpenAIEvent(evt)
		}
	}
}

func (p *sseParser) handleOpenAIEvent(evt map[string]interface{}) {
	// TTFT: first chunk with non-empty delta content
	if !p.ttftDone {
		if content := firstDeltaContent(evt); content != "" {
			elapsed := time.Since(p.t0).Seconds()
			p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
			p.ttftDone = true
		}
	}

	// Usage arrives in the final chunk when stream_options.include_usage=true
	if usage, ok := evt["usage"].(map[string]interface{}); ok {
		attrs := telemetry.BackendAttrs(p.backendID, p.model)
		if v, _ := usage["prompt_tokens"].(float64); v > 0 {
			p.metrics.PromptTokens.Add(p.ctx, int64(v), attrs)
		}
		if v, _ := usage["completion_tokens"].(float64); v > 0 {
			p.metrics.CompletionTokens.Add(p.ctx, int64(v), attrs)
		}
	}
}

func (p *sseParser) handleAnthropicEvent(evt map[string]interface{}) {
	switch evt["type"] {
	case "message_start":
		// input_tokens arrive here
		if msg, ok := evt["message"].(map[string]interface{}); ok {
			if usage, ok := msg["usage"].(map[string]interface{}); ok {
				if v, _ := usage["input_tokens"].(float64); v > 0 {
					p.anthropicInputTokens = int64(v)
				}
			}
		}

	case "content_block_delta":
		// TTFT on first thinking or text delta
		if !p.ttftDone {
			if delta, ok := evt["delta"].(map[string]interface{}); ok {
				dtype, _ := delta["type"].(string)
				if dtype == "text_delta" || dtype == "thinking_delta" {
					elapsed := time.Since(p.t0).Seconds()
					p.metrics.TTFT.Record(p.ctx, elapsed, telemetry.BackendAttrs(p.backendID, p.model))
					p.ttftDone = true
				}
			}
		}

	case "message_delta":
		// output_tokens arrive here
		if usage, ok := evt["usage"].(map[string]interface{}); ok {
			if v, _ := usage["output_tokens"].(float64); v > 0 {
				p.anthropicOutputTokens = int64(v)
			}
		}

	case "message_stop":
		p.recordAnthropicTokens()
	}
}

func (p *sseParser) recordAnthropicTokens() {
	attrs := telemetry.BackendAttrs(p.backendID, p.model)
	if p.anthropicInputTokens > 0 {
		p.metrics.PromptTokens.Add(p.ctx, p.anthropicInputTokens, attrs)
		p.anthropicInputTokens = 0
	}
	if p.anthropicOutputTokens > 0 {
		p.metrics.CompletionTokens.Add(p.ctx, p.anthropicOutputTokens, attrs)
		p.anthropicOutputTokens = 0
	}
}

func firstDeltaContent(evt map[string]interface{}) string {
	choices, _ := evt["choices"].([]interface{})
	for _, c := range choices {
		ch, _ := c.(map[string]interface{})
		delta, _ := ch["delta"].(map[string]interface{})
		if s, _ := delta["content"].(string); s != "" {
			return s
		}
	}
	return ""
}

// ── interceptedBody ───────────────────────────────────────────────────────────

// interceptedBody wraps an http.Response body, feeding bytes through the SSE
// parser as they are read by the HTTP server writing to the client. Zero copy —
// the bytes still flow directly to the client.
type interceptedBody struct {
	io.ReadCloser
	parser  *sseParser
	onClose func()
	closed  bool
}

func (b *interceptedBody) Read(p []byte) (n int, err error) {
	n, err = b.ReadCloser.Read(p)
	if n > 0 {
		b.parser.feed(p[:n])
	}
	return
}

func (b *interceptedBody) Close() error {
	if !b.closed {
		b.closed = true
		if b.onClose != nil {
			b.onClose()
		}
	}
	return b.ReadCloser.Close()
}
