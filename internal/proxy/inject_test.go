package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

// newRouteServer wires a real config (loaded via config.Load so route maps
// populate) pointing at a fake backend that records what it received.
// routeBlock is the raw YAML for the route, indented appropriately.
func newRouteServer(t *testing.T, capture *capturedRequest, backendType, routeBlock string) (srv *Server, backend *httptest.Server) {
	t.Helper()

	backend = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]interface{}
		_ = json.Unmarshal(raw, &body)
		if capture != nil {
			capture.Path = r.URL.Path
			capture.Body = body
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"id": "x", "object": "chat.completion",
			"choices": []interface{}{map[string]interface{}{
				"index": 0, "finish_reason": "stop",
				"message": map[string]interface{}{"role": "assistant", "content": "ok"},
			}},
		})
	}))

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: %s
    base_url: "%s/v1"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m%s
`, backendType, backend.URL, routeBlock)

	cfg, err := config.Load(writeTestConfig(t, yaml))
	if err != nil {
		backend.Close()
		t.Fatalf("config load: %v", err)
	}
	metrics, _, _ := telemetry.Init()
	srv = New("test", "inspect", cfg, metrics, nil)
	return srv, backend
}

func sendInjectChat(_ *Server, virtualModel string, extraBody map[string]interface{}) ([]byte, *http.Request) {
	body := map[string]interface{}{
		"model":    virtualModel,
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	}
	for k, v := range extraBody {
		body[k] = v
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	return raw, req
}

// ── system_prompt tests ──────────────────────────────────────────────────────

func TestSystemPrompt_PrependCreatesSystemMessage(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    system_prompt:
      prepend: "<|think|>\n"`)
	defer backend.Close()

	_, req := sendInjectChat(s, "m", nil)
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	msgs, _ := captured.Body["messages"].([]interface{})
	if len(msgs) != 2 {
		t.Fatalf("expected 2 messages (system + user), got %d", len(msgs))
	}
	sys, _ := msgs[0].(map[string]interface{})
	if r, _ := sys["role"].(string); r != "system" {
		t.Errorf("first message role: got %q, want system", r)
	}
	if c, _ := sys["content"].(string); c != "<|think|>\n" {
		t.Errorf("system content: got %q", c)
	}
}

func TestSystemPrompt_PrependExistingSystemMessage(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    system_prompt:
      prepend: "<|think|>\n"`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	msgs, _ := captured.Body["messages"].([]interface{})
	sys, _ := msgs[0].(map[string]interface{})
	got, _ := sys["content"].(string)
	if got != "<|think|>\nYou are helpful." {
		t.Errorf("prepended content: got %q, want %q", got, "<|think|>\nYou are helpful.")
	}
}

func TestSystemPrompt_AppendOpenAI(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    system_prompt:
      append: "\n\nReply tersely."`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	msgs, _ := captured.Body["messages"].([]interface{})
	sys, _ := msgs[0].(map[string]interface{})
	if got, _ := sys["content"].(string); got != "You are helpful.\n\nReply tersely." {
		t.Errorf("appended content: got %q", got)
	}
}

func TestSystemPrompt_ReplaceOpenAI(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    system_prompt:
      replace: "Strict mode: reply only with code."`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "hi"},
		},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	msgs, _ := captured.Body["messages"].([]interface{})
	sys, _ := msgs[0].(map[string]interface{})
	if got, _ := sys["content"].(string); got != "Strict mode: reply only with code." {
		t.Errorf("replaced content: got %q", got)
	}
}

func TestSystemPrompt_AnthropicStringSystem(t *testing.T) {
	// Anthropic body.system is a top-level string. Use the ollama test
	// fake-server pattern but with a /v1/messages-shaped backend.
	var captured capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.Body)
		captured.Path = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"content": []interface{}{map[string]interface{}{"type": "text", "text": "ok"}},
		})
	}))
	defer backend.Close()

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: anthropic
    base_url: "%s/v1"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
    system_prompt:
      prepend: "[guard]\n"
`, backend.URL)
	cfg, _ := config.Load(writeTestConfig(t, yaml))
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m",
		"system":   "You are helpful.",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := captured.Body["system"].(string); got != "[guard]\nYou are helpful." {
		t.Errorf("anthropic system prepended: got %q", got)
	}
}

func TestSystemPrompt_AnthropicArraySystem(t *testing.T) {
	var captured capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"content": []interface{}{map[string]interface{}{"type": "text", "text": "ok"}},
		})
	}))
	defer backend.Close()

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: anthropic
    base_url: "%s/v1"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
    system_prompt:
      append: "ALWAYS BE TERSE."
`, backend.URL)
	cfg, _ := config.Load(writeTestConfig(t, yaml))
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{
		"model": "m",
		"system": []interface{}{
			map[string]interface{}{"type": "text", "text": "Block A."},
		},
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	sys, ok := captured.Body["system"].([]interface{})
	if !ok {
		t.Fatalf("anthropic system should remain array, got %T", captured.Body["system"])
	}
	if len(sys) != 2 {
		t.Fatalf("expected 2 blocks (original + appended), got %d", len(sys))
	}
	tail, _ := sys[1].(map[string]interface{})
	if got, _ := tail["text"].(string); got != "ALWAYS BE TERSE." {
		t.Errorf("appended block text: got %q", got)
	}
}

func TestSystemPrompt_SkippedOnCompletionsEndpoint(t *testing.T) {
	// /v1/completions has no messages — system_prompt op should be a no-op.
	var captured capturedRequest
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &captured.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "text_completion", "choices": []interface{}{}})
	}))
	defer backend.Close()

	yaml := fmt.Sprintf(`
