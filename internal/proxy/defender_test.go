package proxy

import (
	"encoding/json"
	"testing"

	"github.com/wentbackward/hikyaku/internal/config"
)

func TestGetLastUserMessage(t *testing.T) {
	tests := []struct {
		name     string
		body     map[string]interface{}
		expected string
	}{
		{
			name: "simple user message",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "hello"},
				},
			},
			expected: "hello",
		},
		{
			name: "multi-turn picks last user",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "user", "content": "first"},
					map[string]interface{}{"role": "assistant", "content": "reply"},
					map[string]interface{}{"role": "user", "content": "second"},
				},
			},
			expected: "second",
		},
		{
			name: "no messages",
			body: map[string]interface{}{
				"messages": []interface{}{},
			},
			expected: "",
		},
		{
			name: "only assistant",
			body: map[string]interface{}{
				"messages": []interface{}{
					map[string]interface{}{"role": "assistant", "content": "hi"},
				},
			},
			expected: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			msg := getLastUserMessage(tt.body)
			var got string
			if msg != nil {
				got = getMessageContent(msg)
			}
			if got != tt.expected {
				t.Errorf("got %q, want %q", got, tt.expected)
			}
		})
	}
}

func TestCheckZeroContent(t *testing.T) {
	cfg := config.ZeroContentDetectionConfig{
		Enabled:             true,
		MinUserContentChars: 10,
		MinTotalInputTokens: 500,
	}

	// Large system prompt, tiny user message
	largeSystem := make([]byte, 3000)
	for i := range largeSystem {
		largeSystem[i] = 'a'
	}

	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": string(largeSystem)},
			map[string]interface{}{"role": "user", "content": "..."},
		},
	}

	if !checkZeroContent(body, cfg) {
		t.Error("expected zero-content to be detected")
	}

	// Adequate user content
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "system", "content": string(largeSystem)},
		map[string]interface{}{"role": "user", "content": "this is a meaningful query"},
	}

	if checkZeroContent(body, cfg) {
		t.Error("should not detect zero-content when user message is adequate")
	}

	// Small total payload (below min tokens)
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "user", "content": "hi"},
	}

	if checkZeroContent(body, cfg) {
		t.Error("should not trigger on small payloads")
	}
}

func TestLoopDetector_Basic(t *testing.T) {
	ld := &loopDetector{state: make(map[string]*loopEntry)}
	cfg := config.LoopDetectionConfig{
		Enabled:              true,
		ConsecutiveThreshold: 3,
		WindowSeconds:        60,
		Action:               "inject_forcing_message",
		EscalateAfter:        2,
		EscalateAction:       "refuse_429",
	}

	// First 2 requests: no detection
	for i := 0; i < 2; i++ {
		detected, _, _ := ld.checkLoop("session1", "hash1", cfg)
		if detected {
			t.Errorf("unexpected detection at request %d", i+1)
		}
	}

	// 3rd request: detected
	detected, escalated, action := ld.checkLoop("session1", "hash1", cfg)
	if !detected {
		t.Error("expected loop detection at request 3")
	}
	if escalated {
		t.Error("should not escalate yet")
	}
	if action != "inject_forcing_message" {
		t.Errorf("expected action inject_forcing_message, got %s", action)
	}
}

func TestLoopDetector_Escalation(t *testing.T) {
	ld := &loopDetector{state: make(map[string]*loopEntry)}
	cfg := config.LoopDetectionConfig{
		Enabled:              true,
		ConsecutiveThreshold: 2,
		WindowSeconds:        60,
		Action:               "inject_forcing_message",
		EscalateAfter:        1,
		EscalateAction:       "refuse_429",
	}

	// Build up to threshold
	ld.checkLoop("sess", "hash", cfg)
	detected, escalated, action := ld.checkLoop("sess", "hash", cfg)
	if !detected {
		t.Fatal("expected detection at threshold")
	}
	if escalated {
		t.Error("should not escalate on first detection")
	}
	if action != "inject_forcing_message" {
		t.Errorf("expected inject_forcing_message, got %s", action)
	}

	// Next request: should escalate
	detected, escalated, action = ld.checkLoop("sess", "hash", cfg)
	if !detected {
		t.Fatal("expected detection")
	}
	if !escalated {
		t.Error("expected escalation")
	}
	if action != "refuse_429" {
		t.Errorf("expected refuse_429, got %s", action)
	}
}

