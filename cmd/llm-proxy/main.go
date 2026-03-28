package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/wentbackward/llm-proxy/internal/config"
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

	// ── Graceful shutdown ──────────────────────────────────────────────────
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGINT, syscall.SIGTERM)
	<-quit

	log.Println("[llm-proxy] shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	proxyServer.Shutdown(ctx)
	if metricsServer != nil {
		metricsServer.Shutdown(ctx)
	}
}
