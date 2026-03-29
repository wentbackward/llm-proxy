package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

// newTestServer creates a Server with a fake backend that captures the
// forwarded request body and returns a canned chat completion response.
func newTestServer(t *testing.T, capture *map[string]interface{}) (*Server, *httptest.Server) {
	t.Helper()

	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		json.Unmarshal(raw, &body)
		if capture != nil {
			*capture = body
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{
			"id":      "chatcmpl-test",
			"object":  "chat.completion",
			"model":   "test-model",
			"choices": []interface{}{
				map[string]interface{}{
					"index":         0,
					"finish_reason": "stop",
					"message": map[string]interface{}{
						"role":    "assistant",
						"content": "completed code here",
					},
				},
			},
		})
	}))

	cfg := &config.Config{
		Backends: []config.Backend{
			{ID: "test", Type: "openai", BaseURL: backend.URL},
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

func TestCompletions_TranslatesPromptToMessages(t *testing.T) {
	var captured map[string]interface{}
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

	// Verify the forwarded request has messages, not prompt
	messages, ok := captured["messages"].([]interface{})
	if !ok {
		t.Fatal("forwarded request should have messages array")
	}
	if len(messages) != 1 {
		t.Fatalf("expected exactly 1 message, got %d", len(messages))
	}

	msg, _ := messages[0].(map[string]interface{})
	if msg["role"] != "user" {
		t.Errorf("message role: got %q, want %q", msg["role"], "user")
	}
	if msg["content"] != "function add(a, b) {" {
		t.Errorf("message content: got %q, want original prompt", msg["content"])
	}
}

func TestCompletions_NoInjectedMessages(t *testing.T) {
	var captured map[string]interface{}
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

	messages, _ := captured["messages"].([]interface{})

	// There must be NO system message or any other injected message
	for i, m := range messages {
		msg, _ := m.(map[string]interface{})
		if msg["role"] == "system" {
			t.Errorf("message[%d] has role=system — no messages should be injected by the proxy", i)
		}
	}

	// Only the user's prompt should be present
	if len(messages) != 1 {
		t.Errorf("expected exactly 1 message (the user prompt), got %d", len(messages))
	}
}

func TestCompletions_PromptNotAltered(t *testing.T) {
	var captured map[string]interface{}
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	original := "def fibonacci(n):\n    if n <= 1:\n        return n\n    "
	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": original,
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	messages, _ := captured["messages"].([]interface{})
	msg, _ := messages[0].(map[string]interface{})
	if msg["content"] != original {
		t.Errorf("prompt was altered:\n  got:  %q\n  want: %q", msg["content"], original)
	}
}

func TestCompletions_ArrayPrompt(t *testing.T) {
	var captured map[string]interface{}
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": []string{"first prompt", "second prompt"},
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	messages, _ := captured["messages"].([]interface{})
	msg, _ := messages[0].(map[string]interface{})
	if msg["content"] != "first prompt" {
		t.Errorf("array prompt: got %q, want %q", msg["content"], "first prompt")
	}
}

func TestCompletions_PreservesParams(t *testing.T) {
	var captured map[string]interface{}
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

	if v, _ := captured["max_tokens"].(float64); v != 100 {
		t.Errorf("max_tokens: got %v, want 100", captured["max_tokens"])
	}
	if v, _ := captured["temperature"].(float64); v != 0.5 {
		t.Errorf("temperature: got %v, want 0.5", captured["temperature"])
	}
}

func TestCompletions_ResponseFormat(t *testing.T) {
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

	// Legacy format uses "text", not "message"
	if _, hasMessage := choice["message"]; hasMessage {
		t.Error("legacy response should not have message field")
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

func TestCompletions_PromptFieldRemoved(t *testing.T) {
	var captured map[string]interface{}
	s, backend := newTestServer(t, &captured)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":  "test-model",
		"prompt": "test code",
	})

	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if _, hasPrompt := captured["prompt"]; hasPrompt {
		t.Error("prompt field should be removed from forwarded request")
	}
}
