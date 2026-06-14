# Plan: OpenTelemetry trace spans (roadmap #5b)

**Date:** 2026-06-14
**Related:** ADR-011 (this plan), audit `trace_id`, CLAUDE.md (OTel GenAI semconv)
**Base:** main @ b388636 · **Produces:** opt-in OTLP tracing

## Goal

Opt-in OpenTelemetry tracing: real OTLP span export with GenAI semantic
conventions + W3C trace-context propagation + `trace_id` in the audit chain —
**no-op by default** (zero spans, zero overhead, deps inert) so the single
pure-Go binary is unchanged. LiteLLM-parity, SaaS-free (exports to the operator's
own collector).

## Core architecture (from ADR-011)

- **`internal/tracing`** owns the OTel SDK import: `Init(cfg) (shutdown, error)`
  (OTLP `http` default / `grpc` opt; batch processor; parent-based ratio sampler;
  W3C TraceContext propagator; sets the global TracerProvider) + small helpers
  `StartChat`, `SetGenAIRequest/Response`, `InjectTraceparent`, `TraceID`. Absent
  config → no Init → OTel global no-op tracer.
- **Handlers** (`/v1/messages`, `/v1/chat/completions`) start one server span
  `chat {model}`, set `gen_ai.*` attributes + status, inject `traceparent` into
  the upstream request headers, and fill audit `Record.TraceID` from the span.
  **Span work is best-effort — never affects the response.**
- **`count_tokens` is not spanned** (high-volume, must-never-fail).
- **Assembly** calls `tracing.Init` at boot (when configured) and `shutdown` on
  serve teardown (flush in-flight spans).

## Hard safety invariants (the gate's checklist)

- **Span started ONCE, defer-ended, terminal-error-only** (gate, all three):
  `ServeHTTP` starts the span after parsing the model, `defer span.End()` (so
  every early return closes it once); the span is owned OUTSIDE the fallback loop
  and threaded to `serveComplete`/`serveStream` via `req.WithContext`; status is
  Error only when `retriable == false` (a fallback that ultimately succeeds is
  not red). Pinned by a fallback test (multiple attempts → ONE span, ended once,
  green on eventual success).
- **Header injection clone-only, per-attempt** (gate): `traceparent` is injected
  into a CLONE of the upstream headers, never the shared `req.Header`; RawBody
  byte-identical. Pinned.
- **No-op + zero overhead when `otel` absent**: explicit `enabled` fast path — no
  span, no header mutation, `trace_id` null, request path byte-identical. Pinned
  (handler with no tracer → response + headers + body unchanged, trace_id nil).
- **Tracing never affects serving**: an exporter/span failure is swallowed; the
  response and fallback behavior are identical. Pinned (handler works with a
  failing exporter).
