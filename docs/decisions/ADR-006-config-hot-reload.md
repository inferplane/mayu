# ADR-006: Config hot-reload via a single atomic topology generation

**Date:** 2026-06-13
**Status:** Accepted
**Deciders:** maintainers; 2-round multi-model design gate (codex gpt-5.5,
gemini 3.1-pro) — 19 design findings (incl. a billing-correctness CRITICAL and
a leaf-boundary CRITICAL) resolved before implementation; P4 code gate
**Related:** ADR-005 (deferred stage 2), ADR-003 (policy-as-code), spec §4.5, §5.4

## Context

ADR-005 shipped read-only provider visibility and deferred UI-write
registration, which needs the gateway to apply config changes at runtime — it
was boot-static. The source of truth for the eventual write path was decided as
**DB-authoritative with Git export** (2026-06-13). This ADR builds the
foundation: a hot-reload MECHANISM, triggered now by `SIGHUP` re-reading the
config file (the write path + DB store layer on top later).

## Decision

**One immutable generation behind one atomic pointer.** `internal/live.State`
holds the whole reloadable generation — providers, model routes, and the
pricing table — built by `live.BuildState` (the topology-only builder).
`live.Holder` is an `atomic.Pointer[State]`; reload publishes a new generation
with a single `Swap`, so no reader ever observes a mixed generation.

`reload()` re-reads config → `BuildState` (full validation: every provider
builds, every model target resolves to an existing provider) → `Swap` +
`router.RetainBreakers`. **Validate-then-swap**: a config that fails to
load/build returns an error and leaves the current generation serving
(fail-safe rollback). A single `reloadWorker` goroutine serializes `SIGHUP`
triggers (reload mutex; clean lifecycle — `signal.Stop`, exits on ctx cancel,
waited before `serve` returns); reload failures are logged, never fatal.

### Invariants (each pinned by a gate finding)

- **Stateful components are never rebuilt by reload** — the governor's limiter
  (rate buckets) and budget (µUSD counters), the keystore, and the audit writer
  are the same instances across reloads (proven by a pointer-identity test).
  Only the topology generation swaps.
- **Pricing swaps WITH topology** (P2 r1 CRITICAL): pricing lives in the same
  `live.State`, so adding a provider + route + rate in one edit takes effect
  atomically — a route can never bill at stale/0 pricing from a half-applied
  reload.
- **Request-scoped consistency** (P2 r2 CRITICAL): `ResolveChain` does one
  `Load` and returns the `*live.State` it used; the handler bills `Settle` with
  that generation's pricing table — a mid-request reload cannot split resolve
  vs settle. (`PreCheck` does not price.)
- **Leaf boundary preserved** (P2 r2 CRITICAL): `internal/governance` never
  imports `live`/`config` — `Settle` takes the pricing table as a parameter.
  `internal/live` is a topology-only builder importing only
  config/providers/pricing (an import-guard test enforces it) and never
  `configapi` (the admin view is derived in the assembly layer).
- **Breaker keyed by provider identity** (`type+base_url`): a removed/re-pointed
  provider gets a fresh breaker; `RetainBreakers` prunes stale identities under
  the breaker mutex; `RecordResult` no-ops for an absent provider (never
  recreates a pruned entry).
- **Immutability**: `NewState` deep-copies maps and `Targets` slices; accessors
  return copies.

## Alternatives considered

1. **Per-component atomic pointers (router topology, pricing, view separately).**
   Rejected — three sequential swaps leave a window where a reader sees routing
   gen N+1 with pricing gen N; one generation pointer is atomic by construction.
2. **Governor reads pricing from the holder.** Rejected — makes the
   mandate-defined leaf import `live`/`config`; passing the table per-call keeps
   it a leaf and gives request-scoped consistency for free.
3. **Reload governance policy (team rate/quota/budget limits) too.** Deferred —
   limit changes interact with live counters and need counter-preserving
   semantics; team-limit changes still require a restart. Pricing is reloaded
   because it is a stateless lookup table.
4. **fsnotify auto-reload / DB-poll trigger now.** Deferred — SIGHUP is the
   simple, well-understood trigger; the DB-authoritative write path (next plan)
   reuses the same `reload()` mechanism.

## Consequences

- Operators add/repoint providers, change endpoints, edit routes, and adjust
  pricing without downtime: edit config, `kill -HUP <pid>` (K8s: signal PID 1,
  or roll the pods). Server settings (listen addrs, TLS, drain) and team policy
  limits are NOT hot — they still need a restart.
- The hot path now reads topology through one atomic `Load` per `ResolveChain`
  / `Settle`; no per-request map copies (dedicated read accessors).
- The next plan (UI-write registration) persists to the DB and calls this same
  `reload()`; secrets still never enter the gateway (refs only).
