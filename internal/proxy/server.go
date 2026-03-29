// Package proxy implements the HTTP reverse-proxy with LLM-aware telemetry.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/router"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

// Server handles all proxy traffic.
type Server struct {
	cfg     *config.Config
	router  *router.Router
	metrics *telemetry.Metrics
}

func New(cfg *config.Config, metrics *telemetry.Metrics) *Server {
	return &Server{
		cfg:     cfg,
		router:  router.New(cfg),
		metrics: metrics,
	}
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
	mux.HandleFunc("/v1/messages", auth(s.handleProxy))
}

// bearerAuth wraps a handler with bearer token authentication.
// If no api_key is configured the handler is returned unwrapped.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	key := s.cfg.Server.APIKey
	if key == "" {
		return next
	}
	expected := "Bearer " + key
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != expected {
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

	if len(s.cfg.Backends) > 0 {
		backend := &s.cfg.Backends[0]
		upURL := backend.BaseURL + "/v1/models"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upURL, nil)
		if err == nil {
			if backend.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil && resp.StatusCode == http.StatusOK {
				defer resp.Body.Close()
				data, readErr := io.ReadAll(resp.Body)
				if readErr == nil {
					rewritten := s.rewriteModelsResponse(data)
					w.Header().Set("Content-Type", "application/json")
					w.Write(rewritten)
					return
				}
			}
			if resp != nil {
				resp.Body.Close()
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
	for _, route := range s.cfg.Routes {
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

	// ── Detect protocol ────────────────────────────────────────────────────
	protocol := detectProtocol(r)

	// ── Resolve model → backend ────────────────────────────────────────────
	modelName, _ := body["model"].(string)

	var backend *config.Backend
	var realModel string

	res, err := s.router.Resolve(modelName, body)
	if err != nil {
		// No route configured — pass through to first backend if available.
		if len(s.cfg.Backends) == 0 {
			jsonError(w, "no backends configured", http.StatusServiceUnavailable)
			return
		}
		b := &s.cfg.Backends[0]
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
	}

	body["model"] = realModel
	isStreaming, _ := body["stream"].(bool)

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
	ctx := r.Context()
	backendID := backend.ID

	s.metrics.ActiveRequests.Add(ctx, 1, telemetry.BackendAttrs(backendID, realModel))

	rp := &httputil.ReverseProxy{
		Director: director(targetURL, backend, newBody, protocol),
		ModifyResponse: s.modifyResponse(backendID, realModel, backend.Type, isStreaming, t0, ctx),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.metrics.ActiveRequests.Add(ctx, -1, telemetry.BackendAttrs(backendID, realModel))
			elapsed := time.Since(t0).Seconds()
			s.metrics.RequestDuration.Record(ctx, elapsed, telemetry.Attrs(backendID, realModel, "error"))
			s.metrics.RequestsTotal.Add(ctx, 1, telemetry.Attrs(backendID, realModel, "error"))
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

// modifyResponse returns a ModifyResponse function that instruments the
// upstream response with telemetry.
func (s *Server) modifyResponse(backendID, model, backendType string, streaming bool, t0 time.Time, ctx context.Context) func(*http.Response) error {
	return func(resp *http.Response) error {
		statusStr := fmt.Sprintf("%d", resp.StatusCode)

		if streaming && resp.StatusCode == http.StatusOK {
			parser := newSSEParser(backendID, model, backendType, t0, s.metrics, ctx)
			resp.Body = &interceptedBody{
				ReadCloser: resp.Body,
				parser:     parser,
				onClose: func() {
					parser.recordFinal()
					elapsed := time.Since(t0).Seconds()
					s.metrics.ActiveRequests.Add(ctx, -1, telemetry.BackendAttrs(backendID, model))
					s.metrics.RequestDuration.Record(ctx, elapsed, telemetry.Attrs(backendID, model, statusStr))
					s.metrics.RequestsTotal.Add(ctx, 1, telemetry.Attrs(backendID, model, statusStr))
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
			s.metrics.ActiveRequests.Add(ctx, -1, telemetry.BackendAttrs(backendID, model))
			s.metrics.RequestDuration.Record(ctx, elapsed, telemetry.Attrs(backendID, model, statusStr))
			s.metrics.RequestsTotal.Add(ctx, 1, telemetry.Attrs(backendID, model, statusStr))

			if resp.StatusCode == http.StatusOK {
				extractNonStreamingUsage(data, backendID, model, backendType, s.metrics, ctx)
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
func (s *Server) rewriteModelsResponse(raw []byte) []byte {
	// Build real_model → []route lookup (skip auto-route entries).
	// Multiple virtual models may share the same real_model.
	type routeInfo struct {
		virtualModel  string
		contextLength int
	}
	byReal := make(map[string][]routeInfo, len(s.cfg.Routes))
	for _, r := range s.cfg.Routes {
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

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "proxy_error"},
	})
}

