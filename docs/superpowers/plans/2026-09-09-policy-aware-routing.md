# Policy-aware routing implementation plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement local sensitivity-aware destination restrictions and observable,
opt-in context routing for coding-agent requests.

**Architecture:** A stdlib-only inspector reads original ingress bytes. Shared
GovernancePolicy semantics constrain one router decision; all ingresses consume
that filtered chain before admission and provider calls.

**Tech Stack:** Go 1.25, existing SQLite/policy/Prometheus/audit packages; no new dependency.

**Spec:** `docs/superpowers/specs/2026-09-09-policy-aware-routing-design.md`

## Global Constraints

- Go 1.25, pure-Go dependencies, both binaries build with `CGO_ENABLED=0`.
- No new dependency or remote classifier call. Tests use local fakes only.
- Preserve all provider/core and leaf-package boundaries.
- Preserve original request bytes during inspection and routing.
- Existing configurations without the new policies retain their behavior.
- Every candidate, including retries and cross-model fallbacks, must satisfy RBAC,
  region restrictions, ingress compatibility, and the request's data restriction.
- Original-model RBAC must pass; routing cannot rescue an unauthorized request.
- All token-count endpoints return HTTP 200; security refusal makes no upstream call.
- PreCheck precedes provider billing; settlement remains integer microUSD.
- New audit fields are appended with `omitempty` and mixed-version verification.
- No prompt, PII value, client session identifier, or key ID in new metrics/traces.
- Every commit uses DCO sign-off.
- Work in `/tmp/inferplane-policy-routing`; do not edit the user's original checkout.
- Use `env GOCACHE=/tmp/inferplane-go-build GOPROXY=off` for Go commands.
- Never spawn subagents from an implementation/review worker.

## Task 1: Read-only inspection and context signals

**Files:** Create `internal/sensitivity/inspect.go`, `text.go`, `inspect_test.go`,
`text_test.go`; keep additional focused files within this package.

**Consumes:** original request bytes, protocol string and context.
**Produces:** the exact Result, Inspector, NewInspector and MatchesKeywords
interfaces in the spec. This package imports only the standard library.

- [ ] Write table tests before implementation. The cases include email in system,
  nested tool results and escaped JSON tool arguments; valid/invalid Luhn cards;
  Korean identifier shape; no PII; image/document/redacted-thinking as incomplete;
  invalid/unknown shape; cancellation; all three protocols; absent output budget;
  single user turn vs assistant/tool history; keyword matches in decoded text.

