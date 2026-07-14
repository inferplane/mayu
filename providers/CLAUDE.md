# providers Module

## Role
The upstream-provider extension surface. Every provider implements one interface and
lives in its own package. This is the project's headline extensibility promise
(design §8): **a new provider is one package + one blank-import line, with zero core diff.**

## Key Files
- `provider.go` — the `Provider` interface (`Name`, `Models`, `Complete`, `Stream`), optional `TokenCounter` and `HealthChecker` (connection-probe capability, ADR-014: anthropic/openai_compatible probe `GET /v1/models`, bedrock a 1-token converse classified by SigV4-vs-service error; a non-implementer is "probe unsupported"), and transport types (`ProxyRequest`, `ProxyResponse`, `StreamEvent`, `IngressProtocol`). `Config.HTTPClient` (optional) lets the probe inject an SSRF-guarded client; nil ⇒ default. `ProxyRequest.GuardrailID`/`GuardrailVersion` (D6, ADR-019) is a narrow, explicit exception to provider isolation — a per-team Bedrock Guardrail override threaded from the team record; every provider but `bedrock/` ignores it.
- `registry.go` — `Register(type, factory)` / `New(Config)`.
- `errors.go` — `UpstreamError{StatusCode, Body, Header}` (so non-2xx upstream responses tee through losslessly); consumed on both the streaming AND non-streaming Complete path (anthropicapi/openaiapi's `serveComplete` both tee it, matching `serveStream`).
- `anthropic/` — Messages passthrough; verbatim body, gateway-injected `x-api-key`; byte-exact SSE reader.
- `bedrock/` — Claude via InvokeModel, non-Claude via Converse; AWS SDK isolated behind invoker/converser interfaces. Applies a Guardrail (D6, ADR-019 — the data-plane anti-bypass fix) on every one of the four call paths: a per-team override (`ProxyRequest.GuardrailID`) wins over the provider's configured default (`Settings["guardrail_id"]`); no per-team opt-out exists. Empty version defaults to `"DRAFT"`. `errors.go`'s `upstreamError(err)` classifies an AWS SDK error into its real HTTP status (a typed exception's `OriginalStatusCode` > its `ErrorCode` via a status table shared with `health.go`'s `credentialErrorCodes` > the transport `ResponseError`'s status > 502 fallback) and returns a `providers.UpstreamError` with a synthesized Anthropic-shaped error body — so a throttled model surfaces as 429/`rate_limit_error` to the client instead of a generic 502. Applied on `completeInvoke`/`completeConverse` and the PRE-call error of `streamInvoke`/`streamConverse` only; a mid-stream error (after the first SSE event is already committed) is intentionally left as a plain error — the response's HTTP status can no longer change at that point. The synthesized body never echoes `ErrorMessage()` (can carry an account id/ARN — same principle as `health.go`'s comment), only the error CODE. `thinking.go`'s `toInvokeBody` step rewrites a legacy extended-thinking request (`thinking: {"type":"enabled","budget_tokens":N}` — what Claude Code still sends) into the adaptive-thinking shape (`thinking: {"type":"adaptive"}` + top-level `output_config: {"effort":...}`) that newer models require instead of a 400 (ADR-022) — only for upstream IDs matching `legacyThinkingBrokenModels` (an allow-list of models confirmed to reject the legacy shape, not a deny-list of old ones); every other model's `thinking` field passes through byte-identical.
- `openaicompat/` — vLLM/Ollama/any OpenAI endpoint; order-preserving model rewrite.
- `testing/mockprovider/` — deterministic provider for unit tests.

## Rules — adding a provider
1. Create `providers/<name>/<name>.go` implementing `Provider` (and `TokenCounter` if it supports counting).
2. Call `providers.Register("<type>", factory)` in an `init()`.
3. Add one blank import in `cmd/inferplane/main.go`.
4. Document it in `docs/reference/agent-llm.md`.
5. **Do not touch core packages.** A provider PR that edits `internal/*` has violated the boundary.

## Invariants
- When provider protocol == ingress protocol, forward `RawBody` verbatim (cache safety).
- Streaming: `Stream` returns an `iter.Seq2[*StreamEvent, error]`; never retry mid-stream (pre-TTFT failover only).
- Cache-affecting rewrites (e.g. Bedrock model injection) must be top-level-only so `cache_control` stays byte-stable.
