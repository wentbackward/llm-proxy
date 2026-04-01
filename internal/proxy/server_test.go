package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

// capturedRequest holds what the fake backend received.
type capturedRequest struct {
	Path string
	Body map[string]interface{}
}

// newTestServer creates a Server with a fake backend that captures the
// forwarded request and returns a canned response based on the endpoint hit.
func newTestServer(t *testing.T, capture *capturedRequest) (*Server, *httptest.Server) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		if capture != nil {
			capture.Path = r.URL.Path
			capture.Body = body
		}

		isStreaming, _ := body["stream"].(bool)

		if r.URL.Path == "/v1/completions" {
			if isStreaming {
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Cache-Control", "no-cache")
				w.Header().Set("Connection", "keep-alive")
				flusher, _ := w.(http.Flusher)
				chunks := []string{
					`{"id":"cmpl-test","object":"text_completion","choices":[{"index":0,"text":"completed ","finish_reason":null}]}`,
					`{"id":"cmpl-test","object":"text_completion","choices":[{"index":0,"text":"code here","finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3}}`,
				}
				for _, chunk := range chunks {
					fmt.Fprintf(w, "data: %s\n\n", chunk)
					if flusher != nil {
						flusher.Flush()
					}
				}
				fmt.Fprintf(w, "data: [DONE]\n\n")
				if flusher != nil {
					flusher.Flush()
				}
			} else {
				w.Header().Set("Content-Type", "application/json")
				json.NewEncoder(w).Encode(map[string]interface{}{
					"id":     "cmpl-test",
					"object": "text_completion",
					"model":  "test-model",
					"choices": []interface{}{
						map[string]interface{}{
							"index":         0,
							"finish_reason": "stop",
							"text":          "completed code here",
						},
					},
				})
			}
		} else {
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]interface{}{
				"id":     "chatcmpl-test",
				"object": "chat.completion",
				"model":  "test-model",
				"choices": []interface{}{
					map[string]interface{}{
						"index":         0,
						"finish_reason": "stop",
						"message": map[string]interface{}{
							"role":    "assistant",
							"content": "chat response",
						},
					},
				},
			})
		}
	}))

	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL, TimeoutSeconds: 30},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}

	metrics, _, _ := telemetry.Init()
	s := New(cfg, metrics, nil)

	return s, backend
}

// ── /v1/completions tests ──────────────────────────────────────────────────────

func TestCompletions_ForwardsToCompletionsEndpoint(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":      "test-model",
		"prompt":     "function add(a, b) {",
		"max_tokens": 50,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if captured.Path != "/v1/completions" {
		t.Errorf("should forward to /v1/completions, got %q", captured.Path)
	}
}

func TestCompletions_PromptPassedThrough(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "<fim_prefix>def hello():\n    <fim_suffix>\n    return msg<fim_middle>",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	prompt, _ := captured.Body["prompt"].(string)
	expected := "<fim_prefix>def hello():\n    <fim_suffix>\n    return msg<fim_middle>"
	if prompt != expected {
		t.Errorf("prompt should pass through untouched:\n  got:  %q\n  want: %q", prompt, expected)
	}
}

func TestCompletions_NoFormatTranslation(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "hello world",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Must NOT have messages — prompt should stay as prompt
	if _, hasMessages := captured.Body["messages"]; hasMessages {
		t.Error("should not translate prompt to messages — forward as-is")
	}

	// Must still have prompt
	if _, hasPrompt := captured.Body["prompt"]; !hasPrompt {
		t.Error("prompt field should be preserved in forwarded request")
	}
}

func TestCompletions_PreservesParams(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":       "test-model",
		"prompt":      "test",
		"max_tokens":  100,
		"temperature": 0.5,
		"stop":        []string{"\n"},
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if v, _ := captured.Body["max_tokens"].(float64); v != 100 {
		t.Errorf("max_tokens: got %v, want 100", captured.Body["max_tokens"])
	}
	if v, _ := captured.Body["temperature"].(float64); v != 0.5 {
		t.Errorf("temperature: got %v, want 0.5", captured.Body["temperature"])
	}
}

func TestCompletions_ResolvesModel(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	model, _ := captured.Body["model"].(string)
	if model != "test-model" {
		t.Errorf("model should be resolved: got %q, want %q", model, "test-model")
	}
}

