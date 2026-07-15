# ADR-024: Bedrock InvokeModel passthrough ingress

**Date:** 2026-07-15
**Status:** Accepted (implemented).
**Related:** ADR-022 (the outbound Bedrock body-rewrite family this mirrors),
`fix/bedrock-context-management-beta` (the beta-injection fix discovered while
diagnosing the same client mode), §4.4 (cache invariant), §8 (provider isolation).

## Context

Claude Code has a native Bedrock mode: `CLAUDE_CODE_USE_BEDROCK=1` +
`ANTHROPIC_BEDROCK_BASE_URL=<gateway>` + `CLAUDE_CODE_SKIP_BEDROCK_AUTH=1`
makes it speak the AWS Bedrock runtime wire protocol at a custom base URL,
sending its credential as `Authorization: Bearer` instead of SigV4. This is
the pattern AWS's own LiteLLM-gateway blog documents, and users who already
run Claude Code against Bedrock expect to point that mode at a gateway
without changing anything else. inferplane previously had no Bedrock-shaped
*ingress* — Bedrock existed only as an outbound provider — so that mode got
401/404 and users had to switch their environment to `ANTHROPIC_BASE_URL`
mode instead. Supporting the native mode directly removes that friction and
is a capability LiteLLM markets; the maintainer decided to build it even
without a hard compatibility requirement ("근본적인 우리가 LiteLLM보다 강점이
될 거 같아, 원래 의도와 맞기도 하고").

Wire facts this design rests on (verified against AWS API references and, for
the client behavior, the AWS blog + Claude Code docs):

- `POST /model/{modelId}/invoke` and `POST /model/{modelId}/invoke-with-response-stream`;
  the request body is the Anthropic Messages body **minus** top-level
  `model`/`stream` (URL/operation carry those) **plus** `anthropic_version`.
  The non-streaming response body is Anthropic-shaped verbatim, with token
  counts duplicated into `X-Amzn-Bedrock-Input/Output-Token-Count` headers.
- The streaming response is NOT SSE: it is `application/vnd.amazon.eventstream`
  binary framing (prelude + CRC32 + header TLVs + payload + message CRC).
  Each data frame carries `:message-type: event`, `:event-type: chunk`,
  `:content-type: application/json`, and a payload of
  `{"bytes": base64(<the same Anthropic streaming-event JSON an SSE line
  would carry>)}`. Claude Code rejects a streaming response whose
  Content-Type isn't the eventstream MIME.
- `POST /model/{modelId}/count-tokens` (GA 2025-08) takes
  `{"input":{"invokeModel":{"body": base64(<InvokeModel body>)}}}` and returns
  `{"inputTokens": N}` (camelCase). Claude Code's `/context` genuinely calls
  it and crashes on a non-200 — the same mandate as `/v1/messages/count_tokens`.
- In skip-auth mode the client's credential arrives as a Bearer header, which
  inferplane's existing `KeyAuth` already accepts — **zero auth changes**.

## Decision

**`internal/server/bedrockapi` — a third sibling ingress package**, following
the exact precedent `openaiapi` set: the pipeline (Canonical → Allows →
ResolveChain → team policy → region lock → masking → PreCheck → audit →
fallback loop → Settle) is deliberately *duplicated* per ingress package, not
abstracted; only the wire edges differ.

**Verbatim request body (§4.4).** The ingress never re-serializes the client
body. An earlier draft rewrote it (strip `anthropic_version`, inject
`model`/`stream`) — the plan-gate review caught that as both a violation of
the same-protocol verbatim invariant *and* unnecessary: the outbound
provider's existing `toInvokeBody` already deletes `model`/`stream` (no-ops
on a Bedrock-shaped body) and keeps an existing `anthropic_version`. The
ingress supplies `Model` (resolved from the URL) and `Stream` (from the
endpoint) as ProxyRequest metadata only.

**Model resolution (`resolveModel`).** The URL `modelId` is tried first as a
canonical/alias name (operators often name models by their Bedrock IDs), then
by a **deterministic** (sorted) reverse scan over the topology for a
bedrock-serving target whose `Target.Model` matches. Single-segment IDs only:
colons are fine (`us.anthropic...-v1:0`), ARN-style IDs containing `/` are a
documented non-goal (Claude Code sends slash-free IDs).

**Bedrock-serving targets only (v1).** The resolved chain is filtered by one
truth source, `servesBedrockIngress(provider.Name())` — the same surface
`openaiapi.providerWire` uses, including its mock-is-compatible test
allowance. An `openai_compatible` target would tee the wrong response shape,
and an `anthropic` target cannot accept a model-less body; both are filtered,
and an emptied chain is a 404. Cross-protocol conversion for this ingress is
explicitly deferred.

**AWS-shaped errors, always.** Errors emit `{"message": ...}` +
`X-Amzn-ErrorType`, never the Anthropic envelope. In particular a
`providers.UpstreamError` — whose `Body` the bedrock *provider* synthesizes
in Anthropic shape — contributes only its `StatusCode`; the body is never
teed. Error messages never echo the caller-controlled URL `modelId`.
Mid-stream failures (status already committed) surface as
`:message-type: exception` frames.

**Eventstream encoding via the AWS SDK's own encoder**
(`aws-sdk-go-v2/aws/protocol/eventstream`, already a transitive dependency) —
no hand-rolled CRC/TLV. Frame payloads are `json.Marshal(ev.Chunk)` (the
canonical chunk, lossless) — never `ev.Raw`, which on the bedrock provider is
SSE-framed *text*. Events with a nil `Chunk` (keepalives/unparseable
payloads) are skipped rather than encoded as `base64("null")`. Round-trip
tests decode our frames with the SDK's own `Decoder` as the correctness
oracle.

**`IngressProtocol: "bedrock"` is an inert label.** No provider branches on
it (the bedrock provider always runs `toInvokeBody`); it exists for
audit/metrics only. Recorded here so a future reader doesn't assume it
drives conversion behavior.

## Non-goals (v1)

- **Converse API ingress** — Claude Code's Bedrock mode uses the Invoke API
  only.
- **ARN/slash-containing model IDs** — single-segment route patterns only.
- **Client-supplied guardrail headers** (`X-Amzn-Bedrock-Guardrail*`) —
  guardrails remain a server-side (provider/team) concern, consistent with
  the other ingresses accepting no client guardrail override.
- **Cross-protocol targets** — a model whose targets are all
  anthropic/openai-wire is 404 on this ingress.
- **SigV4 verification** — skip-auth Bearer only; nothing validates an AWS
  SigV4 envelope, and a SigV4-signed request simply fails key resolution.

## Consequences

- Claude Code's native Bedrock mode (`CLAUDE_CODE_USE_BEDROCK=1` +
  `ANTHROPIC_BEDROCK_BASE_URL` + `CLAUDE_CODE_SKIP_BEDROCK_AUTH=1` +
  the virtual key as the bearer token) now works against inferplane
  unmodified, including streaming and `/context` token counting.
- Three new data-plane routes, all behind the existing `KeyAuth`; the
  documented `/metrics`/audit surfaces gain the `bedrock` ingress label.
- The eventstream encoder is the first *inbound* use of the AWS protocol
  packages; it stays inside `internal/server/bedrockapi`, leaving provider
  isolation (§8) untouched.