func TestLoopDetector_Disabled(t *testing.T) {
	ld := &loopDetector{state: make(map[string]*loopEntry)}
	cfg := config.LoopDetectionConfig{
		Enabled: false,
	}

	detected, _, _ := ld.checkLoop("key", "hash", cfg)
	if detected {
		t.Error("should not detect when disabled")
	}
}

func TestApplyLoopDetected_Refuse429(t *testing.T) {
	short, status, resp := applyLoopDetected("refuse_429", 3, nil)
	if !short {
		t.Error("expected short circuit")
	}
	if status != 429 {
		t.Errorf("expected status 429, got %d", status)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errObj, ok := result["error"].(map[string]interface{})
	if !ok {
		t.Fatal("missing error object")
	}
	if errObj["type"] != "defender" {
		t.Errorf("expected error type 'defender', got %v", errObj["type"])
	}
}

func TestApplyLoopDetected_Inject(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "you are helpful"},
			map[string]interface{}{"role": "user", "content": "do stuff"},
		},
	}

	short, status, resp := applyLoopDetected("inject_forcing_message", 3, body)
	if short {
		t.Error("inject should not short-circuit")
	}
	if status != 0 {
		t.Errorf("expected status 0, got %d", status)
	}
	if resp != "" {
		t.Error("inject should return empty response body")
	}

	// Verify system message was appended
	msgs := body["messages"].([]interface{})
	sysMsg := msgs[0].(map[string]interface{})
	content := sysMsg["content"].(string)
	if content[:15] != "you are helpful" {
		t.Error("original system message should be preserved")
	}
	if len(content) <= 15 {
		t.Error("forcing message should be appended to system message")
	}
}

func TestApplyZeroContent_Refuse400(t *testing.T) {
	short, status, resp := applyZeroContent("refuse_400")
	if !short {
		t.Error("expected short circuit")
	}
	if status != 400 {
		t.Errorf("expected status 400, got %d", status)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	errObj, ok := result["error"].(map[string]interface{})
	if !ok {
		t.Fatal("missing error object")
	}
	if errObj["type"] != "defender" {
		t.Errorf("expected error type 'defender', got %v", errObj["type"])
	}
}

func TestApplyZeroContent_InjectMinimalResponse(t *testing.T) {
	short, status, resp := applyZeroContent("inject_minimal_response")
	if !short {
		t.Error("expected short circuit")
	}
	if status != 200 {
		t.Errorf("expected status 200, got %d", status)
	}

	var result map[string]interface{}
	if err := json.Unmarshal([]byte(resp), &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result["object"] != "chat.completion" {
		t.Errorf("expected object 'chat.completion', got %v", result["object"])
	}
}

func TestCheckDefenders_ZeroContent(t *testing.T) {
	ld := &loopDetector{state: make(map[string]*loopEntry)}
	cfg := &config.Config{
		Defenders: config.DefenderConfig{
			ZeroContentDetection: &config.ZeroContentDetectionConfig{
				Enabled:             true,
				MinUserContentChars: 10,
				MinTotalInputTokens: 500,
				Action:              "refuse_400",
			},
			LoopDetection: &config.LoopDetectionConfig{
				Enabled: false,
			},
		},
	}

	largeSystem := make([]byte, 3000)
	for i := range largeSystem {
		largeSystem[i] = 'a'
	}

	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": string(largeSystem)},
			map[string]interface{}{"role": "user", "content": "..."},
		},
	}

	short, status, _, header := checkDefenders(ld, cfg, nil, body, "model-x")
	if !short {
		t.Error("expected short circuit")
	}
	if status != 400 {
		t.Errorf("expected status 400, got %d", status)
	}
	if header != "zero_content_blocked" {
		t.Errorf("expected header 'zero_content_blocked', got %s", header)
	}
}
