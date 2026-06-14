// Package tracing is the opt-in OpenTelemetry seam (ADR-011). It owns the OTel
// SDK import (Init builds the OTLP exporter + TracerProvider) and exposes small
// request-span helpers so the rest of the gateway depends only on this package,
// not the SDK. When Init is not called, every helper is a cheap no-op (the
// default tracer is the library no-op and an `enabled` guard skips header/context
// work), so a deployment without an `otel` config is byte-for-byte unchanged.
//
// It defines its OWN Config (mirror) so it never imports internal/config — the
// assembly maps config.OTelConfig → tracing.Config.
package tracing

import (
	"context"
	"net/http"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

const scopeName = "github.com/inferplane/inferplane"

// Config mirrors config.OTelConfig (so tracing imports no config). SampleRatio
// nil → 1.0; explicit 0.0 → none.
type Config struct {
	Endpoint    string
	Protocol    string // "" | "http" | "grpc"
	Insecure    bool
	SampleRatio *float64
	ServiceName string
}

var (
	enabled bool
	// tracer defaults to the library no-op, so Start works (cheaply) before Init.
	tracer trace.Tracer                  = tracenoop.NewTracerProvider().Tracer(scopeName)
	prop   propagation.TextMapPropagator = propagation.TraceContext{}
)

// Init installs the OTLP exporter + TracerProvider and the W3C propagator, and
// returns the provider's Shutdown (the caller flushes it on teardown under a
// bounded context). An unreachable collector is non-fatal — the OTLP exporter
// connects lazily and the batch processor isolates export failures from serving.
func Init(ctx context.Context, cfg Config) (func(context.Context) error, error) {
	var (
		exp sdktrace.SpanExporter
		err error
	)
	switch cfg.Protocol {
	case "grpc":
		opts := []otlptracegrpc.Option{otlptracegrpc.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracegrpc.WithInsecure())
		}
		exp, err = otlptracegrpc.New(ctx, opts...)
	default: // "" | "http"
		opts := []otlptracehttp.Option{otlptracehttp.WithEndpoint(cfg.Endpoint)}
		if cfg.Insecure {
			opts = append(opts, otlptracehttp.WithInsecure())
		}
		exp, err = otlptracehttp.New(ctx, opts...)
	}
	if err != nil {
		return nil, err
	}

	ratio := 1.0
	if cfg.SampleRatio != nil {
		ratio = *cfg.SampleRatio
	}
	svc := cfg.ServiceName
	if svc == "" {
		svc = "inferplane"
	}
	res := resource.NewSchemaless(attribute.String("service.name", svc))
	tp := sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(exp),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.TraceIDRatioBased(ratio))),
		sdktrace.WithResource(res),
	)
	SetTracerProvider(tp)
	return tp.Shutdown, nil
}

// SetTracerProvider installs an explicit TracerProvider and enables tracing.
// Init uses it; tests in other packages use it with a recorder-backed provider.
func SetTracerProvider(tp trace.TracerProvider) {
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(prop)
	tracer = tp.Tracer(scopeName)
	enabled = true
}

// Disable reverts to the no-op tracer (test cleanup).
func Disable() {
	tracer = tracenoop.NewTracerProvider().Tracer(scopeName)
	enabled = false
}

// Enabled reports whether tracing is installed (false → no-op fast path).
func Enabled() bool { return enabled }

// Extract joins an incoming W3C trace (traceparent) so the gateway span becomes
// a child of the client's trace. No-op (returns ctx) when tracing is off.
func Extract(ctx context.Context, h http.Header) context.Context {
	if !enabled {
		return ctx
	}
	return prop.Extract(ctx, propagation.HeaderCarrier(h))
}

// Start begins a server span (no-op span when tracing is off).
func Start(ctx context.Context, name string) (context.Context, trace.Span) {
	return tracer.Start(ctx, name, trace.WithSpanKind(trace.SpanKindServer))
}

// Inject writes the current trace context as a traceparent header into dst. The
// caller MUST pass a CLONE of the upstream headers (never the shared inbound
// map). No-op when tracing is off (dst is left untouched).
func Inject(ctx context.Context, dst http.Header) {
	if !enabled {
		return
	}
	prop.Inject(ctx, propagation.HeaderCarrier(dst))
}

// TraceID returns the 32-hex trace id in ctx, or "" if none (off / no span).
func TraceID(ctx context.Context) string {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.HasTraceID() {
		return ""
	}
	return sc.TraceID().String()
}

// SetGenAIRequest sets the request-side GenAI attributes, known at span start
// (the provider/system is only known after routing → SetGenAIResponse).
func SetGenAIRequest(span trace.Span, model string) {
	span.SetAttributes(
		attribute.String("gen_ai.operation.name", "chat"),
		attribute.String("gen_ai.request.model", model),
	)
}

// SetGenAIResponse sets the response-side GenAI attributes: the provider system,
// the resolved upstream model, and token usage (set once the target is known).
func SetGenAIResponse(span trace.Span, system, model string, inputTokens, outputTokens int64) {
	if system != "" {
		span.SetAttributes(attribute.String("gen_ai.system", system))
	}
	span.SetAttributes(attribute.String("gen_ai.response.model", model))
	if inputTokens > 0 {
		span.SetAttributes(attribute.Int64("gen_ai.usage.input_tokens", inputTokens))
	}
	if outputTokens > 0 {
		span.SetAttributes(attribute.Int64("gen_ai.usage.output_tokens", outputTokens))
	}
}

// SetStatus marks the span ok or error (set Error only on a terminal outcome).
func SetStatus(span trace.Span, ok bool, desc string) {
	if ok {
		span.SetStatus(codes.Ok, "")
		return
	}
	span.SetStatus(codes.Error, desc)
}
