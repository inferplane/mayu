# Policy-aware routing

Policy-aware routing restricts where protected content may go and records context
model recommendations. The goal is useful coding tasks at lower total cost within
approved data-processing boundaries. Savings and task quality require workload
evaluation; selecting a cheaper model is not evidence of either.

This guide describes the legacy two-class mode. For the compatible normal-class,
session stability, strict budget cutover, complete masking and Codex additions,
see [adaptive routing](adaptive-routing.md). Eligibility limits below apply when
the new stability option is absent.

## Configure an isolated example

Use `examples/config.policy-routing.json` with
`examples/policy-routing/governance.yaml`, from the repository root. The policy is
intentionally outside `examples/policies/`: that directory belongs to quick-start
examples with different model names. Loading it as one directory must not acquire
these unrelated routing targets.

Replace the example `.invalid` endpoints and upstream model IDs with your actual
OpenAI-compatible deployments. Verify their context windows/capabilities and
replace the illustrative USD-per-million-token rates with your chargeback rates.
The provider marked internal must really meet your approved processing boundary;
`data_boundary` is an operator assertion, never inferred from a hostname or model.

Supply `INFERPLANE_ADMIN_TOKEN`, `POLICY_ROUTING_PUBLIC_KEY`, and
`POLICY_ROUTING_PRIVATE_KEY` through your secret manager. This runnable example
uses environment references only; no inline secret values are stored. Create the demo state
directory and use the usual key-issuance flow for team `engineering`:

```bash
mkdir -p /tmp/inferplane-policy-routing-demo
mayu pricing check --config examples/config.policy-routing.json
mayu keys create --team engineering --models '*' --store /tmp/inferplane-policy-routing-demo/keys.db
mayu serve --config examples/config.policy-routing.json
```

Treat the one-time key output as a secret. Send an ordinary Anthropic Messages or
OpenAI Chat Completions request for `premium-coder` (or its configured alias). The
example's `tools` capability is illustrative; no vision/reasoning/structured-output
support is declared. Native Bedrock InvokeModel/InvokeModelWithResponseStream
requests with supported Anthropic-shaped text bodies can also route through these
`openai_compatible` targets; the gateway translates requests and renders responses
in the ingress format. This is subject to the existing cross-protocol limits:
vision, reasoning, structured-output and uninspectable content cannot use that
policy-selected path. Capability declarations do not override translator limits.

## Policy fields

Each rule has exactly one kind. `routing` has exactly one subtype: affinity
(currently rejected), budgetTiers, or context. Selectors `team` and `user` are ANDed
within one policy; all matching policies apply.

| Field | Contract |
|---|---|
| `sensitiveData.onDetected` | Required `InternalOnly` or `Block` |
| `sensitiveData.onUninspectable` | Required `InternalOnly` or `Block` |
| `sensitiveData.internalModels` | Nonempty explicit names if either action is InternalOnly; no wildcards; aliases resolve on the current topology |
| Sensitive rule `failurePolicy` | Must be `FailClosed`; inspection/lookup errors deny |
| `routing.context.mode` | `Shadow` by default, or explicit `Enforce` |
| `routing.context.fromModels` | Required nonempty explicit names; matches the post-budget-tier model before privacy substitution |
| `simpleModel`, `complexModel` | Required explicit targets; both must be routed and priced |
| `maxSimpleInputTokens` | Required positive conservative input threshold; above it recommends complex |
| `complexKeywords` | Optional nonblank strings, case-insensitive decoded-text match; a match recommends complex even below threshold |
| Context rule `failurePolicy` | Must be `FailOpen`; unusable preferences retain the safe route |

Privacy rules always enforce, including while context is Shadow. Block wins;
InternalOnly intersects approved model sets and requires an explicitly internal
provider on every attempt. An empty intersection or no compatible safe target
returns an ingress-shaped 403. Context cannot loosen that destination restriction.
Overlapping context recommendations must agree; any matching Shadow rule prevents
an enforced switch.

The example explicitly sets `mode: Shadow`. After workload gates pass, change only
that context rule to opt in:

```yaml
routing:
  context:
    mode: Enforce
    fromModels: [premium-coder]
    simpleModel: economy-coder
    complexModel: premium-coder
    maxSimpleInputTokens: 4096
    complexKeywords: [security, authentication, migration]
```

`maxSimpleInputTokens` chooses simple versus complex; it is not an Enforce length
ceiling. An eligible request above the threshold can switch to a distinct compatible
`complexModel`. In this example the complex target equals the source, so context
alone leaves an above-threshold `premium-coder` request on that model.

Keep `failurePolicy: FailOpen` on this rule. Enforce selects only completely
inspectable single-user-turn requests without assistant/tool history, tool
schemas/calls, media, reasoning, or structured-output requirements. Multi-turn
traffic is observed without context switching. Privacy still applies on every
turn; the entire submitted history is inspected each time. There is no durable
session identity or pinning inferred from client headers.

## Inspection and capability limits

Inspection reads original bytes before masking or body capture and never changes
them. JSON keys/strings, exact numeric scalar spellings, system/developer prompts,
messages, metadata, tool definitions/results, and nested JSON tool arguments are
visited. Finite detectors cover:

