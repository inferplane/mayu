# Plan: S3 Object Lock audit anchoring (roadmap #3)

**Date:** 2026-06-14
**Related:** ADR-012 (this plan), §5.4 audit chain, aws-sdk-go-v2
**Base:** main @ 9bd7908 · **Produces:** opt-in audit anchoring

## Goal

Opt-in periodic anchoring of the audit chain head to an Object-Lock S3 bucket —
upgrading tamper-EVIDENT → tamper-RESISTANT — behind an `Anchorer` interface
(S3 impl + in-memory fake), **best-effort and off the critical path**, **no-op
when unconfigured**.

## Core architecture (from ADR-012)

- `audit.Writer.HeadHash() (string, int64)` — race-safe snapshot of the chain
  head (`prev_hash`) + record count, published via atomics by the single writer
  goroutine.
- `audit.Anchorer` interface + `AnchorPoint{Instance, HeadHash, Count, TS}`. An
  S3 anchorer (`internal/audit/s3anchor`) PutObjects a small JSON to
  `s3://bucket/prefix/instance/<ts>.json` (aws-sdk-go-v2/service/s3), optional
  per-object retention; an in-memory `fakeAnchorer` backs tests.
- A periodic anchor worker in the assembly (ticker @ interval), best-effort
  (failures logged, never fatal), clean lifecycle (ctx-cancel + final anchor),
  anchors only when the count advanced.
- Opt-in via `audit.anchor` config; absent → no worker, no S3, no dep active.

## Hard safety invariants (the gate's checklist)

- **Off the critical path**: an `Anchor` error NEVER affects request serving or
  audit writing — logged only. Pinned (failing anchorer → requests/audit
  unaffected).
- **No-op when unconfigured**: no `audit.anchor` → no worker, no S3 client, head
  snapshot still cheap. Pinned.
- **Race-safe, untearable, post-durable head**: `HeadHash()` returns a single
  atomic `{hash,count}` snapshot (never a torn N/N±1 mix), published ONLY after
  `wal.Append` persists (never witness a non-durable record). Pinned by a
  concurrent `-race` test + an order assertion.
- **Failed anchors retry**: `lastAnchoredCount` advances only on a SUCCESSFUL
  `Anchor`; a transient failure is retried on later ticks (not skipped). Pinned.
- **Clean worker lifecycle**: stops on ctx cancel; final anchor under a FRESH
  bounded-timeout context (no hang on a stuck S3); `serve` waits for exit; no
  goroutine leak. Pinned (hanging/failing fake anchorer).
- **Anchors carry no secret/PII**: only instance id + head hash + count + ts.
  Pinned.
- **Pure-Go / CGO=0** still builds.

## Tasks

- [ ] **T1 — `audit.Writer.HeadHash()` single-atomic, post-durable snapshot.**
  A `chainHead{hash string; count int64}` published via ONE `atomic.Pointer`
  (init `{genesis,0}`), swapped in `loop()` **after `wal.Append` returns**;
  `HeadHash() (string, int64)` reads it. Tests: head advances after Append;
  `-race` concurrent read while appending; the published head/count are never
  torn (single struct).
  *Files:* `internal/audit/writer.go`, `internal/audit/writer_test.go`.

- [ ] **T2 — `Anchorer` interface + fake + AnchorPoint.**
  `internal/audit/anchor.go`: interface + AnchorPoint + a `fakeAnchorer`
  (records calls). Tests: fake captures the point.
  *Files:* `internal/audit/anchor.go`, `internal/audit/anchor_test.go`.

- [ ] **T3 — S3 anchorer (`internal/audit/s3anchor`).**
  `aws-sdk-go-v2/service/s3` PutObject of the JSON anchor; deterministic key;
  optional `ObjectLockMode=COMPLIANCE` + `RetainUntilDate` when `retain_days`
  set; `endpoint` override for S3-compatible WORM (MinIO). Constructor takes a
  PutObject-ish client interface so a stub verifies the request shape offline
  (no real AWS). Tests: PutObject called with bucket/key/body; retention set when
  configured; body is the JSON anchor (no secret).
  *Files:* `internal/audit/s3anchor/s3anchor.go`,
  `internal/audit/s3anchor/s3anchor_test.go`.

- [ ] **T4 — config `audit.anchor` block.**
  `AnchorConfig{Type, Bucket, Prefix, Region, Endpoint, Interval, RetainDays}` on
  `AuditConfig`. Validation: when present, `type=="s3"`, bucket required,
  interval parses (default e.g. 5m). Tests: parse; absent → nil; validation.
  *Files:* `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **T5 — periodic anchor worker in the assembly.**
  `cmd/inferplane/gateway.go`: when `audit.anchor` set, build the S3 anchorer and
  run `anchorWorker(ctx, interval)` (ticker; reads HeadHash; anchors only when
  count advanced beyond the last SUCCESSFUL anchor — failures retried; logs
  failures + bumps an anchor-failure metric; on ctx cancel a FINAL anchor under a
  fresh bounded-timeout context; `serve` waits for the worker to exit). Tests:
  worker anchors on tick (fake + fast interval); a FAILING anchor is retried next
  tick (count not advanced); a HANGING anchor does not block shutdown (bounded);
  final anchor on cancel; absent config → no worker.
  *Files:* `cmd/inferplane/gateway.go`, `cmd/inferplane/anchor_test.go`,
  `internal/metrics/metrics.go` (anchor-failure counter), `cmd/inferplane/main.go`.

- [ ] **T6 — docs + runbook (PRECISE prerequisites + verification).**
  `docs/reference/data.md` + `infrastructure.md`; a runbook
  `docs/runbooks/audit-anchoring.md` that states tamper-resistance is
  **conditional**: the bucket MUST have Object Lock **compliance** retention +
  versioning, and IAM MUST forbid retention bypass / object delete (else the
  gateway only writes mutable JSON); and gives the EXACT auditor procedure (fetch
  the latest WORM anchor; `inferplane audit verify` the local chain; assert the
  re-verified local head equals the anchored head). Note opaque-instance-id
  guidance. `internal/CLAUDE.md`, `examples/config.json` (commented
  `audit.anchor`); mark ADR-012 Accepted.
  *Files:* docs + runbook + example.

## File scope (allow-list)

```
docs/decisions/ADR-012-s3-object-lock-audit-anchoring.md
docs/superpowers/plans/2026-06-14-s3-audit-anchoring.md
internal/audit/writer.go
internal/audit/writer_test.go
internal/audit/anchor.go
internal/audit/anchor_test.go
internal/audit/s3anchor/s3anchor.go
internal/audit/s3anchor/s3anchor_test.go
internal/config/config.go
internal/config/config_test.go
cmd/inferplane/gateway.go
cmd/inferplane/anchor_test.go
cmd/inferplane/main.go
internal/metrics/metrics.go
internal/metrics/metrics_test.go
docs/reference/data.md
docs/reference/infrastructure.md
docs/runbooks/audit-anchoring.md
internal/CLAUDE.md
examples/config.json
go.mod
go.sum
```

## Out of scope (explicit)

- Automated anchor-aware `verify` (fetch S3 anchor + cross-check) — documented
  follow-up; v1 ships the write path + runbook.
- The gateway creating/configuring the Object-Lock bucket (operator/IaC concern).
- Real-AWS E2E (no S3 here) — verified via a stub S3 client offline.
- Anchoring anything but the chain head (no per-record S3).
