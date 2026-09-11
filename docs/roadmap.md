# Roadmap: closing the five operational gaps vs central-proxy gateways

Status updated 2026-09-11: item ② ships as the opt-in Postgres monetary
authority in ADR-045. Global rates/quotas, shared key storage and the remaining
fleet features below are still open. The original comparison against LiteLLM
motivated these gaps; implementation status is recorded separately below.

[Enterprise product strategy](enterprise-strategy.md) is the canonical source for
target market, product contracts, priorities, and production-release gates. This
roadmap tracks execution status and retains the original five-gap work breakdown.

## Purpose alignment (2026-09-11)

`CLAUDE.md` → Core Purpose lists five goals. This table is the internal
priority lens the LiteLLM-gap framing above doesn't give you — it's ordered
by which goal each gap blocks, not by feature parity with a competitor. Its
scope is broader than the five sprint items below: a row can be ✅ even
though none of sprints S1-S3 have shipped, because some goals (e.g. #5) were
already met by earlier work (ADR-031) outside this roadmap.

| Purpose | Status | Evidence |
|---|---|---|
| #1 A single entry point for Claude Code/OpenCode/Codex | 🔶 protocol support implemented; model evaluation remains | Messages, Chat Completions, Bedrock Invoke and Responses ingresses; native Responses plus stateless adapters, original-byte policy enforcement, and opt-in installed Codex CLI tool round trips (ADR-044). Opaque/stateful cross-model transfer and arbitrary model quality are not implied. |
| #2 Per-user model choice | ✅ done | User-subject `modelAccess` rules are enforced: `Store.ModelAllowed` (`internal/policy/store.go`), wired into the router via `SetPolicyGate` in `cmd/mayu/gateway.go`. (Per-user *rate* is a separate, still-blocked item — see #4b; per-user *budget* is enforced as of ADR-042 Phase 3.) |
| #3 Cost-driven model substitution via policy (routing) | ✅ implemented, rollout gates remain (ADR-041/043/044) | Legacy optional tiers retain their behavior. Opt-in `enforceTargets` constrains all choices/retries and allows threshold 100. Soft strict-tier references are switching meters, independent of total hard admission caps. Three-class context and bounded local successful-target affinity compose with privacy constraints. Evaluation uses integer utilization and referenced day/month windows; durable mode uses database-owned UTC window IDs (ADR-045). |
| #4a Team budget + block | ✅ durable profile implemented | ADR-045 reserves finite global monetary authority in Postgres and per-attempt bounds in private node journals. Legacy ADR-034 and standalone/key-local budgets retain their limits. |
| #4b Per-user budget/rate | 🔶 budget implemented; rate open | ADR-045 includes user-subject monetary budgets across nodes/teams using stable opaque identity. Legacy ADR-042 local enforcement remains. User-subject rate still needs item ①. |
| #4c Rate/quota global accuracy under horizontal scale | ❌ blocked | item ① below — in-memory per-replica buckets; N replicas admit up to N× the configured rate/TPM/quota in aggregate |
| #4d Spend visibility | ✅ done | `internal/analytics` + console + `GET /admin/logs` (`analyticsapi.LogsHandler`, backed by the same analytics index — its `events` rows carry `cost_micros` per request, `internal/analytics/index.go:40`) |
| #5 No central inference SPOF | ✅ separation and finite outage authority | ADR-045 supports interchangeable control planes sharing Postgres; valid node grants continue until deadline. Database availability must be deployed, and shared-gateway key/rate/quota state is still separate. |

**The known tension:** #4c and #5 pull against each other — see `CLAUDE.md` →
Core Purpose. HA work here means closing that gap, not merely adding
replicas; a naive multi-replica deployment currently breaks #4c further
without an accurate shared rate/quota store.

Sprint plan (each phase = separate PR(s), reviewed before the next):

| Sprint | Items | Why together |
|---|---|---|
| S1 | ② implemented (ADR-045); ① remains open | Durable monetary authority has an explicit negotiated protocol; rate shares remain separate work |
| S2 (~1 wk) | ④ `mayu doctor` + ③ phase 1 (version visibility) | Pure observability, no protocol risk, unblocks real-world debugging |
| S3 (1–2 wk) | ③ phase 2 (signed self-update) + ⑤ embeddings lane | Release pipeline work + first non-chat modality |

---

## Policy-aware routing v1 (ADR-043, extended by ADR-044)

Implemented: original-byte local inspection; FailClosed sensitiveData destination
restrictions on every attempt; Shadow-default context recommendations; opt-in
Enforce for completely inspectable single-user-turn requests without history/tools/
media/reasoning/structured output; persisted topology metadata; ingress/count
integration; bounded audit/headers/counter evidence; startup and policy-apply target
validation. The input threshold chooses simple versus complex, not eligibility;
a distinct compatible complex target can be selected above it. This extends
cost-driven routing without replacing ADR-041 budgets.

Rollout evaluation remains open: task success, total cost including cold-cache
writes/retries, p95 latency, and privacy-policy negative cases must pass declared
baseline-relative gates before Enforce. Finite detectors and translator capabilities
remain limits; this is not universal PII detection or measured savings. Durable
cross-node session pinning, learned/remote classifiers and shared-state HA
remain separate work. ADR-044 adds bounded local pins, Responses and complete Mask
policy handling; see [adaptive routing](adaptive-routing.md). Upgrade binaries/CRD before activating rules; require_sync is
needed for CP privacy before first request. See [guide](policy-routing.md).

## ① Global rate limits via rate shares (ADR candidate — unassigned; ADR-036 has since shipped as control-plane usage telemetry)

**Gap.** `rpm`/`tpm` enforce against per-proxy in-memory buckets
(`limiter.NewMemory`): a team capped at 300 rpm with 20 connected data planes
can actually reach ~6,000 rpm. Budgets were globalized by leases (ADR-034);
rate was not. LiteLLM gets this "free" via Redis.

**Why budgets' lease design doesn't transfer as-is.** A budget is a stock
(cumulative, settles later); rate is a flow (per-minute, must be right *now*).
Cumulative allowances don't mean anything for a flow — what can be divided is
the *rate itself*.

**Design — rate shares.** The control plane divides each rate rule's global
rpm/tpm among currently-active data planes and hands each a share in the
existing heartbeat:

- `SyncResponse` gains `rateShares: [{policy, rule, team, rpm, tpm, expiresAt}]`.
- Split policy: proportional to each plane's reported recent consumption
  (EWMA over the last few heartbeats, reported in `ConsumptionReport` as
  `recentRPM`/`recentTPM`), with an equal-split floor so an idle plane can
  always start working without waiting a rebalance. Σ shares ≤ global limit,
  always.
- mayu clamps the governor's team `RatePerMin`/`TokensPerMinute` to its share
  (same seam as the budget allowance clamp — the team-lookup closure).
- Failure semantics: rate rules are FailOpen in practice — on lease expiry
  keep the last share (never widen to the global limit, never zero). No
  hard-cap analogue: a rate limit protects throughput, not money.
- Rebalance cadence = the heartbeat (10s default): a plane going quiet
  releases its share within one lease horizon (3× renew), same mechanism as
  budget grant release.

**Work items.**
1. Protocol: `RateShare` grant type + EWMA fields in reports (additive,
   `omitempty` — old planes simply don't receive/report them).
2. Control plane: per-rule share ledger keyed on the active-dataplane set
   (reuses `dpInfo.LastSeen` liveness from ADR-034 review fixes).
3. mayu: share table next to `LeaseTable`; clamp wiring; tests: 2-plane
   proportional split, idle floor, dead-plane share release, Σ ≤ limit
   invariant under churn.
4. e2e: two gateways against one control plane, 429 appears at the *global*
   limit, not N× it.

**Risks.** Share rebalancing lag (≤1 heartbeat) lets a suddenly-hot plane 429
briefly while holding a small share — acceptable; document. Bursty split
(burst = share) mirrors existing team-bucket burst semantics.

---

## ② Durable ledger + control-plane-owned budget windows — implemented (ADR-045)

The accepted implementation replaces the earlier proposed SQLite/write-behind
ledger with transactional Postgres escrow. Policies and authority share a
database/schema. Issuers read fresh policy under lock, reserve each grant before
replying, and preserve liabilities across retries, expired leases and restart.
Database time owns UTC day/month windows, including late reports and tier latches.

Each node uses a private SQLite journal to reserve a conservative bound before
every provider attempt. Complete observed usage releases the unused local portion;
partial or unknown results retain uncertainty. Restart burns old open grants and
fences earlier journal owners. Inference does not call Postgres or the control
plane. Real integration tests run two control planes and two gateways against one
database, including restart, concurrency, rollover and replay cases.

Enable both ends explicitly; see [configuration and failure behavior](durable-budgets.md).
Legacy clients are rejected by a durable authority server instead of receiving
weaker grants. This implements global GovernancePolicy money budgets, including
user scopes; it does not globalize key-local budgets, rates, quotas or key storage.

---

## ③ mayu version channel + signed self-update (ADR candidate — unassigned; ADR-038 has since shipped as the control-plane policy store)

**Gap.** mayu on developer laptops is an endpoint-agent fleet with no update
mechanism. Version skew is *detected* (heartbeat + `/v1alpha1/dataplanes`)
but the tail of stale planes can only shrink by hand today.

**Phase 1 — visibility & advice (S2, ~1 day).**
- Embed version via `-ldflags -X` at build; add `version` to `SyncRequest`;
  dataplane view shows the version distribution (the operator's "can I ship
  this rule yet" check gets a second axis besides apiVersions).
- Control plane config `minimumVersion`: sync responses include
  `updateAdvice {minVersion, url}`; mayu logs a loud warning and exposes it
  on `mayu version --check`. Advice only — nothing auto-applies.

**Phase 2 — signed manual update (S3).**
- Release pipeline: goreleaser + minisign/cosign signatures on artifacts;
  public key embedded in the binary at build.
- `mayu update [--channel stable]`: fetch → verify signature → atomic swap
  (write sibling, rename, keep previous as `.old`) → user restarts. No
  root: installs to the user-writable location it runs from. K8s is
  excluded — the image pipeline owns node upgrades there.

**Phase 3 — auto-update channel (later).** Idle-window self-update with
health self-check + rollback to `.old` on boot failure.

**The security constraint that shapes all of it.** The control plane must
never be able to push executable content — only a *version pin*, which the
data plane independently verifies against the embedded release public key.
Otherwise a control-plane compromise is RCE on every laptop. This is
non-negotiable and goes in the ADR's security section.

---

## ④ `mayu doctor` (S2, ~1–2 days)

**Gap.** Distributed debugging: "it fails only on my machine" requires
inspecting that node's state, and today that means grepping logs.

**Design.** One command, human output + `--json` for support tickets:

- config: parse/validation result, which policy source (files vs control
  plane), secret refs resolvable (never the values);
- control plane: reachability, auth OK, latency, applied generation vs
  server generation, pending rejections;
- governance: applied policies per team, lease table (allowance/spent/expiry,
  from `Governor.UsageOf` + `LeaseTable`), rate shares once ① lands;
- providers: connection probes (reuse `configapi/probe.go`'s SSRF-guarded
  prober), pricing coverage (reuse `live.UnpricedTargets`);
- environment: version, supported apiVersions, clock skew vs control plane
  (lease expiry math depends on it), listen-port conflicts.

Also `GET /admin/debug/governance` (admin-auth, secret-free DTO — same
redaction posture as `/admin/config`) so an operator can pull the same
snapshot remotely from a machine they can't shell into.

**Risk.** Leakage — every field goes through the existing secret-free view
discipline; `key_id`/owner stay out of the JSON by default.

---

## ⑤ Provider coverage: embeddings first (ADR candidate — unassigned; next available slot is ADR-040 as of 2026-08-14)

**Gap.** Three provider types, chat-only. The canonical schema is a
Messages-superset — embeddings structurally don't fit it, and forcing them
through it would violate the lossless-round-trip invariant.

**Design — a governed passthrough lane, not a canonical one.**
- New ingress `POST /v1/embeddings` (OpenAI wire shape — the de-facto
  standard clients speak).
- Providers opt in via an *optional* interface (`providers.Embedder`,
  discovered by type assertion) so existing provider packages and the §8
  zero-core-diff rule survive: `openai_compatible` forwards verbatim
  (§4.4 applies trivially), `bedrock` adds Titan/Cohere embed model mapping;
  `anthropic` simply doesn't implement it → clean 404 per model.
- Governance is identical: KeyAuth → canonicalize → modelAccess gate →
  PreCheck → forward → Settle with usage tokens × per-mtok input rate
  (embeddings have no output tokens; pricing table already keys on
  (provider, upstream)).
- Explicitly NOT in this phase: images, audio, rerank — each gets its own
  lane decision later; and no new chat providers until the lane pattern is
  proven (Gemini/Vertex next, via their OpenAI-compat endpoints first).

**Risk.** Scope creep is the failure mode — the lane pattern (optional
interface + governed passthrough) is the deliverable; Titan/Cohere mapping
details are swappable.

---

## Explicitly deferred (so the list stays five)

- **Credential brokering (ADR-040, Accepted — design gate passed)** — inferplaned vends
  short-lived STS Bedrock credentials so `bedrock:Invoke*` leaves
  developer/node IAM entirely (bypassing mayu then yields no credentials).
  Accepted 2026-08-18 after a 3-round 3-AI design gate (10 findings fixed).
  Requires a dedicated `INFERPLANED_BROKER_TOKEN` (never the heartbeat
  token) and auth-mode validation in mayu's config loader.
- **Data-plane shared key/rate/quota state** remains deferred. ADR-045 implements
  global money budgets and interchangeable control-plane issuers; it does not
  replace SQLite virtual-key storage or node-local rate/token buckets. ADR-013's
  broader shared-gateway design is not declared complete by that narrower change.
- SSE push stream for policy distribution (poll-at-lease-cadence already
  beats the 60s/15s requirement).
- CRD-watch controller in inferplaned (ADR-035 follow-up).
- User-subject *rate* rules (need a rate-share model — item ① — before the
  ADR-033 gate can accept them; user-subject *budget* shipped in ADR-042
  Phase 3, which narrowed that gate to rate-only).
- Cache-affinity routing engine (the `routing` rule's *affinity* half stays
  rejected until then; the *budgetTiers* half shipped as ADR-041 — it never
  depended on the affinity engine).