- **Propagation does not corrupt the cache**: only the `traceparent` *header* is
  injected upstream (headers don't affect the prompt-cache key, §4.4); the body
  is untouched. Pinned (RawBody unchanged after injection).
- **`trace_id` is the span's 32-hex trace id** when tracing is on, null when off.
  Pinned with an in-memory exporter.
- **GenAI semconv attributes**: `gen_ai.operation.name`, `gen_ai.system`,
  `gen_ai.request.model`, `gen_ai.response.model`, usage tokens. Pinned via the
  span recorder.
- **count_tokens still never non-200** and is not spanned. Pinned.
- **Dependency stays pure-Go / CGO-free**: `CGO_ENABLED=0` build still succeeds.

## Tasks

Each task: failing test → minimal code → refactor; one `git commit -s`; all four
gates green (build, test -race, vet+gofmt, tests/run-all.sh).

- [ ] **T1 — config `otel` block.**
  `OTelConfig{Endpoint, Protocol, Insecure bool, SampleRatio *float64,
  ServiceName}` + `Config.OTel *OTelConfig`. **`SampleRatio` is a nullable
  pointer** so explicit `0.0` (sample none) ≠ unset (→1.0) (gate). Validation:
  when present, `endpoint` required; `protocol` ∈ {"", "http", "grpc"} (default
  http); `sample_ratio` (if set) ∈ `[0,1]`. Tests: parse; absent → nil;
  endpoint-missing rejected; protocol validation; ratio range; explicit 0.0
  preserved vs unset.
  *Files:* `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **T2 — `internal/tracing` (Init/Shutdown + helpers; OWN Config mirror).**
  `tracing.Config` (mirror — tracing does NOT import `internal/config`; the
  assembly maps it, gate). `Init(Config) (func(context.Context) error, error)`:
  build OTLP exporter (http default, grpc opt), `sdktrace.NewTracerProvider`
  (batch + `ParentBased(TraceIDRatioBased(ratio))` + resource service.name), set
  global TP + `propagation.TraceContext`; sets `enabled=true`. Helpers (all guard
  on `enabled` — **no-op fast path**): `Start(ctx,name) (ctx,span)`,
  `SetGenAIRequest/Response`, `Inject(ctx, dst http.Header)` (caller passes a
  CLONE), `TraceID(ctx) string`. Shutdown wrapped in a bounded context by the
  caller. Tests with `tracetest`: spans + attributes exported; TraceID
  round-trips; **disabled → Start/Inject are no-ops (no header change, empty
  TraceID)**.
  *Files:* `internal/tracing/tracing.go`, `internal/tracing/tracing_test.go`.

- [ ] **T3 — wire spans into the generative handlers.**
  `/v1/messages` + `/v1/chat/completions`: extract incoming context
  (`propagation.Extract` from headers) BEFORE starting; `ctx, span :=
  tracing.Start(ctx, "chat "+model)` once after the model is known, **`defer
  span.End()`**, `req = req.WithContext(ctx)` so the fallback loop +
  serveComplete/serveStream share it; `SetGenAIRequest`. Per attempt: build
  `pr.Headers` from a CLONE of `req.Header` and `tracing.Inject` traceparent into
  the clone. On the COMMITTED outcome set `SetGenAIResponse` + status (**Error
  only when terminal/`retriable==false`**). Audit `Record.TraceID =
  tracing.TraceID(ctx)`. Tests: in-memory exporter → ONE span + gen_ai attrs;
  audit trace_id populated; **inbound traceparent → same trace id (parentage)**;
  **off → no span, trace_id nil, response+headers+body identical**; RawBody
  unchanged + `req.Header` not mutated after injection; **fallback → one span,
  green on eventual success**; **count_tokens has NO span**.
  *Files:* `internal/server/anthropicapi/messages.go`,
  `internal/server/openaiapi/chat.go`, `internal/audit/record.go` (TraceID already
  present), `internal/server/anthropicapi/*_test.go`,
  `internal/server/openaiapi/*_test.go`.

- [ ] **T4 — assembly Init + bounded Shutdown lifecycle.**
  `cmd/inferplane/gateway.go`: when `cfg.OTel != nil`, map `config.OTelConfig` →
  `tracing.Config`, `tracing.Init`; store the shutdown func; call it on `serve`
  teardown inside a **bounded `context.WithTimeout`**, errors logged not fatal.
  Boot failure / unreachable collector is logged, **non-fatal** (tracing is
  best-effort, exporter connects lazily). Tests: gateway boots with an otel
  config pointing at a closed port — `newGateway` succeeds, `serve` teardown does
  not hang.
  *Files:* `cmd/inferplane/gateway.go`, `cmd/inferplane/gateway_test.go`.

- [ ] **T5 — docs.** `docs/reference/api.md`/`agent-llm.md` (tracing),
  `docs/architecture.md` (observability/tracing bullet), `internal/CLAUDE.md`
  (`internal/tracing`), `examples/config.json` (commented `otel` block); mark
  ADR-011 Accepted.
  *Files:* docs + `examples/config.json`.

## File scope (allow-list)

```
docs/decisions/ADR-011-otel-trace-spans.md
docs/superpowers/plans/2026-06-14-otel-trace-spans.md
internal/config/config.go
internal/config/config_test.go
internal/tracing/tracing.go
internal/tracing/tracing_test.go
internal/server/anthropicapi/messages.go
internal/server/anthropicapi/messages_test.go
internal/server/openaiapi/chat.go
internal/server/openaiapi/chat_test.go
internal/server/server.go
cmd/inferplane/gateway.go
cmd/inferplane/gateway_test.go
docs/reference/api.md
docs/reference/agent-llm.md
docs/architecture.md
internal/CLAUDE.md
examples/config.json
go.mod
go.sum
```

## Out of scope (explicit)

- Vendor backends (Langfuse/Datadog/…) — OTLP to a collector only (they consume
  OTLP downstream).
- Spanning `count_tokens` (high-volume, must-never-fail) or the admin plane.
- Metrics changes (Prometheus already ships; this is tracing only).
- A log/trace bridge or OTel logs — spans only for v1.
