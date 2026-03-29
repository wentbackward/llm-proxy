package journal

import (
	"context"
	"fmt"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/stdout/stdoutlog"
	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
)

// Journal emits structured request analysis as OTel log records.
type Journal struct {
	logger   otellog.Logger
	provider *sdklog.LoggerProvider
}

// New creates a Journal backed by OTel log exporters.
// It always exports to stdout. If otlpEndpoint is non-empty, it also
// exports to an OTLP collector at that address.
func New(otlpEndpoint string) (*Journal, error) {
	stdoutExp, err := stdoutlog.New()
	if err != nil {
		return nil, fmt.Errorf("journal stdout exporter: %w", err)
	}

	opts := []sdklog.LoggerProviderOption{
		sdklog.WithProcessor(sdklog.NewBatchProcessor(stdoutExp)),
	}

	if otlpEndpoint != "" {
		otlpExp, err := otlploghttp.New(context.Background(),
			otlploghttp.WithEndpoint(otlpEndpoint),
			otlploghttp.WithInsecure(),
		)
		if err != nil {
			return nil, fmt.Errorf("journal OTLP exporter: %w", err)
		}
		opts = append(opts, sdklog.WithProcessor(sdklog.NewBatchProcessor(otlpExp)))
	}

	provider := sdklog.NewLoggerProvider(opts...)
	logger := provider.Logger("llm-proxy-journal")

	return &Journal{
		logger:   logger,
		provider: provider,
	}, nil
}

// Log emits an Entry as a structured OTel log record.
func (j *Journal) Log(ctx context.Context, e Entry) {
	var rec otellog.Record
	rec.SetBody(otellog.StringValue("llm_request"))
	rec.AddAttributes(
		otellog.String("request_id", e.RequestID),
		otellog.String("virtual_model", e.VirtualModel),
		otellog.String("real_model", e.RealModel),
		otellog.String("backend", e.Backend),
		otellog.String("protocol", e.Protocol),
		otellog.Bool("streaming", e.Streaming),

		otellog.Int("message_count", e.MessageCount),
		otellog.Int("system_chars", e.SystemChars),
		otellog.Int("last_user_chars", e.LastUserChars),
		otellog.Int("total_chars", e.TotalChars),
		otellog.Int("est_tokens", e.EstTokens),

		otellog.Bool("has_tools", e.HasTools),
		otellog.Bool("has_tool_choice", e.HasToolChoice),
		otellog.Int("code_fences", e.CodeFences),
		otellog.Int("json_blocks", e.JSONBlocks),
		otellog.Bool("is_multimodal", e.IsMultimodal),
	)

	// Add params as individual attributes
	for k, v := range e.Params {
		key := "params." + k
		switch val := v.(type) {
		case float64:
			rec.AddAttributes(otellog.Float64(key, val))
		case bool:
			rec.AddAttributes(otellog.Bool(key, val))
		case string:
			rec.AddAttributes(otellog.String(key, val))
		case int:
			rec.AddAttributes(otellog.Int(key, val))
		}
	}

	j.logger.Emit(ctx, rec)
}

// Shutdown flushes pending log records. Call on graceful exit.
func (j *Journal) Shutdown(ctx context.Context) error {
	return j.provider.Shutdown(ctx)
}
