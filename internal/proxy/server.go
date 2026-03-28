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
func (s *Server) RegisterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("/v1/models", s.handleModels)
	mux.HandleFunc("/v1/chat/completions", s.handleProxy)
	mux.HandleFunc("/v1/messages", s.handleProxy)
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"status":"ok"}`))
	})
}

// handleModels returns the list of configured virtual models.
func (s *Server) handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type modelEntry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	var data []modelEntry
	for _, route := range s.cfg.Routes {
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
		req.URL.Path = joinPath(target.Path, req.URL.Path)
		req.Host = target.Host

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
			if et, _ := enableThinking.(bool); et
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

func joinPath(base, suffix string) string {
	if strings.HasSuffix(base, "/") && strings.HasPrefix(suffix, "/") {
		return base + suffix[1:]
	}
	if !strings.HasSuffix(base, "/") && !strings.HasPrefix(suffix, "/") {
		return base + "/" + suffix
	}
	return base + suffix
}