func TestCompletions_MethodNotAllowed(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	req := httptest.NewRequest("GET", "/v1/completions", nil)
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET should return 405, got %d", rec.Code)
	}
}

func TestCompletions_ResponsePassedThrough(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	var resp map[string]interface{}
	json.Unmarshal(rec.Body.Bytes(), &resp)

	if resp["object"] != "text_completion" {
		t.Errorf("object: got %q, want %q", resp["object"], "text_completion")
	}

	choices, ok := resp["choices"].([]interface{})
	if !ok || len(choices) == 0 {
		t.Fatal("response should have choices")
	}

	choice, _ := choices[0].(map[string]interface{})
	text, _ := choice["text"].(string)
	if text != "completed code here" {
		t.Errorf("choice text: got %q, want %q", text, "completed code here")
	}
}

// ── Streaming tests ───────────────────────────────────────────────────────────

func TestCompletions_StreamingSSE(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
		"stream": true,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if captured.Path != "/v1/completions" {
		t.Errorf("streaming should forward to /v1/completions, got %q", captured.Path)
	}

	respBody := rec.Body.String()

	// Must contain SSE data lines
	if !strings.Contains(respBody, "data: ") {
		t.Error("streaming response should contain SSE data lines")
	}

	// Must contain [DONE] terminator
	if !strings.Contains(respBody, "data: [DONE]") {
		t.Error("streaming response should contain [DONE] terminator")
	}

	// Must contain both chunks' content
	if !strings.Contains(respBody, "completed ") {
		t.Error("streaming response should contain first chunk text")
	}
	if !strings.Contains(respBody, "code here") {
		t.Error("streaming response should contain second chunk text")
	}
}

func TestCompletions_StreamingContentType(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
		"stream": true,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "text/event-stream") {
		t.Errorf("streaming Content-Type should be text/event-stream, got %q", ct)
	}
}

func TestCompletions_NonStreamingJSON(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("non-streaming should return 200, got %d", rec.Code)
	}

	ct := rec.Header().Get("Content-Type")
	if !strings.Contains(ct, "application/json") {
		t.Errorf("non-streaming Content-Type should be application/json, got %q", ct)
	}

	var resp map[string]interface{}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("non-streaming response should be valid JSON: %v", err)
	}
}

func TestCompletions_BackendError(t *testing.T) {
	// Backend that always returns 500
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		json.NewEncoder(w).Encode(map[string]interface{}{
			"error": map[string]string{"message": "internal error"},
		})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL, TimeoutSeconds: 30},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New(cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Backend returned 500 — proxy should forward it (not crash or hang)
	if rec.Code != http.StatusInternalServerError {
		t.Errorf("backend 500 should be forwarded, got %d", rec.Code)
	}
}

func TestCompletions_BackendUnreachable(t *testing.T) {
	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: "http://127.0.0.1:1", TimeoutSeconds: 2},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "test", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New(cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if rec.Code != http.StatusBadGateway {
		t.Errorf("unreachable backend should return 502, got %d", rec.Code)
	}
}

func TestCompletions_RequestIDHeader(t *testing.T) {
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	rid := rec.Header().Get("X-Request-ID")
	if rid == "" {
		t.Error("response should have X-Request-ID header")
	}
	if len(rid) != 8 {
		t.Errorf("X-Request-ID should be 8 hex chars, got %q", rid)
	}
}

func TestCompletions_NoContentInjection(t *testing.T) {
	var captured capturedRequest
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test prompt only",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	// Must NOT have messages — proxy must never inject content
	if _, hasMessages := captured.Body["messages"]; hasMessages {
		t.Error("proxy must not inject messages into completions requests")
	}

	// Must NOT have system prompt
	if _, hasSystem := captured.Body["system"]; hasSystem {
		t.Error("proxy must not inject system prompt into completions requests")
	}

	// Prompt must be exactly what was sent
	prompt, _ := captured.Body["prompt"].(string)
	if prompt != "test prompt only" {
		t.Errorf("prompt should be untouched: got %q, want %q", prompt, "test prompt only")
	}

	// Body should only contain expected keys
	for key := range captured.Body {
		switch key {
		case "model", "prompt", "stream_options":
			// expected
		default:
			t.Errorf("unexpected key in forwarded body: %q", key)
		}
	}
}

// ── Transport reuse tests ────────────────────────────────────────────────────

