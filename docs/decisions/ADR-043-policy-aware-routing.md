# ADR-043: Policy-aware routing with local inspection

- Status: Accepted
- Date: 2026-09-10
- Related: ADR-006 (topology snapshots), ADR-009 (masking), ADR-033/034
  (policy delivery), ADR-041 (budget tiers), ADR-042 (user budgets)

## Context

Coding tasks should remain useful while total cost falls within an organization's
approved data-processing boundaries. A cheaper model name alone proves neither
savings nor privacy. Budget substitution already exists; destination restrictions
must constrain it, every retry, and any optional context recommendation.

## Decision

Use a stdlib-only, local inspector over original ingress bytes. It returns bounded
categories and request-shape signals without retaining detected text. There is no
remote classifier, new dependency, or client-header session identity.

Add an independent `sensitiveData` GovernancePolicy rule requiring `FailClosed`.
Both `onDetected` and `onUninspectable` explicitly select `InternalOnly` or `Block`.
InternalOnly requires an explicit `internalModels` list. All matching team/user
rules apply: Block wins, internal model sets intersect, and an empty safe chain
denies. Every attempt must name an approved model on a provider explicitly marked
`data_boundary: internal`. Missing/unknown labels convey no trust; labels are
operator attestations, not verification of endpoint ownership or residency.

Add `routing.context` alongside the mutually exclusive affinity and budget-tier
forms. Context requires `FailOpen`; omitted mode is `Shadow`. Recommendations use
a conservative input estimate, an operator threshold, and case-insensitive
keywords. Multiple recommendations must agree; any Shadow match prevents context
switching. The input threshold selects simple versus complex, not Enforce
eligibility: an eligible request above it can select a distinct compatible complex
target. Privacy is enforced independently in both modes. An unusable context
preference retains the already-safe route.

Enforce is opt-in and switches only completely inspectable single-user-turn
requests without assistant/tool history, tool definitions/calls, opaque media,
reasoning, or structured-output requirements. It does not pin future turns.

`Router.RouteRequest` consumes one policy snapshot and the same immutable topology
used for attempts and pricing. It runs after original-model authorization and
budget substitution, before masking, admission, body capture, or upstream calls.
It returns the entire allowed retry chain. New alternatives require a declared
context window, required capabilities, a rate, and a physically compatible ingress
path. Metadata cannot override known translator losses. No-policy and passive
context paths preserve existing authorized behavior, including the input preflight
model; selected alternatives and privacy paths remain strict.

At startup, mayu installs `live.Holder.RoutedAndPriced` on `policy.Store` after
building the effective topology, then reloads local policy before binding any
listener. The same callback validates future file reloads and CP `ApplyWire`
against the current holder. It resolves aliases without model-fallback rescue and
requires rates for every configured target. A later topology change is still
checked at request time; topology and policy are not one atomic transaction.

Rejected distributed sensitive documents gate affected subjects until a valid
replacement arrives, including rejected replacements of an active privacy policy.
A failed local reload retains the previous valid snapshot. The control plane has
no data-plane topology, so target validation belongs to each mayu. Deployments
requiring protection before first sync set `control_plane.require_sync: true`,
with `max_policy_age` to bound staleness. Both count APIs return local estimates
and HTTP 200 on routing refusal or an unready/stale readiness gate, with no upstream
call. Oversized count bodies take the bounded local path too.

## Evidence and semantics

Append an optional audit `request.routing` object: requested, selected, proposed
models; mode/reason; inspection/categories; policy names/generations; planned and
actual provider/boundary. Original means resolved pre-budget-tier model, after
alias/model-fallback handling; context source matching uses the post-tier model
before privacy substitution. Passive results preserve that input model for existing
context preflight; actual fallback attempts are recorded separately. Proposed is
never an instruction to call a provider. Existing budget-substitution evidence is
retained. Mixed old/new audit records preserve exact-byte chain verification.

Headers expose bounded reasons and applied model selection; metrics use only
team/mode/reason. Routing evidence contains no prompt, detected value, secret,
client session header, or key ID. Existing opt-in body capture is independent.

## Limits and tradeoffs

Detectors recognize email, selected phone formats, Luhn-valid card numbers, US
SSN, IPv4, and Korean resident-registration shapes. They are finite heuristics
with false positives and false negatives, not universal PII detection. JSON keys,
strings, exact numeric scalar spellings, and nested JSON tool arguments are
inspected. Images/audio/documents/files, encrypted/redacted thinking, unknown
blocks and unknown body shapes are uninspectable. No URL fetching or arbitrary
binary decoding occurs. Privacy inspection errors deny.

Capabilities are a closed set: tools, vision, reasoning, structured_output. The
current translators impose additional restrictions: OpenAI ingress cannot select
direct Anthropic; native Bedrock remains restricted; Converse and cross-wire
paths cannot be assumed to preserve advanced features. This release adds no
Responses ingress, durable session pinning, shared-state HA, learned classifier,
or broader masking guarantee. Existing budget/rate durability limits remain.

Upgrade all participating data planes, the control plane, and the CRD when used
before activating these rules. Older binaries cannot enforce the new shapes;
version skew is not a privacy rollout strategy.

## Rollout gates

Start context in Shadow on a representative workload; separately test privacy
negative cases and every retry path. Compare task success, total settled cost
including cold-cache writes and retries, and p95 latency with the baseline. Define
acceptable rollout thresholds before enabling Enforce for eligible single-user-turn
requests. Unit
and assembly tests establish enforcement behavior, not measured savings or
production readiness. Disable context enforcement if quality/cost gates regress;
retain privacy restrictions. See [operator guide](../policy-routing.md).
