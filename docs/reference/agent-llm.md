# Agent · LLM

### 1. Overview
The LLM-facing core: the provider abstraction that talks to Anthropic, Amazon Bedrock,
and OpenAI-compatible upstreams, plus the canonical schema used to convert between
client and upstream protocols without losing thinking blocks or `cache_control`.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Provider interface | `providers/provider.go` | `Name`/`Models`/`Complete`/`Stream`, optional `TokenCounter` |
| Registry | `providers/registry.go` | `Register`/`New` factory by type string |
| Anthropic provider | `providers/anthropic/` | Messages passthrough, verbatim body, byte-exact SSE |
| Bedrock provider | `providers/bedrock/` | Claude→InvokeModel, non-Claude→Converse; SDK isolated |
| OpenAI-compatible | `providers/openaicompat/` | vLLM/Ollama; order-preserving model rewrite |
| Mock provider | `providers/testing/mockprovider/` | deterministic provider for unit tests |
| Canonical schema | `pkg/schema/` | Anthropic-superset types, Extra preservation, SSE writer |
| Filter chain | `internal/filter/` | `RequestFilter` interface + registry (the spec's filter chain ⑥, ADR-009) |
| PII mask filter | `plugins/piimask/` | opt-in regex+Luhn PII masking → typed placeholders; one-way (no vault); masks messages text only |
| Tracing | `internal/tracing/` | opt-in OTel: OTLP **trace** exporter + GenAI-semconv spans (+ `inferplane.*` cache-tier / cost / partial attributes) + W3C propagation + trace_id in audit; no-op default (ADR-011) |

### 3. Key Decisions
- One package per provider; adding a provider is one package + a blank import (zero core diff, §8).
- Canonical schema is an Anthropic-superset so thinking blocks and `cache_control` survive conversion.
- Bedrock Claude uses InvokeModel with a cache-safe top-level-only model rewrite; the event stream is re-serialized to Anthropic SSE.
- PII masking is OPT-IN per team: it re-serializes the body (cache loss, ~10× cost — warned, not silent), updates both RawBody and Parsed (so the openai_compatible Parsed-conversion path can't leak), masks text only (never system/tool/cache_control), and fails CLOSED (ADR-009).
- Bedrock Guardrails (D6, ADR-019) are applied on the DATA PLANE — every InvokeModel/InvokeModelWithResponseStream/Converse/ConverseStream call — not just surfaced in the console: a provider-level default (config `guardrail_id`/`guardrail_version`) plus an optional per-team override (`teams.guardrail_id`/`guardrail_version`), with the override winning but no per-team opt-out (a team can pick a different guardrail, never remove the default).
- The `mantle` egress has NO guardrail parameter, so it cannot honour ADR-019's no-opt-out rule. A request whose effective guardrail is non-empty is REFUSED there (400, `providers/bedrock/bedrock.go`'s `mantleGuardrailCheck`) rather than served unguarded — the Bedrock ingress writes `ProxyRequest.GuardrailID` into the audit chain unconditionally, so serving it would record a false compliance attestation and make `routing.model_api: "mantle"` a per-team opt-out. Route guarded models via `converse`/`invoke_model`.
- A Mantle 2xx body that cannot be parsed fails the call with a 502 instead of being teed through: an unparsed response leaves `ProxyResponse.Parsed` nil, which makes the ingress skip settle entirely — the request would bill nothing and audit like a genuinely free model (ADR-030's zero-cost class).
- Bedrock upstream errors (e.g. a throttled model) are classified into their real HTTP status (`providers/bedrock/errors.go`) and returned as a `providers.UpstreamError`, so the client sees the actual 429/4xx/5xx instead of a generic 502 — applied on non-streaming calls and the pre-first-byte error of a stream open; a mid-stream error (after the first SSE event is already committed) cannot change the HTTP status and is left as-is (existing truncated-stream handling).
- Newer Bedrock models (Opus 4.7/4.8, Fable 5, Sonnet 5, Mythos — an allow-list in `providers/bedrock/thinking.go`) reject the legacy extended-thinking shape Claude Code still sends (`thinking: {"type":"enabled","budget_tokens":N}`) with a 400; `toInvokeBody` rewrites it to `thinking: {"type":"adaptive"}` + top-level `output_config: {"effort":...}` for those models only, leaving every other model's `thinking` field untouched (ADR-022 — a compatibility shim for the CLI/model schema gap, not a permanent fix).

- **A span reports the SETTLED numbers, not just the standard two.** GenAI semconv defines
  only `gen_ai.usage.{input,output}_tokens`, which cannot express a prompt-cache request or
  a cost, so the remaining facts go under an `inferplane.` prefix rather than an invented
  `gen_ai.*` name the spec may later assign differently (that would leave a collector
  double-reporting one number under two keys): `inferplane.usage.cache_read_input_tokens`,
  `inferplane.usage.cache_write_{5m,1h}_input_tokens` (zero tiers omitted),
  `inferplane.cost.amount_usd_micros` (integer µUSD — a span must not be the one place a
  float rounding artifact appears) with `inferplane.cost.pricing_missing` always alongside
  it, since a 0 with the flag means "no rate configured" and a 0 without it means "free".
  `inferplane.response.partial` + span status `Error` mark a stream committed to the client
  and then truncated upstream — the wire status was already 200, so without the attribute a
  truncated stream is indistinguishable from a clean one. Span attributes never carry a
  secret, a `key_id`, or raw client input; model/provider values are the config-bounded
  post-routing ones.
- Tracing (OTLP) is the ONLY signal mayu speaks over OTLP. Metrics are Prometheus
  exposition on `:9090/metrics` and control-plane usage windows are `POST /v1alpha1/usage`
  — see the Collector contract in [infrastructure.md](infrastructure.md).

### 4. Prompt-cache preservation by path
The §4.4 cache invariant, per concrete egress path. "Preserved" means the client's
cache breakpoints reach the upstream byte-stable; every rewrite on a preserved path
is top-level-only (`system`/`messages`/`tools` values byte-identical).

| Ingress → egress | Cache marker | Status | Mechanism |
|---|---|---|---|
| Anthropic → `anthropic` | `cache_control` | Preserved | verbatim `RawBody`; only top-level `model` rewritten, HTML escaping off (`providers/anthropic/anthropic.go` `rewriteTopLevelModel`) |
| Anthropic/Bedrock → `bedrock` InvokeModel (Claude default) | `cache_control` | Preserved | `toInvokeBody` parses top level only; drops `model`/`stream`, injects `anthropic_version` + required body betas (`providers/bedrock/invoke.go`) |
| Anthropic/Bedrock → `bedrock` Mantle, `/anthropic/v1/messages` route | `cache_control` | Preserved | `toMantleAnthropicBody` top-level-only rewrite (`providers/bedrock/mantle.go`) |
| OpenAI → `bedrock` Mantle, `/anthropic/v1/messages` route | `cache_control` | Dropped | body re-rendered from `Parsed` (`toMantleAnthropicBodyFromCanonical`) — RawBody is OpenAI-wire JSON the route would misparse, so the verbatim rewrite is Anthropic-wire-ingress-only |
| Anthropic → `bedrock` Converse (non-Claude) | `cache_control` → `cachePoint` | **Dropped** | `toConverseRequest` flattens `system` to plain text and keeps only text/tool_use/tool_result blocks; no `CachePoint` mapping exists — a known gap vs spec §4.4, which promises `cachePoint` pass-through |
| Anthropic → `bedrock` Mantle chat-completions route | `cache_control` | Dropped | body re-rendered from `Parsed` via `internal/openai.CanonicalToRequest`; the OpenAI wire has no cache marker |
| Anthropic → `openai_compatible` | `cache_control` | Dropped (documented) | best-effort cross-protocol conversion; spec §3.3 states cache_control is ignored with a warning |
| OpenAI → `openai_compatible` | (upstream-side caching) | Preserved | `RawBody` forwarded byte-for-byte except the top-level `model` value span, plus — streaming only, when the client did not opt in — an order-preserving `stream_options.include_usage` splice (`ensureIncludeUsageRaw`, the zero-billing guard); the resulting usage-only frame is stripped from the client tee |
| Any path, PII-masked team | `cache_control` | Kept on blocks, **cache lost** | masking re-serializes the whole body (opt-in, ~10× cost warned at boot — ADR-009) |

Even on paths whose cache MARKER is dropped, the conversation prefix itself must
stay byte-stable across turns for upstream AUTOMATIC caching to hit:
`toConverseRequest` keeps non-user/assistant role messages (Claude Code hook
output) in place by merging them into the next user message — folding them into
the system prompt (the pre-2026-08-28 behavior) mutated the prompt head on every
turn a hook fired and invalidated the entire cached prefix (observed live:
cache_creation ≈ full 475k-token input on every request).

OpenAI-wire streams (openai_compatible, Mantle chat routes) are re-rendered by
canonical consumers into the Anthropic frame vocabulary, and the OpenAI wire
has no message_start/message_stop, opens text blocks implicitly, and has no
per-block close — `ReadChatSSE` (`internal/openai/sse.go`) synthesizes the
whole lifecycle (message_start lazily before the first parsed chunk, stamped
with the PUBLIC model name; a text content_block_start at any index no opener
claimed; content_block_stop per opened block before ANY message_delta;
message_stop at [DONE]), Chunk-only with Raw nil so an OpenAI-wire ingress
tees no invented lines. `message_delta` is the only Anthropic frame carrying
`stop_reason`, so an upstream that ends a stream with neither a `finish_reason`
nor a usage-only chunk gets one synthesized (`end_turn`) at [DONE] — without it
a client cannot tell a completed turn from a truncated one. Exactly one is ever
synthesized: an upstream message-level frame of any kind (stop-bearing or
usage-only) suppresses it, since a consumer may treat the first `message_delta`
it sees as end-of-message.

Usage settlement is cache-tier aware on every path that returns cache counts:
Anthropic/Invoke fold `message_start` + `message_delta` frames (`schema.MergeUsage`,
ADR-030); Converse maps `CacheReadInputTokens`/`CacheDetails` into the canonical
split (`usageWithCache`); the 5m/1h write tiers are priced separately end to end.
One family exception: the OpenAI gpt-5.6 family reports Converse `inputTokens`
INCLUSIVE of the cache counts (verified live 2026-08-29→31: input equals
read + write + Δ across 20+ requests), while the Anthropic wire requires the three counts
disjoint — `usageWithCache` subtracts the cache counts back out for the
`converseInclusiveInputUsage` allow-list (clamped at 0), or a client's context
math runs at ~2x the real prompt and settle bills every cached token twice.

### 5. Per-model Converse/Mantle inference-param strip rules
Some Bedrock models 400 (`ValidationException`) on inference params Claude Code
sends routinely (`temperature`, `stop_sequences`), value-independent — without
stripping, every such request fails and the client silently falls back. Both
tables are evidence-based allow-lists probed against live Bedrock
(ap-northeast-2 + us-east-1, 2026-08-28), same posture as ADR-022's
`legacyThinkingBrokenModels`: an unlisted model keeps every param untouched.
Anthropic models are deliberately absent — `apiFor` routes them via InvokeModel
and their param contract belongs to the client.

`converseUnsupportedInference` (`providers/bedrock/converse.go`), Converse `InferenceConfig` keys:

| Upstream id substring | Stripped |
|---|---|
| `openai.gpt-5.6` (luna/sol/terra; NOT gpt-oss, NOT Mantle-only gpt-5.4/5.5) | `temperature`, `topP`, `stopSequences` |
| `xai.` (grok-4.6) | `temperature`, `topP`, `stopSequences` |
| `openai.gpt-oss` | `stopSequences` |
| `deepseek.v` (v3.x only — r1 accepts all three) | `stopSequences` |
| `google.gemma-` | `stopSequences` |
| `minimax.` | `stopSequences` |
| `moonshot` (moonshot. and moonshotai.) | `stopSequences` |
| `qwen.` | `stopSequences` |
| `zai.` | `stopSequences` |

`converseMinMaxTokens` (`providers/bedrock/converse.go`), a maxTokens FLOOR rather than a
strip: these models 400 on `maxTokens` below 16 ("Invalid 'max_output_tokens': integer
below minimum value. Expected a value >= 16") — and Claude Code sends a `max_tokens: 1`
probe on every `/model` switch, so without the floor the switch itself fails. Raised to
the floor (only ever MORE output, never less), logged once per model. Probed live
2026-09-04: Mantle-served `openai.gpt-5.4`/`-5.5` and `zai.glm-5` accept 1 and are unlisted.

| Upstream id substring | maxTokens floor |
|---|---|
| `openai.gpt-5.6` | 16 |
| `xai.` | 16 |

`mantleChatStripParams` (`providers/bedrock/mantle.go`), OpenAI-wire field names on
Mantle's chat-completions route:

| Upstream id substring | Stripped |
|---|---|
| `openai.gpt-5.6` | `temperature`, `top_p`, `stop` |

Mantle chat additionally renames `max_tokens` → `max_completion_tokens` for every
model on that route (the gpt-5.6 family rejects `max_tokens` outright; all probed
models accept the newer name), and streaming requests set
`stream_options.include_usage` so the final chunk carries billable counts.

Route selection is per-VENDOR segment (`anthropic.*` → `/anthropic/v1/messages`,
`openai.*`/`xai.*` → `/openai/v1/chat/completions`, everything else → the bare
`/v1/chat/completions`) with one probed per-MODEL exception: `google.gemma-4-*`
answers only on `/openai/v1/chat/completions` — the bare route 400s "isn't
supported on this route" (probed live 2026-09-02, matching its model card) while
`google.gemma-3-*` still answers on the bare route.

### 6. Code Pointers
- `internal/tracing/tracing.go` — `SetGenAIRequest`/`SetGenAIResponse` (semconv) + `SetUsageDetail`/`SetCost`/`SetPartial` (`inferplane.*`) + `SetStatus`
- `providers/provider.go` — the interface every provider implements; `ProxyRequest.GuardrailID`/`GuardrailVersion` (D6) is the narrow provider-isolation exception carrying a per-team override
- `providers/bedrock/invoke.go` — InvokeModel body build + SSE re-serialization
- `providers/bedrock/client.go` — `Guardrail` type, `buildGuardrailConfig`/`buildGuardrailStreamConfig`
- `providers/bedrock/errors.go` — AWS SDK error → HTTP status classification, synthesized Anthropic-shaped error body
- `providers/bedrock/thinking.go` — legacy `thinking` → adaptive+`effort` rewrite for the allow-listed broken models (ADR-022)
- `providers/bedrock/converse.go` — Anthropic→Converse translation, `converseUnsupportedInference` strip table, `usageWithCache`
- `providers/bedrock/mantle.go` — Mantle route partitioning, `toMantleAnthropicBody` (cache-safe), `mantleChatStripParams`
- `pkg/schema/usage.go` — `MergeUsage` streaming fold + `CacheWriteTiers` 5m/1h resolution (ADR-030)
- `pkg/schema/extra.go` — unknown-field preservation + case-collision rejection

### 7. Cross-references
- Related modules: `internal/router` (resolution/fallback), `internal/openai` (conversion), `internal/keystore` (team-record guardrail override)
- Related ADRs: docs/decisions/ADR-019-bedrock-guardrails-data-plane.md, docs/decisions/ADR-022-bedrock-legacy-thinking-rewrite.md
- Related runbooks: docs/runbooks/
