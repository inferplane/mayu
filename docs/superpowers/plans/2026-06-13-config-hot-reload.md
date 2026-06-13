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
WHOLE reloadable generation together — providers map, model routes, the pricing
table, and the secret-free config view — so a reader never sees a mixed
generation (router gen N+1 with pricing/view gen N). It lives in a
`live.Holder` (`atomic.Pointer[State]`). Reload builds a new `State` and does
**one** `Swap` — every consumer flips together.

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
- The circuit breaker persists for UNCHANGED providers and is **pruned** for
  providers removed (or whose type/base_url identity changed) by the reload —
  no stale open/closed state leaks to a re-added provider (P2 r1).
- `ResolveChain` does exactly ONE `Load()` and uses that snapshot for its whole
  fallback loop — no mixed-generation resolve. `RecordResult` only touches the
  shared breaker.
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

### Task 1: `internal/live` — immutable generation snapshot + atomic holder

**Files:**

- Create: `internal/live/live.go`
- Create: `internal/live/live_test.go`

**Steps:**

- [ ] Failing tests: `TestNewStateDeepCopies` (mutating the caller's providers/
      models map AFTER `NewState` does not change the published state —
      immutability); `TestHolderLoadSwap` (Load returns last Swap); `TestHolderRaceFree`
      (N goroutines Load while one loops Swap, `-race` clean, no torn reads).
- [ ] Implement `State{ providers map[string]providers.Provider; models
      map[string]config.ModelConfig; pricing *pricing.Table; view configapi.View }`
      with unexported fields + accessors; `NewState(...)` deep-copies the maps;
      `Holder{ p atomic.Pointer[State] }` with `Load() *State` / `Swap(*State)`.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 2: Router reads topology from the holder; breaker prune

Router stops owning provs/models; it reads them from the shared `*live.Holder`,
one `Load()` per `ResolveChain`. The breaker stays on the Router (persistent)
and gains identity-aware pruning.

**Files:**

- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`

**Steps:**

- [ ] Failing tests: `TestResolveChainSingleSnapshot` (a Swap mid-resolve does
      not change the chain returned by an in-progress ResolveChain — one Load);
      `TestSwapChangesResolution` (after holder Swap, new ResolveChain sees the
      new route; removed model → error); `TestBreakerPersistsForUnchangedProvider`
      (open A's breaker, swap to a gen still containing A with same identity →
      still open); `TestBreakerPrunedForRemovedProvider` (open A, swap to a gen
      without A, re-add A later → breaker closed/fresh, not stale-open).
- [ ] Refactor `New` to take `*live.Holder`; `ResolveChain`/`Resolve`/
      `ResolveProvider`/`AllModels` read `r.live.Load()` once. Add
      `RetainBreakers(identities map[string]string)` (provider name → identity
      string `type+base_url`) called by the reloader to drop stale entries.
- [ ] All four checks green. Commit (DCO sign-off).

### Task 3: Governor reads pricing from the holder; counters untouched

**Files:**

- Modify: `internal/governance/governance.go`
- Modify: `internal/governance/governance_test.go`

**Steps:**

- [ ] Failing tests: `TestGovernorPricingFromHolder` (Settle uses the holder's
      current pricing table; after a Swap to a table with a new rate, the new
      cost applies); `TestGovernorCountersSurvivePricingSwap` (spend budget,
      Swap pricing, the budget counter is unchanged — limiter/budget instances
      are not rebuilt).
- [ ] Refactor the governor to read `*pricing.Table` from `*live.Holder` (one
      Load per Settle) instead of a fixed field; limiter + budget stay as
      owned, persistent fields. PreCheck/Settle signatures unchanged.
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
      `func() configapi.View` (backed by `live.Holder.Load().View()`); update
      callers/tests.
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
- [ ] Extract `buildState(cfg) (*live.State, identities, error)` — a
      TOPOLOGY-ONLY builder (providers map + router models + pricing table +
      view) that CANNOT reach the governor/keystore/audit constructors; full
      validation (every model target → existing provider; every provider
      builds) inside it. `reload()` = `config.Load(path)` → `buildState` →
      `holder.Swap` + `router.RetainBreakers(identities)`; on any error return
      it WITHOUT swapping. `newGateway` uses `buildState` for the initial
      generation too (shared path).
- [ ] SIGHUP: a buffered channel + single-worker goroutine guarded by a reload
      mutex, started in `serve()`, stopped on ctx cancel; logs each outcome,
      never exits on reload failure. SIGINT/SIGTERM shutdown unchanged.
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
