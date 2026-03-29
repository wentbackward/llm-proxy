// Package journal emits structured request analysis as OTel log records.
package journal

import (
	"strings"
)

const (
	maxSystemText   = 2048  // 2KB cap for system prompt
	maxLastUserText = 8192  // 8KB cap for last user message
)

// Entry holds the analysis of a single proxy request.
type Entry struct {
	RequestID    string
	VirtualModel string
	RealModel    string
	Backend      string
	Protocol     string // "openai" | "anthropic"
	Streaming    bool

	// Message stats
	MessageCount  int
	SystemChars   int
	LastUserChars int
	TotalChars    int
	EstTokens     int // TotalChars / 4

	// Structural signals
	HasTools      bool
	HasToolChoice bool
	CodeFences    int // count of ``` pairs
	JSONBlocks    int // code-fenced json blocks > 50 chars
	IsMultimodal  bool

	// Message content for routing analysis
	SystemText   string // system prompt, capped at 2KB
	LastUserText string // last user message, capped at 8KB

	// Merged params applied to request
	Params map[string]interface{}
}

// Analyze inspects a decoded request body and returns an Entry with message
// statistics and structural signals. Handles both OpenAI and Anthropic formats.
func Analyze(body map[string]interface{}, protocol string) Entry {
	var e Entry
	e.Protocol = protocol
	e.Streaming, _ = body["stream"].(bool)

	_, e.HasTools = body["tools"]
	_, e.HasToolChoice = body["tool_choice"]

	// Anthropic system can be a top-level string
	if sys, ok := body["system"].(string); ok {
		e.SystemChars = len(sys)
		e.TotalChars += len(sys)
		e.SystemText = truncate(sys, maxSystemText)
		e.CodeFences += countCodeFences(sys)
		e.JSONBlocks += countJSONBlocks(sys)
	}

	messages, _ := body["messages"].([]interface{})
	e.MessageCount = len(messages)

	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		text, multimodal := extractMessageText(msg)
		chars := len(text)
		e.TotalChars += chars

		if multimodal {
			e.IsMultimodal = true
		}

		switch role {
		case "system":
			e.SystemChars += chars
			e.SystemText = truncate(text, maxSystemText)
		case "user":
			e.LastUserChars = chars
			e.LastUserText = truncate(text, maxLastUserText)
		}

		e.CodeFences += countCodeFences(text)
		e.JSONBlocks += countJSONBlocks(text)
	}

	e.EstTokens = e.TotalChars / 4
	return e
}

// extractMessageText returns the concatenated text content of a message and
// whether any non-text content parts were found.
func extractMessageText(msg map[string]interface{}) (text string, multimodal bool) {
	switch content := msg["content"].(type) {
	case string:
		return content, false
	case []interface{}:
		var b strings.Builder
		for _, p := range content {
			part, ok := p.(map[string]interface{})
			if !ok {
				continue
			}
			switch part["type"] {
			case "text":
				t, _ := part["text"].(string)
				b.WriteString(t)
			case "image_url", "image", "video_url", "video", "document", "file":
				multimodal = true
			}
		}
		return b.String(), multimodal
	}
	return "", false
}

// countCodeFences counts the number of ``` pairs in text.
func countCodeFences(text string) int {
	count := strings.Count(text, "```")
	return count / 2
}

// truncate returns s capped at max bytes. If truncated, appends "[truncated]".
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "[truncated]"
}

// countJSONBlocks counts code-fenced json blocks with > 50 chars of content.
func countJSONBlocks(text string) int {
	count := 0
	rest := text
	for {
		idx := strings.Index(rest, "```json")
		if idx == -1 {
			break
		}
		rest = rest[idx+7:]
		end := strings.Index(rest, "```")
		if end == -1 {
			break
		}
		block := strings.TrimSpace(rest[:end])
		if len(block) > 50 {
			count++
		}
		rest = rest[end+3:]
	}
	return count
}
