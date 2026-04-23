// Package proxy implements the HTTP reverse-proxy with LLM-aware telemetry.
package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wentbackward/llm-proxy/internal/capture"
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
	journal *journal.Journal                // nil if journal disabled
	capture atomic.Pointer[capture.Capture] // nil value = capture disabled; rebuilt on Reload

	transport  *http.Transport          // shared connection pool for all backends
	mu         sync.RWMutex             // guards semaphores
	semaphores map[string]chan struct{} // per-backend concurrency limiter (nil entry = unlimited)
}

func New(cfg *config.Config, metrics *telemetry.Metrics, j *journal.Journal) *Server {
	tc := cfg.Server.Transport
	s := &Server{
		metrics: metrics,
		journal: j,
		transport: &http.Transport{
			DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			MaxIdleConns:        tc.MaxIdleConns,
			MaxIdleConnsPerHost: tc.MaxIdleConnsPerHost,
			MaxConnsPerHost:     0, // unlimited active — semaphore handles backpressure
			IdleConnTimeout:     time.Duration(tc.IdleConnTimeout) * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		},
	}
	s.cfg.Store(cfg)
	s.rtr.Store(router.New(cfg))
	s.buildSemaphores(cfg)
	s.applyCaptureConfig(cfg)
	return s
}

// applyCaptureConfig builds (or rebuilds) the capture handle from cfg. An init
// failure disables capture rather than failing the whole server — this is a
// debug-only feature and should never take the proxy down.
func (s *Server) applyCaptureConfig(cfg *config.Config) {
	c, err := capture.New(capture.Config{
		Enabled:      cfg.SigMessageCapture.Enabled,
		OutputFolder: cfg.SigMessageCapture.OutputFolder,
		MaxMessages:  cfg.SigMessageCapture.MaxMessages,
	})
	if err != nil {
		log.Printf("[capture] init failed: %v (disabling)", err)
		c = nil
	}
	s.capture.Store(c)
	if c != nil {
		log.Printf("[capture] enabled: folder=%s max_messages=%d (send SIGUSR1 to arm)",
			c.OutputFolder(), c.MaxMessages())
	}
}

// Capture returns the current capture handle (may be nil if disabled).
// Used by the SIGUSR1 handler in main.
func (s *Server) Capture() *capture.Capture {
	return s.capture.Load()
}

// Reload atomically swaps in a new config and router.
// In-flight requests continue with the old config.
func (s *Server) Reload(cfg *config.Config) {
	s.rtr.Store(router.New(cfg))
	s.cfg.Store(cfg)
	s.buildSemaphores(cfg)
	s.applyCaptureConfig(cfg)
	log.Printf("[reload] %d backends, %d routes", len(cfg.Backends), len(cfg.Routes))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		conc := "unlimited"
		if b.MaxConcurrency > 0 {
			conc = fmt.Sprintf("max=%d", b.MaxConcurrency)
		}
		marker := ""
		if b.Default {
			marker = " [default]"
		}
		log.Printf("[reload]   backend %-16s %s (%s)%s", b.ID, b.BaseURL, conc, marker)
	}
	if cfg.Server.PassthroughUnrouted {
		if def := cfg.DefaultBackend(); def != nil && !cfg.HasExplicitDefault() {
			log.Printf("[reload]   passthrough_unrouted: true but no backend has default: true — falling back to first backend (%s)", def.ID)
		}
	}
	for _, r := range cfg.Routes {
		if r.AutoRoute != nil {
			log.Printf("[reload]   route   %-16s → auto(text=%s, vision=%s)", r.VirtualModel, r.AutoRoute.Text, r.AutoRoute.Vision)
		} else {
			log.Printf("[reload]   route   %-16s → %s / %s", r.VirtualModel, r.Backend, r.RealModel)
		}
	}
}

// buildSemaphores creates a fresh semaphore map from the config.
// Backends without max_concurrency get no entry (unlimited).
func (s *Server) buildSemaphores(cfg *config.Config) {
	sems := make(map[string]chan struct{}, len(cfg.Backends))
	for i := range cfg.Backends {
		b := &cfg.Backends[i]
		if b.MaxConcurrency > 0 {
			sems[b.ID] = make(chan struct{}, b.MaxConcurrency)
		}
	}
	s.mu.Lock()
	s.semaphores = sems
	s.mu.Unlock()
}

