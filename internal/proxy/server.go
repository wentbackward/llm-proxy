// Package proxy implements the HTTP reverse-proxy with LLM-aware telemetry.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/journal"
	"github.com/wentbackward/llm-proxy/internal/logger"
	"github.com/wentbackward/llm-proxy/internal/router"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

// requestID generates a short random hex string for correlating log lines.
func requestID() string {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Server handles all proxy traffic.
type Server struct {
	cfg     atomic.Pointer[config.Config]
	rtr     atomic.Pointer[router.Router]
	metrics *telemetry.Metrics
	journal *journal.Journal // nil if journal disabled
}

func New(cfg *config.Config, metrics *telemetry.Metrics, j *journal.Journal) *Server {
	s := &Server{metrics: metrics, journal: j}
	s.cfg.Store(cfg)
	s.rtr.Store(router.New(cfg))
	return s
}

// Reload atomically swaps in a new config and router.
// In-flight requests continue with the old config.
func (s *Server) Reload(cfg *config.Config) {
	s.rtr.Store(router.New(cfg))
	s.cfg.Store(cfg)
	log.Printf("[proxy] config reloaded: %d backends, %d routes", len(cfg.Backends), len(cfg.Routes))
}

// Config returns the current config (for use by main.go probes etc.)
func (s *Server) Config() *config.Config {
	return s.cfg.Load()
}

// RegisterRoutes attaches all proxy endpoints to mux.
// /health is unauthenticated; all other routes require a valid bearer token
// when server.api_key is set in config.
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	auth := s.bearerAuth

	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", auth(s.handleProxy))
	mux.HandleFunc("/v1/completions", auth(s.handleCompletions))
	mux.HandleFunc("/v1/messages", auth(s.handleProxy))
}

