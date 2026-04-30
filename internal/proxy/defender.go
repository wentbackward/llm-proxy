// Package proxy contains the HTTP proxy server.
// This file implements request defenders: loop detection and zero-content detection.
package proxy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/wentbackward/hikyaku/internal/config"
)

// loopEntry tracks repeated requests for a given (affinity_key, last_user_msg_hash).
type loopEntry struct {
	hash      string
	count     int
	firstSeen time.Time
	escaped   bool
}

// loopDetector tracks repeated requests across affinity keys.
type loopDetector struct {
	mu    sync.Mutex
	state map[string]*loopEntry
}

// cleanupStale removes entries older than windowSeconds.
func (ld *loopDetector) cleanupStale(windowSeconds int) {
	cutoff := time.Now().Add(-time.Duration(windowSeconds) * time.Second)
	for k, e := range ld.state {
		if e.firstSeen.Before(cutoff) {
			delete(ld.state, k)
		}
	}
}

// checkLoop checks if the request is part of a suspected loop.
// Returns (detected, escalated, action).
func (ld *loopDetector) checkLoop(affinityKey, lastUserMsgHash string, cfg config.LoopDetectionConfig) (detected, escalated bool, action string) {
	if !cfg.Enabled {
		return false, false, ""
	}

	key := affinityKey + ":" + lastUserMsgHash
	compositeHash := sha256.Sum256([]byte(key))
	hexHash := hex.EncodeToString(compositeHash[:])

	ld.mu.Lock()
	defer ld.mu.Unlock()

	// Periodic cleanup (every 1000th check)
	if len(ld.state)%1000 == 0 && len(ld.state) > 100 {
		ld.cleanupStale(cfg.WindowSeconds)
	}

	entry, exists := ld.state[hexHash]
	now := time.Now()

	if !exists || now.Sub(entry.firstSeen).Seconds() > float64(cfg.WindowSeconds) {
		// New window
		ld.state[hexHash] = &loopEntry{
			hash:      lastUserMsgHash,
			count:     1,
			firstSeen: now,
		}
		return false, false, ""
	}

	entry.count++
	if entry.count < cfg.ConsecutiveThreshold {
		return false, false, ""
	}

	// Loop detected
	action = cfg.Action
	if action == "" {
		action = "inject_forcing_message"
	}

	// Check escalation
	if entry.escaped {
		action = cfg.EscalateAction
		if action == "" {
			action = "refuse_429"
		}
		return true, true, action
	}

	if entry.count >= cfg.ConsecutiveThreshold+cfg.EscalateAfter {
		entry.escaped = true
		action = cfg.EscalateAction
		if action == "" {
			action = "refuse_429"
		}
		return true, true, action
	}

	// Reset count but keep the entry (so we can escalate if it continues)
	entry.count = cfg.ConsecutiveThreshold
	return true, false, action
}

// getEntry retrieves the loop entry for a given key. Caller must hold lock if modifying.
func (ld *loopDetector) getEntry(affinityKey, lastUserMsgHash string) *loopEntry {
	key := affinityKey + ":" + lastUserMsgHash
	compositeHash := sha256.Sum256([]byte(key))
	hexHash := hex.EncodeToString(compositeHash[:])
	return ld.state[hexHash]
}

