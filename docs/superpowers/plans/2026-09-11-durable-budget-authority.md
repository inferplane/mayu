# Durable budget authority implementation

> For agentic workers: use subagent-driven-development in
> `/tmp/inferplane-durable-budget`, preserving all other worktrees.

**Goal:** Postgres-authoritative escrow and durable local per-attempt admission.
**Spec:** `docs/superpowers/specs/2026-09-11-durable-budget-authority.md`.
**Stack:** Go 1.25, existing pgx and pure-Go SQLite; no Redis or remote classifier.

## Task 1 — Authoritative database

Own `internal/authority/pgstore`. Consume `internal/policy/authority.go`.
Provide `New(dsn)`, `Initialize(ctx)`, `Close`, and
`Sync(ctx, dataplane, AuthorityRequest) (SyncResponse,error)`.
Read current policy rows within the issuance transaction; produce the full policy
snapshot, definitions, grants/acknowledgements and durable active tiers.

- [x] Failing tests for concurrency, retry, conflicting replay, restart, stale
  policy cache, UTC rollover, late settlement and expiry retaining liabilities.
- [x] Implement transactional schema/migrations, stable ordered locks, bounded
  arithmetic, idempotent capabilities, old-window reports and tier latches.
- [x] Validate against the disposable local Postgres17 and run race/vet.

## Task 2 — Local journal and admission

Own `internal/authority/local`. Consume shared wire types and structural
governance admission types. Provide `Open(path)`, `Reserve`, `Finish`, `Request`,
`Apply`, `Wake`, `Close`. No CP/network calls inside admission.

- [x] Failing tests for all-scope atomicity, double settlement, sequence replay,
  restart fencing, unfinished attempts, expiry, missing credit and disk errors.
- [x] Persist every reservation before returning a permit; use monotonic expiry;
  accumulate soft meters and close only proven unused authority.
- [x] Validate restart using the same file; race and multi-handle tests.

## Task 3 — Protocol and runtime integration

Parent owns shared policy types, pricing bounds, governance structural interfaces,
control-plane dispatch, syncer negotiation and all ingress hooks.

- [x] Test conservative price bounds and overflow/missing-metadata denial.
- [x] Wire dedicated durable mode, private journal and background grant wakeup.
- [x] Reserve before every provider attempt; settle observed complete usage or
  retain uncertainty on all early errors, stream interruption and cancellation.
- [x] Ensure cached policy cannot widen an established request; rate/key controls
  remain additional, count paths remain nonbillable/local200.

## Task 4 — Acceptance, documentation, review and merge

- [x] Exercise two CP replicas/two node journals against one real Postgres DB.
- [x] Test budget cutover and full request flow, including failure and restart.
- [x] Update ADR/reference/README/config/Helm boundaries and validate examples.
- [x] Static builds, full race suite against PostgreSQL17, vet, format, harness, native CRD validation, installed Codex CLI native/Chat bridge tests.
- [x] Independent review and regression fixes (including stale old-boot meters).
- [ ] Commit with DCO, push PR and latest-HEAD AI/CI loop.
- [ ] Merge after current reviewed HEAD and branch protection pass.
