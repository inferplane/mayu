# Data

### 1. Overview
Persistent and in-memory state: the SQLite virtual-key store, the disk-backed
tamper-evident audit log, and the in-memory two-phase governance stores (limiter,
budget). Legacy governance stores remain instance-local:
`providerstore` remains SQLite-only; `keystore` supports SQLite and Postgres.
The legacy `limiter`/`budget` packages are memory-only
(not persistent stores at all), and no Redis/Valkey dependency exists anywhere
in the tree. ADR-046 adds a shared gateway profile with PostgreSQL identity and atomic resource admission. The default SQLite/in-memory profile remains single-replica.
ADR-045 adds opt-in Postgres monetary authority and a private local SQLite
reservation journal for node-local fleets. Postgres also backs `bodystore`,
`policystore`, `telemetry`, and analytics Mode B.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Global monetary authority | `internal/authority/pgstore/`, `internal/authority/local/` | Postgres commits finite grants and owns UTC budget windows; private node journals reserve each billable attempt before dispatch. No inference-time database call; no refund on expiry/restart; uncertain outcomes retain their bound. See ADR-045. |
| Shared admission | `internal/authority/pgstore/shared*.go` | Atomic global RPM/TPM, calendar token quotas, team/key money and ADR-045 policy money; captured-window replay-safe settlement and global usage. |
| Shared key store | `internal/keystore/postgres*.go`, `import.go` | Full hashed-key/team backend, per-request snapshots, conditional revoke, immutable seed fingerprints and transactional SQLite import. |
| Key store | `internal/keystore/sqlite.go` | SHA-256-hashed virtual keys, Postgres-portable schema; also owns the `teams` table (D3, ADR-016): `name` (PK), `allowed_models`, `rpm`, `tpm`, `tokens_per_day`, `quota_on_exceeded`, `budget_usd_micros`, `budget_on_exceeded`, `budget_usd_micros_per_day` (daily budget Phase 1 — per-team calendar-day µUSD cap, 0 = unlimited), `guardrail_id`, `guardrail_version` (D6, ADR-019 — per-team Bedrock Guardrail override), `allowed_regions` (D7, ADR-020 — per-team region lock), `created_at`, `updated_at`. `guardrail_id`/`guardrail_version`/`allowed_regions`/`budget_usd_micros_per_day` are `teams`' ALTER-TABLE migrations (it shipped as a brand-new table under D3, so no pre-existing rows needed catching up until D6; `budget_usd_micros_per_day` is the table's first budget-related ALTER — the original money columns rode the CREATE TABLE); `ensureSchema` shares a small `existingColumns`/`applyMigrations` helper pair between `keys` and `teams` instead of duplicating the PRAGMA-scan loop. |
| Store interface | `internal/keystore/keystore.go` | `Store`, `Principal`, `Allows()` (RBAC); `TeamStore` (`UpsertTeam`/`GetTeam`/`ListTeams`/`DeleteTeam`, D3) is a separate interface so the existing `Store` fakes in `internal/server`'s tests are unaffected; `KeyEnsurer` (`EnsureKey`, ADR-023) is the same pattern — `EnsureKey` upserts a caller-supplied plaintext (`ON CONFLICT(key_hash) DO UPDATE`, deliberately excluding `revoked`/`created_at`) so a config-declared virtual key (`config.VirtualKeys`, bootstrapped in `newGateway` right after `keystore.OpenSQLite`) survives a wiped-and-recreated key store across restarts, unlike `Create`/`CreateWithOptions` which always generate a fresh random plaintext |
| Provider store | `internal/providerstore/sqlite.go` | opt-in DB topology (ADR-008): `providers` (refs only — no secret column; `guardrail_id`/`guardrail_version` TEXT columns, D6/ADR-019 — a DB-registered Bedrock provider's own default Guardrail, ALTER-TABLE migrations mirroring `auth_header`), `model_targets` (ordered routes), `model_aliases` (`model`, `alias` PK — ADR-021 follow-up: a model's alias→canonical names, group-level so a multi-target fallback chain can't duplicate them; a brand-new `CREATE TABLE IF NOT EXISTS`, no ALTER-TABLE needed), `meta` (durable `seeded` marker); portable TEXT/INTEGER DDL |
| Audit writer | `internal/audit/writer.go` | single-writer hash chain, WAL truncation |
| Audit WAL | `internal/audit/wal.go` | disk buffer for `buffer_then_block` durability |
| Audit verify | `internal/audit/verify.go` | per-instance segmented chain verification |
| Audit anchoring | `internal/audit/s3anchor/` | opt-in WORM (S3 Object Lock) chain-head anchoring → tamper-resistant (ADR-012); refs/PII-free anchor objects |
| Limiter store | `internal/limiter/limiter.go` | in-memory token bucket (TPM/RPM), two-phase |
| Budget store | `internal/budget/budget.go` | in-memory microUSD budget, two-phase; calendar-day and calendar-month windows are separate counters keyed by the window tag (`budget:day:team:acme` vs `budget:month:team:acme` — the tag-first key is what keeps them out of one bucket); both the day window's midnight and the month window's first-of-month anchor honor the `budget_timezone` config key (`Governor.SetBudgetTimezone`, one anchor per deployment) — default UTC when unset |
| Body store | `internal/bodystore/` | opt-in captured-body store (D4, ADR-018), OUTSIDE the audit chain: `bodies` table (`ref` PK, `record_id`, `team`, `created_ts`, `expires_ts`, `size`, `wrapped_key_nonce`/`wrapped_key_ct`, `req_nonce`/`req_ct`, `resp_nonce`/`resp_ct` — BLOB/BYTEA ciphertext; `resp_*` nullable = streaming request-only). Envelope AEAD (per-record data key wrapped by a config-ref master key). Two backends (`sqlite.go`/`postgres.go`), TTL + size-cap `Purge`, hard-deletable per-row (GDPR erasure). Key rotation: `mayu bodies rewrap-key` (ADR-018 deferred item) rewraps `wrapped_key_*` only, via `Store.ListWrappedKeys`/`UpdateWrappedKey` (CAS) — never reads or rewrites `req_*`/`resp_*` |
| Analytics index | `internal/analytics/` | derived usage read-model; `events` table gained `ts` + `body_ref` columns (D4, ADR-018) via ALTER-if-missing (SQLite) / `ADD COLUMN IF NOT EXISTS` (Postgres); backs `GET /admin/logs` |
| ULID | `pkg/ulid/ulid.go` | monotonic record IDs (Crockford base32) |
| Usage telemetry store | `internal/telemetry/` | ADR-036: `UsageBatch`/`UsageEntry` wire types (integer µUSD, 5m/1h cache tiers separate, resolved model names; Validate rejects poison classes — in-batch dup keys, NUL bytes, >1e15 counts, out-of-range windows); `MemoryAggregator` (always-on, wall-clock 24h retention, snapshot-under-lock Rows); `PostgresAggregator` (`usage_windows` table — PK `(dataplane, window_start, team, "user", model)`, `window_start` secondary index, portable types: pg_dump/RDS snapshots are the backup story; lazy connect, advisory-lock migration 847003, batch-replace tx, DSN never in errors); `DurableAggregator` (PG-commit→memory→ack write-through; queries prefer PG, memory fallback marked `degraded`) |

### 3. Key Decisions
- SQLite (`modernc.org/sqlite`, cgo-free) default → static binary, 5-minute boot.
- Per-instance audit hash chain so restarts segment cleanly instead of reading as tampering.
- Admin-plane events (`admin_key_created` / `admin_key_revoked` / `admin_denied`, ADR-004) carry `principal.user` (opaque OIDC `sub` — never email) and `principal.auth_method` (`oidc` | `break_glass`); `auth_method` is appended at the END of `PrincipalRef` so pre-change chains still verify byte-exactly (mixed-version fixture test).
- Two-phase stores (check then debit) so a denied request never charges the team.
- Prompt/response bodies are NEVER in the audit chain (ADR-003 content-free
  invariant preserved): opt-in `audit.log_bodies` (D4, ADR-018) captures them
  into a separate, encrypted, deletable body store; the chain carries only an
  opaque `body_ref`. `Record.body_ref`/`record_ref` are appended at the END of
  the struct (omitempty pointers), so mixed-version chains verify byte-exactly.
- ADR-041 budget-tier substitution: `RequestRef.model_substituted_from`
  (appended at the end of that struct, `omitempty`, same byte-stability rule)
  carries the client's ORIGINAL requested model when a tier substituted it;
  `model_requested` by then already holds the model actually served. The
  `internal/analytics` read-model (`GET /admin/logs`) does not yet carry this
  column — the audit chain is authoritative for it today.
- Team governance policy has two sources — the config file and a `teams` DB
  record — with the DB record winning when both name the same team (D3,
  ADR-016); `internal/governance.Governor` resolves this via a per-request
  keystore lookup (`SetTeamLookup`), not a cache, so a console edit enforces
  on the very next request with no restart.
- Per-team region lock (D7, ADR-020): `TeamRecord.AllowedRegions` restricts a
  team to providers labeled with one of these regions; an UNLABELED provider
  is always dropped for a restricted team (fail-closed). A config-declared
  team with no DB record still gets its config `allowed_regions` enforced (the
  one case `TeamRecord` is synthesized from config rather than read from a
  row) — but a DB record, once it exists, wins wholesale over that config
  policy, same ADR-016 precedence as every other team field.

### 4. Code Pointers
- `internal/keystore/sqlite.go` — schema + SHA-256 lookup
- `internal/audit/writer.go` — single-writer goroutine, pending-based WAL truncate
- `internal/audit/verify.go` — `audit verify` chain check

### Pricing rate table (ADR-030)

Cost is integer µUSD, never float. The rate key is **`(config provider name, UPSTREAM model id)`** — the id in
`models.<name>.targets[].model`, *not* the canonical ingress name. Getting this wrong is the single most common
way to end up billing 0.

| Rule | Detail |
|---|---|
| Key | `(provider, upstream)`; exact match wins |
| Bedrock cross-region | ONE leading `global.` / `us.` / `eu.` / `apac.` / `us-gov.` is stripped on a miss — same model, no published price differential. Declare a prefixed key only for a genuinely different rate. |
| Model versions | **Never** collapsed. Each version needs its own rate even when prices coincide today. |
| Provider | Never collapsed — Bedrock Opus 4.8 is $6.00/$30.00 vs first-party $5.00/$25.00. |
| Cache rates | Derived from input: read **0.1x**, 5m write **1.25x**, 1h write **2.0x**. Declare `input_per_mtok` + `output_per_mtok` and the three follow. An explicit value wins; `0` means derive. |
| `on_missing` | `block` → refuse to boot on an unpriced route AND deny at runtime (402 `pricing_missing`). `allow` (default) → log loudly, bill 0. An unrecognized value is a load error. |
| `version` | Free-form label landing in every audit record's `cost.pricing_version`. Set it so a disputed invoice can be pinned to its rates. |
| `free` | A `0/0` override needs an explicit `"free": true` — the ONLY way to declare a genuinely zero-cost model. That opt-in is what makes cost 0 + `missing=false` a deliberate statement instead of an accident. |
| `0/0` | `input_per_mtok: 0` + `output_per_mtok: 0` without `free` is a LOAD ERROR — 0 means unpriced, not free. A single-sided zero is allowed. Enforced twice: at load, and again at table build, because `BuildState` is also reached by the UI-write overlay and by hot reload, which never see the file loader. |
| `pricing sync` | `mayu pricing sync --config <p> [--out <f>]` generates `pricing.overrides` for the config's **bedrock** routes from the AWS Price List Query API. Offline, operator-run, never on the request path. Needs IAM `pricing:GetProducts`. Exits 1 naming any route it could not resolve — it never emits a placeholder. `openai_compatible` routes are skipped by design. |

Adding a newly released model is therefore: read its two published figures, add
`input_per_mtok` + `output_per_mtok`. With `on_missing: "block"` a forgotten rate
fails the boot and names the route, so it cannot reach production silently.
For a Bedrock route, `mayu pricing sync` fetches those two figures from the AWS
Price List Query API — except Claude-on-Bedrock, which has no current Price List
SKUs (verified against the live API, 2026-08-26: the `us-east-1` Bedrock price
list carries only five legacy Anthropic rows — Claude 2.0, 2.1, Instant, 3
Haiku, 3 Sonnet — each input-only with no output row), so those rates stay
manual.

### 5. Cross-references
- Related modules: `internal/governance`, `internal/server` (auth)
- Related ADRs: docs/decisions/ (SQLite-vs-Postgres decision — to be recorded)
- Related runbooks: docs/runbooks/ (audit verification, backup)

### Policy-routing metadata and generations (ADR-043)

Providerstore adds `providers.data_boundary` (TEXT, omitted/unknown conveys no
trust) and `model_metadata` keyed by model with `context_window` (INTEGER) and
`capabilities` (JSON text). Existing DBs migrate to unknown boundary/zero context/no
capabilities. Seed, overlay, admin read/write/export, and live snapshots preserve
metadata and copy slices. This adds no shared-state enforcement backend.

`policy.Store.MatchingRoutingPolicies` returns caller-owned matching rules from
one atomic snapshot. Rejected distributed sensitive documents retain a fail-closed
subject gate until valid replacement; failed local reloads retain the last valid
snapshot. Apply-time targets use current `live.Holder.RoutedAndPriced`; requests
still validate candidates after topology edits. Policy and topology publication are
separate. Upgrade all participants before activating the new rule shapes.

Audit `request.routing` is appended with `omitempty` and mixed-version exact-byte
verification. It records requested/selected/proposed model, bounded decision fields,
policy names/generations, and planned/actual provider and boundary. Actual attempt
fields distinguish same-model retries across providers. No detected text, prompt,
key ID, or client session value is added. Existing body logging stays independent.
No-rule traffic omits the field. See [decision semantics](../policy-routing.md#read-decisions-and-evaluate-rollout).