// getLastUserMessage extracts the last user role message from the messages array.
// Works for both OpenAI ("messages") and Anthropic ("messages").
func getLastUserMessage(body map[string]interface{}) interface{} {
	msgs, ok := body["messages"].([]interface{})
	if !ok || len(msgs) == 0 {
		return nil
	}

	// Walk backwards to find the last user message
	for i := len(msgs) - 1; i >= 0; i-- {
		msg, ok := msgs[i].(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role == "user" {
			return msg
		}
	}
	return nil
}

// getMessageContent extracts the content field from a message.
func getMessageContent(msg interface{}) string {
	msgMap, ok := msg.(map[string]interface{})
	if !ok {
		return ""
	}
	content, _ := msgMap["content"].(string)
	return content
}

// hashString returns a hex SHA256 of the input.
func hashString(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// checkZeroContent determines if the request has negligible user content
// relative to its total size.
func checkZeroContent(body map[string]interface{}, cfg config.ZeroContentDetectionConfig) bool {
	if !cfg.Enabled {
		return false
	}

	lastUserMsg := getLastUserMessage(body)
	if lastUserMsg == nil {
		return false
	}

	content := getMessageContent(lastUserMsg)
	stripped := strings.TrimSpace(content)
	if len(stripped) >= cfg.MinUserContentChars {
		return false
	}

	// Estimate total input size roughly (bytes / 4 ≈ tokens)
	totalBytes := 0
	msgs, ok := body["messages"].([]interface{})
	if ok {
		for _, m := range msgs {
			if msgMap, ok := m.(map[string]interface{}); ok {
				if c, ok := msgMap["content"].(string); ok {
					totalBytes += len(c)
				}
				// Also count tool_calls, function_call, etc.
				if tc, ok := msgMap["tool_calls"].([]interface{}); ok {
					totalBytes += len(fmt.Sprint(tc))
				}
			}
		}
	}

	minTokens := cfg.MinTotalInputTokens
	if minTokens == 0 {
		minTokens = 2000
	}

	return totalBytes/4 >= minTokens
}

// applyLoopDetected handles the loop detection action.
// Returns (shortCircuit, statusCode, responseBody).
func applyLoopDetected(action string, count int, body map[string]interface{}) (shortCircuit bool, statusCode int, responseBody string) {
	switch action {
	case "refuse_429":
		return true, http.StatusTooManyRequests,
			fmt.Sprintf(`{"error":{"message":"loop detected: same request repeated %d times. Stop retrying.","type":"defender"},"code":429}`, count)

	case "inject_forcing_message":
		msg := fmt.Sprintf(
			"Your previous %d attempts of this exact request returned the same result. Stop retrying. Either fix the underlying issue (e.g. read the tool's error output and address it directly) or stop and surface to the human.",
			count,
		)
		injectSystemMessage(body, msg)
		return false, 0, ""

	case "drain_to_idle":
		// For now, treat as passthrough (lower priority handled by balancer)
		return false, 0, ""

	default:
		return false, 0, ""
	}
}

// injectSystemMessage prepends a system message to the messages array.
func injectSystemMessage(body map[string]interface{}, content string) {
	msgs, ok := body["messages"].([]interface{})
	if !ok {
		return
	}

	// Find existing system message (first message with role=system)
	for _, m := range msgs {
		if msgMap, ok := m.(map[string]interface{}); ok {
			if role, _ := msgMap["role"].(string); role == "system" {
				// Append our message to existing system content
				existing, _ := msgMap["content"].(string)
				msgMap["content"] = existing + "\n\n" + content
				return
			}
		}
	}

	// No system message found, prepend one
	newMsg := map[string]interface{}{
		"role":    "system",
		"content": content,
	}
	msgs = append([]interface{}{newMsg}, msgs...)
	body["messages"] = msgs
}

// applyZeroContent handles the zero-content detection action.
// Returns (shortCircuit, statusCode, responseBody).
func applyZeroContent(action string) (shortCircuit bool, statusCode int, responseBody string) {
	switch action {
	case "refuse_400":
		return true, http.StatusBadRequest,
			`{"error":{"message":"request blocked: minimal user content in a large payload. Add meaningful content or reduce system/tool overhead.","type":"defender"},"code":400}`

	case "inject_minimal_response":
		return true, http.StatusOK,
			fmt.Sprintf(`{"id":"def-zero-content","object":"chat.completion","created":%d,"model":"","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"length"}]}`, time.Now().Unix())

	default:
		return false, 0, ""
	}
}

// checkDefenders runs loop detection and zero-content detection before routing.
// Returns (shortCircuit, statusCode, responseBody, defenderHeader).
func checkDefenders(ld *loopDetector, cfg *config.Config, route *config.Route, body map[string]interface{}, affinityKey string) (shortCircuit bool, statusCode int, responseBody, defenderHeader string) {
	// Zero-content detection
	zcCfg := cfg.GetZeroContentDetection(route)
	if checkZeroContent(body, zcCfg) {
		short, status, resp := applyZeroContent(zcCfg.Action)
		if short {
			return true, status, resp, "zero_content_blocked"
		}
	}

	// Loop detection
	loopCfg := cfg.GetLoopDetection(route)
	lastUserMsg := getLastUserMessage(body)
	if lastUserMsg != nil {
		content := getMessageContent(lastUserMsg)
		lastUserHash := hashString(content)

		detected, escalated, action := ld.checkLoop(affinityKey, lastUserHash, loopCfg)
		if detected {
			entry := ld.getEntry(affinityKey, lastUserHash)
			short, status, resp := applyLoopDetected(action, entry.count, body)
			label := "loop_detection_inject"
			if escalated {
				label = "loop_detection_escalated"
			}
			return short, status, resp, label
		}
	}

	return false, 0, "", ""
}
