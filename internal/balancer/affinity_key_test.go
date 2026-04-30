package balancer

import (
	"strings"
	"testing"
)

func TestFirstUserMessageKey_Stable(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "user", "content": "Hello"},
		},
	}
	a := FirstUserMessageKey(body, 2048)
	b := FirstUserMessageKey(body, 2048)
	if a != b {
		t.Errorf("same body must produce same key: %q vs %q", a, b)
	}
	if a == "" {
		t.Error("key must not be empty for non-empty user message")
	}
}

func TestFirstUserMessageKey_IgnoresSystem(t *testing.T) {
	// Two bodies with different system prompts but same first user message
	// should produce the same affinity key.
	bodyA := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "System A"},
			map[string]interface{}{"role": "user", "content": "Hello world"},
		},
	}
	bodyB := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "System B totally different"},
			map[string]interface{}{"role": "user", "content": "Hello world"},
		},
	}
	ka := FirstUserMessageKey(bodyA, 2048)
	kb := FirstUserMessageKey(bodyB, 2048)
	if ka != kb {
		t.Errorf("different system prompts, same user message: keys differ: %q vs %q", ka, kb)
	}
}

func TestFirstUserMessageKey_Capacity(t *testing.T) {
	longContent := strings.Repeat("A", 5000)
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": longContent},
		},
	}
	// With a low cap, the key should still be computed (truncated).
	key := FirstUserMessageKey(body, 100)
	if key == "" {
		t.Error("key should not be empty even with truncation")
	}
	// Content beyond the cap should not affect the hash.
	truncatedContent := strings.Repeat("A", 100) + strings.Repeat("Z", 5000)
	body2 := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": truncatedContent},
		},
	}
	key2 := FirstUserMessageKey(body2, 100)
	if key != key2 {
		t.Errorf("content beyond cap should not affect key: %q vs %q", key, key2)
	}
}

func TestFirstUserMessageKey_Multipart(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{
				"role": "user",
				"content": []interface{}{
					map[string]interface{}{"type": "text", "text": "Describe this"},
					map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/img.png"}},
				},
			},
		},
	}
	key := FirstUserMessageKey(body, 2048)
	if key == "" {
		t.Error("multipart content should produce a non-empty key")
	}
}

func TestFirstUserMessageKey_SkipsAssistant(t *testing.T) {
	// First user message appears after an assistant turn (e.g. tool result followed by user).
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "assistant", "content": "Let me search."},
			map[string]interface{}{"role": "tool", "content": "Results here"},
			map[string]interface{}{"role": "user", "content": "Based on those results, summarize."},
		},
	}
	key := FirstUserMessageKey(body, 2048)
	if key == "" {
		t.Error("should find first user message even after assistant/tool turns")
	}
}

func TestFirstUserMessageKey_NoUserMessage(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "system", "content": "You are helpful."},
			map[string]interface{}{"role": "assistant", "content": "OK"},
		},
	}
	key := FirstUserMessageKey(body, 2048)
	if key != "" {
		t.Errorf("no user message should produce empty key, got: %q", key)
	}
}

func TestFirstUserMessageKey_NoMessages(t *testing.T) {
	body := map[string]interface{}{}
	key := FirstUserMessageKey(body, 2048)
	if key != "" {
		t.Error("empty body must produce empty key")
	}
}

func TestFirstUserMessageKey_DifferentUsers(t *testing.T) {
	bodyA := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Write a Python script"},
		},
	}
	bodyB := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "Debug this Rust code"},
		},
	}
	ka := FirstUserMessageKey(bodyA, 2048)
	kb := FirstUserMessageKey(bodyB, 2048)
	if ka == kb {
		t.Error("different user messages should produce different keys")
	}
}

func TestFnv64a_Basic(t *testing.T) {
	a := fnv64a([]byte("hello"))
	b := fnv64a([]byte("hello"))
	if a != b {
		t.Error("fnv64a must be deterministic")
	}
	c := fnv64a([]byte("world"))
	if a == c {
		t.Error("different inputs should produce different hashes")
	}
}

func TestStringifyContent_String(t *testing.T) {
	msg := map[string]interface{}{"content": "hello"}
	if got := stringifyContent(msg); got != "hello" {
		t.Errorf("got %q, want %q", got, "hello")
	}
}

func TestStringifyContent_Multipart(t *testing.T) {
	msg := map[string]interface{}{
		"content": []interface{}{
			map[string]interface{}{"type": "text", "text": "part1"},
			map[string]interface{}{"type": "text", "text": "part2"},
			map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "..."}},
		},
	}
	if got := stringifyContent(msg); got != "part1part2" {
		t.Errorf("got %q, want %q", got, "part1part2")
	}
}

func TestStringifyContent_Nil(t *testing.T) {
	msg := map[string]interface{}{"content": nil}
	if got := stringifyContent(msg); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}
