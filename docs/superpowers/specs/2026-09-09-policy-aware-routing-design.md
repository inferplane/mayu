# Policy-aware routing v1

Status: implementation authorized in the 2026-09-09 product-goal conversation.

## Product goal

Keep coding tasks useful while reducing total cost within the organization's
approved data-processing boundaries. Model substitution alone is not evidence of
either savings or privacy. Decisions must be inspectable without retaining prompts.

This release implements the four development steps approved in that conversation:
typed local inspection, sensitive-data routing, observable context selection, and
opt-in selection for short requests. Actual task-success/cost improvement remains a
rollout evaluation gate, not a claim made by this implementation.

## Existing behavior and constraints

`mayu` currently resolves aliases/model fallbacks and budget tiers, checks RBAC and
regions, optionally masks text, then performs governance admission and provider
calls. The new decision stage inspects ORIGINAL ingress bytes before masking.
Existing budget substitution remains optional and never denies on its own.
Sensitive-data restrictions are a separate security policy that CAN deny.

- Go 1.25, pure-Go dependencies, both binaries build with `CGO_ENABLED=0`.
- No new dependency or remote classifier call. Tests use local fakes only.
- Preserve all provider/core and leaf-package boundaries.
- Preserve original request bytes during inspection and routing. Only existing
  explicit masking and provider model-ID/protocol translation may transform bytes.
- Existing configurations without the new policies retain their behavior.
- Every candidate, including retries and cross-model fallbacks, must satisfy RBAC,
  region restrictions, ingress compatibility, and the request's data restriction.
- The original requested/resolved model must pass RBAC; routing cannot rescue an
  unauthorized request.
- All token-count endpoints return HTTP 200. A security refusal uses a local
  estimate and makes NO upstream request.
- PreCheck precedes provider billing; settlement remains integer microUSD.
- New audit fields are appended with `omitempty` and mixed-version verification.
- No prompt, PII value, client session identifier, or key ID in new metrics/traces.
- Every commit uses DCO sign-off.

## Scope and boundaries

The v1 privacy actions are `InternalOnly` and `Block`. Existing opt-in masking
continues independently; this release does not pretend that the legacy mask
walker covers every surface inspected by the new scanner.

Local inspection covers JSON string content across system/developer prompts,
messages, nested tool inputs/results, tool definitions and metadata. It recognizes
email, phone, credit card with Luhn, US SSN, IPv4 and Korean resident-registration
number shapes. These are detectors, not a guarantee that every PII type is found.
Opaque media (image/audio/document/file payloads), redacted/encrypted thinking,
unrecognized content-block types and unrecognized body shapes are explicitly
uninspectable. Inspectable reasoning text is scanned. Malformed JSON and cancelled
inspection are errors. Inspection never fetches links, decodes arbitrary binary
payloads, or persists text.

No session state is inferred from an untrusted client header. Entire submitted
history is inspected on each call. Context enforcement only applies to a single
user-turn request with no assistant/tool history, no tool definitions/calls, no
opaque content, no reasoning or structured-output requirement. Multi-turn traffic
is observed but its model is not switched by context rules. Privacy restrictions
still apply to every turn. Stable session identity/pinning and remote/learned
classifiers require a separate design.

Responses ingress, durable fleet enforcement, identity redesign, fleet deployment,
and new cloud resources are not part of this routing release.

## Public configuration

Providers gain `data_boundary`: `internal` or `external`. Omission is unknown and
does NOT satisfy InternalOnly. This is an operator attestation about the actual
endpoint, not a hostname heuristic or a model-name inference.

Models gain `capabilities`, a list drawn from `tools`, `vision`, `reasoning`,
`structured_output`, plus the existing `context_window`. Unknown capabilities are
rejected. Automatically selected alternatives require a declared context window
large enough for the conservative input estimate plus declared output budget and
all observed required capabilities. Existing explicitly selected models retain
their existing context behavior. Metadata must round-trip through config, live
state, SQLite providerstore seed/overlay, and admin read/write/export.

GovernancePolicy gets an independent `sensitiveData` rule:

```yaml
apiVersion: inferplane.dev/v1alpha1
kind: GovernancePolicy
metadata:
  name: engineering-routing
spec:
  subject: {team: engineering}
  rules:
    - name: protected-content
      failurePolicy: FailClosed
      sensitiveData:
        onDetected: InternalOnly
        onUninspectable: Block
        internalModels: [private-coder]
    - name: short-task-selection
      failurePolicy: FailOpen
      routing:
        context:
          mode: Shadow
          fromModels: [premium-coder]
          simpleModel: economy-coder
          complexModel: premium-coder
          maxSimpleInputTokens: 4096
          complexKeywords: [security, authentication, migration]
```

`onDetected` and `onUninspectable` are explicit required values from
`InternalOnly | Block`. Any InternalOnly action requires a non-empty
`internalModels` list of explicit names (no wildcard). Only FailClosed is accepted.
No matching detector means no extra destination restriction only if inspection
completed. Inspection errors always deny.

