# Plan: Config hot-reload mechanism (Stage 2 foundation)

**Date:** 2026-06-13
**Related:** ADR-005 (deferred stage 2), ADR-003 (policy-as-code), spec §4.5
(routing/breaker), §5.4 (audit)
**Base:** main @ 3156b2f
**Produces:** ADR-006

## Goal

Let the gateway pick up provider/model topology changes **without a restart**:
re-read config on `SIGHUP`, validate + rebuild the provider/router topology,
and **atomically swap** it into the live router. This is the foundation the
UI-write registration path (next plan) builds on — that path will persist to
the DB (source of truth, decided 2026-06-13) and trigger this same reload.
SIGHUP-from-file is the trigger that ships now (also a useful ops/break-glass
path on its own).

## Hard safety invariants (the whole point of the gate)

- **Reload swaps ONLY provider/model topology.** The governor's stateful stores
  (limiter rate buckets, budget µUSD counters) and the keystore and the audit
  writer are NEVER rebuilt — a reload must not reset quotas/budgets or break the
  audit chain.
- **The circuit breaker survives reload** — breaker state is keyed by provider
  name and persists across swaps (a reload must not paper over an open circuit).
- **Atomic, lock-free reads.** In-flight requests see one consistent topology
  snapshot for their whole lifetime; concurrent `ResolveChain` during a `Swap`
  is race-free under `-race`.
- **Fail-safe rollback.** A new config that fails to load/validate/build leaves
  the OLD topology serving and surfaces the error — a bad SIGHUP never takes the
  gateway down or half-applies.
- **Snapshots are immutable.** A published topology snapshot is never mutated in
  place; a swap publishes a freshly built one.
- **Secret mandate preserved.** Reload goes through `config.Load` (env/file refs
  only, inline rejected); `/admin/config` keeps showing ref names, never values.

## Non-goals (recorded in ADR-006)

- No UI-write / `POST /admin/config` (next plan).
- No DB-backed config store yet (next plan; this plan's trigger is the file).
- No governance-policy reload (rate/quota/budget limit changes) — that needs
  counter-preserving semantics and is a separate concern.
- No distributed/multi-replica reload coordination.

---

### Task 1: Router atomic topology snapshot + Swap (tidy-first, no behavior change)

Move the router's `provs`/`models` behind an immutable snapshot held in an
`atomic.Pointer`, so the topology can be replaced live while `*Router` identity
(and the breaker/metrics) stay put — handlers keep their `h.r` reference
unchanged. Pure refactor + one new method.

**Files:**

- Modify: `internal/router/router.go`
- Modify: `internal/router/router_test.go`

**Steps:**

- [ ] Write failing test `TestRouterSwapChangesResolution`: a router resolves
      model→providerA; after `Swap` to a topology routing model→providerB,
      `ResolveChain` returns providerB; a model removed by the swap resolves to
      an error.
- [ ] Write failing test `TestRouterSwapRaceFree`: spawn N goroutines calling
      `ResolveChain` while another loops `Swap`; `go test -race` clean, every
      resolve returns a consistent (non-torn) result.
- [ ] Write failing test `TestRouterSwapPreservesBreaker`: open providerA's
      breaker (record failures), `Swap` to a topology that still includes
      providerA, assert the breaker is still open (state not reset).
- [ ] Refactor: `type snapshot struct { provs map[...]; models map[...] }`;
      `Router` holds `topo atomic.Pointer[snapshot]` + the unchanged `brk`,
      `metrics`. `New` publishes the initial snapshot. `ResolveChain` /
      `Resolve` / `ResolveProvider` / `AllModels` read `r.topo.Load()`. Add
      `func (r *Router) Swap(provs, models)`.
- [ ] `go test ./... -race`; `go vet`; `gofmt -l .` clean. Commit (DCO sign-off).

### Task 2: Live config view (so /admin/config reflects reloads)

The config view is built once at boot and passed by value to `AdminMux`
(ADR-005). For reload to be visible, the handler must read the CURRENT view.
Change `configapi.Handler` to take a `func() View` provider; the gateway backs
it with an `atomic.Pointer[config.Config]` updated on reload.

**Files:**

- Modify: `internal/server/configapi/config.go`
- Modify: `internal/server/configapi/config_test.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`

**Steps:**

- [ ] Write failing test `TestHandlerReadsLiveView`: a `func() View` whose
      return value changes between two GETs is reflected in the responses
      (proves the handler doesn't capture a stale snapshot).
- [ ] Change `Handler(View)` → `Handler(func() configapi.View)`; keep the
      secret-free guarantee (still `ViewFrom`-derived).
- [ ] Update `AdminMux` signature to take `func() configapi.View`; update its
      tests (static func returning a fixed view for non-reload cases).
- [ ] `go test ./... -race` green. Commit (DCO sign-off).

### Task 3: Gateway reloader — SIGHUP, validate, atomic swap, rollback

**Files:**

- Modify: `cmd/inferplane/gateway.go`
- Modify: `cmd/inferplane/main.go`
- Create: `cmd/inferplane/reload_test.go`

**Steps:**

- [ ] Write failing test `TestReloadAppliesNewTopology`: boot gateway from a
      temp config (providerA), rewrite the config file to add a model route via
      providerB, call `g.reload()`, assert the router now resolves the new
      route AND `/admin/config` shows it.
- [ ] Write failing test `TestReloadRollsBackOnBadConfig`: after boot, rewrite
      the config to something invalid (e.g. inline `api_key`, or an unparseable
      file), call `g.reload()`, assert it returns an error, the OLD topology
      still resolves, and the gateway still serves `/healthz`.
- [ ] Write failing test `TestReloadPreservesGovernanceState`: drive a request
      that spends budget, `g.reload()` (unchanged teams), drive again — the
      budget counter is NOT reset (governor/limiter survive reload). [If wiring
      a full spend in-test is heavy, assert the governor/keystore/audit pointers
      are identical before/after reload instead.]
- [ ] Implement: store the file path + an `atomic.Pointer[config.Config]` on
      `gateway`; `reload()` = `config.Load(path)` → rebuild providers map (same
      logic as `newGateway`, extracted to a shared helper) → `router.Swap(...)`
      → update the live-cfg pointer; on any error return it WITHOUT swapping.
      `serve()` installs a `SIGHUP` handler (via `signal.Notify`) that calls
      `reload()` and logs the outcome (never exits on reload failure).
- [ ] `go test ./... -race` green; `bash tests/run-all.sh`. Commit (DCO sign-off).

### Task 4: ADR-006 + docs sync

**Files:**

- Create: `docs/decisions/ADR-006-config-hot-reload.md`
- Modify: `docs/reference/infrastructure.md`
- Modify: `docs/architecture.md`
- Modify: `internal/CLAUDE.md`
- Modify: `README.md`

**Steps:**

- [ ] ADR-006: the reload mechanism (atomic topology swap, validate-then-swap,
      rollback), the safety invariants (governance/keystore/audit untouched,
      breaker persists), the **DB-authoritative source-of-truth decision** for
      the end state with SIGHUP-from-file as today's trigger, and the non-goals.
- [ ] `infrastructure.md`: SIGHUP reload operational note (K8s: `kill -HUP 1`
      or a rollout; no restart needed for topology changes).
- [ ] `architecture.md` + `internal/CLAUDE.md`: router topology is now an
      atomic snapshot; reload boundary documented.
- [ ] README: one line — edit config + `kill -HUP <pid>` to add a provider
      without downtime; UI-write is roadmap.
- [ ] `bash tests/run-all.sh` green. Commit (DCO sign-off).
