# Runbook: body-store key rotation (ADR-018 deferred item)

`inferplane bodies rewrap-key` rotates the AES-256 master key that wraps every
captured body's per-record data key (`audit.log_bodies.key_ref`). It touches
only the `wrapped_key_nonce`/`wrapped_key_ct` columns — the actual
request/response ciphertext (`req_*`/`resp_*`) is never read or rewritten, so
rotation is fast and cheap regardless of how much body content is stored.

## When to rotate

- Routine credential hygiene (the same cadence you'd rotate any long-lived
  symmetric key).
- Suspected exposure of the current `audit.log_bodies.key_ref` value.
- Compliance requirements with a fixed rotation interval.

## Prerequisites

- The OLD master key value (whatever `audit.log_bodies.key_ref` currently
  resolves to) and a freshly generated NEW 64-hex-char (32-byte) key, e.g.
  `openssl rand -hex 32`.
- Both keys available to the CLI as an env var or a file — **never pass a key
  value directly as a flag**; only the ref (env var name or file path) is a
  flag.

## Running it

```bash
inferplane bodies rewrap-key \
  --store /var/lib/inferplane/bodies.db \
  --old-key-env BODY_MASTER_KEY_OLD \
  --new-key-env BODY_MASTER_KEY_NEW
```

For the Postgres backend, replace `--store <path>` with
`--postgres-dsn-env <VAR>` (a DSN secret ref, same shape as every other
Postgres DSN in this repo). Old/new keys may each come from `--old-key-file`/
`--new-key-file` instead of an env var.

**No fleet-wide quiesce is required.** Every replica already writes
independently with no coordination (same posture as `Purge`) — a body
captured by another live replica while rotation is running simply isn't in
this run's row list; a second `bodies rewrap-key` run with the same old/new
keys catches it.

## Reading the output

```
rewrapped=N skipped=M raced=K
```

- `rewrapped` — rows successfully moved from the old key to the new key.
- `skipped` — rows the given old key could not unwrap at all.
- `raced` — rows whose wrapped-key bytes changed between listing and updating
  (another rotation run, or a concurrent `Purge`/delete) — not an error;
  they're simply not yours to touch this run.

## Exit codes

| Exit | Meaning |
|---|---|
| `0` | At least one row was rewrapped (or the store was empty — nothing to do). |
| `1` | **Zero rows were rewrapped, but some were skipped.** This means the given `--old-key-*` opened NOTHING in this store. |
| `2` | Usage error (bad/missing flags, malformed key hex, invalid secret ref shape). |

### ⚠ Exit 1 is ambiguous by design — read this before treating it as a failure

There is deliberately **no key-version column** in the schema (adding one was
out of scope for this CLI). This means the tool **cannot** tell the difference
between:

1. You passed the **wrong** old key, or
2. This store was **already fully rotated** to what you're calling the new
   key (rotation doesn't delete rows — the row is still there, just wrapped
   under a different key now, so re-running the exact same command finds it
   and fails to unwrap it with the "old" key every time).

**A clean re-run of a completed rotation is therefore expected to report
`rewrapped=0 skipped=N` and exit `1`.** Don't page on this alone — if you're
confident the old key you supplied is correct and the store had rows in it
before, an exit-1 with `skipped>0` and no `rewrapped` most likely means
rotation already happened. If you're NOT confident the old key is correct,
exit 1 is exactly the fail-closed signal that stops you from believing an
accidental no-op succeeded.

## What this does NOT do

- Migrate or touch `req_*`/`resp_*` ciphertext — only the wrapped data key.
- Update the running gateway's configured `audit.log_bodies.key_ref` — after
  rotation completes, update the config (and the resolved secret it points
  at) to the NEW key value and restart/reload the gateway, or newly captured
  bodies will keep being wrapped under the old key.
- Provide an automated/scheduled rotation — this is an operator-invoked tool.