Context rules are a third mutually exclusive `routing` shape, alongside affinity
and budgetTiers. `mode` is `Shadow` (default) or `Enforce`; `fromModels`, both target
models, and positive `maxSimpleInputTokens` are required. Keywords must be nonempty
strings when present. Context rules require FailOpen: an unusable recommendation
leaves the already-safe route unchanged. The threshold is an operator rule, not a
quality score. Context chooses complex when input is above threshold or any
configured keyword matches decoded request text case-insensitively.

All matching sensitive rules apply. Block wins. Internal model sets intersect,
including team/user overlays. An empty intersection denies. Multiple applicable
context recommendations must agree; otherwise context makes no change. Any Shadow
rule in the applicable set prevents enforced context switching.

Policy conversion, local files, control-plane distribution and CRD validation use
the same types. A rejected distributed sensitive-data document must NOT silently
remove protection: request-routing policy lookup fails closed until a valid
generation is applied. This includes rejection of a previously active sensitive
document. Other established partial-acceptance semantics remain unchanged.

## Runtime decision

One router method owns the policy decision and returns a filtered immutable
snapshot of the complete attempt chain. Call it after existing model resolution,
budget substitution, RBAC and region/ingress filtering, but before masking,
PreCheck, body capture, or any upstream call.

1. Load matching policy rules from one policy snapshot.
2. If no new rules match, return the existing route without inspection.
3. Inspect original ingress bytes once.
4. Apply all sensitive rules. Restrict every target to both an approved internal
   model and a provider explicitly labeled internal. If the existing route becomes
   empty, resolve the approved internal models, subject to RBAC, region, ingress,
   context and capability checks. Never reinsert a public fallback.
5. Evaluate context recommendations within that safe candidate boundary.
6. In Shadow mode, record proposed model while retaining the safe actual chain.
   In Enforce mode switch only an eligible short request to a compatible candidate.
   A missing/denied/unpriced/incompatible recommendation leaves the safe chain.
7. Return a decision containing only bounded reasons, inspection state, category
   names, policy references/generations, requested/selected/proposed models and mode.

Use the same topology snapshot for decisions, provider attempts and pricing. An
empty safe chain is an explicit security error, never an implicit default route.
All attempts are decided before the first provider call; existing pre-first-event
retry behavior consumes only that chain. Native Bedrock retains its ingress
restriction. OpenAI ingress must not select direct Anthropic egress, whose current
implementation cannot translate an OpenAI raw body.

## Interfaces

The first task establishes `internal/sensitivity` as a stdlib-only leaf:

```go
type Result struct {
    Complete bool
    Categories []string
    InputTokens int64
    OutputTokens int64
    UserTurns int
    HasHistory bool
    HasTools bool
    HasVision bool
    HasReasoning bool
    HasStructuredOutput bool
}
type Inspector interface {
    Inspect(context.Context, string, []byte) (Result, error)
}
func NewInspector() Inspector
func MatchesKeywords([]byte, []string) bool
```

Strings and byte slices passed to these functions are borrowed, never mutated.
Only explicit protocols `anthropic`, `openai`, `bedrock` are accepted. Bedrock
CountTokens passes its already-decoded inner body.

Subsequent implementation tasks may choose internal type names to fit existing
code, but must record produced signatures in their report before consumers begin.
Policy remains the shared semantics leaf. Router may import policy/sensitivity;
neither imports router, server or config.

## Observability and acceptance

Append `routing` to audit RequestRef with `omitempty`. Record the same decision on
started/completed/partial outcomes, including denials where identity is known.
Add a fixed-shape routing decision counter with team, mode and bounded reason.
Advertise an applied selection via `x-inferplane-routed-model` and a bounded reason
header; do not expose raw detections.

Acceptance tests demonstrate:

- Original bytes unchanged, including cache-control/tool payloads.
- PII in user/system/tool-result/nested arguments changes the destination.
- Internal primary failure can retry another approved internal target, never a
  configured external model/provider fallback.
- Unknown destination, RBAC failure, region mismatch, missing capability, oversized
  alternative and inspection error cannot cause an unsafe call.
- Block beats InternalOnly across team/user policies.
- Both token-count APIs make zero upstream calls on denied content and return 200.
- Streaming and non-streaming use the same destination ceiling.
- Shadow records a recommendation without switching; Enforce switches an eligible
  single-turn request, while multi-turn/tools remain on the existing safe route.
- Invalid privacy policy distribution fails closed; corrected generation recovers.
- Old audit fixtures still verify; decisions contain no detected values.

Mandatory final gates:

```bash
CGO_ENABLED=0 go build -trimpath -o /tmp/inferplane-routing-mayu ./cmd/mayu
CGO_ENABLED=0 go build -trimpath -o /tmp/inferplane-routing-inferplaned ./cmd/inferplaned
go test ./... -race
go vet ./...
gofmt -l .
bash tests/run-all.sh
git diff --check
```
