package tracing

import (
	"context"
	"net/http"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// enableRecorder installs an in-memory span recorder (offline — no exporter) and
// flips the package into the enabled state, mimicking a real Init.
func enableRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()
	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	otel.SetTracerProvider(tp)
	otel.SetTextMapPropagator(propagation.TraceContext{})
	oldTracer, oldEnabled := tracer, enabled
	tracer = tp.Tracer(scopeName)
	enabled = true
	t.Cleanup(func() { tracer, enabled = oldTracer, oldEnabled })
	return sr
}

func TestSpanRecordedWithGenAIAttrs(t *testing.T) {
	sr := enableRecorder(t)
	ctx, span := Start(context.Background(), "chat m")
	SetGenAIRequest(span, "claude")
	SetGenAIResponse(span, "anthropic", "claude-up", 10, 20)
	SetStatus(span, true, "")
	if tid := TraceID(ctx); len(tid) != 32 {
		t.Fatalf("TraceID = %q, want 32-hex", tid)
	}
	span.End()

	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 span, got %d", len(ended))
	}
	attrs := map[string]string{}
	ints := map[string]int64{}
	for _, a := range ended[0].Attributes() {
		if a.Value.Type().String() == "INT64" {
			ints[string(a.Key)] = a.Value.AsInt64()
		} else {
			attrs[string(a.Key)] = a.Value.AsString()
		}
	}
	if attrs["gen_ai.operation.name"] != "chat" || attrs["gen_ai.system"] != "anthropic" ||
		attrs["gen_ai.request.model"] != "claude" || attrs["gen_ai.response.model"] != "claude-up" {
		t.Fatalf("gen_ai attrs wrong: %v", attrs)
	}
	if ints["gen_ai.usage.input_tokens"] != 10 || ints["gen_ai.usage.output_tokens"] != 20 {
		t.Fatalf("usage attrs wrong: %v", ints)
	}
}

// TestDisabledNoOp pins the zero-overhead fast path: when tracing is off, Inject
// leaves headers untouched and TraceID is empty.
func TestDisabledNoOp(t *testing.T) {
	// default package state: enabled == false
	if Enabled() {
		t.Skip("tracing already enabled by another test ordering")
	}
	h := http.Header{}
	Inject(context.Background(), h)
	if len(h) != 0 {
		t.Fatalf("disabled Inject mutated headers: %v", h)
	}
	if TraceID(context.Background()) != "" {
		t.Fatal("disabled TraceID must be empty")
	}
	ctx, span := Start(context.Background(), "x")
	span.End() // no-op span — must not panic
	if TraceID(ctx) != "" {
		t.Fatal("no-op span must have no trace id")
	}
}

func TestExtractInjectRoundTrip(t *testing.T) {
	enableRecorder(t)
	// inbound traceparent → Extract → Start child → Inject → same trace id
	in := http.Header{}
	in.Set("traceparent", "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01")
	ctx := Extract(context.Background(), in)
	ctx, span := Start(ctx, "chat m")
	defer span.End()
	if got := TraceID(ctx); got != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("did not join inbound trace: %q", got)
	}
	out := http.Header{}
	Inject(ctx, out)
	if tp := out.Get("traceparent"); len(tp) < 55 || tp[3:35] != "0af7651916cd43dd8448eb211c80319c" {
		t.Fatalf("injected traceparent wrong: %q", tp)
	}
}