```go
raw := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"contact alice@example.test"}]}]}`)
before := append([]byte(nil), raw...)
got, err := NewInspector().Inspect(context.Background(), "anthropic", raw)
if err != nil || !got.Complete || !slices.Contains(got.Categories, "email") {
    t.Fatalf("inspection = %+v, %v", got, err)
}
if !bytes.Equal(raw, before) { t.Fatal("inspection mutated request") }
```

- [ ] Run `go test ./internal/sensitivity -count=1` and record the expected red.
- [ ] Implement bounded JSON traversal of all string values and keys, with
  explicit incomplete status for opaque/unknown blocks and unrecognized body
  shapes. Recursively inspect JSON-encoded tool-argument strings with a depth
  bound. Do not store found text in Result/errors. Use Luhn for cards; phone/PII
  patterns are finite and deterministic. Sort/deduplicate category names.
  Conservatively estimate input tokens from decoded content bytes and parse
  output budgets without floating-point precision loss or overflow.
- [ ] Implement keyword matching using decoded strings and case folding, and
  cancellation checks during traversal. Invalid input to MatchesKeywords cannot
  create a positive result; Inspect owns the error.
- [ ] Run focused tests with `-race`, self-review, and DCO commit. Report exact
  detector coverage and interfaces for the next task.

## Task 2: Policy schema and trusted destination/model metadata

**Files:** Modify `api/v1alpha1/types.go`, `internal/policy/{policy,store}.go`;
add focused policy tests. Modify `internal/config/config.go`, `internal/live/live.go`,
`internal/providerstore/` metadata/migration/overlay code and tests,
`internal/server/configapi/` read/write/export code and tests,
`deploy/crd/` GovernancePolicy schema.

**Consumes:** spec's YAML and destination/capability contract.
**Produces:** validated SensitiveData and Context internal rule types; a Store
method returning matching rules and any fail-closed distribution error; provider
boundary and model capabilities accessible on an immutable live.State snapshot.
Record exact signatures in the task report.

- [ ] Add failing policy conversion cases: valid example; missing/unknown actions;
  FailOpen sensitive rule; wildcard/empty internal model set; context plus
  budgetTiers; invalid mode/threshold/keyword/capability; default Shadow.

```go
// A privacy rule cannot become advisory:
doc.Spec.Rules[0].FailurePolicy = v1alpha1.FailOpen
if _, err := FromV1Alpha1(&doc); err == nil {
    t.Fatal("accepted fail-open sensitive-data policy")
}
```

- [ ] Add failing store tests for team/user matching, caller mutation of returned
  rules, and rejected sensitive policy distribution followed by valid recovery.
  Add metadata round-trip tests through SQLite seed/read/overlay/admin export.
  Existing DB fixtures must migrate with unknown boundary/empty capabilities.
- [ ] Run focused tests and record red.
- [ ] Add `Rule.SensitiveData`, `RoutingRule.Context` and their validated mirror
  types. Exactly one rule kind and one routing subtype; preserve budget/affinity
  semantics. Context requires FailOpen; privacy requires FailClosed.
  New private/context targets must be routed/priced when the store's existing
  target validator is installed. Do not silently discard rejected distributed
  privacy policy; expose an error until corrected.
- [ ] Add provider `data_boundary` (internal/external/unknown default) and model
  `capabilities` (tools/vision/reasoning/structured_output) with closed validation.
  Preserve context_window alongside new metadata through DB routes. Copies in
  live state and policy getters must own their slices.
- [ ] Extend CRD structural schema without changing delivery channels or adding
  unsupported policy claims. Run policy/config/live/providerstore/configapi tests
  with `-race`, self-review, DCO commit, and report concrete consumer signatures.

## Task 3: Shared constrained routing and context decisions

**Files:** Create `internal/router/request_routing.go`,
`request_routing_test.go`; modify router.go only for dependency fields/setters and
captured ChainTarget metadata. Add focused `internal/router/` files as needed.
Modify `cmd/mayu/gateway.go` only to inject policy lookup; add wiring tests.

**Consumes:** Task 1 inspector; Task 2 policy lookup and immutable metadata.
**Produces:** a shared method accepting principal, original body/protocol, current
model/chain/state, allowed regions and count-only flag; returning selected
model/filtered chain/state and safe decision metadata, or a typed error.
Record exact signature and error/status contract for ingress integration.

- [ ] Write adversarial routing tests first. An initial public route must change
  to an approved internal model for protected content. ResolveChain on that model
  may include an external fallback, which must be removed. Add Block tie,
  empty intersection, unknown boundary, RBAC, region, context/capability and
  inspection-error cases.

```go
for _, target := range decision.Chain {
    if target.DataBoundary != "internal" {
        t.Fatalf("unsafe retry candidate: %s", target.ProviderName)
    }
}
```

- [ ] Add Shadow/Enforce tests: source-model match, decoded complex keyword,
  token threshold, conflicting overlapping rules, Shadow dominance, unavailable
  targets, multi-turn/tool/opaque/reasoning/structured-output refusal to switch,
  no new policies equals original behavior, immutable bytes and same generation.
- [ ] Run focused tests and record red.
- [ ] Implement one policy lookup and one original-byte inspection per request.
  Intersect privacy constraints before ranking. Resolve alternatives on the
  supplied live snapshot (do not mix generations with new ResolveChain calls).
  Filter all fallback entries, including primary entries, by all constraints.
  Security errors deny; context failures retain the safe chain.
  Selected alternatives require known context and capabilities plus a price.
  Count-only calls enforce privacy but do not perform context optimization.
- [ ] Wire the Store lookup in mayu's existing policy block; absence is nil-safe.
  Keep logic out of cmd. Test with `go test ./internal/router ./cmd/mayu -race`,
  self-review, DCO commit, and report the exact ingress integration contract.

## Task 4: All ingress paths and decision evidence

**Files:** Modify Anthropic messages/count_tokens, OpenAI chat, Bedrock invoke/
count_tokens handlers. Create focused request-routing integration tests beside
each. Modify `internal/audit/record.go` and a new context helper/test;
`internal/metrics/metrics.go` and cardinality tests; tracing helper if needed.

**Consumes:** Task 3 shared method and bounded decision/error types.
**Produces:** live behavior on all supported ingress paths with safe audit,
headers and metrics.

- [ ] Add failing provider-spy tests BEFORE inserting call sites. Use existing
  handler harnesses and real router/live/policy objects. Cover PII in tool result,
  internal primary transport failure then internal fallback success, external
  fallback never called, Block before admission, native Bedrock decoded token
  count, Anthropic token count always 200, both streaming/non-streaming.

```go
if external.Calls() != 0 { t.Fatal("protected request escaped to external provider") }
if recorder.Code != http.StatusOK { t.Fatal("count_tokens must stay 200") }
```

- [ ] Add Shadow/Enforce handler tests and a mixed old/new audit chain fixture
  proving append-only encoding. Assert detected values never appear in audit,
  new metric labels or decision headers.
- [ ] Run focused tests and record red.
- [ ] Invoke the common method before masking, body capture, PreCheck and any
  provider call. Pass original bytes (decoded inner body for Bedrock counts).
  Replace all of model/chain/pricing state consistently with its result.
  Keep existing budget-substitution evidence. Security errors use the ingress
  error shape; count handlers use local estimates only.
- [ ] Append audit `routing,omitempty` metadata and propagate via request context
  to started/completed/partial records. Add bounded decision counter and applied
  model/reason headers. Old traffic omits new fields.
- [ ] Run relevant server/audit/metrics tests with `-race`, self-review, DCO
  commit, and report all covered call sites.

## Task 5: Release acceptance, examples and product documentation

**Files:** Add `docs/decisions/ADR-043-policy-aware-routing.md`,
`examples/config.policy-routing.json`, `examples/policies/policy-routing.yaml`;
update README, CLAUDE.md, internal/CLAUDE.md, cmd/CLAUDE.md, docs/roadmap.md,
docs/enterprise-strategy.md and affected reference docs. Add acceptance tests in
`cmd/mayu/` or `internal/server/` where real assembly can be exercised offline.

**Consumes:** all prior implemented interfaces and reports.
**Produces:** reviewable working release, documented limits and complete checks.

- [ ] Add any missing end-to-end acceptance cases from the spec using fake
  providers: real policy load/assembly, metadata preserved after DB reload,
  fail-closed rejected privacy generation and recovery, budget substitution
  followed by privacy restriction, governance denial before provider.
  Demonstrate red for any acceptance gap, implement the smallest fix and retest.
- [ ] Create an example whose aliases/providers/pricing/policies agree and whose
  context rule defaults to Shadow. Use env/file secret references only.
  Document that boundary labels are operator assertions and that unsupported
  content fails closed; no claim of universal PII detection, durable session
  pinning, measured savings, HA, or new Responses support.
- [ ] Record the product goal and observable rollout gates: task success,
  total cost including cold-cache/retries, p95 latency, and privacy-policy
  negative cases. Show explicit opt-in Enforce for short requests.
- [ ] Update module docs and reference schemas. AGENTS.md is generated and must
  not be hand-edited; leave it untouched and note CLAUDE.md is the source.
- [ ] Run ALL mandatory build/race/vet/gofmt/harness/diff gates from the spec,
  capture output in the task report, DCO commit, and prepare final review.

## Acceptance record

Worker reports and review packages live in the plan-specific ignored SDD workspace.
The controller records completed tasks, exact commits, decisions and findings in
its progress ledger. Do not report cost savings or production readiness from unit
test success.
