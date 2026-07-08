# ADR-018: Opt-in request/response body logging (D4)

**Date:** 2026-07-08
**Status:** Accepted (implemented).
**Related:** §4.2/§4.7/§6.3/§6.8/§8 D4 of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`
(the console UX spec's nominal ADR-017 slot — see numbering note); ADR-017
(budget alerts — this repo's actual next-available slot, landed first);
ADR-003 (the tamper-evident, content-free audit chain this feature
deliberately does NOT put bodies into); ADR-009 (the PII-masking filter this
feature captures the post-mask bytes of); ADR-015 (analytics Mode A/B dual
backend — the direct precedent for this feature's SQLite/Postgres split).

**Numbering note:** the spec's §14 table nominally assigns body logging
ADR-017 and budget alerts ADR-018. Per the user-confirmed PR order (budget
alerts landed first) and CLAUDE.md's rule ("find the highest number,
increment by 1"), body logging takes the next available slot: ADR-018.

## Context

inferplane's audit chain is deliberately **content-free** (ADR-003): a
tamper-evident hash chain of metadata only, never prompt/response text. This
is a real product advantage — governance without content retention — but it
means an operator debugging "what did this request actually send/receive"
has nothing to look at. The spec (§4.2, §6.3) calls for **opt-in** body
capture that preserves the content-free chain's guarantees for everyone who
doesn't turn it on, while giving those who need it (compliance requirement,
debugging a bad response) a bounded, deletable, encrypted side-channel.

Three things make this harder than "just log the bytes":
1. **The chain must stay content-free even when bodies are captured.** A
   `body_ref` in the chain must be a pointer, never derivable back to content.
2. **A captured body must be genuinely deletable** (GDPR/CCPA right-to-erasure)
   without touching the WORM-anchored, append-only audit chain.
3. **Capture must not touch the verbatim-forwarding cache invariant (§4.4).**
   `RawBody` is forwarded byte-for-byte when protocol matches; body capture is
   a side-channel copy, never a rewrite.

## Decision

### 1. A separate package, `internal/bodystore`, with two backends

Mirrors ADR-015's analytics Mode A/B split exactly: SQLite is the
zero-dependency single-instance default; Postgres is the opt-in HA backend.
Unlike analytics Mode B, **no lease or fencing is needed**: every replica
mints its own collision-free ULID `body_ref` and writes only rows it minted;
`Purge` (TTL-then-size-cap eviction) issues idempotent `DELETE`s that are safe
to run concurrently from every replica with no coordination. This is a
materially simpler HA story than analytics Mode B's fenced aggregator, and is
possible only because bodies have no cross-replica aggregation requirement
(unlike rollups).

### 2. Bodies are captured, encrypted, and stored OUTSIDE the audit chain

`audit.Record` gains two fields, appended at the struct's END (the
`AuthMethod`/ADR-016 `BodyRef`/`RecordRef` precedent — an `omitempty` pointer
keeps pre-change records byte-identical, so mixed-version chains still
verify):
- `BodyRef *string` — set only on a `request_completed` record whose body was
  captured. The opaque ULID (`pkg/ulid`) carries no team/customer/path
  information — it leaks nothing on its own.
- `RecordRef *string` — set only on `body_accessed`/`body_deleted` events,
  pointing back at the `request_completed` record whose body was viewed or
  erased (access accountability, §6.3).

**Anti-recursion, enforced by code path, not just convention:** `body_accessed`
and `body_deleted` NEVER set `BodyRef` — the emitting code
(`adminapi/bodies.go`) simply never touches that field, so a body view can
never itself become body-logged. Pinned by
`TestBodiesHandler_GetRoundTripEmitsBodyAccessed` and
`TestBodiesHandler_DeleteThenGetIsTombstone` asserting `BodyRef == nil` on
every emitted access/delete record.

### 3. Envelope encryption (per-record data key, master key wraps it)

Each captured body: a fresh random 32-byte data key seals the request/response
bytes (AES-256-GCM); the configured master key (`audit.log_bodies.key_ref`,
64 hex chars, never inline) seals the data key. **Rotation = rewrap the data
keys, never touch the (potentially large) body ciphertext.** The rewrap CLI
itself is deferred (format is fixed now; the procedure is: for each row,
unwrap the data key with the old master key, reseal it with the new one,
`UPDATE` only the `wrapped_key_*` columns).

Any decrypt failure — wrong key, tampered ciphertext, corrupted row — returns
a single generic error (`ErrGone`) that the admin API maps to a **410
tombstone**, never a 500, never a plaintext fallback. `Fetch`/`open` never
distinguish *why* a body is unavailable to the caller (fail-closed, no
decryption-oracle surface).

### 4. Copy-only capture — the cache invariant (§4.4) is untouched

Capture happens **after** the response is already written to the client
(non-streaming) or after the request is fully proxied (streaming) — it reads
the SAME `RawBody`/`resp.RawBody` slices the ingress handler already has,
never mutates them, and hands them to the `bodystore.Recorder`'s worker
goroutine by reference (no copy needed: these slices are read-only after this
point in both handlers). Pinned by a required byte-for-byte passthrough
regression test in both ingress packages
(`TestMessagesBodyCapture_RawBodyPassthroughByteForByte`,
`TestChatBodyCapture_RawBodyPassthroughByteForByte`).

### 5. Streaming responses are captured REQUEST-ONLY

A streaming response exists only as per-event `ev.Raw` bytes, teed to the
client one SSE frame at a time — never buffered as a whole. Buffering it just
to capture it would violate the streaming memory posture (an attacker or a
huge response could force unbounded memory growth). So for a streaming
request, only the request body is captured; `Body.Response` is `nil` and the
console renders "not captured — streaming responses are request-only". This
is a stated, permanent limitation, not a TODO.

### 6. Response masking is NOT implemented (explicitly deferred, not silently skipped)

The only PII-masking seam in the repo (ADR-009's `internal/filter`) is
request-text-only — there is no response-filter seam. Captured request bodies
for a masked team ARE masked (capture happens after the ADR-009 mask step);
captured response bodies are stored as the upstream returned them. The spec
(§6.3) already frames masking as "best-effort, not a guarantee" — this ADR
states the response side plainly rather than implying parity that doesn't
exist.

### 7. `body_accessed` is deduped per (viewer, ref) within a 5-minute window

§4.7's anti-flood requirement: a Logs drawer left open, or a viewer
double-clicking, must not grow the audit chain unboundedly. `BodiesHandler`
keeps an in-memory `map[viewer+ref]lastEmitTime`, opportunistically pruned
past 10k entries. This is per-instance state (same posture as every other
in-memory dedup/counter in this codebase, ADR-013) — acceptable because
`body_accessed` is an access log, not an enforcement mechanism.

### 8. `DELETE` is idempotent; `body_deleted` fires even on an already-gone ref

Erasing a body is a hard, idempotent `DELETE` — a second `DELETE` on the same
`ref` still returns `204`. The handler best-effort looks up the record ID
first (`Recorder.Meta`, no decryption) purely so `body_deleted` can carry
`RecordRef`; if the ref is already gone, the lookup fails harmlessly and the
event is still emitted without `RecordRef`.

### 9. Boot-fatal on body-store open failure (unlike analytics' degrade-and-continue)

The analytics index degrades to "disabled" on an open failure (best-effort
observability). Body logging does the opposite — an operator who explicitly
configured `audit.log_bodies` (a compliance-driven opt-in) must not discover
weeks later that it silently never worked. `newGateway` fails to boot if the
configured backend can't be opened.

### 10. Config: `audit.log_bodies` — presence enables, `key_ref` always required

```json
"audit": {
  "log_bodies": {
    "type": "sqlite",
    "key_ref": {"env": "BODY_MASTER_KEY"},
    "ttl": "168h",
    "max_bytes": 1073741824,
    "max_body_bytes": 1048576
  }
}
```
`type: "postgres"` additionally requires `dsn_ref`. An inline `key`/`dsn` is
rejected at `LoadRaw` (§7), same guard shape as every other secret in this
config. Defaults: TTL 7 days, 1 GiB total cap, 1 MiB per-record cap — all
overridable.

## Deferred

- Key-rotation rewrap CLI (format fixed now; manual procedure documented above).
- Response-body PII masking (no seam exists; stated as a limitation, not hidden).
- Streaming-response body capture (would require buffering — rejected outright).
- A `audit.log_bodies` toggle in the Settings view (config is file-authoritative
  for audit; the console shows the resulting `logs_bodies` capability only).
- Team-scoped Logs/body views (full-admin-only for now, matching analytics
  Phase 1a's posture); per-team retention policy.
- A dedicated Prometheus counter for capture drops (stderr logging is the
  interim signal, matching D5b/ADR-017's same interim posture for delivery
  failures).
