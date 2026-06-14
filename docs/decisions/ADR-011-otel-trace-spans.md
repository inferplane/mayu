# ADR-011: OpenTelemetry trace spans (GenAI conventions), opt-in

**Date:** 2026-06-14
**Status:** Accepted — 3-family design gate (codex + gemini + kiro, all
CHANGES-REQUIRED; architecture sound, refinements folded in below): span owned
once outside the fallback loop with `defer End()` + error-status-only-on-terminal
+ token attrs threaded via request context; per-attempt header clone (no shared-
map mutation); explicit no-op fast path; nullable sample_ratio (0.0 ≠ unset) +
[0,1] validation; `tracing.Config` mirror (no config import into tracing);
bounded shutdown flush.
**Related:** CLAUDE.md (OTel GenAI semconv for metric naming — already adopted),
audit `trace_id` (reserved field), ADR-006 (assembly lifecycle), spec §6
(observability), LiteLLM parity (OTLP + `gen_ai.*`)

## Context

The audit record already reserves a `trace_id` field "for v0.2 OTel", and the
metrics layer already names series by the **OTel GenAI semantic conventions**.
The missing piece is **distributed tracing**: emitting spans for each LLM request
so operators can correlate a gateway request with the upstream call (and with the
client's own trace) in their tracing backend. LiteLLM follows OpenTelemetry —
OTLP export + `gen_ai.*` span attributes — so this is table-stakes parity.

Constraint: inferplane is a **single static pure-Go binary with no external SaaS
dependency**. The OTel Go SDK is **pure-Go** (CGO-free), and OTLP exports to the
**operator's own collector** (not a SaaS), so real OTel tracing is compatible
with that identity — the only cost is `go.sum` weight, which is acceptable for
parity with the dominant alternative and consistency with our existing
GenAI-semconv metrics.

## Decision

**Opt-in OpenTelemetry tracing: real OTLP span export, GenAI semantic
conventions, W3C trace-context propagation — and a no-op default so the
zero-config single binary is unchanged.**

### 1. Opt-in config; no-op when absent

```json
"otel": { "endpoint": "otel-collector:4318", "protocol": "http",
          "insecure": true, "sample_ratio": 1.0, "service_name": "inferplane" }
```

- **Absent (default):** no TracerProvider is installed; the OTel global is the
  library's **no-op tracer** → zero spans, zero overhead, `trace_id` stays
  `null`. The single binary boots and runs exactly as today.
- **Present:** the assembly installs an OTLP exporter (`otlptracehttp` by default
  — lighter than gRPC; `protocol:"grpc"` selects `otlptracegrpc`), a batch
  span processor, a parent-based ratio sampler (`sample_ratio`, default 1.0), and
  the W3C **TraceContext propagator**. The provider is `Shutdown`-flushed on
  serve teardown (in-flight spans are exported before exit).

`internal/tracing` owns the OTel SDK import (Init/Shutdown + small request-span
helpers); handlers use those helpers, so the SDK dependency stays contained.
`tracing` defines its **own `Config` mirror type** — it does NOT import
`internal/config` (the assembly maps `config.OTelConfig` → `tracing.Config`),
keeping the leaf clean. `sample_ratio` is a **nullable `*float64`**: unset → 1.0,
explicit `0.0` → sample none (the two are distinguishable), validated to `[0,1]`.
`Init` returns a shutdown func; an unreachable collector at boot is **non-fatal**
(the OTLP exporter connects lazily / the batch processor isolates export
failures), and `shutdown` runs under a **bounded timeout** on teardown with
errors logged, never surfaced as a serving failure.

### 2. One span per generative request, GenAI attributes

`/v1/messages` and `/v1/chat/completions` start a server span named
`chat {model}` carrying the OTel GenAI conventions:

- `gen_ai.operation.name = "chat"`, `gen_ai.system = <provider type>`,
  `gen_ai.request.model`, `gen_ai.response.model` (the upstream id),
  `gen_ai.usage.input_tokens` / `gen_ai.usage.output_tokens` (when settled),
  and the span status from the outcome.

**Span lifecycle (gate — all three reviewers).** The span is started **once in
`ServeHTTP`** right after the model is parsed, with **`defer span.End()`** so
every early return (401/403/404/429, mask-reject) closes it exactly once and is
recorded as a (failed) trace. It is owned **outside the fallback loop**, threaded
to `serveComplete`/`serveStream` via `req = req.WithContext(ctx)` +
`trace.SpanFromContext` (so they set `gen_ai.response.model` + usage on the same
span); a streaming response's span thus ends only after the stream drains. The
span status is set to **Error only on a TERMINAL outcome** (`retriable == false`)
— a pre-TTFT failure that succeeds via fallback must NOT leave the trace red.

**Header injection is per-attempt and clone-only (gate, codex/kiro).** The
`traceparent` is injected into a **clone** of the upstream headers per attempt —
never into the shared inbound `req.Header` — so injection cannot mutate the
client's header map or bleed across fallback attempts. The body is untouched
(only the header; §4.4 cache key is body-prefix-based).

**Explicit no-op fast path (gate, codex).** When tracing is disabled the
`tracing` helpers short-circuit before any work: no span, no header mutation, no
audit `trace_id` — the request path is byte-identical. (Beyond the library's
no-op tracer, an `enabled` guard makes "off" provably zero-effect.)
- **`count_tokens` is NOT spanned**: it is high-volume and must never fail
  (a span error path there is needless risk); metrics already cover it.
- **Span work never affects the response**: a tracing failure is swallowed — the
  request path is identical with or without a working exporter (spans are
  best-effort observability, never on the critical path).

### 3. Propagation + `trace_id` in audit

- **Incoming**: the W3C `traceparent` header is extracted, so a gateway span
  **joins the client's existing trace** when present (one trace end-to-end).
- **Outgoing**: `traceparent` is injected into the upstream provider request
  headers (allowed by the cache invariant — headers don't affect the cache key,
  §4.4), so the provider call correlates under the same trace.
- **Audit**: the active span's trace id (32-hex) fills the reserved
  `Record.TraceID`, linking every audit record to its trace. When tracing is off,
  `trace_id` is `null` (unchanged).

## Alternatives considered

1. **Dependency-free W3C trace-context only (propagate + `trace_id`, no SDK).**
   Rejected as the primary: it gives correlation but **no spans to export**, so
   it is not OTel-parity with LiteLLM and leaves the operator to reconstruct
   spans from logs. (The propagation half is kept — decision §3 — but with real
   span export on top.)
2. **gRPC OTLP exporter by default.** Rejected as default — `otlptracegrpc` pulls
   `google.golang.org/grpc` (heavier); `otlptracehttp` is lighter and sufficient.
   gRPC remains available via `protocol:"grpc"` for operators who want it.
3. **Always-on tracing.** Rejected — it would force the exporter/runtime cost on
   every deployment; tracing is opt-in, no-op by default (the single-binary
   zero-config experience is unchanged).
4. **Vendor callbacks (Langfuse/Datadog/…) like LiteLLM's wide ecosystem.**
   Out of scope — OTLP to a collector is the open standard; vendor backends
   consume OTLP downstream. We ship the standard, not N integrations.
5. **Span `count_tokens` too.** Rejected — high-volume, must-never-fail path;
   the span adds risk and noise for little value.

## Consequences

- Operators get real OpenTelemetry spans (GenAI semconv) exported to their own
  collector, end-to-end-correlated with the client trace and the upstream call —
  LiteLLM-parity tracing — while the binary stays pure-Go and SaaS-free.
- Zero behavior change when `otel` is absent (no-op tracer, no deps active at
  runtime); the dependency is compiled in but inert.
- `trace_id` in the audit chain links audit ↔ traces.
- `go.sum` grows by the OTel SDK + OTLP/HTTP exporter tree (all pure-Go).
- A tracing/exporter outage never affects request serving (best-effort, off the
  critical path).