// bearerAuth wraps a handler with bearer token authentication.
// Checks the current config on each request so api_key changes take effect on reload.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.Load().Server.APIKey
		if key != "" && r.Header.Get("Authorization") != "Bearer "+key {
			w.Header().Set("WWW-Authenticate", `Bearer realm="llm-proxy"`)
			jsonError(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// handleModels forwards /v1/models to the first configured backend so clients
// receive real metadata (context_length, etc.) from the upstream. If no
// backends are configured or the upstream fails, it falls back to the static
// virtual model list.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	cfg := s.cfg.Load()

	if len(cfg.Backends) > 0 {
		backend := &cfg.Backends[0]
		upURL := backend.BaseURL + "/v1/models"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upURL, nil)
		if err == nil {
			if backend.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				data, readErr := io.ReadAll(resp.Body)
				resp.Body.Close()
				if resp.StatusCode == http.StatusOK && readErr == nil {
					rewritten := s.rewriteModelsResponse(data, cfg)
					w.Header().Set("Content-Type", "application/json")
					w.Write(rewritten)
					return
				}
			}
		}
		log.Printf("[proxy] /v1/models upstream failed, falling back to static list")
	}

	// Fallback: return virtual model names only
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []modelEntry
	for _, route := range cfg.Routes {
		if route.AutoRoute != nil {
			continue // skip auto-route entries; they resolve to real routes
		}
		data = append(data, modelEntry{
			ID:      route.VirtualModel,
			Object:  "model",
			Created: 0,
			OwnedBy: "llm-proxy",
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

// handleProxy is the main entry point for all completion requests.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rid := requestID()

	// ── Read & parse body ──────────────────────────────────────────────────
	rawBody, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// ── L2: log incoming headers, L3: log body preview ────────────────────
	logger.Headers("[%s] %s %s", rid, r.Method, r.URL.Path)
	for k, vs := range r.Header {
		for _, v := range vs {
			logger.Headers("[%s]   %s: %s", rid, k, v)
		}
	}
	preview := rawBody
	if len(preview) > 80 {
		preview = preview[:80]
	}
	logger.Body("[%s] %s %s body: %s", rid, r.Method, r.URL.Path, string(preview))

	// ── Detect protocol ────────────────────────────────────────────────────
	protocol := detectProtocol(r)

	// ── Resolve model → backend ────────────────────────────────────────────
	modelName, _ := body["model"].(string)

	var backend *config.Backend
	var realModel string

	cfg := s.cfg.Load()
	rtr := s.rtr.Load()

	res, err := rtr.Resolve(modelName, body)
	if err != nil {
		// No route configured — pass through to first backend if available.
		if len(cfg.Backends) == 0 {
			jsonError(w, "no backends configured", http.StatusServiceUnavailable)
			return
		}
		b := &cfg.Backends[0]
		backend = b
		realModel = modelName
		log.Printf("[proxy] no route for %q, passing through to %s", modelName, b.ID)
	} else {
		backend = res.Backend
		realModel = res.RealModel
		// Apply merged sampling params back to the body
		for k, v := range res.Params {
			body[k] = v
		}
		// Deduplicate max_tokens vs max_completion_tokens — backends reject both
		if _, ok := body["max_tokens"]; ok {
			delete(body, "max_completion_tokens")
		}
	}

	body["model"] = realModel
	isStreaming, _ := body["stream"].(bool)

	// ── L2: log transformation summary ─────────────────────────────────────
	authType := "none"
	if backend.APIKey != "" {
		if backend.Type == "anthropic" {
			authType = "x-api-key"
		} else {
			authType = "bearer"
		}
	}
	if res != nil && len(res.Params) > 0 {
		logger.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s params=%v",
			rid, backend.ID, backend.BaseURL, modelName, realModel, authType, res.Params)
	} else {
		logger.Headers("[%s] → backend=%s target=%s model=%s→%s auth=%s",
			rid, backend.ID, backend.BaseURL, modelName, realModel, authType)
	}

	// ── L4: log full message content ───────────────────────────────────────
	if logger.Get() >= logger.LevelContent {
		logMessageContent(rid, body, protocol)
	}

	// ── Journal: emit structured analysis ──────────────────────────────────
	if s.journal != nil {
		entry := journal.Analyze(body, protocol)
		entry.RequestID = rid
		entry.VirtualModel = modelName
		entry.RealModel = realModel
		entry.Backend = backend.ID
		if res != nil {
			entry.Params = res.Params
		}
		s.journal.Log(r.Context(), entry)
	}

	// ── Protocol-specific param translation ────────────────────────────────
	translateParams(body, backend, isStreaming)

	// ── Re-encode modified body ────────────────────────────────────────────
	newBody, err := json.Marshal(body)
	if err != nil {
		jsonError(w, "failed to encode request", http.StatusInternalServerError)
		return
	}

	// ── Build & execute reverse proxy ──────────────────────────────────────
	targetURL, err := url.Parse(backend.BaseURL)
	if err != nil {
		jsonError(w, fmt.Sprintf("invalid backend URL: %s", backend.BaseURL), http.StatusInternalServerError)
		return
	}

	t0 := time.Now()
	metricsCtx := context.WithoutCancel(r.Context())
	backendID := backend.ID

	w.Header().Set("X-Request-ID", rid)
	s.metrics.ActiveRequests.Add(metricsCtx, 1, telemetry.BackendAttrs(backendID, realModel))

	rp := &httputil.ReverseProxy{
		Director: director(targetURL, backend, newBody, protocol),
		ModifyResponse: s.modifyResponse(rid, backendID, modelName, realModel, r.URL.Path, backend.Type, backend.TimeoutSeconds, isStreaming, t0, metricsCtx),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
			elapsed := time.Since(t0).Seconds()
			s.metrics.RequestDuration.Record(metricsCtx, elapsed, telemetry.Attrs(backendID, realModel, "error"))
			s.metrics.RequestsTotal.Add(metricsCtx, 1, telemetry.Attrs(backendID, realModel, "error"))
			logger.Request("[%s] %s %s model=%s backend=%s status=502 dur=%.3fs ERROR: %v",
				rid, r.Method, r.URL.Path, modelName, backendID, elapsed, err)
			log.Printf("[proxy] upstream error backend=%s: %v", backendID, err)
			jsonError(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

// director returns an httputil.ReverseProxy Director that rewrites the request
// for the given backend.
func director(target *url.URL, backend *config.Backend, body []byte, protocol string) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		// req.URL.Path is kept as-is — the proxy receives /v1/chat/completions
		// and forwards it unchanged. base_url should be scheme+host only
		// (e.g. http://localhost:3022), not include a /v1 suffix.

		// Auth headers
		if backend.APIKey != "" {
			if backend.Type == "anthropic" {
				req.Header.Set("x-api-key", backend.APIKey)
				if req.Header.Get("anthropic-version") == "" {
					req.Header.Set("anthropic-version", "2023-06-01")
				}
			} else {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
		}

		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Del("Transfer-Encoding")
	}
}

// logMessageContent logs full text content of each message at L4.
func logMessageContent(rid string, body map[string]interface{}, protocol string) {
	// OpenAI: messages[].content (string or array of content blocks)
	// Anthropic: system (string) + messages[].content (string or array)
	if sys, ok := body["system"].(string); ok && sys != "" {
		logger.Content("[msg %s] role=system | %s", rid, sys)
	}
	messages, _ := body["messages"].([]interface{})
	for _, m := range messages {
		msg, ok := m.(map[string]interface{})
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		switch content := msg["content"].(type) {
		case string:
			logger.Content("[msg %s] role=%s | %s", rid, role, content)
		case []interface{}:
			var parts []string
			for _, p := range content {
				part, ok := p.(map[string]interface{})
				if !ok {
					continue
				}
				switch part["type"] {
				case "text":
					text, _ := part["text"].(string)
					parts = append(parts, text)
				case "image_url", "image":
					parts = append(parts, "[image]")
				case "video_url", "video":
					parts = append(parts, "[video]")
				case "document", "file":
					parts = append(parts, "[document]")
				default:
					t, _ := part["type"].(string)
					parts = append(parts, "["+t+"]")
				}
			}
			logger.Content("[msg %s] role=%s | %s", rid, role, strings.Join(parts, " "))
		}
	}
}

// modifyResponse returns a ModifyResponse function that instruments the
// upstream response with telemetry.
func (s *Server) modifyResponse(rid, backendID, virtualModel, realModel, path, backendType string, timeoutSec int, streaming bool, t0 time.Time, ctx context.Context) func(*http.Response) error {
	return func(resp *http.Response) error {
		statusStr := fmt.Sprintf("%d", resp.StatusCode)

		// Wrap with idle timeout if configured — cancels if no bytes
		// flow for timeout_seconds. Applied before any other wrappers.
		if timeoutSec > 0 {
			resp.Body = newIdleTimeoutBody(resp.Body, time.Duration(timeoutSec)*time.Second)
		}

		if streaming && resp.StatusCode == http.StatusOK {
			parser := newSSEParser(backendID, realModel, backendType, t0, s.metrics, ctx)
			if logger.Get() >= logger.LevelContent {
				parser.captureContent = true
			}
			resp.Body = &interceptedBody{
				ReadCloser: resp.Body,
				parser:     parser,
				onClose: func() {
					parser.recordFinal()
					elapsed := time.Since(t0).Seconds()
					s.metrics.ActiveRequests.Add(ctx, -1, telemetry.BackendAttrs(backendID, realModel))
					s.metrics.RequestDuration.Record(ctx, elapsed, telemetry.Attrs(backendID, realModel, statusStr))
					s.metrics.RequestsTotal.Add(ctx, 1, telemetry.Attrs(backendID, realModel, statusStr))
					logger.Request("[%s] POST %s model=%s→%s backend=%s status=%s dur=%.3fs stream=true",
						rid, path, virtualModel, realModel, backendID, statusStr, elapsed)
					if parser.captureContent {
						logger.Content("[resp %s] | %s", rid, parser.ResponseText())
					}
				},
			}
		} else {
			// Non-streaming: buffer to extract usage, then restore.
			data, err := io.ReadAll(resp.Body)
			resp.Body.Close()
			if err != nil {
				return err
			}
			elapsed := time.Since(t0).Seconds()
			s.metrics.ActiveRequests.Add(ctx, -1, telemetry.BackendAttrs(backendID, realModel))
			s.metrics.RequestDuration.Record(ctx, elapsed, telemetry.Attrs(backendID, realModel, statusStr))
			s.metrics.RequestsTotal.Add(ctx, 1, telemetry.Attrs(backendID, realModel, statusStr))
			logger.Request("[%s] POST %s model=%s→%s backend=%s status=%s dur=%.3fs",
				rid, path, virtualModel, realModel, backendID, statusStr, elapsed)

			if resp.StatusCode == http.StatusOK {
				extractNonStreamingUsage(data, backendID, realModel, backendType, s.metrics, ctx)
			}

			// L4: log non-streaming response content
			if logger.Get() >= logger.LevelContent {
				logNonStreamingResponse(rid, data, backendType)
			}

			resp.Body = io.NopCloser(bytes.NewReader(data))
			resp.ContentLength = int64(len(data))
		}
		return nil
	}
}

// ── Helpers ───────────────────────────────────────────────────────────────────

// rewriteModelsResponse takes the raw upstream /v1/models JSON and rewrites it:
//   - model entries whose id matches a route's real_model are renamed to the virtual_model name
//   - if the route has context_length set, context_length is overridden in the entry
//   - entries with no matching route are dropped (they are internal implementation names)
func (s *Server) rewriteModelsResponse(raw []byte, cfg *config.Config) []byte {
	// Build real_model → []route lookup (skip auto-route entries).
	// Multiple virtual models may share the same real_model.
	type routeInfo struct {
		virtualModel  string
		contextLength int
	}
	byReal := make(map[string][]routeInfo, len(cfg.Routes))
	for _, r := range cfg.Routes {
		if r.AutoRoute != nil || r.RealModel == "" {
			continue
		}
		byReal[r.RealModel] = append(byReal[r.RealModel], routeInfo{r.VirtualModel, r.ContextLength})
	}

	var upstream struct {
		Object string                   `json:"object"`
		Data   []map[string]interface{} `json:"data"`
	}
	if err := json.Unmarshal(raw, &upstream); err != nil {
		return raw // pass through unparseable response unchanged
	}

	seen := make(map[string]bool) // track which real_models appeared upstream
	out := make([]map[string]interface{}, 0, len(upstream.Data))
	for _, entry := range upstream.Data {
		id, _ := entry["id"].(string)
		infos, ok := byReal[id]
		if !ok {
			continue // not a configured model, hide it
		}
		seen[id] = true
		for i, info := range infos {
			var e map[string]interface{}
			if i == 0 {
				e = entry
			} else {
				// clone the entry for additional virtual models
				e = make(map[string]interface{}, len(entry))
				for k, v := range entry {
					e[k] = v
				}
			}
			e["id"] = info.virtualModel
			if info.contextLength > 0 {
				e["context_length"] = info.contextLength
			}
			out = append(out, e)
		}
	}

	// Append routes whose real_model was not in the upstream list (e.g. cloud backends).
	for realModel, infos := range byReal {
		if seen[realModel] {
			continue
		}
		for _, info := range infos {
			e := map[string]interface{}{
				"id":     info.virtualModel,
				"object": "model",
			}
			if info.contextLength > 0 {
				e["context_length"] = info.contextLength
			}
			out = append(out, e)
		}
	}

	result := map[string]interface{}{"object": upstream.Object, "data": out}
	b, err := json.Marshal(result)
	if err != nil {
		return raw
	}
	return b
}

// translateParams handles backend-specific parameter translation in-place.
func translateParams(body map[string]interface{}, backend *config.Backend, isStreaming bool) {
	// Inject include_usage for OpenAI streaming so token counts arrive.
	if isStreaming && backend.Type == "openai" {
		opts, _ := body["stream_options"].(map[string]interface{})
		if opts == nil {
			opts = make(map[string]interface{})
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}

	// enable_thinking translation
	enableThinking, hasThinking := body["enable_thinking"]
	thinkingBudget, _ := body["thinking_budget"].(float64)
	delete(body, "enable_thinking")
	delete(body, "thinking_budget")

	if hasThinking {
		switch backend.Type {
		case "openai":
			// vLLM Qwen3: chat_template_kwargs.enable_thinking
			kwargs, _ := body["chat_template_kwargs"].(map[string]interface{})
			if kwargs == nil {
				kwargs = make(map[string]interface{})
			}
			kwargs["enable_thinking"] = enableThinking
			body["chat_template_kwargs"] = kwargs

		case "anthropic":
			if et, _ := enableThinking.(bool); et {
				budget := int(thinkingBudget)
				if budget == 0 {
					if mt, _ := body["max_tokens"].(float64); mt > 0 {
						budget = int(mt)
					}
				}
				if budget == 0 {
					budget = 8000
				}
				body["thinking"] = map[string]interface{}{
					"type":          "enabled",
					"budget_tokens": budget,
				}
				// Anthropic requires temperature=1 when thinking is enabled
				body["temperature"] = 1.0
			}
		}
	}
}

func detectProtocol(r *http.Request) string {
	if strings.HasSuffix(r.URL.Path, "/messages") || r.Header.Get("anthropic-version") != "" {
		return "anthropic"
	}
	return "openai"
}

func extractNonStreamingUsage(data []byte, backendID, model, backendType string, m *telemetry.Metrics, ctx context.Context) {
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return
	}
	attrs := telemetry.BackendAttrs(backendID, model)

	var promptKey, completionKey string
	if backendType == "anthropic" {
		promptKey, completionKey = "input_tokens", "output_tokens"
	} else {
		promptKey, completionKey = "prompt_tokens", "completion_tokens"
	}

	if usage, ok := resp["usage"].(map[string]interface{}); ok {
		if v, _ := usage[promptKey].(float64); v > 0 {
			m.PromptTokens.Add(ctx, int64(v), attrs)
			m.PromptTokensPerRequest.Record(ctx, int64(v), attrs)
		}
		if v, _ := usage[completionKey].(float64); v > 0 {
			m.CompletionTokens.Add(ctx, int64(v), attrs)
		}
	}
}

// logNonStreamingResponse extracts and logs the assistant's text content from a non-streaming response.
func logNonStreamingResponse(rid string, data []byte, backendType string) {
	var resp map[string]interface{}
	if json.Unmarshal(data, &resp) != nil {
		return
	}
	var text string
	if backendType == "anthropic" {
		// Anthropic: content[].text
		if content, ok := resp["content"].([]interface{}); ok {
			for _, c := range content {
				block, _ := c.(map[string]interface{})
				if t, _ := block["text"].(string); t != "" {
					text += t
				}
			}
		}
	} else {
		// OpenAI: choices[0].message.content
		if choices, ok := resp["choices"].([]interface{}); ok && len(choices) > 0 {
			choice, _ := choices[0].(map[string]interface{})
			msg, _ := choice["message"].(map[string]interface{})
			text, _ = msg["content"].(string)
		}
	}
	if text != "" {
		if len(text) > 32768 {
			text = text[:32768] + " [truncated]"
		}
		logger.Content("[resp %s] | %s", rid, text)
	}
}

// handleCompletions translates legacy /v1/completions requests into
// /v1/chat/completions format and converts the response back. This supports
// clients (e.g. Continue autocomplete) that only use the legacy endpoint.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	rawBody, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		jsonError(w, "failed to read request body", http.StatusBadRequest)
		return
	}

	var body map[string]interface{}
	if err := json.Unmarshal(rawBody, &body); err != nil {
		jsonError(w, "invalid JSON body", http.StatusBadRequest)
		return
	}

	// Extract prompt and convert to chat format
	prompt, _ := body["prompt"].(string)
	if prompt == "" {
		// prompt can also be an array — take the first element
		if arr, ok := body["prompt"].([]interface{}); ok && len(arr) > 0 {
			prompt, _ = arr[0].(string)
		}
	}
	delete(body, "prompt")
	body["messages"] = []interface{}{
		map[string]interface{}{"role": "user", "content": prompt},
	}

	// Check if streaming
	streaming, _ := body["stream"].(bool)

	// Re-encode and rewrite the request to /v1/chat/completions
	chatBody, _ := json.Marshal(body)
	r.Body = io.NopCloser(bytes.NewReader(chatBody))
	r.ContentLength = int64(len(chatBody))
	r.URL.Path = "/v1/chat/completions"

	if streaming {
		// For streaming, we need to intercept the response and reformat SSE events
		// from chat.completion.chunk to completion format. For simplicity, proxy
		// as-is — most clients handle both formats from streaming endpoints.
		s.handleProxy(w, r)
		return
	}

	// Non-streaming: capture the chat response and convert to legacy format
	rec := &responseRecorder{header: http.Header{}, body: &bytes.Buffer{}}
	s.handleProxy(rec, r)

	// Copy status and headers
	for k, vs := range rec.header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}

	var chatResp map[string]interface{}
	if err := json.Unmarshal(rec.body.Bytes(), &chatResp); err != nil {
		// Can't parse — forward as-is
		w.WriteHeader(rec.code)
		w.Write(rec.body.Bytes())
		return
	}

	// Convert choices from chat to legacy format
	legacyChoices := []interface{}{}
	if choices, ok := chatResp["choices"].([]interface{}); ok {
		for _, c := range choices {
			choice, _ := c.(map[string]interface{})
			msg, _ := choice["message"].(map[string]interface{})
			text, _ := msg["content"].(string)
			legacyChoices = append(legacyChoices, map[string]interface{}{
				"text":          text,
				"index":         choice["index"],
				"finish_reason": choice["finish_reason"],
			})
		}
	}

	chatResp["object"] = "text_completion"
	chatResp["choices"] = legacyChoices

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(rec.code)
	json.NewEncoder(w).Encode(chatResp)
}

// responseRecorder captures an HTTP response for post-processing.
type responseRecorder struct {
	header http.Header
	body   *bytes.Buffer
	code   int
}

func (r *responseRecorder) Header() http.Header         { return r.header }
func (r *responseRecorder) WriteHeader(code int)         { r.code = code }
func (r *responseRecorder) Write(b []byte) (int, error)  { return r.body.Write(b) }

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "proxy_error"},
	})
}

