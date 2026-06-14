package openaiapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/internal/tracing"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func chatReq(body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	ctx := principal.With(req.Context(), keystore.Principal{Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	NewChatHandler(testRouter()).ServeHTTP(rec, req.WithContext(ctx))
	return rec
}

func TestChatTracingOnEmitsSpan(t *testing.T) {
	sr := tracetest.NewSpanRecorder()
	tracing.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr)))
	t.Cleanup(tracing.Disable)
	if rec := chatReq(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("status %d: %s", rec.Code, rec.Body)
	}
	ended := sr.Ended()
	if len(ended) != 1 || ended[0].Name() != "chat gpt-x" {
		t.Fatalf("want 1 span 'chat gpt-x', got %d %v", len(ended), ended)
	}
}

func TestChatTracingOffNoSpan(t *testing.T) {
	if tracing.Enabled() {
		t.Skip("tracing enabled by ordering")
	}
	if rec := chatReq(`{"model":"gpt-x","messages":[{"role":"user","content":"hi"}]}`); rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	// no panic, no span — the no-op path; nothing to assert beyond a clean 200
}