server:
  allow_plaintext: true
backends:
  - id: be
    type: openai
    base_url: "%s/v1"
    timeout_seconds: 30
routes:
  - virtual_model: m
    backend: be
    real_model: real-m
    system_prompt:
      prepend: "should not appear"
`, backend.URL)
	cfg, _ := config.Load(writeTestConfig(t, yaml))
	metrics, _, _ := telemetry.Init()
	s := New("test", "inspect", cfg, metrics, nil)

	body, _ := json.Marshal(map[string]interface{}{"model": "m", "prompt": "hello"})
	req := httptest.NewRequest("POST", "/v1/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleCompletions(rec, req)

	if _, hasMsgs := captured.Body["messages"]; hasMsgs {
		t.Error("completions request should not have messages injected")
	}
	if got, _ := captured.Body["prompt"].(string); !strings.HasPrefix(got, "hello") {
		t.Errorf("prompt should be unmodified, got %q", got)
	}
}

// ── inject tests ─────────────────────────────────────────────────────────────

func TestInject_TopLevelKeyMerged(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    inject:
      reasoning_effort: "high"`)
	defer backend.Close()

	_, req := sendInjectChat(s, "m", nil)
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	if got, _ := captured.Body["reasoning_effort"].(string); got != "high" {
		t.Errorf("reasoning_effort: got %v, want \"high\"", captured.Body["reasoning_effort"])
	}
}

func TestInject_DeepMergeNestedMap(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    inject:
      chat_template_kwargs:
        preserve_thinking: true
        thinking_mode: "deep"`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		// Caller already supplied one chat_template_kwarg; inject must merge.
		"chat_template_kwargs": map[string]interface{}{"caller_key": true},
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	kw, ok := captured.Body["chat_template_kwargs"].(map[string]interface{})
	if !ok {
		t.Fatalf("chat_template_kwargs should be a map, got %T", captured.Body["chat_template_kwargs"])
	}
	if v, _ := kw["preserve_thinking"].(bool); !v {
		t.Error("inject's preserve_thinking should be present")
	}
	if v, _ := kw["thinking_mode"].(string); v != "deep" {
		t.Errorf("inject's thinking_mode: got %q", v)
	}
	if v, _ := kw["caller_key"].(bool); !v {
		t.Error("caller's chat_template_kwargs.caller_key should survive merge")
	}
}

func TestInject_RouteWinsOnConflict(t *testing.T) {
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    inject:
      chat_template_kwargs:
        preserve_thinking: true`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":                "m",
		"messages":             []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
		"chat_template_kwargs": map[string]interface{}{"preserve_thinking": false}, // caller says false
	})
	req := httptest.NewRequest("POST", "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	kw, _ := captured.Body["chat_template_kwargs"].(map[string]interface{})
	if v, _ := kw["preserve_thinking"].(bool); !v {
		t.Errorf("route's inject must win on key conflict; got preserve_thinking=%v", v)
	}
}

func TestInject_ComposesWithEnableThinkingTranslation(t *testing.T) {
	// Route sets defaults.enable_thinking AND inject.chat_template_kwargs.preserve_thinking.
	// Both must end up under chat_template_kwargs at the backend.
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "openai", `
    defaults:
      enable_thinking: true
    inject:
      chat_template_kwargs:
        preserve_thinking: true`)
	defer backend.Close()

	_, req := sendInjectChat(s, "m", nil)
	rec := httptest.NewRecorder()
	s.handleProxy(rec, req)

	kw, ok := captured.Body["chat_template_kwargs"].(map[string]interface{})
	if !ok {
		t.Fatalf("chat_template_kwargs should be a map, got %T", captured.Body["chat_template_kwargs"])
	}
	if v, _ := kw["enable_thinking"].(bool); !v {
		t.Error("translateParams should add enable_thinking under chat_template_kwargs")
	}
	if v, _ := kw["preserve_thinking"].(bool); !v {
		t.Error("inject's preserve_thinking should coexist with enable_thinking")
	}
}

func TestInject_OllamaRoutesIntoOptions(t *testing.T) {
	// Ollama nests sampling params under body.options. Inject of an unknown
	// top-level key for the route should land under options if the proxy's
	// re-nesting picks it up.
	var captured capturedRequest
	s, backend := newRouteServer(t, &captured, "ollama", `
    inject:
      mirostat: 2`)
	defer backend.Close()

	body, _ := json.Marshal(map[string]interface{}{
		"model":    "m",
		"messages": []interface{}{map[string]interface{}{"role": "user", "content": "hi"}},
	})
	req := httptest.NewRequest("POST", "/api/chat", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.handleOllamaChat(rec, req)

	opts, ok := captured.Body["options"].(map[string]interface{})
	if !ok {
		t.Fatalf("body.options expected map, got %T", captured.Body["options"])
	}
	if v, _ := opts["mirostat"].(float64); v != 2 {
		t.Errorf("inject.mirostat should land in options for ollama, got %v", opts["mirostat"])
	}
	if _, atTop := captured.Body["mirostat"]; atTop {
		t.Error("mirostat should not be at top level for ollama backend")
	}
}
