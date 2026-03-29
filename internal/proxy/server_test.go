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
		w.Header().Set("Content-Type", "application/json")

		if r.URL.Path == "/v1/completions" {
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
		} else {
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
