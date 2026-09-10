# Roadmap: closing the five operational gaps vs central-proxy gateways

Status: proposed (2026-07-31), still fully open as of 2026-08-14 — none of
the five items below have shipped. Source: critical comparison against LiteLLM —
the split architecture's costs are already being paid (fleet of data planes,
version skew management, distributed accounting); these five items are where
the benefits are still only partially collected.

[Enterprise product strategy](enterprise-strategy.md) is the canonical source for
target market, product contracts, priorities, and production-release gates. This
roadmap tracks execution status and retains the original five-gap work breakdown.

## Purpose alignment (2026-08-14)

`CLAUDE.md` → Core Purpose lists five goals. This table is the internal
priority lens the LiteLLM-gap framing above doesn't give you — it's ordered
by which goal each gap blocks, not by feature parity with a competitor. Its
scope is broader than the five sprint items below: a row can be ✅ even
though none of sprints S1-S3 have shipped, because some goals (e.g. #5) were
already met by earlier work (ADR-031) outside this roadmap.

| Purpose | Status | Evidence |
|---|---|---|
| #1 A single entry point for Claude Code/OpenCode/Codex | 🔶 partial | No Codex-specific code, fixture, or test anywhere in the tree (`grep -ri codex internal/ providers/ tests/` → 0 hits, excluding this doc); the OpenAI-compat ingress (`internal/server/openaiapi/chat.go`) is the presumed path but has never been verified against a real Codex client |
| #2 Per-user model choice | ✅ done | User-subject `modelAccess` rules are enforced: `Store.ModelAllowed` (`internal/policy/store.go`), wired into the router via `SetPolicyGate` in `cmd/mayu/gateway.go`. (Per-user *rate* is a separate, still-blocked item — see #4b; per-user *budget* is enforced as of ADR-042 Phase 3.) |
| #3 Cost-driven model substitution via policy (routing) | ✅ implemented, rollout gates remain (ADR-041/043) | `routing.budgetTiers` is enforceable: `internal/policy/store.go` `checkEnforceable` now rejects the cache-affinity subtype of `routing` (budgetTiers and context are enforceable); the control plane judges utilization globally from the ADR-034 ledger (`internal/controlplane/controlplane.go` `handleSync`) and latches the active tier per budget window (`internal/tier.Latch`); mayu applies it at ingress via `router.SubstituteTier`, never widening access or turning into a denial. Config-level `model_fallbacks` (`internal/router/router.go` `ResolveModel`) remains the separate availability-triggered substitution. Caveats: the window-latch key is an interim calendar-month-UTC derivation pending item ② below's real `windowID`; providerstore/UI pricing fields for `openai_compatible` GPU targets (ADR-041 item 6) and the full two-plane e2e (item 7) are follow-ups. |
| #4a Team budget + block | ✅ done, with caveats | ADR-034 lease pattern bounds team-level overspend across data planes when a control plane is attached (worst case = Σ outstanding grants, not exact; window edges are approximate — ADR-034 §Known limits). Per-key budgets are not lease-managed. Standalone `mayu` (no control plane) gets no lease at all — budget is plain in-memory there, like rate. |
| #4b Per-user budget/rate | 🔶 partial | *Budget* is unblocked (ADR-042 Phase 3): `checkEnforceable` (`internal/policy/store.go`) now rejects only user-subject *rate*; user-subject budget rules are merged by `mergeUserLimits`/`Store.UserLimits` and enforced by the Governor via `governance.SetUserLookup`/`UserPolicy`. *Rate* stays blocked — a per-user rate limit needs the rate-share model (item ① below). And per-user budget has no lease: a user-subject rule is excluded from the control-plane ledger and the consumption report, so with N data planes a user's effective cap is up to N× the configured value (ADR-042 §Accepted limitation, Phase 3) |
| #4c Rate/quota global accuracy under horizontal scale | ❌ blocked | item ① below — in-memory per-replica buckets; N replicas admit up to N× the configured rate/TPM/quota in aggregate |
| #4d Spend visibility | ✅ done | `internal/analytics` + console + `GET /admin/logs` (`analyticsapi.LogsHandler`, backed by the same analytics index — its `events` rows carry `cost_micros` per request, `internal/analytics/index.go:40`) |
| #5 No SPOF (control/data plane split) | ✅ done | ADR-031 — scoped to the control plane not gating the inference path; it does not mean any one `mayu` instance is itself highly available (see #4c and "Current limits") |

**The known tension:** #4c and #5 pull against each other — see `CLAUDE.md` →
Core Purpose. HA work here means closing that gap, not merely adding
replicas; a naive multi-replica deployment currently breaks #4c further
without an accurate shared rate/quota store.

Sprint plan (each phase = separate PR(s), reviewed before the next):

| Sprint | Items | Why together |
|---|---|---|
| S1 (~1 wk) | ① global rate limits + ② durable ledger & window epochs | Both change the sync protocol — one protocol revision, not two |
| S2 (~1 wk) | ④ `mayu doctor` + ③ phase 1 (version visibility) | Pure observability, no protocol risk, unblocks real-world debugging |
| S3 (1–2 wk) | ③ phase 2 (signed self-update) + ⑤ embeddings lane | Release pipeline work + first non-chat modality |

---

## Policy-aware routing v1 (ADR-043)

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
session pinning, learned/remote classifiers, Responses ingress and shared-state HA
are separate work. Upgrade binaries/CRD before activating rules; require_sync is
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

## ② Durable ledger + control-plane-owned budget windows (ADR candidate — unassigned; ADR-037 has since shipped as inferplaned console SSO)

**Gap.** The lease ledger is in-memory: restart re-learns from cumulative
reports, grants issued moments before a crash are re-derived, and — the real
correctness hole — budget windows are per-data-plane tumbling windows, so
"the monthly team budget" is only approximately global. Rollover is detected
heuristically (a cumulative report DECREASING), and pruned dead planes drop
their window spend entirely. ADR-041's budget-tier latch
(`internal/tier.Latch`) currently derives its own interim window key
(calendar-month UTC) rather than waiting on this item — once a real
`windowID` exists it should replace that derivation.

**Design — the control plane owns the window.**
- Each budget rule gets a control-plane-computed `windowID` (calendar month
  UTC, `"2026-08"` — operator-legible beats rolling-30d) carried in every
  grant and echoed in every report.
- mayu keys its cumulative counter by `(rule, windowID)`; when the grant's
  windowID changes, it starts a fresh counter — no more decrease-detection
  heuristics, no more per-plane phase drift. (The local `budget.BudgetStore`
  gains window-id-keyed entries; the 30-day-duration window stays for
  standalone mode.)
- Ledger rows become `(policy, rule, windowID, dataplane) → spent, allowance`.
  Old windows are dropped wholesale at rollover — cleanly, because the ID
  changed, not because a heuristic guessed.

**Design — durability.**
- SQLite file for inferplaned (modernc driver — already a dependency, CGO
  stays off), single-writer, write-behind on each heartbeat (QPS is
  heartbeat-rate × planes; trivial). Tables: `lease_ledger`, `dataplanes`.
- Restart: load ledger → grants resume exactly; the cumulative-report
  self-healing stays as a fallback, stops being the *only* mechanism.
- HA (multiple control-plane replicas) is explicitly out of scope here; the
  interface mirrors `bodystore`'s sqlite/postgres split so a Postgres backend
  can land later without protocol changes.

**Work items.** Store interface + SQLite impl; windowID through
`LeaseGrant`/`ConsumptionReport`; mayu window-keyed counters; rollover
integration test (clock-injected); restart-preserves-ledger test; remove the
decrease-detection path (superseded).

**Risks.** Local budget store schema touch is the riskiest edit (shared with
standalone mode) — gate with the full e2e suite. Calendar-month vs the
existing rolling-30d standalone semantics must be documented as a behavioral
difference between modes until standalone also adopts windowIDs.

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
- Control-plane HA / Postgres ledger backend (interface prepared in ②) — see
  §"HA (multiple control-plane replicas) is explicitly out of scope here"
  above.
- **Data-plane (mayu) shared-state HA** — `keystore`/`limiter`/`budget`
  moving off SQLite/in-memory onto a shared backend so #4c above stops being
  blocked. Maintainer direction as of 2026-08-14 is **Postgres-only**, recorded
  here pending a real ADR (narrower than
  ADR-013's original Postgres+Redis/Valkey design — no Redis dependency is
  planned). Still deferred: implementation has not started, and ADR-013
  itself carries no superseded-by marker yet — a new ADR is needed before
  work begins, and it must resolve what ADR-013 left open (fail-open vs.
  fail-closed on a Postgres outage). See "Purpose alignment" above.
- SSE push stream for policy distribution (poll-at-lease-cadence already
  beats the 60s/15s requirement).
- CRD-watch controller in inferplaned (ADR-035 follow-up).
- User-subject *rate* rules (need a rate-share model — item ① — before the
  ADR-033 gate can accept them; user-subject *budget* shipped in ADR-042
  Phase 3, which narrowed that gate to rate-only).
- Cache-affinity routing engine (the `routing` rule's *affinity* half stays
  rejected until then; the *budgetTiers* half shipped as ADR-041 — it never
  depended on the affinity engine).
