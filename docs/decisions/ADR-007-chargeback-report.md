# ADR-007: Chargeback report from the audit log (`inferplane report`)

**Date:** 2026-06-14
**Status:** Accepted
**Deciders:** maintainers; 2-round multi-model design gate (codex, gemini)
**Related:** ADR-003 (v0.2 priority #3, finance lock-in), spec §5.3 (cost), §5.4 (audit)

## Context

ADR-003 made a chargeback report a v0.2 priority: a finance team that exports
per-team LLM spend from the gateway is locked in, and it showcases the
integer-µUSD cost accuracy that differentiates inferplane from float-accumulating
proxies. The data already exists — every settled request writes an audit record
carrying `principal.team`, `request.model_resolved`, and
`cost.amount_usd_micros`.

## Decision

`inferplane report --file <audit.jsonl> [--since RFC3339] [--until RFC3339]
[--by team|team,model]` aggregates settled cost from the audit log and writes
CSV to stdout. It is a read-only CLI over the existing log — **no new store**.

- **Source of truth is the audit log**, not a separate ledger: the tamper-evident
  chain is already the billing record; a second store could drift from it.
- **Only `request_completed` records with a non-nil `cost` are summed** (started,
  denied, and `count_tokens` carry no settled cost).
- **Group by team, or team + the RESOLVED model** (`model_resolved` — the model
  actually billed; falls back to `model_requested` if absent). Requested-model
  grouping would misattribute cost across aliases/fallbacks.
- **Integer µUSD end to end.** USD is formatted directly from micros
  (`micros/1e6` whole part, `micros%1e6` zero-padded 6-digit fraction,
  sign-aware) — never float division or accumulation, so the report is exact at
  any magnitude (a billing artifact must not show float-rounded money).
- **CSV via `encoding/csv`** — team/model names are operator-defined and may
  contain commas/quotes/newlines; naive joining would corrupt or inject.
- **`--since` inclusive, `--until` exclusive**, both parsed as `time.Time`
  (RFC3339, offset-aware) and compared to each record's parsed `ts`.
- **Robust to a live/partial log**: a partial trailing line (file mid-write) is
  trimmed; a malformed JSON line or a record with an unparseable `ts` is skipped
  and counted (reported on stderr) — a single corrupt line never denies the
  whole report.

## Alternatives considered

1. **A dedicated spend store / DB table.** Rejected — duplicates the audit log,
   can drift from the tamper-evident truth, and adds a write path on the hot
   path. The log already has everything.
2. **Float USD accumulation.** Rejected — month-end reconciliation drift is
   exactly the failure mode the integer-µUSD mandate exists to prevent.
3. **Fail the whole report on a bad line.** Rejected — one corrupt/truncated
   line (e.g. crash mid-write) would deny finance its numbers; skip + count is
   the resilient choice, and the audit-verify path separately proves integrity.

## Consequences

- Finance gets per-team (and per-model) CSV they can pivot, straight from the
  gateway, with exact integer money.
- The report is decoupled from settlement: it re-derives totals from the log, so
  it works offline, on rotated/archived logs, and against any instance's file.
- It shares the complete-line + integrity discipline with the `/admin/audit/verify`
  endpoint (ADR-003 #2); a future "verified report" could refuse to total a log
  whose chain does not verify.
