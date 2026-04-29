package balancer

import (
	"bytes"
	"strings"
	"testing"
)

func TestCanonicalPrefix_Stable(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{"role": "system", "content": "You are helpful."},
		map[string]interface{}{"role": "user", "content": "Hello"},
	}
	a := CanonicalPrefix(messages, 1024)
	b := CanonicalPrefix(messages, 1024)
	if !bytes.Equal(a, b) {
		t.Error("same messages must produce same prefix")
	}
}

func TestCanonicalPrefix_Truncation(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{"role": "user", "content": strings.Repeat("A", 2000)},
	}
	prefix := CanonicalPrefix(messages, 100)
	if len(prefix) != 100 {
		t.Errorf("expected 100 bytes, got %d", len(prefix))
	}
}

func TestCanonicalPrefix_Multipart(t *testing.T) {
	messages := []interface{}{
		map[string]interface{}{
			"role": "user",
			"content": []interface{}{
				map[string]interface{}{"type": "text", "text": "Describe this"},
				map[string]interface{}{"type": "image_url", "image_url": map[string]interface{}{"url": "https://example.com/img.png"}},
			},
		},
	}
	prefix := CanonicalPrefix(messages, 1024)
	if !bytes.Contains(prefix, []byte("Describe this")) {
		t.Error("multipart text should be included in prefix")
	}
}

func TestAffinityKey_Deterministic(t *testing.T) {
	body := map[string]interface{}{
		"messages": []interface{}{
			map[string]interface{}{"role": "user", "content": "hello"},
		},
	}
	ka := AffinityKey(body, 1024)
	kb := AffinityKey(body, 1024)
	if ka != kb {
		t.Errorf("same body must produce same key: %q vs %q", ka, kb)
	}
	if ka == "" {
		t.Error("key must not be empty for non-empty messages")
	}
}

func TestAffinityKey_NoMessages(t *testing.T) {
	body := map[string]interface{}{}
	key := AffinityKey(body, 1024)
	if key != "" {
		t.Error("empty body must produce empty key")
	}
}

func TestFnv64a_Basic(t *testing.T) {
	// Smoke test: same input → same output
	a := fnv64a([]byte("hello"))
	b := fnv64a([]byte("hello"))
	if a != b {
		t.Error("fnv64a must be deterministic")
	}
	// Different input → different output (probabilistic, but fnv64a is good)
	c := fnv64a([]byte("world"))
	if a == c {
		t.Error("different inputs should produce different hashes")
	}
}