- Email address shapes; selected North American, Korean, and international phone formats.
- 13–19 digit Luhn-valid card numbers; US SSN shapes with range exclusions.
- Valid IPv4 literals and Korean resident-registration number shapes.

These signals have false positives/negatives and do not cover every PII type or
obfuscation. Inspectable reasoning text is scanned. Opaque image/audio/document/file
payloads, encrypted/redacted thinking, unknown content blocks, and unknown body
shapes are uninspectable. The example blocks those requests. No remote fetch or
arbitrary binary decode occurs. Malformed JSON, cancellation, and inspection-limit
errors deny under privacy policy; a context-only failure preserves the route.
Limits include 64 MiB input, 128 MiB decoded content, 262144 nodes, JSON depth 64,
and nested encoded-JSON depth 8. These are resource bounds, not quality guarantees.

`models.<name>.capabilities` accepts only `tools`, `vision`, `reasoning`, and
`structured_output`; empty means none declared. `context_window` is nonnegative
input-plus-output tokens; zero means unknown. Automatically selected alternatives
and their cross-model fallbacks require sufficient declared context for the
conservative input estimate plus output budget, every observed capability, a rate,
RBAC/region permission, and compatible transport. Known translator losses override
capability declarations. OpenAI ingress cannot select direct Anthropic, native
Bedrock requires a compatible path, and Converse/cross-protocol paths have further
feature restrictions. This does not add Responses ingress or expand legacy masking.

With a provider store enabled, the console's provider/model edit forms prefill and
replace these declarations. Select `unknown`, blank the context window (or set 0),
or uncheck capabilities to clear them deliberately. Ordinary model edits preserve
existing aliases; NEW PROVIDER / NEW MODEL resets the draft for a new entry.

## Delivery, upgrade, and recovery

Upgrade mayu, inferplaned, and the GovernancePolicy CRD (if used) before activating
new rules. Verify every participating data plane recognizes the schema and has the
required routes/prices. Older data planes cannot enforce these rules; do not treat
mixed-version rejection as protection. Existing configs without new policies keep
their behavior.

Choose local `policies` or `control_plane`, never both. Local policy is checked
against the effective file/DB topology before listeners bind; failed file reloads
retain the last valid snapshot. Future policy application uses the current live
holder. All configured targets of privacy/context/budget substitutions must have
rates, even with `pricing.on_missing: allow`; aliases are accepted, missing-model
fallbacks do not rescue a policy target. Metadata survives SQLite seed/overlay,
admin writes/readback/export, and topology reloads. Later topology changes still
face runtime checks; policy and topology publication are separate operations.

A rejected distributed sensitive document fails routing closed for affected
subjects until a valid replacement is applied. This includes invalid replacement
of an active policy and does not silently remove its protection. Rejected
context-only preferences do not create a privacy gate.

For CP protection from the first request, configure:

```json
"control_plane": {
  "url": "https://control-plane.example.invalid",
  "token_ref": {"env": "INFERPLANED_TOKEN"},
  "require_sync": true,
  "max_policy_age": "1m"
}
```

Without `require_sync`, legacy boot behavior serves before first policy sync.
With it, governed generation requests return 503 until sync and when the accepted
policy readiness gate becomes stale. Both token-count APIs keep HTTP 200 but use
local estimates with zero upstream calls on unready/stale gates, privacy refusal,
or oversized/unreadable count bodies. Readiness exceptions do not authorize egress.

## Read decisions and evaluate rollout

The router's requested model is the resolved pre-tier model, after alias and
missing-model fallback resolution. Context `fromModels` matches the post-tier
model before privacy substitution. Selected identifies the actual policy choice;
proposed is observational. Passive/no-rule results retain the input model for
existing preflight behavior even if breaker ranking puts a fallback first. Audit
actual model/provider fields identify the attempt; never infer it from proposed.

`x-inferplane-routing-reason` gives a bounded reason.
`x-inferplane-routed-model` advertises applied privacy/context selection, including
privacy enforced alongside Shadow. Existing `x-inferplane-substituted-model`
records the earlier budget tier; it can differ from the final routed model.
`request.routing` audit evidence includes policy names/generations, inspection
state/categories, planned provider/boundary, and actual attempted provider/boundary
on completion/partial outcomes. The optional field preserves old audit chains.
`inferplane_routing_decisions_total{team,mode,reason}` has no model/user/key/session
or detected-value labels. Decision evidence contains no prompt or detected value;
separate opt-in body logging retains its existing behavior.

Before Enforce, define acceptable baseline-relative thresholds and evaluate:

1. Coding-task success, including tests and human acceptance on representative tasks.
2. Total settled cost, including cold-cache writes, repeated turns, and retries.
3. p95 request latency and failures, including fallback paths.
4. Privacy negative cases: opaque content, denied/missing targets, invalid generations,
   region/capability conflicts, internal failures, and attempted external retries.

No measured savings, production readiness, shared-state HA, or durable budgets
follow from unit-test success. Existing per-instance rate/quota/budget limitations
remain. If context quality/cost regresses, return context to Shadow while retaining
privacy enforcement. See [ADR-043](decisions/ADR-043-policy-aware-routing.md).