// acquireSemaphore blocks until a slot is available for the given backend,
// or the request context is canceled. Returns a release function and true,
// or nil and false if the context was canceled while waiting.
func (s *Server) acquireSemaphore(ctx context.Context, backendID string) (release func(), ok bool) {
	s.mu.RLock()
	sem := s.semaphores[backendID]
	s.mu.RUnlock()

	if sem == nil {
		return func() {}, true
	}
	select {
	case sem <- struct{}{}:
		return func() { <-sem }, true
	case <-ctx.Done():
		return nil, false
	}
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
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/v1/models", auth(s.handleModels))
	mux.HandleFunc("/v1/chat/completions", auth(s.handleProxy))
	mux.HandleFunc("/v1/completions", auth(s.handleCompletions))
	mux.HandleFunc("/v1/embeddings", auth(s.handleEmbeddings))
	mux.HandleFunc("/v1/messages", auth(s.handleProxy))
}

// bearerAuth wraps a handler with bearer token authentication.
// Checks the current config on each request so api_key changes take effect on reload.
// The compare is constant-time to avoid leaking the token via response timing.
func (s *Server) bearerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := s.cfg.Load().Server.APIKey
		if key == "" {
			next(w, r)
			return
		}
		expected := []byte("Bearer " + key)
		got := []byte(r.Header.Get("Authorization"))
		if subtle.ConstantTimeCompare(got, expected) != 1 {
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

	if backend := cfg.DefaultBackend(); backend != nil {
		upURL := backend.BaseURL + "/v1/models"
		req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upURL, http.NoBody)
		if err == nil {
			if backend.APIKey != "" {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
			resp, err := http.DefaultClient.Do(req)
			if err == nil {
				data, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode == http.StatusOK && readErr == nil {
					rewritten := s.rewriteModelsResponse(data, cfg)
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write(rewritten)
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
	_ = json.NewEncoder(w).Encode(map[string]interface{}{"object": "list", "data": data})
}

// proxyOpts controls per-endpoint behavior differences in the shared proxy pipeline.
type proxyOpts struct {
	pathOverride string // if set, forces the backend request path (e.g. "/v1/completions")
	protocol     string // if set, skips auto-detection (e.g. "openai" for completions)
}

// handleProxy is the main entry point for chat completion and Anthropic requests.
func (s *Server) handleProxy(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{})
}

// proxyRequest is the shared reverse-proxy pipeline used by all endpoints.
func (s *Server) proxyRequest(w http.ResponseWriter, r *http.Request, opts proxyOpts) {
	rid := requestID()

	// ── Read & parse body ──────────────────────────────────────────────────
	// Cap request body size to avoid OOM on a malicious/misbehaving client.
	if maxMB := s.cfg.Load().Server.MaxRequestBodyMB; maxMB > 0 {
		r.Body = http.MaxBytesReader(w, r.Body, int64(maxMB)*1024*1024)
	}
	rawBody, err := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			jsonError(w, fmt.Sprintf("request body exceeds max_request_body_mb limit (%d MB)", s.cfg.Load().Server.MaxRequestBodyMB), http.StatusRequestEntityTooLarge)
			return
		}
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
	protocol := opts.protocol
	if protocol == "" {
		protocol = detectProtocol(r)
	}

	// ── Resolve model → backend ────────────────────────────────────────────
	modelName, _ := body["model"].(string)

	var backend *config.Backend
	var realModel string

	cfg := s.cfg.Load()
	rtr := s.rtr.Load()

	res, err := rtr.Resolve(modelName, body)
	if err != nil {
		if !cfg.Server.PassthroughUnrouted {
			available := cfg.VirtualModels()
			log.Printf("[proxy] rejected unknown model %q (from=%s ua=%s)",
				modelName, r.RemoteAddr, r.UserAgent())
			jsonError(w, fmt.Sprintf("unknown model %q — available models: %v", modelName, available), http.StatusNotFound)
			return
		}
		// Passthrough mode — forward to the default backend.
		b := cfg.DefaultBackend()
		if b == nil {
			jsonError(w, "no backends configured", http.StatusServiceUnavailable)
			return
		}
		backend = b
		realModel = modelName
		log.Printf("[proxy] no route for %q, passing through to %s (from=%s ua=%s)",
			modelName, b.ID, r.RemoteAddr, r.UserAgent())
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
		authType = backend.AuthType
		if authType == "" {
			if backend.Type == "anthropic" {
				authType = "x-api-key"
			} else {
				authType = "bearer"
			}
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
		logMessageContent(rid, body)
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

	// ── Capture: reserve a slot if the SIGUSR1 window is open ─────────────
	// Reserved here (after successful validation) so that 400/404/500
	// early-returns do not consume slots from the bounded window.
	capCtx := s.startCapture(r, rid, t0, modelName, realModel, backend.ID, protocol, isStreaming, rawBody, newBody)

	// ── Per-backend concurrency guard ─────────────────────────────────────
	release, ok := s.acquireSemaphore(r.Context(), backendID)
	if !ok {
		s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
		capCtx.finishError(t0, "backend concurrency limit reached")
		jsonError(w, "backend concurrency limit reached", http.StatusServiceUnavailable)
		return
	}
	defer release()

	rp := &httputil.ReverseProxy{
		Transport:      s.transport,
		Director:       director(targetURL, backend, newBody, opts.pathOverride),
		ModifyResponse: s.modifyResponse(rid, backendID, modelName, realModel, r.URL.Path, backend.Type, backend.TimeoutSeconds, isStreaming, t0, metricsCtx, capCtx), //nolint:bodyclose // ReverseProxy owns the response body; ModifyResponse wraps or reads-and-restores it, and the framework closes it client-side.
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			s.metrics.ActiveRequests.Add(metricsCtx, -1, telemetry.BackendAttrs(backendID, realModel))
			elapsed := time.Since(t0).Seconds()
			s.metrics.RequestDuration.Record(metricsCtx, elapsed, telemetry.Attrs(backendID, realModel, "error"))
			s.metrics.RequestsTotal.Add(metricsCtx, 1, telemetry.Attrs(backendID, realModel, "error"))
			logger.Request("[%s] %s %s model=%s backend=%s status=502 dur=%.3fs ERROR: %v",
				rid, r.Method, r.URL.Path, modelName, backendID, elapsed, err)
			log.Printf("[proxy] upstream error backend=%s: %v", backendID, err)
			capCtx.finishError(t0, "upstream error: "+err.Error())
			jsonError(w, "upstream error: "+err.Error(), http.StatusBadGateway)
		},
	}

	rp.ServeHTTP(w, r)
}

// ── Capture helpers ──────────────────────────────────────────────────────────

// captureCtx holds a reserved capture slot and the request-side snapshot.
// A nil *captureCtx is safe — every method is a no-op on nil receiver.
type captureCtx struct {
	slot    *capture.Slot
	payload capture.Payload
}

// startCapture reserves a capture slot if the window is open and assembles
// the request-side payload. Returns nil if capture is disabled or the window
// is closed — callers should treat that nil as "do nothing".
func (s *Server) startCapture(r *http.Request, rid string, t0 time.Time,
	virtualModel, realModel, backendID, protocol string, streaming bool,
	rawBody, resolvedBody []byte,
) *captureCtx {
	c := s.capture.Load()
	if c == nil {
		return nil
	}
	slot := c.Reserve()
	if slot == nil {
		return nil
	}
	return &captureCtx{
		slot: slot,
		payload: capture.Payload{
			RequestID: rid,
			Timestamp: t0.UTC().Format(time.RFC3339Nano),
			Request: capture.RequestSnapshot{
				Method:       r.Method,
				Path:         r.URL.Path,
				VirtualModel: virtualModel,
				RealModel:    realModel,
				Backend:      backendID,
				Protocol:     protocol,
				Streaming:    streaming,
				Headers:      safeHeaders(r.Header),
				Incoming:     rawJSON(rawBody),
				Resolved:     rawJSON(resolvedBody),
			},
		},
	}
}

// finishSuccess records the response and writes the capture file.
func (c *captureCtx) finishSuccess(t0 time.Time, resp capture.ResponseSnapshot) {
	if c == nil {
		return
	}
	c.payload.Response = resp
	c.payload.Timing = timing(t0)
	if err := c.slot.Write(c.payload); err != nil {
		log.Printf("[capture] write failed for %s: %v", c.payload.RequestID, err)
	}
}

// finishError records a pre-response error and writes the capture file.
func (c *captureCtx) finishError(t0 time.Time, msg string) {
	if c == nil {
		return
	}
	c.payload.Response = capture.ResponseSnapshot{Error: msg}
	c.payload.Timing = timing(t0)
	if err := c.slot.Write(c.payload); err != nil {
		log.Printf("[capture] write failed for %s: %v", c.payload.RequestID, err)
	}
}

func timing(t0 time.Time) capture.TimingSnapshot {
	return capture.TimingSnapshot{
		StartedAt:  t0.UTC().Format(time.RFC3339Nano),
		DurationMs: float64(time.Since(t0).Microseconds()) / 1000.0,
	}
}

// rawJSON returns b as a RawMessage if it is valid JSON; otherwise quotes it
// as a JSON string so the payload stays parseable.
func rawJSON(b []byte) json.RawMessage {
	if len(b) == 0 {
		return nil
	}
	if json.Valid(b) {
		return json.RawMessage(b)
	}
	q, _ := json.Marshal(string(b))
	return json.RawMessage(q)
}

// safeHeaders returns a copy of h with sensitive values redacted.
func safeHeaders(h http.Header) map[string]string {
	out := make(map[string]string, len(h))
	for k, vs := range h {
		switch strings.ToLower(k) {
		case "authorization", "x-api-key", "cookie", "set-cookie", "proxy-authorization":
			out[k] = "[redacted]"
		default:
			out[k] = strings.Join(vs, ", ")
		}
	}
	return out
}

// director returns an httputil.ReverseProxy Director that rewrites the request
// for the given backend. If pathOverride is non-empty, it replaces the request
// path (used by /v1/completions to force the backend path).
func director(target *url.URL, backend *config.Backend, body []byte, pathOverride string) func(*http.Request) {
	return func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
		if pathOverride != "" {
			req.URL.Path = pathOverride
		}
		// When pathOverride is empty, req.URL.Path is kept as-is — the proxy
		// receives /v1/chat/completions and forwards it unchanged. base_url
		// should be scheme+host only (e.g. http://localhost:3022).

		// Auth headers — auth_type overrides the default for the backend type.
		// Default: "x-api-key" for anthropic, "bearer" for openai.
		// Explicit "bearer" is needed for OAuth tokens on Anthropic.
		if backend.APIKey != "" {
			authType := backend.AuthType
			if authType == "" {
				if backend.Type == "anthropic" {
					authType = "x-api-key"
				} else {
					authType = "bearer"
				}
			}
			if authType == "x-api-key" {
				req.Header.Set("x-api-key", backend.APIKey)
			} else {
				req.Header.Set("Authorization", "Bearer "+backend.APIKey)
			}
			// Anthropic always needs a version header
			if backend.Type == "anthropic" && req.Header.Get("anthropic-version") == "" {
				req.Header.Set("anthropic-version", "2023-06-01")
			}
		}

		req.Body = io.NopCloser(bytes.NewReader(body))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Del("Transfer-Encoding")
	}
}

// logMessageContent logs full text content of each message at L4.
func logMessageContent(rid string, body map[string]interface{}) {
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
func (s *Server) modifyResponse(rid, backendID, virtualModel, realModel, path, backendType string, timeoutSec int, streaming bool, t0 time.Time, ctx context.Context, capCtx *captureCtx) func(*http.Response) error {
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
			var sseBuf *capture.CappedBuffer
			var teeWriter io.Writer
			if capCtx != nil {
				sseBuf = &capture.CappedBuffer{Max: capture.MaxResponseBytes}
				teeWriter = sseBuf
			}
			status := resp.StatusCode
			resp.Body = &interceptedBody{
				ReadCloser: resp.Body,
				parser:     parser,
				tee:        teeWriter,
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
					if capCtx != nil {
						capCtx.finishSuccess(t0, capture.ResponseSnapshot{
							StatusCode: status,
							SSE:        sseBuf.String(),
							Truncated:  sseBuf.Truncated,
						})
					}
				},
			}
		} else {
			// Non-streaming: buffer to extract usage, then restore.
			data, err := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if err != nil {
				if capCtx != nil {
					capCtx.finishError(t0, fmt.Sprintf("read upstream body: %v", err))
				}
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

			if capCtx != nil {
				truncated := false
				bodyForCap := data
				if len(bodyForCap) > capture.MaxResponseBytes {
					bodyForCap = bodyForCap[:capture.MaxResponseBytes]
					truncated = true
				}
				capCtx.finishSuccess(t0, capture.ResponseSnapshot{
					StatusCode: resp.StatusCode,
					Body:       rawJSON(bodyForCap),
					Truncated:  truncated,
				})
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

// handleCompletions forwards /v1/completions requests to the backend's
// /v1/completions endpoint using the same reverse-proxy infrastructure as
// handleProxy. FIM tokens pass through untouched — no format translation.
func (s *Server) handleCompletions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{
		pathOverride: "/v1/completions",
		protocol:     "openai",
	})
}

// handleEmbeddings forwards /v1/embeddings requests to the backend.
func (s *Server) handleEmbeddings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.proxyRequest(w, r, proxyOpts{
		pathOverride: "/v1/embeddings",
		protocol:     "openai",
	})
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]interface{}{
		"error": map[string]string{"message": msg, "type": "proxy_error"},
	})
}
