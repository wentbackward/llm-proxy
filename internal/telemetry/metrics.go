// Package telemetry initialises OpenTelemetry metrics with a Prometheus exporter.
package telemetry

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/prometheus"
	"go.opentelemetry.io/otel/metric"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
)

// Metrics holds all instrumentation handles for the proxy.
type Metrics struct {
	RequestDuration  metric.Float64Histogram
	TTFT             metric.Float64Histogram
	PromptTokens     metric.Int64Counter
	CompletionTokens metric.Int64Counter
	ActiveRequests   metric.Int64UpDownCounter
	RequestsTotal    metric.Int64Counter
}

// Init creates the OTel meter provider backed by Prometheus and returns the
// populated Metrics struct together with the HTTP handler to expose /metrics.
func Init() (*Metrics, http.Handler, error) {
	exporter, err := prometheus.New()
	if err != nil {
		return nil, nil, err
	}

	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))
	meter := provider.Meter("llm-proxy")

	reqDur, err := meter.Float64Histogram("llm_request_duration_seconds",
		metric.WithDescription("End-to-end request duration in seconds"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 30, 60, 120, 300),
	)
	if err != nil {
		return nil, nil, err
	}

	ttft, err := meter.Float64Histogram("llm_time_to_first_token_seconds",
		metric.WithDescription("Seconds from request start to first streamed content token"),
		metric.WithExplicitBucketBoundaries(0.5, 1, 2, 5, 10, 20, 30, 45, 60, 90, 120, 180, 300),
	)
	if err != nil {
		return nil, nil, err
	}

	promptTok, err := meter.Int64Counter("llm_prompt_tokens_total",
		metric.WithDescription("Cumulative prompt/input tokens processed"),
	)
	if err != nil {
		return nil, nil, err
	}

	completionTok, err := meter.Int64Counter("llm_completion_tokens_total",
		metric.WithDescription("Cumulative completion/output tokens generated"),
	)
	if err != nil {
		return nil, nil, err
	}

	active, err := meter.Int64UpDownCounter("llm_active_requests",
		metric.WithDescription("Number of requests currently in flight"),
	)
	if err != nil {
		return nil, nil, err
	}

	total, err := meter.Int64Counter("llm_requests_total",
		metric.WithDescription("Total requests by backend, model and HTTP status"),
	)
	if err != nil {
		return nil, nil, err
	}

	return &Metrics{
		RequestDuration:  reqDur,
		TTFT:             ttft,
		PromptTokens:     promptTok,
		CompletionTokens: completionTok,
		ActiveRequests:   active,
		RequestsTotal:    total,
	}, promhttp.Handler(), nil
}

// Attrs returns a MeasurementOption carrying the standard label set.
func Attrs(backend, model, status string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("model", model),
		attribute.String("status", status),
	)
}

// BackendAttrs returns a MeasurementOption for metrics that don't track status.
func BackendAttrs(backend, model string) metric.MeasurementOption {
	return metric.WithAttributes(
		attribute.String("backend", backend),
		attribute.String("model", model),
	)
}
