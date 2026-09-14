package telemetry

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	tracesdk "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
)

const defaultShutdownTimeout = 5 * time.Second

// Config contains non-sensitive telemetry settings. Endpoint must be an OTLP
// HTTP base URL (for example, http://collector:4318); Headers are copied and
// passed to the exporter without being recorded in spans.
type Config struct {
	ServiceName     string
	ServiceVersion  string
	Endpoint        string
	Headers         map[string]string
	ShutdownTimeout time.Duration
	ExportTimeout   time.Duration
}

// Provider owns the process tracer provider and its exporter lifecycle.
type Provider struct {
	provider        *tracesdk.TracerProvider
	shutdownTimeout time.Duration
}

// New installs a process tracer provider and W3C Trace Context propagator.
// An empty Endpoint intentionally keeps export disabled while preserving the
// normal SDK provider for callers that install an in-memory processor in tests.
func New(cfg Config) (*Provider, error) {
	if cfg.ServiceName == "" {
		cfg.ServiceName = "az-agent-platform"
	}
	if cfg.ShutdownTimeout <= 0 {
		cfg.ShutdownTimeout = defaultShutdownTimeout
	}
	if cfg.ExportTimeout <= 0 {
		cfg.ExportTimeout = 10 * time.Second
	}

	resource, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			semconv.ServiceName(cfg.ServiceName),
			semconv.ServiceVersion(cfg.ServiceVersion),
		),
	)
	if err != nil {
		return nil, err
	}
	options := []tracesdk.TracerProviderOption{tracesdk.WithResource(resource)}
	if cfg.Endpoint != "" {
		exporterOptions := []otlptracehttp.Option{
			otlptracehttp.WithHeaders(cloneHeaders(cfg.Headers)),
			otlptracehttp.WithTimeout(cfg.ExportTimeout),
		}
		exporterOptions = append(exporterOptions, otlptracehttp.WithEndpointURL(cfg.Endpoint))
		exporterCtx, cancel := context.WithTimeout(context.Background(), cfg.ExportTimeout)
		defer cancel()
		exporter, err := otlptracehttp.New(exporterCtx, exporterOptions...)
		if err != nil {
			return nil, err
		}
		options = append(options, tracesdk.WithBatcher(exporter))
	}
	provider := tracesdk.NewTracerProvider(options...)
	otel.SetTracerProvider(provider)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	return &Provider{provider: provider, shutdownTimeout: cfg.ShutdownTimeout}, nil
}

// Shutdown flushes spans within the configured bound. A caller deadline can
// shorten the bound, but never extend it.
func (p *Provider) Shutdown(ctx context.Context) error {
	if p == nil || p.provider == nil {
		return nil
	}
	shutdownCtx, cancel := context.WithTimeout(ctx, p.shutdownTimeout)
	defer cancel()
	return p.provider.Shutdown(shutdownCtx)
}

func cloneHeaders(headers map[string]string) map[string]string {
	if len(headers) == 0 {
		return nil
	}
	copy := make(map[string]string, len(headers))
	for key, value := range headers {
		copy[key] = value
	}
	return copy
}
