# Plan: Config hot-reload mechanism (Stage 2 foundation)

**Date:** 2026-06-13
**Related:** ADR-005 (deferred stage 2), ADR-003 (policy-as-code), spec §4.5, §5.4
**Base:** main @ 3156b2f · **Produces:** ADR-006

## Goal

Pick up **provider / model / pricing** changes without a restart: on `SIGHUP`,
re-read config, validate + build a fresh immutable topology snapshot, and
**atomically publish it as a single generation**. Foundation for the UI-write
path (next plan), whose source of truth is the **DB** (decided 2026-06-13);
SIGHUP-from-file is the trigger that ships now.

## Core architecture (rewritten after P2 round-1 — both panels)

**One immutable snapshot, one atomic pointer.** A `live.State` value holds the
reloadable generation — providers map, model routes, and the pricing table —
in a `live.Holder` (`atomic.Pointer[State]`). Reload builds a new `State` and
does **one** `Swap`. `live` imports only `config`, `providers`, `pricing` — it
does **NOT** import `configapi` (the secret-free view is DERIVED by the
assembly layer from `state.Providers()/Models()`, so `live` stays clean) and
nothing in `internal/governance` imports `live` (leaf-package mandate — see
below).

**Request-scoped consistency (P2 r2 CRITICAL, both panels).** A request must
resolve AND bill on the SAME generation. `ResolveChain` does the single
`Load()` and RETURNS the `*live.State` it used; the handler threads that
snapshot's pricing table into `Settle`. (Confirmed in code: `PreCheck` does NOT
price — it takes `estimateTokens` and checks rate/quota/budget; only `Settle`
prices. So the only intra-request pricing read is `Settle`, which now uses the
request's snapshot, not a fresh `Load`.)

**Leaf-package boundary preserved (P2 r2 CRITICAL, both panels).**
`internal/governance` is a mandate-defined leaf that must not import `config`
or `server`. So the governor does NOT read `live`/the holder. Instead
`Settle(team, provider, model, usage, table *pricing.Table)` takes the table as
a parameter (governance already imports `pricing`); the handler passes the
table from the snapshot it resolved against. `PreCheck` is unchanged.

**Stateful components live OUTSIDE the snapshot and are never rebuilt by
reload:** the governor's limiter (rate buckets) and budget (µUSD counters), the
keystore (SQLite), the audit writer (hash chain + WAL), and the router's
circuit breaker. Reload touches none of their constructors.

**Immutability is enforced:** `live.NewState` deep-copies the caller's maps
into private fields; nothing mutates a published `State`. Readers `Load()` once
and use that pointer for the whole operation.

## Hard safety invariants (the gate's checklist)

- Reload swaps ONLY the `live.State` (providers/models/pricing/view). Governor
  counters, keystore, audit chain are byte-for-byte continuous across reload.
- **Pricing swaps WITH topology** — adding a provider+route+rate in one config
  edit takes effect atomically; a route can never bill at stale/0 pricing
  because of a half-applied reload (P2 r1 CRITICAL, both panels).
- The circuit breaker is **keyed by provider identity** (`type+base_url`), not
  name alone, so a removed-then-re-added provider (or one whose endpoint
  changed) gets a FRESH breaker, never stale open/closed state. `RetainBreakers`
  drops identities absent from the new generation; all breaker ops (`Allow`,
  `RecordResult`, `RetainBreakers`) are guarded by the breaker's own mutex, and
  `RecordResult` for an identity not present is a silent no-op (never
  auto-recreates a pruned entry) (P2 r1+r2, both panels).
- `ResolveChain` does exactly ONE `Load()`, uses that snapshot for its whole
  fallback loop, and RETURNS it so the handler bills the same generation.
- `live.NewState` deep-copies nested mutable data (the `models` map AND each
  `ModelConfig.Targets` slice), and accessors never leak mutable internals
  (return copies) — a published `State` is frozen (P2 r2). `providers.Provider`
  interface values are shared by reference (correct — providers are
  concurrency-safe and identity-stable within a generation).
- **Validate-then-swap:** the new generation is fully validated offline —
  config loads AND every model target references a provider that exists in the
  new providers map AND every provider builds — BEFORE the swap. Any failure
  returns an error and leaves the old generation serving (fail-safe rollback).
- SIGHUP is handled by a single-worker goroutine over a buffered channel with a
  reload mutex — concurrent/overlapping SIGHUPs serialize; the signal path does
  no heavy work inline; SIGINT/SIGTERM shutdown is unaffected.
- Secret mandate preserved: reload goes through `config.Load` (env/file refs,
  inline rejected); the view stays ref-name-only.
