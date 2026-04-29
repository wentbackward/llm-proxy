package balancer

import (
	"bytes"
	"net/http"
	"strconv"
	"strings"
)

const (
	unitSeparator   = '\x1f'
	recordSeparator = '\x1e'
)

// CanonicalPrefix builds a deterministic byte representation of the leading
// conversation, mirroring how the chat template lays out tokens.
func CanonicalPrefix(messages []interface{}, n int) []byte {
	var buf bytes.Buffer
	buf.Grow(n)
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		buf.WriteString(role)
		buf.WriteByte(unitSeparator)
		buf.WriteString(extractText(msg))
		buf.WriteByte(recordSeparator)
		if buf.Len() >= n {
			break
		}
	}
	out := buf.Bytes()
	if len(out) > n {
		out = out[:n]
	}
	return out
}

// extractText concatenates text parts from a message's content field.
// Handles string content and multipart arrays.
func extractText(msg map[string]interface{}) string {
	switch content := msg["content"].(type) {
	case string:
		return content
	case []interface{}:
		var b strings.Builder
		for _, p := range content {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			if part["type"] == "text" {
				text, _ := part["text"].(string)
				b.WriteString(text)
			}
		}
		return b.String()
	}
	return ""
}

// AffinityKey computes a 16-char hex key from the request body.
// Returns empty string if messages are missing or empty.
func AffinityKey(body map[string]interface{}, prefixBytes int) string {
	messages, ok := body["messages"].([]interface{})
	if !ok || len(messages) == 0 {
		return ""
	}
	prefix := CanonicalPrefix(messages, prefixBytes)
	h := fnv64a(prefix)
	return strconv.FormatUint(h, 16)
}

// fnv64a is a hand-rolled FNV-64a hash. No external dependency needed.
func fnv64a(data []byte) uint64 {
	const (
		offset64 = 14695981039346656037
		prime64  = 1099511628211
	)
	h := uint64(offset64)
	for _, b := range data {
		h ^= uint64(b)
		h *= prime64
	}
	return h
}

// HeaderAffinityKey returns the trimmed value of the named header,
// or empty string if absent.
func HeaderAffinityKey(header http.Header, name string) string {
	v := header.Get(name)
	return strings.TrimSpace(strings.ToLower(v))
}
