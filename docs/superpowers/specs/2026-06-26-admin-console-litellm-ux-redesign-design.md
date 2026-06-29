# Admin Console UX Redesign — LiteLLM-parity within the data-free / toolchain-free envelope

**Date:** 2026-06-26
**Status:** Draft — hardened through **two** multi-AI consensus review rounds. Round 1
(codex gpt-5.5 · agy · kiro-cli glm-5): 2 CRITICAL + 11 MAJOR + 4 MINOR, all resolved
(§16). Round 2 (codex gpt-5.5 · agy · kiro-cli kimi-k2.5), after user decisions A1/A2:
panel verified all round-1 fixes SOUND; 16 new findings on the Mode B aggregator +
body-store surface, all resolved (§17). No CRITICAL/MAJOR remains at the design-spec
layer. **Pending final user approval → writing-plans.**
**Author:** brainstorming session (host: Claude Code)
**Source needs:** [`Customer_needs.md`](../../../Customer_needs.md) (Slack pains, web research, KG)
**Related:** ADR-001 (data-free admin-key console), ADR-002 (console grows but stays
toolchain-free), ADR-003 (governance + usability differentiation vs LiteLLM),
ADR-004 (OIDC admin authz), ADR-005 (provider-visibility, no-UI-write default),
ADR-008 (UI-write provider registration / `providerstore`), ADR-010 (self-service
key page / `/admin/whoami`), ADR-014 (provider-registration UX, LiteLLM parity),
design spec §2.2 (canonical schema), §4.4 (cache invariant), §7 (secret-ref mandate),
§8 (provider isolation / zero-core-diff).

## 요약 (Korean abstract)

inferplane 관리 콘솔을 LiteLLM 수준의 정보구조(IA)와 일반 SaaS 대시보드 외관으로
재설계한다. 단, inferplane의 차별점인 **data-free 브라우저**(ADR-001)와 **toolchain-free
envelope**(ADR-002, vanilla JS + `go:embed`, 프레임워크/빌드 금지)는 절대 유지한다.
LiteLLM의 풍부한 분석/로그가 가능한 이유는 스펜드·프롬프트 데이터를 저장·조회하기
때문인데, 우리는 이를 **변조불가 감사 로그(authoritative) → 재구성 가능한 분석
인덱스(derived) → 토큰 게이트 조회 API → data-free 브라우저** 파이프라인으로 구현해
보안 불변식을 깨지 않고 LiteLLM 풍부함을 얻는다. `Customer_needs.md`의 20개 항목 전부를
콘솔 표면 / 백엔드 의존성 / 명시적 범위외 중 하나로 매핑한다(§7 커버리지 매트릭스).

---

## 1. Goal & non-goals

### Goal
Bring the admin console to **LiteLLM-parity on information architecture, governance
UX, and visual polish** — usage/spend analytics, a request-log viewer, full key
lifecycle, teams & users, model health, and governance config — **without** weakening
the two properties that are inferplane's actual moat over LiteLLM:

> **Scope of "parity" (review-corrected).** This is parity on the *console / IA /
> governance* dimensions, **not** on LiteLLM's *routing-engine* features. Weighted
> load balancing (rpm/tpm/weight per deployment), wildcard model mapping, and
> multi-cloud provider breadth are **explicitly out of scope** (§11) — they are
> router/HA-engine changes (ADR-013/014 defer them), not console UX. The summary and
> any marketing copy must not claim "full LiteLLM parity" unqualified.

1. **Data-free browser** (ADR-001/004): no secret and no business data is persisted
   client-side; the admin credential lives in page memory only; everything is fetched
   on demand from token-gated admin APIs. Enforced by `adminui_test`.
2. **Toolchain-free envelope** (ADR-002): vanilla HTML/CSS/JS embedded via `go:embed`;
   **no SPA framework, no node build step** in the critical path.

The redesign also closes the relevant `Customer_needs.md` must-haves *that have a
console surface*, and honestly flags the ones that require backend work before the UI
can light up.

### Non-goals (this spec)
- It does **not** itself implement the backend features it depends on (analytics index,
  per-key budgets, teams as records, budget-alert emitter, Bedrock Guardrails, region
  enforcement). Those are **named as dependencies** and sequenced (§8–§9); each is its
  own ADR + plan.
- It does **not** change the data-plane request path, the canonical schema, the cache
  invariant, or `count_tokens` semantics.
- It does **not** adopt LiteLLM behaviors that violate our mandates (paste-raw-key,
  client-side data storage) — see §11.

---

## 2. Where we are today (baseline)

Current console (`internal/server/adminui/static/`, ~313-line `index.html` + ~763-line
`app.js`) is a 5-view data-free SPA served at `/admin/ui/`:

| View | Data source | Notes |
|------|-------------|-------|
| Overview | `/metrics` | stat cards + traffic-by-model + recent keys |
| Virtual keys | `/admin/keys` + `/admin/whoami` | issue (team + allowed-models only), one-time reveal, revoke |
| Providers | `/admin/config`, `/admin/providers/*` | read-only by default; register/test/export when `providerstore` on (ADR-008/014) |
| Governance | `/metrics` + `/admin/audit/verify` | quota gauge, cumulative spend counter, chain verify |
| Quickstart | client-side | copy-paste snippets |

**Honest gaps vs LiteLLM** (grounded in the inventory):
- No time-series/drill-down spend analytics (only cumulative counters).
- No request-log viewer (cannot inspect a single request).
- Key creation captures only `team` + `allowed models` — no budget / TPM / RPM /
  expiry / metadata / owner.
- Teams are *implicit* (derived from keys); no team or user records, budgets, membership.
- No model health/latency dashboard (probe exists per ADR-014 D2/D5 but is on-demand,
  single-shot, in the Providers form).
- No budget-alert config, guardrail config, or region-policy surface.
- `app.js` is a single 763-line file (a "file grown too large" signal).

