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

**Plan-gate round 1 findings folded in (verified against crypto.go directly):**
`openEnvelope` (crypto.go:103-107) checks `len(dataKeyBytes) != 32` after unwrapping — `open`
itself does not. A `RewrapKey` that calls `open` directly and reseals whatever comes back,
without that same length check, could "successfully" rewrap an already-malformed wrapped key
(passing GCM auth but wrong-length plaintext) and count it as rotated, when it was already
broken and stays broken under the new key too — silently converting a real corruption into a
falsely-"migrated" row. `RewrapKey` must replicate `openEnvelope`'s `len == 32` check. Also:
`open`'s underlying errors vary by failure mode (bad nonce vs AEAD failure, crypto.go:59 vs
:62) — `RewrapKey` must not leak that distinction (same fail-closed-generic-error contract as
`openEnvelope`/`Fetch`→`ErrGone`), so it returns one sentinel error for any unwrap problem.

Steps:
- [ ] Add `var ErrRewrapFailed = errors.New("bodystore: rewrap failed")` (exported sentinel,
      mirrors `ErrGone`'s existing shape) to `crypto.go` or `store.go`.
- [ ] Add `func RewrapKey(oldMaster, newMaster [32]byte, nonce, ct []byte) (newNonce, newCT []byte, err error)`
      to `crypto.go`: reconstruct a `sealed{nonce: nonce, ct: ct}`, `open(oldMaster, ...)` it to
      recover the data key; if that errors **or the recovered plaintext is not exactly 32
      bytes** (the same check `openEnvelope` does), return `ErrRewrapFailed` — never the raw
      underlying error. Otherwise `seal(newMaster, dataKey)` and return its `.nonce`/`.ct`.
      Reuses the existing unexported `seal`/`open`/`sealed` — no new crypto primitive, no
      change to `sealEnvelope`/`openEnvelope`/`ParseMasterKey`.
- [ ] Doc-comment `RewrapKey` explaining it operates on the wrapped-key columns only — the
      request/response ciphertext (`req_*`/`resp_*`) is never touched, per the ADR — and that
      it never distinguishes wrong-key from tampered-key from malformed-length (same
      fail-closed posture as `Fetch`/`ErrGone`).
- [ ] Test (mirrors `crypto_test.go`'s `testKey(t, seed)` helper and
      `TestOpenEnvelope_WrongMasterKeyFailsClosed`'s two-master-key shape):
      `TestRewrapKey_RoundTrip` — seal an envelope with `testKey(t,1)` (old), call
      `RewrapKey(testKey(t,1), testKey(t,2), env.wrappedKey.nonce, env.wrappedKey.ct)`,
      then confirm `openEnvelope(testKey(t,2), envelope{wrappedKey: sealed{new nonce/ct}, ...})`
      succeeds (recovers the SAME req/resp bytes) while the old key no longer opens it.
      `TestRewrapKey_WrongOldKeyFails` — call with a master key that never wrapped this row;
      `RewrapKey` returns `ErrRewrapFailed` (never a corrupted-but-succeeding rewrap).
      `TestRewrapKey_MalformedDataKeyLengthFails` (pins the round-1 CRITICAL fix, plan-gate
      round-2 NIT): `seal(testKey(t,1), []byte("not-32-bytes"))` directly (bypassing
      `sealEnvelope`, to construct a wrapped-key blob whose plaintext is authentic under
      `testKey(t,1)` but the wrong length) → `RewrapKey(testKey(t,1), testKey(t,2), that
      sealed's nonce/ct)` must return `ErrRewrapFailed`, not a "successful" reseal of the
      malformed bytes.

### Task 2: `Store` gains row-iteration + wrapped-key update

**Files:**
- Modify: `internal/bodystore/store.go`
- Modify: `internal/bodystore/sqlite.go`
- Modify: `internal/bodystore/postgres.go`
- Test: `internal/bodystore/backend_test.go`

**Plan-gate round 1 finding folded in:** the fleet can be live during a rotation run (ADR-018
§1's own design: every replica writes independently, no coordination) — a plain `UPDATE ...
WHERE ref=?` after a separate `List` can silently affect a row that a concurrent `Purge`/erase
already removed, or double-apply against a row a second rotation run already touched, with no
signal back to the caller. Fix: make the update a **compare-and-swap** keyed on the OLD wrapped
bytes, and report whether it actually matched — no fleet-wide quiesce required (matches the
ADR's own no-coordination philosophy), just an honest per-row success signal.

Steps:
- [ ] Add to the `Store` interface (`store.go`): `ListWrappedKeys(ctx context.Context)
      ([]WrappedKeyRow, error)` and `UpdateWrappedKey(ctx context.Context, ref string,
      oldNonce, oldCT, newNonce, newCT []byte) (matched bool, err error)`. Define
      `WrappedKeyRow struct { Ref string; Nonce, CT []byte }` next to the interface (a minimal
      projection — never carries `req_*`/`resp_*`, so a rotation pass never needs to touch or
      hold the actual body ciphertext in memory). `matched=false, err=nil` means the row's
      wrapped-key bytes had already changed since `ListWrappedKeys` (purged, or rewrapped by a
      concurrent run) — not an error, just "nothing to do here anymore."
- [ ] `sqlite.go`: implement both — `ListWrappedKeys` is `SELECT ref, wrapped_key_nonce,
      wrapped_key_ct FROM bodies` (no pagination needed at this store's bounded size —
      `max_bytes`/TTL already cap total rows); `UpdateWrappedKey` is `UPDATE bodies SET
      wrapped_key_nonce=?, wrapped_key_ct=? WHERE ref=? AND wrapped_key_nonce=? AND
      wrapped_key_ct=?`, then check `RowsAffected()` — `>0` → `matched=true`.
- [ ] `postgres.go`: same two methods + the same CAS `WHERE` clause and `RowsAffected` check,
      Postgres placeholder syntax (`$1`/`$2`/...), mirroring the existing sqlite/postgres
      method-pairing pattern already used for `Put`/`Get`/`Delete`/`Purge` in this file.
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

**Plan-gate round 1 finding folded in (verified against `audit.go` directly):** `auditCmd`
(audit.go:13-44) has only ONE verb, `verify` — there is no `anchor` sub-verb at this dispatch
level (the earlier draft's "verify/anchor" precedent was wrong; corrected here). Its exit-1
case (`audit.go:42-43`, "chain BROKEN") is a **completed run reporting a real problem**, not a
Go-error/IO-error case (that's exit 1 too, at `audit.go:29-30,35-36` — the two meanings share
the same code, distinguished only by the printed message) — this IS a valid precedent for
`bodies rewrap-key` returning exit 1 on "0 rewrapped, N skipped" (see below), contrary to the
plan-gate's earlier claim that this would be *inconsistent* with `auditCmd`.

**Second plan-gate finding folded in:** returning exit 0 whenever the run merely *completes*
(regardless of how many rows were skipped) is fail-open for the one failure mode that matters
most: an operator passing the WRONG `--old-key-env` gets 0 rewrapped, N skipped, exit 0 — and
walks away believing rotation succeeded, while every row is still only readable by the (never
actually retired) old key. Fix: exit 1 when the store was non-empty AND zero rows were
rewrapped (nothing matched the given old key at all).

**Round-2 correction (plan-gate round 2, opus + codex, both confirmed against the actual
code):** the round-1 draft above justified this by claiming a re-run against an
already-fully-rotated store would harmlessly report "0 rewrapped/0 skipped, no rows to
visit." That is factually wrong and contradicts this plan's OWN Out-of-scope note: rewrap
never deletes rows (`UpdateWrappedKey` only overwrites `wrapped_key_*`), and there is
deliberately no key-version column, so re-running the exact same `oldKey→newKey` command
against a store that `oldKey` already fully rotated OUT of will find every row still present,
try to unwrap each with `oldKey`, fail every time (they're now wrapped with `newKey`), and hit
the identical `rewrapped==0 && skipped>0 → exit 1` path as a genuinely wrong key. **This is not
a bug to fix — it is the same "cannot distinguish wrong-key from already-rotated" limitation
already stated in Out-of-scope, and exit 1 is still the correct, honest signal for
either case** (staying silent/exit-0 on either would be the actual fail-open risk). The
runbook (below) must say so explicitly rather than promising a harmless 0/0 re-run: an
operator who sees `rewrapped=0 skipped=N exit=1` on a re-run should read it as "nothing
needed doing here" if they're confident about the key, not as a hard failure to alarm on. A
run with SOME rewrapped and some skipped (the "resuming an interrupted prior rotation" case)
still exits 0 — partial progress on a plausible key is not the failure mode being guarded
against.

Steps:
- [ ] `main.go`: add a `"bodies"` case to the subcommand dispatch (mirrors the `"audit"` case
      — `os.Exit(bodiesCmd(os.Args[2:]))`), and one line to `usage()`.
- [ ] `cmd/inferplane/bodies.go`: `func bodiesCmd(args []string) int`. Sub-dispatches on
      `args[0]` — only one verb today, `"rewrap-key"` (mirrors `auditCmd` having exactly one
      verb, `verify`). Unknown/missing verb → print usage to stderr, return 2 (usage error,
      matching `audit.go`'s convention).
- [ ] `rewrap-key` flags (`flag.NewFlagSet("bodies rewrap-key", flag.ContinueOnError)`):
      `--store <path>` (SQLite file) OR `--postgres-dsn-env <VAR>` (mutually exclusive,
      mirrors `keys.go`'s `--store` flag naming; the Postgres branch resolves the DSN via
      `config.SecretRef{Env: *dsnEnv}` + `config.ValidateSecretRef`/`ResolveSecretRef`, same
      as the two key refs below — reject an empty resolved value); `--old-key-env <VAR>` /
      `--old-key-file <path>` (exactly one); `--new-key-env <VAR>` / `--new-key-file <path>`
      (exactly one). Missing/conflicting flags → usage message, exit 2.
- [ ] Resolve both key refs via `config.ValidateSecretRef` + `config.ResolveSecretRef` (a
      `config.SecretRef{Env: ...}` or `{File: ...}` built from the flags) then
      `bodystore.ParseMasterKey` on each resolved hex string — reusing existing exported
      functions, zero changes to `config`. An invalid ref shape or a `ParseMasterKey` error
      (not 64 hex chars) → print the error, exit 2 (usage/input error, not a runtime error).
- [ ] Open the store (`bodystore.OpenSQLite(path)` or the Postgres constructor; open failure →
      exit 1, matching the ADR's boot-fatal-on-open-failure posture for this store), call
      `ListWrappedKeys`, and for each row: `bodystore.RewrapKey(oldMaster, newMaster,
      row.Nonce, row.CT)` — on success, `UpdateWrappedKey(ctx, row.Ref, row.Nonce, row.CT,
      newNonce, newCT)` (the CAS from Task 2); if `matched`, count **rewrapped**; if
      `!matched`, count **raced** (row changed concurrently since `ListWrappedKeys` — log it,
      don't treat as an error: a follow-up run catches it). On `RewrapKey` returning
      `ErrRewrapFailed` (old key does not open this row), count it **skipped** (do NOT abort
      the run — one row wrapped by an unrelated key must not block the rest). Print a final
      summary line (`rewrapped=N skipped=M raced=K`). Exit 1 if `N==0 && M>0` (see finding
      above — nothing matched the given old key at all); otherwise exit 0.
- [ ] Test (`bodies_test.go`, mirrors `cmd/inferplane`'s existing CLI test style for
      `auditCmd`/`keysCmd`), using ONLY exported `bodystore` API (no new test-only exports
      needed — `NewRecorder`/`Capture`/`Close`/`Fetch` are already exported, `store.go:92,`
      `106,159,~171`): seed a real encrypted row via `bodystore.NewRecorder(store, oldMaster,
      ttl, maxBytes)` → `.Capture(recordID, team, req, resp)` → `.Close()` (drains the worker
      synchronously, so the row is durably written before the CLI runs). Run
      `bodiesCmd(["rewrap-key", ...])` with `t.Setenv` for both key env vars pointing at
      old/new hex keys; assert exit code **0** (rewrapped=1). Then black-box-verify via the
      SAME exported API: `bodystore.NewRecorder(store, newMaster, ...).Fetch(ctx, ref)` must
      now succeed and return the original req/resp bytes; a separate
      `bodystore.NewRecorder(store, oldMaster, ...).Fetch(ctx, ref)` must now fail (`ErrGone`)
      — proving the rewrap actually happened, not just "ran without error."
      `TestBodiesRewrapKey_WrongOldKeyFailsClosed`: run the CLI with a WRONG `--old-key-env`
      value (a key that never wrapped this row) → assert exit code **1** (the fail-open fix —
      0 rewrapped, 1 skipped), and confirm via `Fetch` with the REAL old key that the row was
      left byte-for-byte untouched (correctly skipped, not corrupted).

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-018-opt-in-body-logging.md`: strike the "Key-rotation rewrap CLI"
  Deferred bullet, replace with an "implemented" note naming the `bodies rewrap-key`
  subcommand and this plan doc. Leave the "Response-body PII masking" bullet untouched
  (still genuinely deferred, out of scope here as stated above).
- `docs/reference/data.md`: add one clause to the existing "Body store"/"Body 스토어" row
  noting the `bodies rewrap-key` CLI rotates `wrapped_key_*` without touching `req_*`/`resp_*`.
- `docs/runbooks/body-key-rotation.md` (new): operator-facing procedure — when to rotate,
  the exact command invocation, what "skipped"/"raced" in the summary output mean, the
  explicit non-goal (this does not migrate `req_*`/`resp_*` ciphertext, only the wrap), and
  the round-2-corrected exit-code note: `exit 1` with `rewrapped=0` means "the given old key
  opened nothing in this store" — either the key is wrong, or this store was already fully
  rotated to what you're calling the new key; the two are indistinguishable by design
  (no key-version column). Re-running the exact same command a second time is expected to
  also exit 1 once rotation has genuinely completed — that is not a failure to page on.

## Out of scope

- Response-body PII masking (ADR-018's other Deferred bullet) — needs a new response-filter
  seam design, materially larger than this bounded CLI; left for a future round.
- A key-version/master-key-id column — would let rotation distinguish "already rotated" from
  "wrong key" with certainty; a schema migration, which the ADR explicitly did not call for
  ("format is fixed now"). The skip-on-unwrap-failure heuristic is the deliberate trade-off.
- Any change to `sealEnvelope`/`openEnvelope`/the request/response ciphertext format.
- Automating rotation (a cron/scheduler) — this is an operator-invoked CLI, matching the
  ADR's own "manual procedure" framing.
