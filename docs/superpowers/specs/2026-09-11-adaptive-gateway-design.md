# Adaptive coding-agent gateway

Date: 2026-09-11. Scope: implement the user's clarified routing goals on top of
main and the previously reviewed local ADR-043 implementation.

## Product contract

The same authenticated entry point serves Claude Code and Codex. An operator
maps application model names to weak, normal and strong models. A local
classifier selects among those models without sending prompts to an additional
classification service. Model choice remains stable across related requests to
protect provider prompt caches. PII policy and spending restrictions always win
over that preference.

Reference: the supplied engineering-playbook advanced-features page describes
keyword/length/turn-based weak/strong routing. Its per-request classifier, public
fallbacks, and semantic response caching are not an adequate implementation of
this product contract. This design preserves provider prompt caching; it does
not introduce a semantic response cache.

## Critical assessment of the starting point

- Main has Anthropic Messages, Chat Completions and Bedrock Invoke ingress, but
  no Responses ingress. Chat Completions compatibility alone does not serve
  current Codex clients.
- The imported ADR-043 code has complete local inspection and constrained
  retries, but context enforcement accepts only simple single-user-turn traffic.
  Tool-using coding sessions therefore cannot use it as an automatic router.
- Context chooses only simple/complex and does not remember a session's model.
- InternalOnly/Block privacy policies exist; policy-selected masking does not.
  Legacy masking covers fewer surfaces than the inspector and cannot be treated
  as proof that a protected request has been fully transformed.
- ADR-041 substitutions are optional and may return to the original model.
  Thresholds stop at 99%, and substitutions alone do not constrain every retry.
  That is insufficient for an explicit exhausted-budget routing requirement.
- Mayu already keeps inference off the control plane. Local counters and the
  control-plane lease ledger are not durable shared enforcement; adding replicas
  is not evidence of HA budget correctness.

## Decision order

1. Authenticate and authorize the original requested model.
2. Inspect original ingress bytes; apply every applicable privacy rule.
3. Intersect explicit exhausted-budget target restrictions and region/capability
   constraints. A separate total hard cap still denies when exhausted.
4. Reuse a still-valid session model during its minimum hold interval.
5. Apply weak/normal/strong context preference within the remaining safe set.
6. Enforce the same set for every retry; settle actual usage and audit the
   provider/model actually attempted.

Privacy and cost ceilings are independent sets. Neither can cancel the other.
Masking is an independent obligation, including when another rule also requires
an internal destination. No safe intersection means refusal before upstream.

## Context and cache stability

Extend the existing context rule compatibly: optional `normalModel` and
`maxNormalInputTokens` provide a third class. `simpleModel` is the weak class and
`complexModel` is strong. Existing two-class rules keep their original behavior.
An explicit `stability` object enables multi-turn/tool-capable routing and
contains `minHold`, `minRequests` and `sessionTTL`.

Defaults for that object are 5 minutes, 3 requests and 30 minutes. Validate finite
bounds and require sessionTTL >= minHold. Classify with local signals; a context
window overflow or unsupported capability makes a candidate unusable regardless
of its class. Opaque provider state is not portable to arbitrary models.

Session hints are performance hints only. Hash them with authenticated key/team,
ingress and requested model; never use them as identity or authorization. Without
a client hint derive a bounded stable conversation-prefix fingerprint. Keep only
hashes and configured model identifiers in a bounded, concurrency-safe local
store. Never retain prompts or raw session IDs. Every hit revalidates policy and
topology. Policy changes, privacy, cost ceilings and unavailable/incompatible
targets override a pin immediately. Count requests do not create/refresh pins.

Local pin loss may produce a cold cache; it cannot widen access. There is no
mandatory affinity database or remote classifier on the request path. Failover
can sacrifice warmth to keep an authorized compatible route available.

## PII routing and masking

Keep InternalOnly and Block. Add explicit Mask for detected inspectable content;
Mask is invalid for uninspectable content. Detection errors refuse. Unknown or
opaque surfaces block or remain within an explicitly allowed internal boundary.

The request transformer must use the same finite detector coverage as inspection,
including Korean resident-ID shapes and numeric/encoded tool arguments. Reinspect
the transformed body; remaining detected data, incomplete coverage, key collision,
unsupported structure or any error refuses before any provider call.

Do not rewrite protocol identifiers/model names or corrupt tool schemas to make
a scan pass. If protected content in a structural field cannot be safely masked,
refuse. Stable typed replacements preserve identical transformed prefixes across
turns; this changes the cache namespace, but need not invalidate every subsequent
cache hit. No reversible PII vault is introduced.

Masking is evidenced separately from the selected destination. Regenerate the
canonical request from the transformed bytes before provider dispatch. Original
inspection is never replaced by inspecting a lossy protocol conversion.

## Cost routing

Extend budget tiers with opt-in `enforceTargets`. Legacy ADR-041 tiers retain
never-deny/fallback-to-original semantics. Strict tiers allow threshold 100 and
constrain all subsequent context choices and retry targets to the selected
configured model. Multiple physical low-cost providers can sit behind that model.
Unavailable strict targets refuse; they do not restore an expensive original.

A soft budget used as a switching threshold does not become a hard spending cap.
An independent total hard cap, rate/quota rule and privacy policy remain binding.
Operator pricing and topology determine suitability: names such as Qwen, Gemma
or Kimi do not imply zero price, small size, private hosting or PII approval.

Use integer utilization arithmetic and the referenced budget period for tier
evaluation. Preserve monotonically active tiers within the correct window.
Control-plane mode consumes local snapshots; standalone mode uses its documented
local counters. No new claim of exact fleet spending is made by this feature.

## Responses / Codex

Add authenticated POST /v1/responses through the same body/readiness, routing,
governance and audit controls as the existing ingresses. Add a native
openai_responses provider, preserving original bytes except the existing narrow
model substitution and explicitly requested privacy transformation.

For other providers, use an explicit Responses-to-canonical adapter and translate
the response/SSE lifecycle back. Preserve message phase, function call identifiers,
arguments/results and usage. Support local-client text/function/custom-tool
workflows; reject unsupported built-in tools, provider-owned conversation IDs,
opaque reasoning transfer and asynchronous storage operations before egress.
No in-process response-history store is required; the client supplies history.
Native Responses routes may preserve features that cross-protocol adapters reject.

Tests must include streaming, interrupted streams, tool round trips, two turns,
cache-aware usage, original key isolation, policy refusal and real installed
Codex CLI against local fake upstreams. Protocol smoke tests do not prove a
particular model's coding quality.

## Availability and deployment

Keep node-local mayu routing and cached policy evaluation. Replicate model
backends; fallback remains within the admitted data/cost boundary. Optional
control-plane management uses a stable HA endpoint with replicated services and
Postgres-authoritative management data. The new classifier and affinity store
create no centralized dependency.

Hard-cap authority is finite: an unexpired reserved lease may continue offline;
unknown/exhausted authority must fail closed. Continuous service through an
arbitrary control-plane/storage outage and exact hard caps cannot both be promised
without reserved authority. Existing nondurable lease/counter limitations remain
explicit, and multi-replica enforcement stays blocked pending its durable
reserve/settle/window implementation and integration tests.

This change must test local routing during control-plane unavailability and
strict safe fallback. It must not describe current in-memory counters or a
replica count as completed shared-state HA.

## Release gates

Both static binaries, repository race tests, vet, empty gofmt, harness tests,
pricing/example validation, protocol/PII/cost/stability adversarial tests and
latest-HEAD AI review. Commit with DCO. Apply substantive findings, push again,
then merge only the reviewed HEAD after required checks pass.
