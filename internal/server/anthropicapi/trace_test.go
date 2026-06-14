package anthropicapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/tracing"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestTracingOffNoOp pins the zero-overhead invariant: with tracing disabled the
// response is unchanged and the upstream headers carry no traceparent.
func TestTracingOffNoOp(t *testing.T) {
	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("t", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	if rec.last.Headers.Get("traceparent") != "" {
		t.Fatal("tracing off must not inject traceparent")
	}
}

// TestTracingOnEmitsSpan pins the on-path: a span is exported with the GenAI
// request attribute, and the upstream headers carry a traceparent (correlation).
func TestTracingOnEmitsSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tracing.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(tracing.Disable)

	rec := &recProvider{}
	h := NewMessagesHandler(recRouter(rec))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, maskedReq("t", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if rr.Code != 200 {
		t.Fatalf("status %d", rr.Code)
	}
	// upstream got a traceparent (cloned header — inbound req unaffected)
	if tp := rec.last.Headers.Get("traceparent"); !strings.HasPrefix(tp, "00-") {
		t.Fatalf("upstream missing traceparent: %q", tp)
	}
	ended := sr.Ended()
	if len(ended) != 1 {
		t.Fatalf("want 1 span, got %d", len(ended))
	}
	span := ended[0]
	if span.Name() != "chat m" {
		t.Fatalf("span name = %q", span.Name())
	}
	attrs := map[string]string{}
	for _, a := range span.Attributes() {
		attrs[string(a.Key)] = a.Value.AsString()
	}
	if attrs["gen_ai.request.model"] != "m" || attrs["gen_ai.operation.name"] != "chat" {
		t.Fatalf("gen_ai request attrs wrong: %v", attrs)
	}
	if attrs["gen_ai.response.model"] != "up" {
		t.Fatalf("gen_ai.response.model = %q, want up", attrs["gen_ai.response.model"])
	}
}