**Strengths to preserve & surface better:** verbatim cache invariant (§4.4 — directly
counters LiteLLM's caching-cost bug), tamper-evident audit chain + S3 anchoring
(ADR-003/012), data-free console, secret-ref-honest provider registration (ADR-014),
priority failover + circuit breaker, single static binary / region-deployable (NCT fit).

---

## 3. The hard constraints this design must honor

These are non-negotiable; every decision below is checked against them.

| # | Constraint | Source | Implication for this redesign |
|---|-----------|--------|------------------------------|
| C1 | **Data-free browser** — no client-side persistence of secrets or data | ADR-001, `adminui_test` | charts render from on-demand fetched JSON; no `localStorage`/`sessionStorage`; in-memory page-session cache only |
| C2 | **Toolchain-free** — vanilla JS + `go:embed`, no framework/build | ADR-002, ADR-014 §alt-3 | conventional *aesthetic* via plain CSS/JS; charting via a tiny vendored lib or hand-rolled SVG, embedded — no React/Vite |
| C3 | **Secret-ref mandate** — refs only, never values | §7 | Settings/Providers show env/file ref names or IAM mode only |
| C4 | **`/metrics` cardinality-bounded** — no `key_id`/raw-input labels | CLAUDE.md security mandate | per-key / per-request drill-down comes from the **query API**, never from metric labels |
| C5 | **`count_tokens` never non-200** | CLAUDE.md | unchanged; console never alters the data path |
| C6 | **Zero-core-diff provider isolation** | §8 | health/latency surface reuses the optional `HealthChecker` capability; no per-provider core change |
| C7 | **Audit chain stays authoritative & tamper-evident** | ADR-003/012 | the analytics index is a **derived, rebuildable read-model**, never the source of truth; it never mutates audit |
| C8 | **Multi-replica HA** — audit is single-writer *per process*; governance counters are in-memory *per instance* | ADR-013 | analytics index, query results, and displayed limits must be **HA-correct or honestly labeled** (§4.1, §6.4/§6.7) — a per-process index must not silently serve a fractional slice |
| C9 | **Content-free audit is a stated advantage** — records carry metadata/usage/cost, never bodies | ADR-003 | prompt/response **bodies must NOT enter the tamper-evident chain**; if logged at all, they live in a separate, deletable, TTL-bounded store (§4.2) |

---

## 4. Data architecture — how we get LiteLLM richness without breaking C1/C4/C7

```
┌────────────────────────────────────────────────────────────────┐
│ Audit log  (AUTHORITATIVE · tamper-evident hash chain · WAL)     │  internal/audit/
│   one record per request: ts, team, key_id, model, provider,     │  (unchanged; source of truth)
│   status, tokens(in/out/cache), cost_µUSD, latency_ms, trace_id, │
│   fallback_used, cache_hit, body_ref (omitempty) — NEVER bytes   │
└───────────────────────────────┬──────────────────────────────────┘
                                 │  derive (forward-only tail; rebuildable from chain)
                                 ▼
┌────────────────────────────────────────────────────────────────┐
│ Analytics index (DERIVED · default-on w/ audit · disable-able)   │  internal/analytics/ (NEW)
│   rollup tables for fast time-series + a queryable request index │  like providerstore: absent ⇒ degrade
└───────────────────────────────┬──────────────────────────────────┘
                                 │  query (read-only, token-gated, bounded windows + pagination)
                                 ▼
┌────────────────────────────────────────────────────────────────┐
│ Admin query API   GET /admin/analytics/*   GET /admin/logs/*     │  internal/server/analyticsapi/ (NEW)
│   authz: full-admin = all; team-mapped (ADR-004/010) = own team  │
└───────────────────────────────┬──────────────────────────────────┘
                                 │  on-demand fetch (no persistence)
                                 ▼
┌────────────────────────────────────────────────────────────────┐
│ Browser console  (DATA-FREE · token in memory · charts client-   │  internal/server/adminui/
│   side · no localStorage/sessionStorage)                         │
└────────────────────────────────────────────────────────────────┘
```

### Key design rules
- **Audit is the source of truth; the analytics index is a derived, rebuildable
  read-model** (C7). The index is populated by tailing newly-written audit records
  (the writer is single-writer already) and can be fully rebuilt by replaying the
  audit files. Corruption or loss of the index never affects audit integrity or the
  data plane.
- **The index is configurable, defaulting ON when audit is enabled** (§15 Q2 — chosen so
  a fresh deployment is not stuck in the "invisible cost spike / logging off by default"
  pain), with a one-line flag to disable for minimal single-binary deployments. When
  disabled or absent, the console degrades gracefully to metrics-only (Usage/Logs show a
  capability-driven affordance, §9.1 — not an error).
- **`/metrics` stays cardinality-bounded** (C4). Overview KPIs that *can* be bounded
  (totals, per-model status, p95) keep coming from metrics; anything per-key /
  per-request / time-series comes from the query API.
- **Authorization (review-corrected).** **Until team/user records exist (dep D3),
  `/admin/analytics/*` and `/admin/logs/*` are full-admin only.** Once D3 lands, a
  full admin sees all teams and a team-mapped identity sees only its own team. The
  identity→team resolution reuses the existing `adminauth.Resolve` / `/admin/whoami`
  source (ADR-004/010); **multi-team membership = the union of the caller's teams**;
  resolution failure is **fail-closed** (deny, not "all"). The query API enforces
  scoping server-side and a client-supplied team filter can only **narrow**, never
  widen, the server-enforced set.
- **Identity minimization (review-corrected).** The Users view and all API responses
  display **opaque subject IDs only** (or an explicitly configured non-sensitive
  alias) — **never** email or raw IdP groups (which CLAUDE.md/audit rules bar from
  request context and audit). `key_id` is shown to full-admin; for team-scoped viewers
  it is **aliased/truncated** so log access cannot enumerate other teams' key existence.
- **Prompt/response bodies are never in `/metrics`, never in the audit chain, and never
  in the analytics index** (C9, §4.2). They live only in the separate deletable body
  store when `log_bodies` is enabled, and the Logs drawer fetches them through a
  separate, full-admin-gated, access-audited path.

### 4.1 Multi-replica (HA) correctness — the index model (CRITICAL, review-driven)

Under ADR-013 HA each replica writes its **own** audit chain segment; there is no shared
counter state and no shared log by default. A naive per-process SQLite index would let a
load-balanced `/admin/analytics/*` query hit replica A and see only A's fraction of
traffic. The spec defines **two explicit, named deployment modes**; ADR-015 picks the
default and the exact mechanism, but the contract is fixed here:

- **Mode A — single-replica / local index (default for non-HA).** Local SQLite, derived
  from this process's audit tail. **Documented as single-replica-only.** In an HA
  deployment *without* Mode B configured, the analytics/logs views **degrade to
  metrics-only** (which is already cluster-global via Prometheus federation) and display
  a clear "cluster analytics require a shared analytics store" affordance — they do
  **not** silently serve a partial slice.
- **Mode B — shared analytics store (required for cluster-wide HA analytics).** The index
  lives in a **shared, Postgres-portable store** (the `providerstore` DDL is already
  TEXT-only / Postgres-portable — reuse that discipline). Ingestion is **single-writer**:
  exactly one aggregator (a leader-elected replica, or a dedicated ingest worker) tails
  the **aggregated** audit (the HA audit-collection contract that ships per-replica
  segments to a shared location, ADR-012/013) and writes the index. All replicas **read**
  the shared index; none writes it but the aggregator. This avoids query-time scatter-
  gather (rejected: fragile, slow, and racy across replica clocks).

The query API reports which mode is active via the capabilities endpoint (§4.4) so the UI
renders the correct affordance.

### 4.2 Prompt/response bodies live OUTSIDE the audit chain (CRITICAL, review-driven)

Bodies are **never** written into the tamper-evident hash chain (C9, ADR-003). The chain
stays content-free; an audit record may carry an **opaque body reference** (`body_ref`)
only. When `audit.log_bodies` is enabled, bodies go to a **separate body store**:

- **Mutable + deletable + TTL-bounded** — supports retention caps (time + max bytes) and
  **hard delete of a single record** (GDPR / CCPA right-to-erasure). The tamper-evident
  chain is unaffected by a body deletion (the chain only held the `body_ref`).
- **Encrypted at rest**, full-admin-gated read path, **access-audited** (§6.3).
- **PII-masked on write** by the existing filter (ADR-009) — but see the residual-risk
  treatment in §6.3 (masking is best-effort, not a guarantee).
- Separate from the S3 Object-Lock anchor (which anchors the *content-free* chain only).

This keeps inferplane's "content-free audit, governance without content retention"
advantage (ADR-003) intact while still offering opt-in body inspection for teams that
accept the trade-off.

### 4.3 Rebuild & ingestion correctness (MAJOR, review-driven)

The index is derived; correctness of derivation is specified, not assumed:

- **Idempotent event key**: the request ULID. Re-ingesting the same record is a no-op.
- **Two-phase precedence**: a `request_completed` event supersedes/closes its
  `request_started`; a start with no completion (crash, in-flight) is materialized as
  status `incomplete`, never dropped silently.
- **Checkpointing**: ingestion records the last consumed `(segment, offset, chain_hash)`;
  restart resumes from the checkpoint. Rebuild enumerates **all** segments (local files
  **and** configured S3/shared prefixes) — the operator supplies the segment source list;
  rebuild is per-segment then merged by event key.
- **Schema versioning**: the index carries a schema version; an aggregation-code change
  bumps it and triggers (or prompts) a rebuild. Mixed-version fixtures are tested (§13).

### 4.4 Capability negotiation + drift detection (MAJOR, review-driven)

- **`GET /admin/capabilities`** (new, token-gated) returns the live feature/runtime map the
  console fetches **on bootstrap**: `{ analytics_index: A|B|off, logs_bodies: bool,
  teams_records: bool, key_governance_fields: bool, providerstore: bool, ha_mode: bool,
  guardrails: {...}, region_policy: bool, index_health: {...} }`. The UI renders
  degradation affordances from this — it does **not** discover missing features by
  probing endpoints and catching 404/5xx (which would cause race conditions / broken
  first paint). Falls back safely if the endpoint itself is absent (treat all optional
  features as off).
- **Index health / drift**: capabilities + a `GET /admin/analytics/health` expose
  `last_ingested_offset`, `chain_verification_status`, and `lag` (records/seconds behind
  the audit head). When the index is stale beyond a threshold, corrupt, or unverifiable,
  the UI shows a "data may be stale / rebuilding" banner instead of silently serving old
  aggregates.

### 4.5 Why a derived SQLite index (not "query the JSONL directly", not "a separate spend store")
- Querying append-only JSONL for time-series/drill-down is O(n) per request and
  unindexable — fine for `verify`, wrong for an interactive dashboard.
- A *separate* spend store (LiteLLM's model) would duplicate the source of truth and
  risk drift from the audit chain. Deriving from audit keeps **one** authoritative
  record and makes the index disposable. This is the data-modeling decision that lets
  us match LiteLLM analytics while *strengthening* (not copying) its data posture.

### 4.6 Mode B ingestion-safety contract (CRITICAL, round-2 review — A1 surface)

Decision A1 puts the shared-store aggregator in the first analytics milestone, so its
concurrency safety is specified here (ADR-015 implements it). The contract:

- **Hard isolation invariant (overrides everything below):** the shared analytics store
  is **derived and disposable**. Its outage, saturation, lag, or corruption **must NEVER
  affect the data plane** — routing, governance PreCheck/Settle, and **audit writes
  continue unaffected**. The aggregator reads audit; it is never in the request path.
  This preserves the single-binary / no-external-SaaS posture: a deployment that doesn't
  run Mode B loses *cluster analytics*, never *gateway function*.
- **Fenced single writer (anti split-brain):** exactly one aggregator writes the store,
  guarded by a **lease with a fencing epoch** — concretely a **DB advisory lock / lease
  row with a monotonically increasing epoch** (Postgres advisory lock or a `lease(holder,
  epoch, expires_at)` row; K8s Lease is an acceptable alternative). Every index write
  carries the holder's epoch and is rejected if a newer epoch exists. Two replicas that
  both believe they are leader cannot both commit — the stale epoch is fenced out.
- **Atomic ingest unit:** **`event upsert (PK = request ULID) + rollup mutation +
  checkpoint advance` happen in ONE DB transaction.** Rollups are derived only from the
  deduped immutable event table (never incremented blindly), so a re-applied batch is a
  no-op. This closes mid-batch-crash double-count / gap (round-2 codex + kimi).
- **Ordering by ULID, not wall clock:** time-series buckets key off the **ULID timestamp
  component**, and cross-replica clock skew is tolerated up to a documented bound; late-
  arriving segments are merged by `(segment, offset, ULID)`, not by ingest order, so a
  slow-clock replica's earlier event lands in the correct bucket.
- **Bounded lag + backpressure:** the aggregator exposes a lag metric (`/admin/analytics/
  health`, §4.4); when lag exceeds a threshold the console shows "analytics catching up",
  **but replicas are never blocked** (backpressure shapes the *index*, never the data
  plane). Replica-side audit WAL growth is bounded by the existing segment rotation.
- **Mode A→B transition (1→N):** when a shared store is configured, the local SQLite
  index is **drained then retired** (or simply ignored — it is rebuildable); the
  aggregator owns the shared store from epoch 1. Queries read exactly one store at a time
  (capabilities reports which), so no double-counting across the two during transition.
- **Recovery from corruption/staleness:** `/admin/analytics/health` reports it; recovery
  is **operator-triggered rebuild by default** (auto-rebuild only behind a flag, and
  rate-limited so it can't overwhelm the audit tail). The data plane is unaffected
  throughout.
- **Prerequisite flagged (round-2 kimi):** Mode B tails an **aggregated audit** across
  per-replica segments. ADR-012/013 today define per-process chains + S3 anchoring but do
  **not** define a turnkey cross-replica *collection* contract. ADR-015 must **either**
  specify that collection contract (segment discovery, finalization markers, shared
  prefix) **or** declare Mode B's input to be an operator-provided aggregated audit
  source. This is a known design task, not an assumed-existing mechanism.

### 4.7 Body-store contract (MAJOR, round-2 review — A2 surface)

Expanding §4.2 with the operational details round-2 flagged:

- **Encryption keys are refs only (§7):** the body store's at-rest key is referenced via
  `env:` / `file:` / KMS-style ref — **never inline** (same mandate as provider secrets).
  Per-record **AEAD** (nonce + auth tag); **fail-closed** reads (unreadable body → "body
  unavailable", never plaintext fallback). Key **rotation / re-encryption** policy is
  defined in ADR-017 (envelope encryption so rotation rewraps data keys, not every body).
- **`body_ref` is opaque & non-derivable:** an opaque token (no path, no team, no
  customer identifier encoded). Bodies are a side-channel; the ref leaks nothing.
- **Dangling ref after TTL purge / erasure is EXPECTED and acceptable** (the chain is a
  governance record, not a content archive — you cannot and must not mutate the WORM
  chain to null a ref). A body-fetch for a purged/erased ref returns a **tombstone
  response** (`410 Gone`-style "body purged or erased", with the reason) — **never a
  500**, and the Logs drawer renders it as "body no longer retained". An optional
  content-free **`body_deleted`** audit event records erasure accountability.
- **Copy-only capture — does NOT touch `RawBody` / the cache invariant (§4.4):** body
  capture is a **side-channel copy taken after auth + governance**, masked into the body
  store. It **must not mutate the ingress `RawBody`** (which is forwarded verbatim when
  protocol matches, §4.4). A **byte-for-byte passthrough regression test with
  `log_bodies` enabled** is required (§13).
- **Audit event taxonomy (anti-recursion, anti-confusion):** `request_*` events feed the
  request Logs view; **`body_accessed` / `body_deleted` are a separate access-audit event
  type** — metadata-only **by schema** (they can never carry a `body_ref`, enforced by
  code path not just config, so a body view can never itself be body-logged → no
  recursion), surfaced only in a full-admin access-audit view, **never** rendered as a
  request row and **never** body-fetchable. `body_accessed` cardinality is bounded
  (deduped per viewer+record within a short window) so heavy viewing can't flood the chain.

---

## 5. Information architecture — 8 sections (LiteLLM depth)

Nav restructures from 5 to 8 sections. Each degrades gracefully when its backend
dependency (§8) is absent.

| # | Section | Purpose | Primary data source |
|---|---------|---------|--------------------|
| 1 | **Overview** | at-a-glance health + spend | `/metrics` + analytics rollup |
| 2 | **Usage** | spend & token analytics, drill-down | analytics query API |
| 3 | **Logs** | request log viewer + detail drawer | logs query API (+ audit bodies if enabled) |
| 4 | **Virtual keys** | full key lifecycle | `/admin/keys` (extended) |
| 5 | **Teams & Users** | team/user records, budgets, membership | keystore (extended) + analytics |
| 6 | **Providers & Models** | registration + routing + health/latency | config/providerstore + `HealthChecker` + metrics |
| 7 | **Governance** | quota, budgets, alerts, guardrails, region, audit | metrics + config + new policy APIs |
| 8 | **Settings** | gateway config view (+ writable where `providerstore`) | config API |

(Optional, deferred: **Test** playground — §12.)

---

## 6. Per-view design

### 6.1 Overview
- KPI cards: **spend (today / MTD)**, **requests (total + error %)**, **active keys**,
  **p95 latency**, **cache-hit rate** (the last directly markets the §4.4 advantage).
- **Spend sparkline** (last 30d), **top teams** and **top models** bars, **provider
  health strip** (●ok/●fail per provider).
- Sources: bounded KPIs from `/metrics`; sparkline + top-N from the analytics rollup.
- Degradation: without the index, cards fall back to metrics-only (today's behavior)
  and the sparkline/top-N show the "enable analytics" affordance.
- Needs covered: #2 (cost visibility), #10 (observability), surfaces #11 cache value.

### 6.2 Usage (analytics)
- **Time-series charts**: spend, tokens (in/out/cache separated), request count;
  granularity hour/day; bounded window (e.g. ≤ 90d) with pagination.
- **Drill-down filters**: team, key, model, provider, and **user/project when that
  dimension exists** (depends on D2/D3 — see §8).
- **Breakdown tables**: spend by team / model / key / user, sortable. **CSV export
  (review-corrected):** export is a **server-side, token-gated, authz-scoped endpoint**
  (`GET /admin/analytics/export.csv`) that streams a download — **not** a client-side
  blob assembled from page memory. This keeps the browser data-free (C1): the browser
  never accumulates/persists a dataset; it triggers a scoped server download honoring the
  caller's team scope.
- **Cache panel (review-corrected):** show **only metrics the system actually records** —
  cache-hit rate and cache-read vs input token counts from audit/usage. **Do not display
  a fabricated "estimated savings"** number. The honest message is "the §4.4 verbatim
  cache invariant preserves prompt-cache hits" (a *correctness* win vs LiteLLM's
  caching-cost bug), distinct from a semantic-cache *savings* engine (#11, not built).
- Sources: analytics query API only.
- Needs covered: #2 (team/user/project attribution — user/project gated on D2/D3),
  #11 (cache visibility).

### 6.3 Logs (request viewer)
- **Table**: ts, team, key_id, model, provider, status, tokens in/out, cost µUSD,
  latency, cache-hit, fallback-used, trace_id. Filters + search; server-side
  pagination (bounded page size).
- **Row → detail drawer**: full request metadata + governance decision (precheck/settle,
  on_exceeded) + the resolved route/fallback path + trace link.
- **Bodies (prompt/response)** — shown **only** when (a) config flag `audit.log_bodies`
  is enabled, **and** (b) the viewer is full-admin. Bodies come from the **separate
  deletable body store (§4.2), never the audit chain or index** (C9).
  - **Residual-risk honesty (review-corrected):** PII masking (ADR-009) is **best-effort,
    ingress-only, regex-based** — it does **not** guarantee removal of secrets, PHI,
    credentials, proprietary source code, or customer identifiers a request may contain.
    The spec states masking is mitigation, not a guarantee; ADR-017 requires
    adversarial-leak tests and an explicit retention/size cap before any body is stored.
  - **Body-access audit (review-corrected):** a successful body view emits a dedicated
    **`body_accessed`** record — fields: opaque viewer `sub`, the viewed request ULID,
    timestamp — written into the content-free chain (it carries no body). Per the
    project's audit rule, an **authenticated 403** (denied view) is audited; an
    unauthenticated 401 is **not**. The `body_accessed` log is full-admin-only and is
    **not** surfaced in any team-scoped Logs view (no recursive leak).
  - A prominent banner states retention + privacy + that this cedes the content-free-audit
    default. Default **OFF**.
- Sources: logs query API for metadata; a separate full-admin body-fetch path for bodies.
- Needs covered: #3 (prompt logging — opt-in, compliance-honest), #14 (audit trail —
  drawer links to `VERIFY CHAIN`).

### 6.4 Virtual keys
- **List**: key_id, team, allowed models, budget, TPM/RPM, expiry, last-used, status,
  spend-to-date.
- **Create form** (extends today's team + allowed-models): **budget (USD), TPM, RPM,
  expiry, metadata (k/v), optional owner/user**. One-time plaintext reveal preserved
  (shown once, never recoverable, §security).
- **Actions**: revoke, **rotate**, edit limits.
- Sources: `/admin/keys` extended (depends on D2 — keystore gains per-key governance
  fields; until then the form shows only fields the store supports and labels the rest
  "requires key-governance fields").
- **HA honesty (review-corrected):** governance counters are in-memory **per instance**
  (ADR-013). A TPM/RPM/daily limit of *N* is enforced *per replica*, so an *R*-replica
  cluster admits up to *R×N*. The UI must label these limits **"per instance"** with the
  cluster multiplier shown, **or** the feature is gated on a shared-counter backend
  (Redis/DB) dependency. The console must **not** present a per-instance limit as a global
  guarantee (that would falsely satisfy need #5). Same caveat applies to displayed
  utilization in §6.7.
- Needs covered: #4 (RBAC), #5 (per-key rate limit/quota — with the HA caveat above).

### 6.5 Teams & Users
- **Teams**: list; create/edit with **budget, default per-key limits, allowed models**;
  membership; team spend (from analytics).
- **Users**: list of identities seen (OIDC principals / keystore owners); per-user
  spend; team membership; role (admin / team / viewer per ADR-004).
- Sources: keystore extended to treat **teams and users as first-class records**
  (depends on D3 — today they're implicit). Until then this view is read-only,
  derived from observed traffic + issued keys, with create/edit disabled + a
  "requires team/user records" note.
- Needs covered: #2 (attribution), #4 (RBAC), #8 (SSO identity surface).

### 6.6 Providers & Models
- Keep ADR-014: provider table, dynamic register form, **TEST CONNECTION** probe,
  catalog/typeahead, route editor, git export.
- **Add a Health & latency panel**: per-provider live health via the **optional**
  `HealthChecker` (C6). The UI **must render "probe unsupported" for providers that do
  not implement it** (like `TokenCounter`) — no provider-specific core switch, no
  mandatory health method (zero-core-diff, §8). **p50/p95 latency + error rate** (from
  metrics — config-bounded labels only, C4), **circuit-breaker state** (open/half/closed)
  from `router/breaker.go`, last-probe time. On-demand by default (ADR-014 D5); periodic
  background probing remains a flagged follow-up (needs scheduler + bounded status store).
- **Route visualization**: primary → fallback chain per model with live per-target status.
- Needs covered: #1 (unified API/providers), #6 (failover visibility), #18 (self-hosted
  shown identically via `openai_compatible`).

### 6.7 Governance
- **Quota utilization** (per team/window) — keep.
- **Budgets**: per-team / per-key budget config + a **true utilization gauge**
  (requires exposing the configured limit alongside the cumulative counter — D5a). The
  same per-instance HA caveat as §6.4 applies to the gauge until shared counting exists.
- **Budget alerts (#17)**: configure thresholds (e.g. 80% / 100%) and destinations
  (webhook / SNS / Slack); show alert status + recent fires. Depends on D5b (an alert
  emitter — none exists today; only a Prometheus counter + block/warn enforcement).
- **Guardrails (#7)**: view/configure the existing **PII-masking filter** (ADR-009)
  per team; surface a **Bedrock Guardrails** binding when present (depends on D6 — not
  yet implemented).
  - **Data-plane gap flagged (review-corrected):** the customer pain is *Bedrock
    Guardrails **bypass*** when routed through a gateway. The real fix is a **data-plane**
    guarantee — that inferplane *preserves/applies* the configured Bedrock Guardrail when
    invoking a `bedrock` target — **not** a console toggle. This spec flags that as the
    substance of D6 (ADR-019 must specify the data-plane mechanism); the console only
    *surfaces* policy + status. We do not claim #7 is solved by UI alone.
- **Region policy (#9)**: NCT-critical.
  - **Partial surface available now (review-corrected):** today's `bedrock` per-provider
    `region` and ADR-014's `probe.allowed_hosts` allowlist are existing controls the
    console can **display read-only immediately** (visibility), so #9 is a *partial console
    surface now* + **enforcement** as backend dep D7 (an enforced "requests for team T may
    only egress to region R" policy — distinct from per-provider config).
- **Audit integrity**: `VERIFY CHAIN` — keep.
- Needs covered: #5, #7, #9, #14, #17.

### 6.8 Settings
- **Routing / fallback** config view (read-only under file-config; writable with
  `providerstore`, per ADR-008).
- **Caching** settings (today: verbatim passthrough; semantic cache when added — #11).
- **Compliance**: `audit.log_bodies` toggle + retention (drives §6.3 bodies).
- **Auth**: OIDC issuer/client display (read-only), admin-token presence (never value).
- **Secret refs**: env/file ref **names** only (C3) — never values.
- Needs covered: config UX, #3 (body-logging control).

---

## 7. `Customer_needs.md` coverage matrix (all 20 + cross-cutting pains)

Every listed item is mapped to exactly one disposition. "Console surface" = visible in
this redesign; "Backend dep" = needs backend work first (named in §8); "Out of scope" =
not a console concern (tracked elsewhere).

| Need | Item | Disposition | Where |
|------|------|-------------|-------|
| #1 | Unified API (Anthropic + OpenAI ingress) | **Done** (surfaced) | §6.6, Quickstart |
| #2 | Cost attribution (team/user/project) | **Console surface** (team now; user/project = Backend dep D2/D3) | §6.2, §6.5 |
| #3 | Prompt logging | **Console surface, opt-in** (Backend dep D4 for bodies) | §6.3, §6.8 |
| #4 | RBAC / access control | **Console surface** (D2/D3 for per-key/team limits) | §6.4, §6.5 |
| #5 | Rate limiting / quota | **Console surface** (D2 per-key) | §6.4, §6.7 |
| #6 | Automatic failover | **Console surface** (visibility) | §6.6 |
| #7 | Guardrails (PII + Bedrock) | PII surface done; **Bedrock fix is data-plane (preserve guardrail on routing), not UI** = dep D6 | §6.7 |
| #8 | Auth / SSO (OIDC/JWT; SAML) | **Console surface** (OIDC done; SAML out of scope) | §6.5, §6.8 |
| #9 | Region locking | **Partial surface now** (region/allowlist visibility) + **enforcement** = dep D7 | §6.7 |
| #10 | Observability | **Console surface** | §6.1, §6.6 |
| #11 | Semantic caching | cache-hit **visibility** of recorded metrics (no fabricated savings); engine = dep D8 | §6.1, §6.2, §6.8 |
| #12 | Model routing | **Done** (surfaced) | §6.6 |
| #13 | Load balancing (weighted) | **Out of scope** (router-engine change, ADR-014 defers; ADR-013 HA) | §11 |
| #14 | Audit trail (tamper-evident) | **Done** (surfaced) | §6.3, §6.7 |
| #15 | Prompt/response format conversion | **Done** (verified: `internal/openai/convert.go`, `pkg/schema/`) | §2 (existing) |
| #16 | A/B testing | **Out of scope (deferred)** | §12 |
| #17 | Budget alert | **Console surface** (emitter = Backend dep D5b) | §6.7 |
| #18 | Self-hosted models | **Done** (surfaced via `openai_compatible`) | §6.6 |
| #19 | MCP / tool use | **Out of scope** (pass-through exists; no MCP server/client) | §11 |
| #20 | Multi-cloud | **Partial/Out of scope** (Anthropic+Bedrock+OpenAI-compat; no Azure/Vertex provider) | §11 |
| Pain | LiteLLM caching bug → cost spike | cache **invariant preserved** (§4.4 correctness win) + **surfaced** (recorded cache metrics only — not a savings claim) | §6.1, §6.2 |
| Pain | Cost spike invisible | **Console surface** (Usage analytics + alerts) | §6.2, §6.7 |
| Pain | Logging off by default | **Console surface** (explicit toggle + status) | §6.8 |
| Pain | Guardrail / RBAC bypass | **Mitigated** (data-free, secret-ref) + surfaced | §6.7 |

**Result: 0 unmapped items.** This is the "문서내용 꼭 해결" guarantee the panel review must verify.

---

## 8. Backend dependencies (named, since the console can't be honest without them)

The focus is UX, but the UI cannot show data that the backend does not produce. Each
dependency is its own ADR + plan; the console is built to **degrade gracefully** until
each lands (§9).

| Dep | What | Enables | New ADR |
|-----|------|---------|---------|
| **D1** | Analytics index (`internal/analytics/`, default-on when audit enabled / disable-able, derived from audit; Mode A local SQLite / Mode B shared Postgres-portable §4.1) + query API (`internal/server/analyticsapi/`: `GET /admin/analytics/*`, `GET /admin/logs/*`, `GET /admin/analytics/export.csv`, `GET /admin/analytics/health`) + **`GET /admin/capabilities`** (§4.4) | Usage, Logs (metadata), rich Overview, reliable degradation | ADR-015 |
| **D2** | Per-key governance fields in keystore + `/admin/keys` (budget, TPM, RPM, expiry, metadata, owner) | full Keys view (#4,#5) | ADR-016 |
| **D3** | Teams & Users as first-class keystore records (budgets, membership, roles) | Teams & Users (#2,#4) | ADR-016 (or split) |
| **D4** | **Separate deletable body store** (§4.2 — mutable, TTL/size-capped, encrypted, NOT the audit chain) + `audit.log_bodies` opt-in + best-effort PII mask on write + full-admin access-audited body-fetch path + `body_accessed` record | Logs bodies (#3) | ADR-017 |
| **D5a** | Expose configured budget limits to the API (utilization gauge, not just counter) | Governance budget gauge | ADR-015 (with D1) |
| **D5b** | Budget-alert emitter (threshold eval → webhook/SNS/Slack) | Budget alerts (#17) | ADR-018 |
| **D6** | **Data-plane** Bedrock Guardrails *preservation/application* on `bedrock` routing (the real anti-bypass fix) + per-team guardrail policy + console surface | Guardrails (#7) | ADR-019 |
| **D7** | Region-locking **enforcement** policy + status (distinct from existing per-provider region/allowlist *visibility*, surfaceable now) | Region policy (#9) | ADR-020 |
| **D8** | (optional) Semantic cache engine | cache panel "savings" beyond passthrough (#11) | future |

> Scope note for review: this spec **defines the console UX and the contracts it needs**
> (API shapes, authz, degradation). The host decides per-phase whether to (a) ship the
> UI shell first against stubs, or (b) gate each view behind its dependency. The
> recommended sequencing is §9.

---

## 9. Phasing — every phase ships a working console (graceful degradation)

Mirrors the `providerstore` opt-in pattern: the console always runs; features light up
as their dependency lands.

- **Phase 0 — IA + skin + capability gating (UI-only + one tiny endpoint).** Restructure
  nav to 8 sections; apply the conventional dashboard skin within the toolchain-free
  envelope (C2); split `app.js` into per-view modules + shared `api.js`/`charts.js`/
  `ui.js`. Add **`GET /admin/capabilities`** (§4.4) so the UI knows what's on *before*
  first paint. Overview / Providers / Governance work on **today's** metrics + APIs.
  Usage / Logs / Teams render their affordance from capabilities. **Minimal backend
  change** → low risk, immediate UX win. (Capabilities is the one piece Phase 0 cannot
  skip — without it degradation is guesswork.)
- **Phase 1 — analytics foundation (D1, D5a).** Usage + Logs (metadata) + rich Overview
  + budget gauges. **Per decision A1, this milestone includes Mode B** (shared
  Postgres-portable analytics store + single-writer aggregator, §4.1) so cluster-wide HA
  analytics are correct from the start; Mode A local SQLite remains the single-replica
  default.
- **Phase 2 — key/team governance (D2, D3).** Full Virtual keys + Teams & Users.
- **Phase 3 — compliance & alerts (D4, D5b).** Body logging (opt-in) + budget alerts.
- **Phase 4 — security parity (D6, D7).** Guardrails config + region policy.
- **Phase 5 — differentiators (D8, Test playground).** Optional.

Each phase is independently shippable and independently reviewable.

### 9.1 Degradation contract (review-corrected — was an assertion, now a contract)
Degradation is a defined per-view contract, not a hope:
- **Discovery is capability-driven, not error-driven.** The UI reads `/admin/capabilities`
  on bootstrap and renders affordances from it. It does **not** infer "feature off" from a
  404/405/5xx (which caused the broken-first-paint / race the reviewers flagged).
- **`api.js` status mapping:** `404`/`405`/`501` on an optional endpoint → treat as
  *disabled*, show the affordance. `5xx`/timeout on an *enabled* endpoint → show an
  inline "temporarily unavailable / retry" state and, where a cheaper source exists
  (e.g. Overview KPIs), **fall back to metrics-only** rather than blanking the view.
- **Stale/corrupt index** (from `/admin/analytics/health`): show a "data may be stale /
  rebuilding" banner; never silently serve old aggregates as current.
- **Affordance copy** is explicit and non-alarming: "Enable the analytics store to see
  usage history" — not "not implemented". Nav shows all 8 sections **disabled-with-reason**
  rather than hiding them (discoverability > pretending the feature doesn't exist), unless
  the user research in implementation says otherwise.
- **Tests (§13)** assert each missing-dependency path renders an affordance, not a 5xx or
  blank paint.

---

## 10. Frontend approach (within the toolchain-free envelope, C2)

- **Conventional aesthetic, vanilla implementation.** The "general dashboard look"
  (cards, color-coded status, charts, light/dark) is achieved with **plain CSS + vanilla
  JS**. No React/Vue/Svelte, no Vite/webpack, no node build in the critical path
  (ADR-002, ADR-014 alt-3). If any tooling is ever introduced, its output must be
  committed and `go:embed`-ed so the single-binary build is unaffected.
- **Charts**: a tiny vendored chart lib (candidate: **uPlot**, ~40 KB, dependency-free)
  **or** hand-rolled SVG for sparklines/bars — embedded via `go:embed`. Decision
  criterion: smallest footprint that renders time-series + bars accessibly; vendored
  file is committed, not fetched at runtime (no external SaaS/CDN).
  - **Vendored-dependency policy (review-corrected):** the lib is **pinned** (committed
    file + recorded version/SHA + license check, must be permissive), makes **no network
    calls**, uses **no storage APIs** (`localStorage`/`sessionStorage`/IndexedDB), and
    `adminui_test` scans the vendored asset for those patterns (C1 guard) and for the
    pinned hash. Updates are a deliberate, reviewed bump — not transitive drift.
- **Module split** (addresses the 763-line `app.js`): `app.js` (shell/router) +
  per-view modules (`overview.js`, `usage.js`, `logs.js`, `keys.js`, `teams.js`,
  `providers.js`, `governance.js`, `settings.js`) + shared `api.js` (token-gated fetch,
  401 handling), `charts.js`, `ui.js` (DOM helpers). ES modules, no bundler.
- **Data-free preserved (C1)**: admin token in a JS variable only; **no** `localStorage`
  / `sessionStorage`; per-page-session in-memory cache only (same rule ADR-014 D5
  established); charts render from on-demand JSON; a full reload re-fetches everything.
  The in-memory cache is **bounded by an LRU cap** (e.g. ≤ 50 query results) so a long
  session with wide log/time-series queries cannot balloon browser memory (review-corrected).
- **i18n**: keep the existing EN/KO bilingual copy pattern (ADR-014).

---

## 11. What we deliberately do NOT adopt from LiteLLM (and why)

- **Paste raw provider keys into the UI / store them** — violates §7. We probe via
  server-resolved refs (ADR-014); strictly more secure.
- **Client-side data/secret persistence** — violates C1 (ADR-001). Status & filters are
  in-memory page-session only.
- **SPA framework rewrite** — violates C2 (ADR-002). Conventional look is done in vanilla.
- **Weighted load balancing UI (#13)** — a router-engine + shared-state change
  (ADR-014 defers it; ADR-013 HA territory), not a console change. Out of scope.
- **MCP server/client (#19)** — tool_use blocks already pass through; standing up MCP is
  a provider/agent concern, not console UX. Out of scope.
- **SAML (#8)** — OIDC is implemented; SAML is an auth-backend change. Out of scope.
- **Per-request prompt logging ON by default (#3)** — privacy/compliance hazard; bodies
  are opt-in, masked, full-admin-only, and access-audited (§6.3).

---

## 12. Deferred / nice-to-have

- **Test playground** — send a test request to a key from the UI (LiteLLM "Test Key").
  Useful but optional; must route through the data plane with a real virtual key (no
  secret in the browser).
- **A/B testing (#16)** — model comparison experiments.
- **Periodic background health probing** — needs a scheduler + bounded status store
  (ADR-014 follow-up).
- **Semantic cache (#11 engine)**, **multi-cloud providers (#20)**, **weighted LB (#13)**.

---

## 13. Non-functional & security requirements

- **Authz**: query API enforces full-admin (all) vs team-mapped (own team only) server-
  side (ADR-004/010); never trust a client team filter. Body-fetch path is full-admin only.
- **`/metrics` cardinality unchanged** (C4): no `key_id` / raw-input labels added.
- **Bodies**: PII-masked on store (ADR-009); access-audited; opt-in (`audit.log_bodies`).
- **Bounded queries**: every analytics/logs endpoint enforces a max window + page size;
  the index query is bounded and the index is rebuildable from audit.
- **Degradation, not errors**: missing index / missing `providerstore` / file-config
  produce informative affordances, never 5xx.
- **Performance**: Overview KPIs from metrics are cheap; index queries are indexed and
  paginated. Body fetch is lazy (drawer-only).
- **Data plane untouched**: no change to request path, schema, cache invariant, or
  `count_tokens` (C5).
- **(round-2) Shared analytics store is never in the request path** — its outage/lag/
  corruption never affects routing, governance, or audit writes (hard invariant, §4.6).
- **(round-2) Body-store encryption keys are refs only** (`env`/`file`/KMS), never inline
  (§7); per-record AEAD; fail-closed reads; envelope rotation (§4.7).

### Testing
- Extend `adminui_test` to assert the data-free invariant across all 8 views (no
  `localStorage`/`sessionStorage`; no secret in any served asset).
- Query-API authz tests (full-admin vs team-scoped; rejected client team filter).
- Body-access audit test (viewing a body emits an audit record).
- Index-rebuild test (drop index → rebuild from audit → identical aggregates).
- Degradation tests (index absent / providerstore absent / file-config) render
  affordances, not errors.
- Cardinality regression test (`/metrics` label set unchanged).
- **(round-2) Mixed-version audit fixtures**: old records without `body_ref` and new
  records with `body_ref,omitempty`, plus `body_accessed`/`body_deleted` event types, all
  `verify` identically (the exact-line-bytes / append-only audit invariant).
- **(round-2) `RawBody` passthrough byte-for-byte test with `log_bodies` enabled** — body
  capture must not mutate the verbatim-forwarded ingress body (§4.4/§4.7).
- **(round-2) Mode B concurrency tests**: two aggregators contending → fencing rejects the
  stale epoch (no double-write); re-applied batch is idempotent (rollups unchanged);
  mid-batch crash → restart converges to correct aggregates; data plane unaffected when
  the shared store is down/saturated.
- **(round-2) Body-store tombstone test**: fetching a purged/erased `body_ref` returns the
  tombstone (410-style), never a 500; `body_accessed`/`body_deleted` never carry a body
  and are never body-fetchable.
- **(round-2) Index-corruption recovery test**: `/admin/analytics/health` flags it; rebuild
  is operator-triggered (or rate-limited auto) and never overwhelms the audit tail.

---

## 14. New ADRs to create (per the auto-sync rules)

- **ADR-015** — Analytics read-model (derived index) + admin query API + **HA mode A/B
  (§4.1)** + **`/admin/capabilities` + `/admin/analytics/health` (§4.4)** + rebuild/ingestion
  correctness contract (§4.3) + server-side CSV export.
- **ADR-016** — Console IA redesign to 8 sections + conventional skin within the toolchain-free envelope; per-key + team/user governance model; team-scoped query authz + identity minimization (§4 rules).
- **ADR-017** — Opt-in request-body logging in a **separate deletable/TTL body store
  (§4.2), not the audit chain**; best-effort masking + residual-risk + retention/size caps;
  `body_accessed` audit record; full-admin-only.
- **ADR-018** — Budget-alert emitter (webhook/SNS/Slack).
- **ADR-019** — Bedrock Guardrails integration + per-team guardrail policy.
- **ADR-020** — Region-locking enforcement policy.

(Docs to update on implementation: `docs/architecture.md`, `internal/CLAUDE.md`,
`docs/reference/api.md` (new endpoints), `docs/reference/data.md` (index schema),
`docs/reference/security.md` (body-access authz), a Usage/Logs runbook.)

---

## 15. Open questions (resolved positions after review round 1)

1. **Index storage** — *resolved*: separate, disposable, rebuildable store (Mode A local
   SQLite file / Mode B shared Postgres-portable, §4.1), never coupled into the
   authoritative key store. ADR-015 picks the default.
2. **Default-on vs opt-in** — *resolved as a stated trade-off (glm-5 finding)*: the index
   **defaults ON when audit is enabled** (so a fresh deployment is **not** stuck in the
   "invisible cost spike" / "logging off by default" pain that the customers hit with
   LiteLLM), with a one-line flag to disable for minimal single-binary deployments. This
   reverses the original "opt-in" lean precisely because need #2/#3 and the documented
   pains argue for richness out of the box. (HA still requires Mode B for *cluster-wide*
   analytics — §4.1.)
3. **Body retention** — *resolved direction*: when `log_bodies` is on, the separate body
   store (§4.2) enforces a time-based purge **and** a max-bytes cap, both configurable;
   exact defaults set in ADR-017. Hard single-record delete supported for erasure requests.
4. **Phase 0 scope** — *resolved*: ship the full 8-section shell with capability-driven
   affordances (§9 Phase 0) — the one required backend piece is `/admin/capabilities`.
5. **Chart lib** — *resolved direction*: vendor uPlot if it passes the §10 supply-chain
   policy (pinned, no network, no storage APIs, permissive license, `adminui_test`-scanned);
   else hand-rolled SVG. Decided at implementation under those acceptance criteria.

### User decisions (resolved 2026-06-26)
- **A1 — DECIDED: Mode B is in scope for the first analytics milestone.** The shared,
  Postgres-portable analytics store + single-writer aggregator (§4.1 Mode B) ships with
  the first analytics milestone so **cluster-wide HA analytics are correct from day one**
  (not a fast-follow). Mode A (local SQLite) remains the zero-config default for
  single-replica / dev deployments; Mode B activates when a shared store is configured.
  Consequence: a shared store (Postgres, or the shared SQLite-over-network discipline the
  `providerstore` DDL already supports) becomes a **declared dependency of the analytics
  milestone**, and ADR-015 must specify the single-writer aggregator (leader election or
  dedicated ingest worker) and the audit-aggregation source it tails.
- **A2 — DECIDED: body logging is offered, opt-in, separate store.** The product ships
  prompt/response body logging per §4.2/§6.3 — **default OFF**, separate deletable/TTL
  store (never the chain), full-admin + access-audited. The content-free metadata audit
  (ADR-003) remains the default and the compliance selling point; body logging is the
  documented opt-in trade-off that satisfies need #3 for teams that accept it. ADR-017
  must ship the erasure/retention story so regulated (NCT) customers can stay body-OFF
  and still pass audit requirements on metadata alone.

## 16. Multi-AI consensus review log (round 1)

Panel: **codex (gpt-5.5)**, **agy (default)**, **kiro-cli (glm-5)** — kiro-cli (kimi-k2.5)
throttled (`MODEL_TEMPORARILY_UNAVAILABLE`). Host (Claude Code) chaired and synthesized.
Unanimous verdict: *not ready as originally drafted*; top blockers = multi-replica index
correctness + body-logging-in-immutable-chain.

| # | Severity (consensus) | Finding | Resolution in this revision |
|---|----------------------|---------|----------------------------|
| 1 | CRITICAL (3/3) | Per-process analytics index breaks under multi-replica HA | §3 C8, §4.1 Mode A/B; HA without shared store degrades to metrics-only, never partial |
| 2 | CRITICAL (3/3) | Bodies in tamper-evident WORM chain → no GDPR delete; cedes content-free advantage | §3 C9, §4.2 separate deletable/TTL body store; chain holds only `body_ref` |
| 3 | CRITICAL→MAJOR | Client-side CSV export persists data (C1) | §6.2 server-side scoped `export.csv` endpoint, no client blob |
| 4 | MAJOR (3/3) | Degradation was an assertion; risk of 404/5xx broken first paint | §4.4 `/admin/capabilities`; §9.1 degradation contract + `api.js` status map |
| 5 | MAJOR (2/3) | Team-scoped query authz undefined (teams implicit) | §4 rules: full-admin-only until D3; adminauth resolution, multi-team=union, fail-closed |
| 6 | MAJOR (2/3) | HA per-instance limits shown as global → false guarantee | §6.4/§6.7 per-instance labeling + cluster multiplier, or shared-counter dep |
| 7 | MAJOR | Identity minimization (no email/IdP groups; key_id leak) | §4 rules: opaque sub only; key_id aliased for team-scoped |
| 8 | MAJOR | PII mask asserted sufficient | §6.3 best-effort/residual-risk language + adversarial-leak tests in ADR-017 |
| 9 | MAJOR | Index rebuild/two-phase correctness undefined | §4.3 idempotent ULID key, completion precedence, checkpoints, schema version |
| 10 | MAJOR | Index drift/staleness undetected | §4.4 `/admin/analytics/health` + stale banner |
| 11 | MAJOR | Body-access audit recursion/schema undefined; 401 vs 403 | §6.3 `body_accessed` record (opaque sub, ULID); authed-403 audited, 401 not |
| 12 | MAJOR | Matrix overstates cache "savings" & #7/#9/#15 | §6.2 + §7: cache=recorded metrics only; #7 data-plane gap; #9 partial-now; #15 verified |
| 13 | MAJOR | Health panel could break zero-core-diff if required | §6.6 explicit "probe unsupported" handling; optional capability only |
| 14 | MINOR | Unbounded client cache | §10 LRU cap |
| 15 | MINOR | Vendored chart-lib supply chain | §10 pinned/no-network/no-storage/license + `adminui_test` scan |
| 16 | MINOR | opt-in vs default-on contradicts the pain it targets | §15 Q2 resolved: default-on when audit enabled, flag to disable |
| 17 | MINOR | "LiteLLM-parity" framing misleading (LB/wildcard excluded) | §1 parity-scope qualifier |

Round-2 re-review is recommended **after** the user picks A1/A2, since those change the
HA-store and body-logging surface the panel would re-examine.

## 17. Multi-AI consensus review log (round 2)

Panel: **codex (gpt-5.5)**, **agy (default)**, **kiro-cli (kimi-k2.5)** — kiro-cli (glm-5)
failed to load context this round. Run **after** A1/A2 were decided. Both codex and
kimi-k2.5 independently **verified all 17 round-1 findings as SOUNDLY RESOLVED** at the
spec-body level (not merely asserted). agy: "structural foundation (derived index,
separate body store) is sound." The remaining issues are concentrated on the **new Mode B
aggregator surface (from decision A1)** and **body-store operational details (A2)** — all
three name *Mode B ingestion safety* as the top fix.

| # | Severity (consensus) | Round-2 finding | Resolution in this revision |
|---|----------------------|-----------------|----------------------------|
| R2-1 | CRITICAL (codex) | §4 diagram still showed bodies in the audit chain — contradicts C9/§4.2 | diagram fixed: `body_ref (omitempty) — NEVER bytes` |
| R2-2 | CRITICAL (codex+kimi+agy) | Mode B single-writer asserted, not safe vs split-brain/double-write | §4.6 fenced writer (lease+epoch), DB-unique ULID, epoch-checked writes |
| R2-3 | CRITICAL (codex+kimi) | Ingest crash mid-batch → gaps/double-count | §4.6 atomic `event upsert + rollup + checkpoint` in one txn; rollups from deduped table |
| R2-4 | MAJOR (codex+kimi) | Cross-replica clock skew corrupts time buckets | §4.6 order by ULID timestamp, not wall clock; documented skew tolerance |
| R2-5 | CRITICAL (codex+kimi) | Shared store = new SPOF / data-plane coupling | §4.6 hard isolation invariant: store outage never affects routing/governance/audit |
| R2-6 | MAJOR (codex+kimi) | Mode B lag/backpressure unbounded | §4.6 lag metric + console "catching up"; replicas never blocked |
| R2-7 | MAJOR (kimi) | Mode A→B transition double-write (1→N) | §4.6 drain/retire local index; queries read one store; capabilities reports which |
| R2-8 | MAJOR (codex) | Body-store encryption underspecified for a secret-ref system | §4.7 keys as `env`/`file`/KMS refs only, AEAD, fail-closed, envelope rotation |
| R2-9 | MAJOR (codex+kimi) | `body_ref` dangling after TTL/erasure | §4.7 dangling is expected/acceptable; tombstone (410-style) not 500; optional `body_deleted` |
| R2-10 | MAJOR (codex+kimi) | body-access recursion / event typing | §4.7 `body_accessed`/`body_deleted` metadata-only by schema (code-path), access-audit view only, never body-fetchable, cardinality-bounded |
| R2-11 | MAJOR (codex) | Body capture could mutate `RawBody`/cache invariant | §4.7 copy-only after auth/governance; §13 byte-for-byte passthrough test |
| R2-12 | MAJOR (codex) | Missing mixed-version audit fixtures for new fields/events | §13 mixed-version fixtures for `body_ref,omitempty` + new event types |
| R2-13 | MAJOR (kimi) | No specified recovery from index corruption | §4.6 operator-triggered (or rate-limited auto) rebuild |
| R2-14 | MAJOR (kimi) | HA audit-aggregation contract assumed but undefined in ADR-012/013 | §4.6 prerequisite flagged: ADR-015 defines the collection contract or takes an operator-provided source |
| R2-15 | MINOR (codex) | "opt-in" vs default-on wording still inconsistent | §4 box + D1 now "default-on when audit enabled, disable-able" |
| R2-16 | MINOR (kimi) | Mode-B-failure affordance copy unspecified | §9.1 capability-driven affordance ("analytics catching up" / "cluster analytics need a shared store") |

**Chair assessment after round 2:** round-1 issues are closed; round-2 issues are all
resolved at spec level above. The residual work the panel wants (exact leader-election
primitive, the HA audit-collection contract) is correctly **deferred to ADR-015** with the
*contract* (fencing, atomicity, isolation invariant) now fixed in the spec — which is the
right altitude for a design spec feeding an implementation plan. No CRITICAL/MAJOR remains
unaddressed at the design-spec layer.
