# Roadmap: closing the five operational gaps vs central-proxy gateways

Status updated 2026-09-12: item ② ships as ADR-045 node-local monetary escrow;
ADR-046 now implements shared keys and global rate/token quotas (item ①) in an
explicit Postgres shared gateway profile. Remaining fleet features are listed below.

[Enterprise product strategy](enterprise-strategy.md) is the canonical source for
target market, product contracts, priorities, and production-release gates. This
roadmap tracks execution status and retains the original five-gap work breakdown.

## Purpose alignment (2026-09-12)

`CLAUDE.md` → Core Purpose lists five goals. This table is the internal
priority lens the LiteLLM-gap framing above doesn't give you — it's ordered
by which goal each gap blocks, not by feature parity with a competitor. Its
scope is broader than the five sprint items below: a row can be ✅ even
though none of sprints S1-S3 have shipped, because some goals (e.g. #5) were
already met by earlier work (ADR-031) outside this roadmap.

| Purpose | Status | Evidence |
|---|---|---|
| #1 A single entry point for Claude Code/OpenCode/Codex | 🔶 protocol support implemented; model evaluation remains | Messages, Chat Completions, Bedrock Invoke and Responses ingresses; native Responses plus stateless adapters, original-byte policy enforcement, and opt-in installed Codex CLI tool round trips (ADR-044). Opaque/stateful cross-model transfer and arbitrary model quality are not implied. |
| #2 Per-user model choice | ✅ done | User-subject `modelAccess` rules are enforced: `Store.ModelAllowed` (`internal/policy/store.go`), wired into the router via `SetPolicyGate` in `cmd/mayu/gateway.go`. ADR-046 also supports global per-user rate/token quota in the shared profile. |
| #3 Cost-driven model substitution via policy (routing) | ✅ implemented, rollout gates remain (ADR-041/043/044) | Legacy optional tiers retain their behavior. Opt-in `enforceTargets` constrains all choices/retries and allows threshold 100. Soft strict-tier references are switching meters, independent of total hard admission caps. Three-class context and bounded local successful-target affinity compose with privacy constraints. Evaluation uses integer utilization and referenced day/month windows; durable mode uses database-owned UTC window IDs (ADR-045). |
| #4a Team budget + block | ✅ durable profile implemented | ADR-045 reserves finite global monetary authority in Postgres and per-attempt bounds in private node journals. Legacy ADR-034 and standalone/key-local budgets retain their limits. |
| #4b Per-user budget/rate | ✅ shared profile implemented | ADR-045 global monetary budgets; ADR-046 global user-only/team-user RPM/TPM and tokenQuota. Non-shared profiles still reject unsupported user-rate/token-quota rules. |
| #4c Rate/quota global accuracy under horizontal scale | ✅ shared profile implemented | ADR-046 Postgres transactions reserve all matching scopes atomically; shared keys and counters survive gateway restart. Requires an available HA DB endpoint. |
| #4d Spend visibility | ✅ done | `internal/analytics` + console + `GET /admin/logs` (`analyticsapi.LogsHandler`, backed by the same analytics index — its `events` rows carry `cost_micros` per request, `internal/analytics/index.go:40`) |
| #5 No central inference SPOF | ✅ explicit deployment profiles | Node-local ADR-045 continues on valid local credit. Shared ADR-046 uses multiple gateways plus HA Postgres, with no per-request control-plane HTTP call; DB partitions fail closed. |

**The known tension:** #4c and #5 pull against each other — see `CLAUDE.md` →
Core Purpose. HA work here means closing that gap, not merely adding
replicas; a naive multi-replica deployment currently breaks #4c further
without an accurate shared rate/quota store.

Sprint plan (each phase = separate PR(s), reviewed before the next):

| Sprint | Items | Why together |
|---|---|---|
| S1 | ①/② implemented as ADR-045/046 profiles | Local monetary escrow and synchronous shared resource admission have explicit, separate failure contracts |
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

## ① Shared keys and global rate/token quotas — implemented (ADR-046)

The implementation uses transactional Postgres admission for the explicit shared
gateway profile. It replaces the earlier proposed rate-share/Redis design for
that deployment mode. Keys and team permissions share one authority; each provider
attempt atomically reserves all matching team/key/user resources. Complete known
usage releases unused amounts, while uncertain outcomes remain reserved.

Policy money competes in the same ADR-045 accounts as outstanding node-local
grants. A persistent namespace and policy-generation checks prevent mixed authority
sources. Rate refill is exact and database-clock based; token quotas use UTC
calendar day/month windows. Shared usage identifies reservations separately.

See [operator and migration guide](shared-governance.md). Default node-local rate
buckets remain local; disconnected global rate/key authority is not claimed.
Mutable shared provider topology, retention/reconciliation APIs and production
throughput benchmarking remain outside this implementation.

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
- **Mutable shared provider topology** remains deferred. ADR-046 shared gateways
  use a common file/ConfigMap rollout and reject the SQLite provider-store option.
  Shared key/rate/quota and key-local money enforcement are implemented.
- SSE push stream for policy distribution (poll-at-lease-cadence already
  beats the 60s/15s requirement).
- CRD-watch controller in inferplaned (ADR-035 follow-up).
- Disconnected global rate/key leases; shared-profile user rates/token quotas ship in ADR-046.
- Cache-affinity routing engine (the `routing` rule's *affinity* half stays
  rejected until then; the *budgetTiers* half shipped as ADR-041 — it never
  depended on the affinity engine).