func TestSharedTransport(t *testing.T) {
	// Verify the server's transport is set and used by the reverse proxy.
	// We check indirectly: two requests to the same backend should not panic
	// and should both succeed — confirming the shared transport works.
	s, backend := newTestServer(t, nil)
	defer backend.Close()

	for i := 0; i < 3; i++ {
		body, _ := json.Marshal(map[string]interface{}{
			"model":  "test-model",
			"prompt": "test",
		})
		req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		s.handleCompletions(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("request %d: got status %d, want 200", i, rec.Code)
		}
	}

	if s.transport == nil {
		t.Error("server should have a shared transport")
	}
}

// ── Semaphore tests ──────────────────────────────────────────────────────────

func TestSemaphore_LimitsConcurrency(t *testing.T) {
	// Backend that holds requests until we signal them to complete.
	gate := make(chan struct{})
	var inflight atomic.Int32

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		inflight.Add(1)
		<-gate
		inflight.Add(-1)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "cmpl-test", "object": "text_completion", "model": "test-model",
			"choices": []interface{}{map[string]interface{}{"index": 0, "text": "ok", "finish_reason": "stop"}},
		})
	}))
	defer backend.Close()

	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "limited", Type: "openai", BaseURL: backend.URL, TimeoutSeconds: 30, MaxConcurrency: 2},
		},
		Routes: []config.Route{
			{VirtualModel: "test-model", Backend: "limited", RealModel: "test-model"},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New(cfg, metrics, nil)

	makeReq := func() (*httptest.ResponseRecorder, *http.Request) {
		body, _ := json.Marshal(map[string]interface{}{"model": "test-model", "prompt": "test"})
		req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		return httptest.NewRecorder(), req
	}

	// Launch 4 requests concurrently with max_concurrency=2
	var wg sync.WaitGroup
	recorders := make([]*httptest.ResponseRecorder, 4)
	for i := 0; i < 4; i++ {
		rec, req := makeReq()
		recorders[i] = rec
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handleCompletions(rec, req)
		}()
	}

	// Wait for 2 to be in-flight (the semaphore limit)
	deadline := time.After(2 * time.Second)
	for {
		if inflight.Load() >= 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for 2 in-flight requests, got %d", inflight.Load())
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}

	// Give a moment for any extra requests to sneak through (they shouldn't)
	time.Sleep(50 * time.Millisecond)
	if n := inflight.Load(); n > 2 {
		t.Errorf("in-flight should be capped at 2, got %d", n)
	}

	// Release all and let everything finish
	close(gate)
	wg.Wait()
}

func TestSemaphore_ContextCancellation(t *testing.T) {
	s := &Server{}
	s.semaphores = map[string]chan struct{}{
		"full": make(chan struct{}, 1),
	}
	// Fill the semaphore
	s.semaphores["full"] <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled

	_, ok := s.acquireSemaphore(ctx, "full")
	if ok {
		t.Error("acquireSemaphore should fail when context is cancelled")
	}
}

func TestSemaphore_UnlimitedBackend(t *testing.T) {
	s := &Server{}
	s.semaphores = map[string]chan struct{}{}

	release, ok := s.acquireSemaphore(context.Background(), "no-limit")
	if !ok {
		t.Error("acquireSemaphore should succeed for backends without max_concurrency")
	}
	release() // should not panic
}

func TestSemaphore_ReloadUpdatesSemaphores(t *testing.T) {
	cfg1 := &config.Config{
		Backends: []config.Backend{
			{ID: "b1", Type: "openai", BaseURL: "http://localhost", TimeoutSeconds: 30, MaxConcurrency: 5},
		},
	}
	metrics, _, _ := telemetry.Init()
	s := New(cfg1, metrics, nil)

	s.mu.RLock()
	sem := s.semaphores["b1"]
	s.mu.RUnlock()
	if cap(sem) != 5 {
		t.Errorf("initial semaphore cap: got %d, want 5", cap(sem))
	}

	cfg2 := &config.Config{
		Backends: []config.Backend{
			{ID: "b1", Type: "openai", BaseURL: "http://localhost", TimeoutSeconds: 30, MaxConcurrency: 10},
		},
	}
	s.Reload(cfg2)

	s.mu.RLock()
	sem = s.semaphores["b1"]
	s.mu.RUnlock()
	if cap(sem) != 10 {
		t.Errorf("reloaded semaphore cap: got %d, want 10", cap(sem))
	}
}