- Every task ends green on ALL four checks: `CGO_ENABLED=0 go build ./...`,
  `go test ./... -race`, `go vet ./... && gofmt -l .`, `bash tests/run-all.sh`.

## Non-goals (ADR-006)

- No UI-write / `POST /admin/config`, no DB store yet (next plan; trigger here
  is the file via SIGHUP).
- **No governance-POLICY reload** (team rate/quota/budget LIMIT changes) — that
  interacts with live counters and needs counter-preserving semantics; team
  limit changes still require a restart. (Pricing IS reloaded; it is a stateless
  lookup table, distinct from the stateful policy counters.)
- No distributed/multi-replica reload coordination.

---

### Task 1: `internal/live` — immutable generation snapshot + holder + builder

`internal/live` is also the **topology-only builder** boundary: it imports only
`config`, `providers`, `pricing` (+ stdlib) — never `governance`, `keystore`,
`audit`, or `configapi`. An import-guard test makes that boundary STRUCTURAL,
not conventional (P2 r2).

**Files:**

- Create: `internal/live/live.go`
- Create: `internal/live/live_test.go`

**Steps:**

- [ ] Failing tests: `TestNewStateDeepCopies` (mutating the caller's `models`
      map AND a `ModelConfig.Targets` slice AFTER `NewState` does not change the
      published state; an accessor's returned value cannot mutate internals);
      `TestHolderLoadSwap`; `TestHolderRaceFree` (N goroutines Load while one
      loops Swap, `-race` clean); `TestBuildStateValidatesRoutes` (a config
      whose model target names a missing provider → build error, no State);
      `TestLiveImportsAreLeafSafe` (parse this package's imports via
      `go/parser`; assert none match governance/keystore/audit/server/configapi).
- [ ] Implement `State{ providers map; models map; pricing *pricing.Table }`
      (unexported) + accessors returning copies; `NewState` deep-copies maps and
      `Targets` slices; `Holder{ p atomic.Pointer[State] }` (Load/Swap);
      `BuildState(cfg *config.Config) (st *State, identities map[string]string, err error)`
      — builds the providers map (the bedrock-settings logic moved from
      gateway.go), the pricing table, validates every model target → existing
      provider, computes identities (`type+base_url`); CANNOT reach stateful
      constructors.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 2: Router reads topology from the holder; breaker prune

Router stops owning provs/models; it reads them from the shared `*live.Holder`,
one `Load()` per `ResolveChain`. The breaker stays on the Router (persistent)
and gains identity-aware pruning.

**Files:**

- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`

**Steps:**

- [ ] Failing tests: `TestResolveChainReturnsSnapshot` (ResolveChain returns the
      `*live.State` it Loaded; a Swap mid-loop does not change that call's
      result); `TestSwapChangesResolution` (after Swap, a NEW ResolveChain sees
      the new route; removed model → error); `TestBreakerKeyedByIdentity` (open
      A@urlX, swap to A@urlY → fresh breaker, not stale-open);
      `TestRetainBreakersDropsRemoved` + `TestRecordResultIgnoresUnknown` (after
      a provider is pruned, an in-flight `RecordResult` for it is a no-op, does
      not recreate the entry); `TestBreakerOpsRaceFree` (RecordResult +
      RetainBreakers concurrent, `-race` clean).
- [ ] Refactor `New(*live.Holder)`; `ResolveChain` Loads once and returns
      `([]ChainTarget, *live.State, error)`; `Resolve`/`ResolveProvider`/
      `AllModels` Load once each. Breaker keyed by identity (`type+base_url`),
      all ops mutex-guarded; add `RetainBreakers(identities map[string]string)`
      (name→identity) dropping absent identities; `RecordResult` no-ops for an
      identity not currently present.
- [ ] Update the two ingress handlers (`anthropicapi/messages.go`,
      `openaiapi/chat.go`) for the new `ResolveChain` return arity (thread the
      snapshot to Settle — Task 3).
- [ ] All four checks green. Commit (DCO sign-off).

### Task 3: Governor reads pricing from the holder; counters untouched

`internal/governance` MUST stay a leaf (no `live`/`config`/`server` import).
So Settle takes the pricing table as a PARAMETER (governance already imports
`pricing`); the handler passes the table from its resolved snapshot. This
fixes both the leaf-boundary violation AND request-scoped pricing consistency
(P2 r2 CRITICAL, both panels). `PreCheck` is unchanged (it does not price).

**Files:**

- Modify: `internal/governance/governance.go`
- Modify: `internal/governance/governance_test.go`
- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/openaiapi/chat.go`

**Steps:**

- [ ] Failing tests: `TestSettleUsesPassedTable` (Settle bills with the table
      argument, not a stored one; two different tables → two different costs);
      `TestGovernorCountersIndependentOfTable` (the limiter/budget instances are
      not affected by which table is passed — counters are the governor's own
      persistent fields). Verify `internal/governance` imports do NOT include
      `live`/`config`/`server` (guard test or manual assertion).
- [ ] Change `Settle(team, provider, model string, u pricing.Usage)` →
      `Settle(team, provider, model string, u pricing.Usage, table *pricing.Table)`;
      drop the stored `price` field. Handlers pass `snapshot.Pricing()` (the
      `*live.State` from ResolveChain) into Settle.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 4: Live config view + AdminMux wiring

**Files:**

- Modify: `internal/server/configapi/config.go`
- Modify: `internal/server/configapi/config_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Steps:**

- [ ] Failing tests: `TestHandlerReadsLiveView` (a `func() View` whose result
      changes between GETs is reflected — no stale capture); existing
      secret-free assertions still hold.
- [ ] `Handler(View)` → `Handler(func() configapi.View)`; `AdminMux` takes
      `func() configapi.View`. The assembly layer backs it with
      `func() View { st := holder.Load(); return configapi.ViewFrom(st.Providers(), st.Models()) }`
      — so `live` never imports `configapi`. Update callers/tests.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 5: Gateway reloader — topology-only builder, SIGHUP worker, rollback

**Files:**

- Modify: `cmd/inferplane/gateway.go`
- Modify: `cmd/inferplane/main.go`
- Create: `cmd/inferplane/reload_test.go`

**Steps:**

- [ ] Failing tests: `TestReloadAppliesNewGeneration` (boot with providerA;
      rewrite config to add a route via providerB + its pricing; `reload()`;
      router resolves the new route, Settle uses the new rate, `/admin/config`
      shows it — all consistent); `TestReloadRollsBackOnBadConfig` (invalid
      config — inline api_key / unparseable / route→missing provider — `reload()`
      errors, OLD generation still serves, `/healthz` ok);
      `TestReloadPreservesGovernanceAndAudit` (governor/limiter/budget/keystore/
      audit pointers identical before/after reload; a pre-reload budget spend is
      still counted after); `TestConcurrentReloadsSerialize` (two `reload()`
      calls in parallel do not race under `-race`; final state is one of the
      two, never torn).
- [ ] `reload()` = `config.Load(path)` → `live.BuildState(cfg)` (Task 1; the
      topology-only builder, full validation inside) → `holder.Swap(st)` +
      `router.RetainBreakers(identities)`; on any error return it WITHOUT
      swapping (old generation keeps serving). `newGateway` uses
      `live.BuildState` for the initial generation too (shared path — no
      duplicated provider-build logic).
- [ ] SIGHUP worker lifecycle (P2 r2): buffered channel (`signal.Notify`),
      ONE worker goroutine guarded by a reload mutex, started in `serve()`;
      `signal.Stop` + worker drains and exits before `serve()` returns; no
      double-start across serve/test lifecycles; a ctx cancel during an active
      reload lets the in-flight reload finish then exits. Each outcome logged;
      reload failure never exits the process. SIGINT/SIGTERM shutdown unchanged.
- [ ] Tests also cover: `TestSighupTriggersReload` (send SIGHUP to the worker
      channel, assert swap) and `TestWorkerStopsOnCtxCancel` (no goroutine leak).
- [ ] All four checks green. Commit (DCO sign-off).

### Task 6: ADR-006 + docs sync

**Files:**

- Create: `docs/decisions/ADR-006-config-hot-reload.md`
- Modify: `docs/reference/infrastructure.md`
- Modify: `docs/architecture.md`
- Modify: `internal/CLAUDE.md`
- Modify: `README.md`

**Steps:**

- [ ] ADR-006: single-generation `live.State` + atomic swap; the safety
      invariants (counters/keystore/audit continuous, pricing swaps with
      topology, breaker prune); validate-then-swap rollback; serialized SIGHUP
      worker; the **DB-authoritative source-of-truth** end state with
      SIGHUP-from-file as today's trigger; non-goals (no policy reload).
- [ ] `infrastructure.md`: `kill -HUP <pid>` reload note (no restart for
      provider/model/pricing changes; team-limit changes still need restart).
- [ ] `architecture.md` + `internal/CLAUDE.md`: `internal/live` generation
      snapshot; what reload does / does not touch.
- [ ] README: one line on hot-add via config edit + SIGHUP; UI-write is roadmap.
- [ ] All four checks green. Commit (DCO sign-off).
