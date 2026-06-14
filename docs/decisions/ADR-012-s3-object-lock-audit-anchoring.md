# ADR-012: S3 Object Lock audit anchoring — tamper-evident → tamper-resistant

**Date:** 2026-06-14
**Status:** Accepted — 3-family design gate (codex + gemini + kiro, all
CHANGES-REQUIRED; architecture sound, refinements folded in): single-atomic
`{hash,count}` snapshot (no tearing); publish the head ONLY after `wal.Append`
(no witnessing a non-durable record); final anchor under a fresh bounded
timeout + worker wait (no shutdown hang/leak); `lastAnchoredCount` advances only
on SUCCESS (failed heads retry); bounded threat-model/RPO + tamper-resistance
made conditional on Object Lock compliance retention + restricted IAM;
nanosecond+count unique keys; precise runbook verification procedure.
**Related:** §5.4 (audit hash chain), ADR-003 (audit differentiation), audit
`verify` (chain check), aws-sdk-go-v2 (already used by the bedrock provider)

## Context

The audit log is a **hash chain**: each record carries `prev_hash`, so any edit
to a past record breaks the chain and `inferplane audit verify` detects it. That
is **tamper-EVIDENT** — but only against an attacker who cannot rewrite the whole
chain. An attacker with write access to the audit file can re-hash every record
from the edit point forward and produce a valid-looking chain; nothing external
pins the original history.

**Tamper-RESISTANT** requires an immutable external witness. Writing the chain's
**head hash** periodically to **S3 with Object Lock** (WORM — compliance mode:
not even the account root can delete/overwrite until retention expires) means a
local rewrite can no longer hide itself: the anchored head hashes prove what the
chain contained at each anchor time, and a re-verified local chain whose head no
longer matches the latest anchor is provably tampered.

## Decision

**Opt-in periodic anchoring of the audit chain head to an Object-Lock S3 bucket,
behind an `Anchorer` interface (S3 impl + fake), best-effort and off the
critical path.**

### 1. Chain-head snapshot (single-atomic, post-durable)

`audit.Writer` exposes `HeadHash() (hash string, count int64)` — the current
`prev_hash` and record count. To avoid a **torn snapshot** (gate: hash from
record N, count from N±1), the two are published as ONE immutable
`*chainHead{hash,count}` via a single `atomic.Pointer` (init `{genesis,0}`).
Critically, the head is published **only AFTER `wal.Append` durably persists the
record** (gate, gemini CRITICAL): publishing before persistence could let an
anchor witness a hash for a record a crash then loses, making a recovered chain
look maliciously truncated. So the order is: compute hash → `wal.Append` → swap
the atomic head.

### Threat model (bounded, explicit)

Tamper-resistance applies to **anchored windows**: a local rewrite of any record
up to the latest *successful* WORM anchor is detectable (the re-verified local
head won't match the anchored head). It is bounded by the **RPO = anchor
interval** — records since the last successful anchor are only tamper-EVIDENT
(the chain links them, but no external witness yet). It is **conditional** on the
S3 bucket having Object Lock **compliance** retention + versioning and on IAM
that forbids retention bypass / object delete (else the gateway only writes
mutable JSON — the runbook makes this explicit). It does not defend against a
writer compromised *before* anchoring (it can only anchor what it sees); it
defends against after-the-fact rewriting.

### 2. `Anchorer` interface + S3 implementation

```go
type AnchorPoint struct { Instance, HeadHash string; Count int64; TS time.Time }
type Anchorer interface { Anchor(ctx context.Context, p AnchorPoint) error }
```

The S3 anchorer (`internal/audit/anchor` or `providers`-style) `PutObject`s a
small JSON `{instance, head_hash, count, ts}` to `s3://<bucket>/<prefix>/<instance>/<ts>.json`.
Object Lock is a **bucket property the operator enables** (compliance mode +
retention); the gateway optionally sets `ObjectLockMode`/`RetainUntilDate` on the
put when `retain_days` is configured. It uses `aws-sdk-go-v2/service/s3` (sibling
of the bedrock SDK already in the tree) with the same credential chain. A `fake`
anchorer (records calls in memory) backs the unit tests — the real S3 path is
verified against AWS out of band (this environment has no S3).

### 3. Periodic, best-effort anchor worker

The assembly starts an anchor worker (ticker at `interval`) when `audit.anchor`
is configured: each tick reads `HeadHash()` and calls `Anchor`. An anchor
failure is **logged, never fatal** (anchoring is durability/forensics, not the
request path — identical posture to the SIGHUP reload worker). It skips ticks
where the count has not advanced **beyond the last SUCCESSFUL anchor** — so a
failed anchor is **retried** on later ticks rather than skipped (gate, codex):
`lastAnchoredCount` advances only after `Anchor` returns nil. The worker stops on
ctx cancel and makes a **final anchor under a fresh bounded-timeout context**
(not the canceled ctx, not unbounded — gate: no shutdown hang, no leak), and
`serve` waits for it to exit. Unique keys use `RFC3339Nano + count` so a
tick/final anchor at the same second never collide.

### 4. Verification (this ADR ships the WRITE path)

`inferplane audit verify` keeps verifying the local chain. **Anchor-aware
verification** (fetch the latest S3 anchor, re-verify the local chain, and assert
the local head still matches or descends from the anchored head) is a documented
follow-up; v1 ships the anchoring writer + the operator runbook (an auditor
fetches the WORM anchors and compares). The value — an immutable external
witness — is delivered by the write path; automated cross-check is additive.

## Alternatives considered

1. **Anchor every record (synchronous).** Rejected — an S3 round-trip per audit
   record would couple request latency/throughput to S3 and is needless: the
   chain already links records; periodic head anchoring bounds the
   un-witnessed window without per-record cost.
2. **A blockchain / transparency-log (e.g. Trillian).** Rejected — heavy external
   dependency and operational surface against the single-binary ethos; S3 Object
   Lock is a WORM primitive every AWS operator already has.
3. **Local WORM (append-only file, fs immutability).** Rejected — a local file
   shares the attacker's blast radius; the witness must be external and
   independently access-controlled. (Non-AWS operators can point the anchorer at
   any S3-compatible WORM store — MinIO with Object Lock — via `endpoint`.)
4. **Gateway enforces Object Lock on the bucket.** Rejected — bucket Object Lock
   config + retention is an operator/IaC concern (and can't be set post-creation
   for the enable flag); the gateway writes objects and optionally sets
   per-object retention, and the runbook tells the operator to create the bucket
   with Object Lock.

## Consequences

- The audit chain gains an immutable external witness: a local rewrite cannot be
  hidden, upgrading tamper-evident → tamper-resistant for the anchored windows.
- Opt-in: no `audit.anchor` → behavior unchanged (no S3, no worker, no dep
  active at runtime). The S3 SDK compiles in but is inert.
- Best-effort: an S3 outage never affects request serving or audit writing.
- v1 ships the anchoring writer + runbook; automated anchor-aware verify is a
  tracked follow-up.
- Adds `aws-sdk-go-v2/service/s3` (pure-Go, sibling of the existing bedrock SDK).
