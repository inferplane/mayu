# Adaptive Gateway Implementation Plan

> **For agentic workers:** Use subagent-driven-development for the independent
> workstreams below. Work in `/tmp/inferplane-adaptive-routing`; preserve the
> original checkout and the original local routing branch.

**Goal:** Stable context routing, explicit PII mask/internal routing, strict
budget cutover and Codex Responses compatibility without a central classifier.

**Architecture:** Node-local constrained selection over immutable topology and
policy snapshots; existing governance remains authoritative; protocol adapters
share the same pre-egress decisions.

**Tech Stack:** Go 1.25, net/http, existing pure-Go dependencies, local fakes.

**Spec:** `docs/superpowers/specs/2026-09-11-adaptive-gateway-design.md`

## Global constraints

- CGO_ENABLED=0 builds for both binaries; no new remote classifier.
- Leaf/provider boundaries, integer microUSD and actual-usage settlement.
- Original identity authorization, no secret/session/PII values in observability.
- Every retry constrained; no count-token non-200; no masking error ignored.
- DCO sign-off; no network/credential dependencies in repository tests.
- Existing configuration semantics retained unless the new option is explicit.

## Task 1: Routing policy and stable selection

**Files:** `api/v1alpha1/types.go`, `internal/policy`, `internal/router`,
`internal/tier`, `deploy/crd/inferplane.dev_governancepolicies.yaml`.

**Contracts:** extend Context with optional NormalModel/MaxNormalInputTokens and
Stability; add Mask action and RequestRoutingResult.MaskRequired; extend
BudgetTiers/ActiveTier with EnforceTargets; expose tier constraint lookup without
changing the existing substitution lookup. RequestRoutingInput.SessionHint is
untrusted and used only for scoped hashing.

- [ ] Add failing schema/clone/target validation tests for all new fields.
- [ ] Add weak/normal/strong, tool/history, hold-time/request-count and expiry
  tests using an injected clock.
- [ ] Test concurrent requests, bounded storage, identity separation, hot reload,
  privacy/budget override and cold-start behavior.
- [ ] Implement strict budget target intersection and independent masking
  obligation. Legacy optional context/budget rules retain regression behavior.
- [ ] Run policy/router/tier race tests and review the scoped diff.

## Task 2: Responses codecs and native provider

**Files:** new `internal/responses`, new `providers/openairesponses`.

**Contracts:** RequestToCanonical(raw) returns canonical request; validation
distinguishes native passthrough from cross-protocol conversion.
ResponseToCanonical, CanonicalToResponse and a stateful SSE renderer/reader
preserve usage, call IDs, tool arguments and terminal state.
Provider registers `openai_responses` and implements the existing Provider
interface, with SupportsIngress("responses").

- [ ] Write recorded-shape local fixtures for text, functions, custom tools,
  phase, tool results, streamed arguments, usage, cancellation and malformed data.
- [ ] Verify unsupported stateful/opaque cross-protocol input refuses.
- [ ] Implement codecs and the native provider with fake HTTP tests, raw
  passthrough/model-rewrite assertions and missing-usage refusal.
- [ ] Run focused race tests and provide exact public signatures to Task 3.

## Task 3: Transformation, ingress and assembled routing

**Files:** `internal/sensitivity`, `internal/server/responsesapi`,
`internal/server/{anthropicapi,openaiapi,bedrockapi,requestpolicy}`,
`internal/server/server.go`, `cmd/mayu`, `internal/audit`, `internal/metrics`.

- [ ] Add original-byte Responses inspection and deterministic complete masking
  with post-transform inspection; test all detector categories, nested numeric
  tool arguments, structural-key refusal and malformed/opaque content.
- [ ] Apply MaskRequired in every generation/count path before PreCheck/egress;
  regenerate parsed request after transformation; emit safe transformation evidence.
- [ ] Register Responses ingress and native provider; apply the same readiness,
  body, identity, routing, region, cost and retry constraints.
- [ ] Fix standalone period-aware tier evaluation and integer utilization;
  wire strict constraints in both standalone and control-plane modes.
- [ ] Exercise assembled gateway behavior, including offline policy routing,
  privacy+budget intersections and hard total-cap refusal.

## Task 4: Documentation, examples and end-to-end verification

**Files:** ADR-044, Responses/operator guides, examples, README/roadmap/reference
context and focused acceptance fixtures.

- [ ] Provide one runnable example with auto alias, three classes, stable
  sessions, PII internal/mask alternatives, soft switching budget and hard cap.
- [ ] Add Codex user-level custom-provider configuration and bounded local fake
  CLI smoke test; record unsupported surfaces and actual tested CLI version.
- [ ] Document failure domains and the durable shared-authority gap explicitly.
- [ ] Run both static builds, full race tests, vet, gofmt, harness, pricing and
  diff checks. Run independent review, fix substantive issues and repeat checks.
- [ ] Push PR, inspect latest-HEAD review and CI, fix/re-push, then merge under
  the user's standing authorization when all release conditions are satisfied.
