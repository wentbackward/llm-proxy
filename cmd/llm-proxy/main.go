package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
	"github.com/wentbackward/llm-proxy/internal/logger"
	"github.com/wentbackward/llm-proxy/internal/proxy"
	"github.com/wentbackward/llm-proxy/internal/telemetry"
)

func main() {
	cfgPath := os.Getenv("CONFIG_PATH")
	if cfgPath == "" {
		cfgPath = "config.yaml"
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		log.Fatalf("config: %v", err)
	}

	metrics, metricsHandler, err := telemetry.Init()
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// ── Startup backend probes ─────────────────────────────────────────────
	probeBackends(cfg)

	// ── Proxy server ───────────────────────────────────────────────────────
	proxyMux := http.NewServeMux()
	proxy.New(cfg, metrics).RegisterRoutes(proxyMux)

	proxyAddr := fmt.Sprintf("%s:%d", cfg.Server.Host, cfg.Server.Port)
	proxyServer := &http.Server{
		Addr:         proxyAddr,
		Handler:      proxyMux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // disabled — streaming responses can take minutes
		IdleTimeout:  120 * time.Second,
	}

	// ── Metrics server ─────────────────────────────────────────────────────
	var metricsServer *http.Server
	if cfg.Telemetry.Prometheus.Enabled {
		metricsMux := http.NewServeMux()
		metricsMux.Handle(cfg.Telemetry.Prometheus.Path, metricsHandler)
		metricsServer = &http.Server{
			Addr:        fmt.Sprintf(":%d", cfg.Telemetry.Prometheus.Port),
			Handler:     metricsMux,
			ReadTimeout: 5 * time.Second,
		}
	}

	// ── Start ──────────────────────────────────────────────────────────────
	go func() {
		tls := cfg.Server.TLS
		if tls.Cert != "" && tls.Key != "" {
			log.Printf("[llm-proxy] listening on %s (TLS)", proxyAddr)
			if err := proxyServer.ListenAndServeTLS(tls.Cert, tls.Key); err != http.ErrServerClosed {
				log.Fatalf("proxy server: %v", err)
			}
		} else {
			log.Printf("[llm-proxy] listening on %s", proxyAddr)
			if err := proxyServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Fatalf("proxy server: %v", err)
			}
		}
	}()

	if metricsServer != nil {
		go func() {
			log.Printf("[llm-proxy] metrics on :%d%s",
				cfg.Telemetry.Prometheus.Port, cfg.Telemetry.Prometheus.Path)
			if err := metricsServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Fatalf("metrics server: %v", err)
			}
		}()
	}

	// ── Signal handling ────────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	hup := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	signal.Notify(hup, syscall.SIGHUP)

	go func() {
		for range hup {
			log.Println("[llm-proxy] SIGHUP received — reloading log level and re-probing backends")
			logger.Reload()
			probeBackends(cfg)
		}
	}()

	<-quit

	log.Println("[llm-proxy] shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proxyServer.Shutdown(ctx)
	if metricsServer != nil {
		metricsServer.Shutdown(ctx)
	}
}

type vmodel struct {
	virtual string
	real    string
}

// probeBackends checks each configured backend at startup, logs its status
// and the virtual models that map to it.
func probeBackends(cfg *config.Config) {
	// Build backend → virtual models index
	byBackend := make(map[string][]vmodel, len(cfg.Backends))
	for _, r := range cfg.Routes {
		if r.AutoRoute != nil {
			continue
		}
		byBackend[r.Backend] = append(byBackend[r.Backend], vmodel{r.VirtualModel, r.RealModel})
	}

	client := &http.Client{Timeout: 10 * time.Second}

	for _, b := range cfg.Backends {
		url := b.BaseURL + "/v1/models"
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			log.Printf("[probe] backend %-12s ERROR building request: %v", b.ID, err)
			continue
		}
		if b.APIKey != "" {
			req.Header.Set("Authorization", "Bearer "+b.APIKey)
		}

		resp, err := client.Do(req)
		if err != nil {
			log.Printf("[probe] backend %-12s UNREACHABLE: %v", b.ID, err)
			logVirtualModels(byBackend[b.ID])
			continue
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			log.Printf("[probe] backend %-12s HTTP %d", b.ID, resp.StatusCode)
			logVirtualModels(byBackend[b.ID])
			continue
		}

		body, _ := io.ReadAll(resp.Body)
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		upstreamModels := []string{}
		if json.Unmarshal(body, &result) == nil {
			for _, m := range result.Data {
				upstreamModels = append(upstreamModels, m.ID)
			}
		}

		log.Printf("[probe] backend %-12s OK  upstream models: %v", b.ID, upstreamModels)
		logVirtualModels(byBackend[b.ID])
	}
}

func logVirtualModels(vmodels []vmodel) {
	for _, vm := range vmodels {
		log.Printf("[probe]   → %-30s (real: %s)", vm.virtual, vm.real)
	}
}
