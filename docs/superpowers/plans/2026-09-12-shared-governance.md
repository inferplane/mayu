# Shared Governance Implementation Plan

> **For agentic workers:** Use superpowers:subagent-driven-development task by
> task; the parent owns integration, review and all external git operations.

**Goal:** Complete shared keys and atomic global rate/token quota enforcement.
**Architecture:** Opt-in shared gateways use one Postgres authority for identity,
token buckets, quota windows and money. Policy monetary reservations share
ADR-045 accounts; node-local/default profiles remain separate.
**Tech Stack:** Go 1.25, existing pgx and pure-Go SQLite, Postgres17 tests.
**Spec:** `docs/superpowers/specs/2026-09-12-shared-governance.md`.

## Global Constraints

- No new provider or provider-code changes; no leaf imports of config/server.
- Secrets only via env/file references; hashes and DSNs never in errors/metrics.
- Integer monetary arithmetic; exact rate arithmetic; fail closed on DB errors.
- Count paths local/200; each actual provider attempt reserves independently.
- Existing no-database default and ADR-045 node-local behavior remain compatible.
- DCO on commits; no pushes/merges by workers. Parent performs latest-HEAD review.

### Task 1: Shared key backend

**Files:** `internal/keystore/postgres*.go`, `internal/keystore/import*.go`,
`internal/keystore/keystore.go`; scoped tests in the same package.

**Interfaces:** Preserve Store/TeamStore/KeyEnsurer. Add:

```go
func OpenPostgres(ctx context.Context, dsn string) (*PostgresStore, error)
func (s *PostgresStore) Ready(context.Context) error
func (s *PostgresStore) Seed(context.Context, []TeamRecord, []SeedKey) error
func (s *PostgresStore) RevokeSnapshot(context.Context, Principal) error
func (s *PostgresStore) ImportSQLite(context.Context, string) (ImportResult, error)
// The caller owns a transaction and locks the key/team tables before admission.
func ReadPostgresPrincipal(context.Context, pgx.Tx, string, time.Time) (Principal, error)
```

`SeedKey` contains Plaintext, Team, AllowedModels and Options (KeyOptions).
`ImportResult` contains Keys and Teams counts only.
`Principal` gains `SharedRevision string`, `TeamSnapshot *TeamRecord`,
`TeamSnapshotLoaded bool`, all `json:"-"`. The revision includes every key/team
authorization/governance field, not database time. `ReadPostgresPrincipal` reads
by KeyID, validates revocation/expiry using supplied DB time, and attaches the
same snapshot/digest as Resolve. Export `ErrStoreUnavailable` and
`ErrSnapshotChanged` sentinels without embedding private details.

Use existing `keys` and `teams` column names (TEXT time/JSON/list encodings,
BIGINT monetary/rate/quota fields, INTEGER revoked), so parent admission can
consume the schema without a parallel identity store.

- [x] Write failing parity and independent-pool tests for all fields, BIGINT,
  expired/revoked keys, concurrent schema initialization, Seed conflicts,
  compare-and-set revoke and import tombstones.
- [x] Implement bounded pools/operations, transactional migrations and exact
  snapshot reads, with no active connection on the default SQLite path.
- [x] Persist original seed fingerprints. An unchanged original declaration is a
  no-op after admin edits/deletion; a conflicting declaration refuses. On first
  seed, require preexisting/imported records to match before attaching a marker.
- [x] Import raw SQLite rows (including revoked hashes) transactionally; test
  retry and rollback on conflicts. Never call Create to migrate an identity.
- [x] Run focused real-PG tests and race; report files and evidence.

### Task 2: Global admission

**Files:** new `internal/authority/pgstore/shared*.go` and tests. Parent owns
shared wire interfaces and policy schema; workers do not alter their signatures.

**Interfaces:** `(*Store).InitializeShared(ctx) error`, and implementation of
`governance.SharedAuthority` defined by the parent in
`internal/governance/shared.go`. Existing New/Close/Ready remain available.

- [x] Write independent-pool tests where concurrent requests across two stores
  admit exactly one global RPM/TPM/quota allowance, not one per process.
- [x] Lock policies, keys and teams before fresh definition reads; use
  ReadPostgresPrincipal and reject an auth revision mismatch.
- [x] Build independent team/key/policy-rule constraints. Use shared token
  buckets and UTC day/month windows, preserving rule identity across edits.
- [x] Reserve all scopes atomically; include policy money in existing
  authority_accounts so ADR-045 grants and shared permits compete globally.
- [x] Implement terminal Finish/Cancel replay fingerprints, original-window
  settlement, no automatic crash refund, overrun freeze and authorized Usage.
- [x] Cover user-only cross-team scopes, soft/hard layering, fresh policy cuts,
  old-window finish, aborted batches, reopen, missing metadata and overflow.
- [x] Run focused real-PG race tests and report evidence.

### Task 3: Schema, runtime, deployment and acceptance

**Files:** parent owns `api/v1alpha1/types.go`, `internal/policy/*`,
`internal/governance/*`, `internal/config/*`, `internal/proxy/*`,
`internal/server/*`, `cmd/mayu/*`, CRD/chart, docs and examples.

- [x] Define SharedAuthority types first; add tokenQuota parsing, deep-copy
  and shared-only enforceability gates, plus native CRD/CEL validation.
- [x] Validate strict backend/profile configuration and create a policy-only
  escrow-v1 sync adapter for shared mode. Reject unsafe hot profile/DSN changes.
- [x] Wire shared backend/insert-only bootstrap and preserve the key/team auth
  snapshot for guardrails/region selection; add conditional admin revocation.
- [x] Extend existing pre-attempt reservation/finalization to SharedAuthority;
  retain uncertainty and avoid legacy double mutation. Add shared /v1/usage.
- [x] Add configuration-based key CLI and explicit SQLite import command.
- [x] Assemble two real gateways with one DB and verify cross-replica keys,
  RPM/TPM/day/month quotas, budgets, fallback, streams, revocation and outages.
- [x] Add shared-profile config/operator guide, accurate HA chart guards and
  per-replica state handling; update ADR/README/reference/AGENTS context.
- [x] Run both static builds, full real-PG race suite, vet, format, harness,
  native CRD and installed Codex CLI acceptance.
- [ ] Complete latest-HEAD independent whole-branch AI review.
- [ ] DCO commit/push, create PR, inspect latest-HEAD AI and inline feedback,
  fix actual blockers and merge only after all required checks pass.
