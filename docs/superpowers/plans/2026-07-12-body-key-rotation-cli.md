# body-store key-rotation rewrap CLI (ADR-018 deferred item)

**Date:** 2026-07-12
**Source:** ADR-018 "Deferred" section — "Key-rotation rewrap CLI (format fixed now; manual
procedure documented above)." The ADR's own §3 already specifies the procedure exactly:
"Rotation = rewrap the data keys, never touch the (potentially large) body ciphertext...
for each row, unwrap the data key with the old master key, reseal it with the new one,
`UPDATE` only the `wrapped_key_*` columns." This plan implements that procedure as a CLI
subcommand. **Response-body PII masking** (ADR-018's other Deferred item) is explicitly
OUT OF SCOPE here — it requires designing a new response-filter seam (none exists today;
ADR-018 §6 states this plainly), a materially larger architectural change than a bounded
rewrap CLI. Left for its own future round.

**Scope finding (from direct code read):** `internal/bodystore`'s envelope crypto
(`crypto.go`) is entirely unexported (`seal`/`open`/`sealEnvelope`/`openEnvelope`,
`sealed`/`envelope` types) — only `ParseMasterKey` is exported. `Store` (`store.go`) has
no row-iteration or wrapped-key-update method (`Put`/`Get`/`Delete`/`Purge`/`Close` only).
Both backends (`sqlite.go`, `postgres.go`) store the wrapped key as two columns:
`wrapped_key_nonce`, `wrapped_key_ct` (confirmed identical names in both `CREATE TABLE`
DDLs) — this is the ADR's "`wrapped_key_*`" reference.

**No key-version column exists** in either schema — a row does not record which master
key wrapped it. Adding one would be a schema migration beyond this plan's bounded scope
(the ADR says "format is fixed now"). The rewrap CLI therefore attempts unwrap-with-old-key
on every row; a row that fails to unwrap is reported as **skipped** (already rotated, or a
different key entirely) rather than treated as an error — consistent with the bodystore's
existing fail-closed-without-distinguishing-why philosophy (`Fetch`'s `ErrGone`).

**CLI dispatch precedent (`cmd/inferplane/main.go`, `audit.go`):** two exit-code
conventions coexist; `auditCmd` returns an `int` directly (not `error`) so it can express
"ran, but N rows had a problem" separately from a usage error — this plan's `bodiesCmd`
follows that shape, since "X rewrapped, Y skipped" is exactly this kind of outcome, not a
binary success/failure.

**Secret-ref resolution (`internal/config/config.go`):** `ResolveSecretRef(ref *SecretRef)
(string, error)` is free-standing — it does not read from `config.Config`, so the CLI can
resolve two independent key refs (old, new) by constructing two `config.SecretRef` values
from its own flags, with zero changes to the `config` package. `ValidateSecretRef` guards
shape before resolving, matching every other secret-ref call site in the repo (§7).

## Task list

### Task 1: exported `RewrapKey` in `internal/bodystore`

**Files:**
- Modify: `internal/bodystore/crypto.go`
- Test: `internal/bodystore/rewrap_test.go`

Steps:
- [ ] Add `func RewrapKey(oldMaster, newMaster [32]byte, nonce, ct []byte) (newNonce, newCT []byte, err error)`
      to `crypto.go`: reconstruct a `sealed{nonce: nonce, ct: ct}`, `open(oldMaster, ...)` it
      to recover the data key, then `seal(newMaster, dataKey)` and return its `.nonce`/`.ct`.
      Reuses the existing unexported `seal`/`open`/`sealed` — no new crypto primitive, no
      change to `sealEnvelope`/`openEnvelope`/`ParseMasterKey`.
- [ ] Doc-comment `RewrapKey` explaining it operates on the wrapped-key columns only — the
      request/response ciphertext (`req_*`/`resp_*`) is never touched, per the ADR.
- [ ] Test (mirrors `crypto_test.go`'s `testKey(t, seed)` helper and
      `TestOpenEnvelope_WrongMasterKeyFailsClosed`'s two-master-key shape):
      `TestRewrapKey_RoundTrip` — seal an envelope with `testKey(t,1)` (old), call
      `RewrapKey(testKey(t,1), testKey(t,2), env.wrappedKey.nonce, env.wrappedKey.ct)`,
      then confirm `openEnvelope(testKey(t,2), envelope{wrappedKey: sealed{new nonce/ct}, ...})`
      succeeds (recovers the SAME req/resp bytes) while the old key no longer opens it.
      `TestRewrapKey_WrongOldKeyFails` — call with a master key that never wrapped this row;
      `RewrapKey` returns an error (never a corrupted-but-succeeding rewrap).

### Task 2: `Store` gains row-iteration + wrapped-key update

**Files:**
- Modify: `internal/bodystore/store.go`
- Modify: `internal/bodystore/sqlite.go`
- Modify: `internal/bodystore/postgres.go`
- Test: `internal/bodystore/backend_test.go`

Steps:
- [ ] Add to the `Store` interface (`store.go`): `ListWrappedKeys(ctx context.Context)
      ([]WrappedKeyRow, error)` and `UpdateWrappedKey(ctx context.Context, ref string,
      nonce, ct []byte) error`. Define `WrappedKeyRow struct { Ref string; Nonce, CT []byte }`
      next to the interface (a minimal projection — never carries `req_*`/`resp_*`, so a
      rotation pass never needs to touch or hold the actual body ciphertext in memory).
- [ ] `sqlite.go`: implement both — `ListWrappedKeys` is `SELECT ref, wrapped_key_nonce,
      wrapped_key_ct FROM bodies` (no pagination needed at this store's bounded size —
      `max_bytes`/TTL already cap total rows); `UpdateWrappedKey` is `UPDATE bodies SET
      wrapped_key_nonce=?, wrapped_key_ct=? WHERE ref=?`.
- [ ] `postgres.go`: same two methods, Postgres placeholder syntax (`$1`/`$2`/...), mirroring
      the existing sqlite/postgres method-pairing pattern already used for
      `Put`/`Get`/`Delete`/`Purge` in this file.
- [ ] Test (extends `backend_test.go`'s existing `backends(t) map[string]Store` loop — SQLite
      always, Postgres gated on `INFERPLANE_TEST_PG_DSN`, same as every other backend test in
      this file): `TestStore_ListAndUpdateWrappedKey` — `Put` a row via `testRow(...)` (its
      placeholder `wknonce`/`wkct` bytes are fine here, this test is about column plumbing,
      not crypto), `ListWrappedKeys` returns it with matching bytes, `UpdateWrappedKey` with
      new bytes, `ListWrappedKeys` again reflects the update — for every backend in the loop.

### Task 3: `bodies rewrap-key` CLI subcommand

**Files:**
- Create: `cmd/inferplane/bodies.go`
- Modify: `cmd/inferplane/main.go`
- Test: `cmd/inferplane/bodies_test.go`

Steps:
- [ ] `main.go`: add a `"bodies"` case to the subcommand dispatch (mirrors the `"audit"` case
      — `os.Exit(bodiesCmd(os.Args[2:]))`), and one line to `usage()`.
- [ ] `cmd/inferplane/bodies.go`: `func bodiesCmd(args []string) int`. Sub-dispatches on
      `args[0]` — only one verb today, `"rewrap-key"` (mirrors `auditCmd`'s own verb dispatch
      onto `verify`/`anchor`). Unknown/missing verb → print usage to stderr, return 2 (usage
      error, matching `audit.go`'s convention).
- [ ] `rewrap-key` flags (`flag.NewFlagSet("bodies rewrap-key", flag.ContinueOnError)`):
      `--store <path>` (SQLite file) OR `--postgres-dsn-env <VAR>` (mutually exclusive,
      mirrors `keys.go`'s `--store` flag plus the config-level sqlite/postgres choice);
      `--old-key-env <VAR>` / `--old-key-file <path>` (exactly one); `--new-key-env <VAR>` /
      `--new-key-file <path>` (exactly one). Missing/conflicting flags → usage message, exit 2.
- [ ] Resolve both key refs via `config.ValidateSecretRef` + `config.ResolveSecretRef` (a
      `config.SecretRef{Env: ...}` or `{File: ...}` built from the flags) then
      `bodystore.ParseMasterKey` on each resolved hex string — reusing existing exported
      functions, zero changes to `config`.
- [ ] Open the store (`bodystore.OpenSQLite(path)` or the Postgres constructor), call
      `ListWrappedKeys`, and for each row: `bodystore.RewrapKey(oldMaster, newMaster,
      row.Nonce, row.CT)` — on success, `UpdateWrappedKey` with the new bytes and count it
      rewrapped; on error (old key does not open this row), count it **skipped** (do NOT
      abort the run — a partially-rotated store from an interrupted prior run, or a row
      wrapped by an unrelated key, must not block the rest). Print a final summary line
      (`rewrapped=N skipped=M`) and return 0 if the run completed (skips are not a failure
      exit — only an open/flag/DB error is), matching the ADR's "the rewrap CLI itself is
      deferred (format is fixed now; the procedure is..." framing as an operational tool,
      not a strict all-or-nothing migration.
- [ ] Test (`bodies_test.go`, mirrors `cmd/inferplane`'s existing CLI test style for
      `auditCmd`/`keysCmd`), using ONLY exported `bodystore` API (no new test-only exports
      needed — `NewRecorder`/`Capture`/`Close`/`Fetch` are already exported, `store.go:92,106,159,170`):
      seed a real encrypted row via `bodystore.NewRecorder(store, oldMaster, ttl, maxBytes)`
      → `.Capture(recordID, team, req, resp)` → `.Close()` (drains the worker synchronously,
      so the row is durably written before the CLI runs). Run `bodiesCmd(["rewrap-key", ...])`
      with `t.Setenv` for both key env vars pointing at old/new hex keys; assert exit code 0.
      Then black-box-verify via the SAME exported API: `bodystore.NewRecorder(store,
      newMaster, ...).Fetch(ctx, ref)` must now succeed and return the original req/resp
      bytes; `bodystore.NewRecorder(store, oldMaster, ...).Fetch(ctx, ref)` must now fail
      (`ErrGone`) — proving the rewrap actually happened, not just "ran without error."
      A second test: run the CLI with a WRONG `--old-key-env` value (a key that never
      wrapped this row) → exit code still 0 (skips are not failures), but `Fetch` with the
      REAL old key still succeeds afterward (the row was left untouched, correctly skipped).

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-018-opt-in-body-logging.md`: strike the "Key-rotation rewrap CLI"
  Deferred bullet, replace with an "implemented" note naming the `bodies rewrap-key`
  subcommand and this plan doc. Leave the "Response-body PII masking" bullet untouched
  (still genuinely deferred, out of scope here as stated above).
- `docs/reference/data.md`: add one clause to the existing "Body store"/"Body 스토어" row
  noting the `bodies rewrap-key` CLI rotates `wrapped_key_*` without touching `req_*`/`resp_*`.
- `docs/runbooks/body-key-rotation.md` (new): operator-facing procedure — when to rotate,
  the exact command invocation, what "skipped" in the summary output means, and the
  explicit non-goal (this does not migrate `req_*`/`resp_*` ciphertext, only the wrap).

## Out of scope

- Response-body PII masking (ADR-018's other Deferred bullet) — needs a new response-filter
  seam design, materially larger than this bounded CLI; left for a future round.
- A key-version/master-key-id column — would let rotation distinguish "already rotated" from
  "wrong key" with certainty; a schema migration, which the ADR explicitly did not call for
  ("format is fixed now"). The skip-on-unwrap-failure heuristic is the deliberate trade-off.
- Any change to `sealEnvelope`/`openEnvelope`/the request/response ciphertext format.
- Automating rotation (a cron/scheduler) — this is an operator-invoked CLI, matching the
  ADR's own "manual procedure" framing.
