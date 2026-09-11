# ADR-044: Stable adaptive routing for coding agents

Status: Accepted implementation design, 2026-09-11.
Extends ADR-043's context/privacy contract and ADR-041's optional budget tiers.
The legacy modes in those ADRs retain their behavior.

## Decision

Keep the classifier, inspection and affinity decisions inside each mayu.
Select from operator-declared weak/simple, normal and strong/complex model
classes. An optional normal class extends the original two-class rule. Explicit
stability enables compatible multi-turn/tool workloads; old rules retain their
single-turn eligibility. Extended rules use the latest user instruction for
keywords, excluding old history, system prompts and tool descriptions. Full
request size still constrains model capacity and input thresholds.

Retain the actual successful model/provider/upstream during the configured
minimum hold and request count. Use a bounded local store of scoped HMAC digests,
configured target identifiers and expiry, never prompts or raw session IDs.
Pins are hints, not permission: revalidate the current policy, destination,
capabilities and region before every use. Explicit model changes, privacy and
budget restrictions, incompatibility, expiry and failures can invalidate a pin.
Only successful actual attempts establish affinity. A restart/capacity loss may
cause a cold cache but cannot widen access.

PII restrictions precede preference. Mask is now an explicit detected-content
action; it is not allowed for uninspectable input. Complete the transformation
and independently reinspect it before returning any usable route. All matching
rules accumulate: masking does not remove an internal-only obligation. Reject
unsafe structural/numeric transformations, unknown coverage and errors. Native
Responses tools that can create remote egress cannot bypass internal-only
protection merely by selecting an internal provider.

Strict budget tiers opt in with `enforceTargets`. Their active target constrains
every preference, pin and retry. Legacy tiers remain optional and never deny by
themselves. Strict tiers may activate at 100%; a missing/incompatible/forbidden
target refuses rather than restoring a more expensive route.

A soft budget referenced by a strict tier is a switching/accounting threshold.
It does not issue an admission lease or shrink a separate hard total cap. Its
usage meter remains available when no other cap exists. All independent hard
limits remain binding, with the more restrictive configuration/policy value
retained. Tier utilization uses saturating integer arithmetic and the referenced
calendar period. A named fallback is an operator decision, not a proof that a
model is inexpensive or privately hosted.

## Codex

Add POST `/v1/responses` and the `openai_responses` provider. Native Responses
forwarding preserves original bytes except the existing narrow model rewrite
and explicitly required masking. The stateless cross-protocol bridge preserves
supported text/tool workflows and rejects unsupported state instead of silently
discarding it. Both paths pass through original-model authorization, original
request inspection, every routing ceiling, admission and observed-usage
settlement. The same rules apply to retries and streaming.

Codex compatibility is demonstrated with a local CLI/fake-upstream tool round
trip, separately from model quality. A wire-compatible arbitrary model is not
automatically a good coding model.

## Failure domains and HA boundary

The classifier and affinity store add no network dependency. Node-local mayu
instances isolate failures to their own clients. Multiple approved backend
targets can fail over within the same data/cost boundary. A control-plane
endpoint never handles inference; already installed policy and valid authority
can be evaluated locally.

Replicated control-plane services and an HA Postgres service are the intended
management deployment. Before enabling interchangeable enforcement replicas,
budget authority must move to a Postgres-authoritative atomic reserve/settle/
release ledger with durable window IDs and idempotent reports. This is the
maintainer's Postgres-only direction; do not add Redis or treat local affinity
as financial authority.

That durable enforcement migration is **not implemented by this ADR's routing
change**. Current budget/rate counters, keys/topology stores and the CP lease
ledger retain the documented limitations of ADR-013/034. In particular, memory
restart, pruning and expired-but-unsettled authority are not proof of safe
refund. A replica count alone is not HA enforcement.

Hard-cap availability is bounded by valid reserved authority. Once that
authority is unavailable or exhausted, fail closed; a cheap model is not an
exemption from the total cap. There is no promise of both unlimited offline
availability and exact hard limits.

## Rollout

Upgrade every policy producer/consumer and the CRD together before activating
new enforcement fields. Rejected strict/privacy policy must gate its affected
subjects. Start context in Shadow, calibrate thresholds on coding sessions,
then enable stability/Enforce. Check actual task success, total settled cost
including cache writes/retries, cache-read utilization and latency. Preserve
privacy/budget enforcement when rolling back an optional context preference.
