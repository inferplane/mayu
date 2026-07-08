# ADR-016: Teams as first-class keystore records (D3) + derived read-only Users

**Date:** 2026-07-08
**Status:** Accepted (implemented).
**Related:** §6.5/§8 D3 of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`
(the console UX spec naming D3); ADR-013 (multi-replica HA — the per-instance
counter caveat this ADR's console surface must repeat); `internal/keystore/`
(extended here); `internal/governance/` (the enforcement pipeline this ADR
wires a second policy source into).

## Context

Phase 2's first half (per-key governance fields — budget/TPM/RPM/expiry/owner)
shipped earlier. Its other half, D3, was deferred: teams existed only as a
free-text `TEXT` column on `keys` — no table, no CRUD, no enforcement path
other than the config file. Team governance (`internal/governance.Governor`)
was built once at startup from `config.Teams` and never changed at runtime;
the console's Teams view was a permanent placeholder card; the
`teams_records` capability was always `false`.

Three questions needed a decision before implementing:

1. **Should a team record's budget/limits actually enforce in the request hot
   path, or just be stored and displayed (like the per-key fields were at
   first)?** User-confirmed scope: **enforce, dynamically, no restart.**
2. **When the same team name exists in both the config file and a DB record,
   which wins?**
3. **How much of "Users" ships — a first-class table, or a derived view?**
   User-confirmed scope: **derived, read-only.**

## Decision

### 1. `teams` table in the existing keystore, not a new store

Teams live as rows in the same SQLite database as `keys`
(`internal/keystore/sqlite.go`), added via `CREATE TABLE IF NOT EXISTS` inside
the existing `ensureSchema` migration transaction. Because it's a brand-new
table (not new columns on an existing one), no `ALTER TABLE` migration path
was needed — unlike the D2 governance-column addition to `keys`, which had to
handle pre-existing databases.

A separate `keystore.TeamStore` interface (`UpsertTeam`/`GetTeam`/
`ListTeams`/`DeleteTeam`), NOT folded into `keystore.Store`, is implemented by
`*SQLiteStore`. Folding it into `Store` would have broken the three fake
`Store` implementations in `internal/server`'s test suite for no benefit —
only the assembly and `adminapi` need `TeamStore`.

### 2. Precedence: a DB record wins over a config entry of the same name

`internal/governance.Governor` gains an optional
`lookup func(team string) (TeamPolicy, bool)`, installed via
`SetTeamLookup` and consulted **before** the static config map in both
`PreCheck` and `Settle` (`policyOf`). A lookup **hit** (`ok=true`) always wins,
even over a config entry for the same name; a **miss** falls through to
config; absent from both, the team is ungoverned (unchanged prior behavior).

Rationale: the console's whole point is to let an operator edit a team's
budget without touching the config file. If config won ties, editing a
config-declared team via the console would silently do nothing — the worst
possible failure mode for a governance control. Config teams are **not**
seeded into the DB on boot; a config-declared team simply has no record until
an operator explicitly creates one (`source: "config"` vs `"record"` in the
`/admin/teams` list response marks which is in effect).

`internal/governance/fromconfig.go`'s burst-rate rule (`RateBurst` defaults to
`RatePerMin`, floor 1) was factored into a shared `PolicyFromLimits` helper so
the config path and the record path can never diverge on it.

### 3. Enforcement freshness: a per-request keystore point-read, not a cache

The assembly (`cmd/inferplane/gateway.go`) wires `SetTeamLookup` to call
`store.GetTeam(ctx, team)` on **every** `PreCheck`/`Settle` — no TTL cache, no
hot-reload trigger, no in-memory copy kept in sync. Justification:

- `keystore.Resolve` already does one SQLite point read per request on this
  exact hot path (virtual-key auth). One more PK lookup is microseconds on
  pure-Go SQLite, not a new latency class.
- It is **correct under ADR-013's future multi-replica shape**: a shared-file
  or shared-DB keystore means every replica sees an edit immediately, with no
  cross-replica invalidation problem a cache would introduce.
- A lookup **error** (not a miss — a real I/O failure) logs to stderr and
  falls back to the config policy rather than blocking all traffic on a
  transient keystore hiccup; `keystore.Resolve` succeeding on the same
  request makes this a rare, already-degraded-mode path.
- `// ponytail:` marks the no-cache choice in `gateway.go` — add one if
  profiling ever shows the extra read matters; nothing here needs it yet.

Counter keys (`"budget:"+team`, `"quota:"+team`, `"rate:"+team`, `"tpm:"+team`)
are **unchanged** regardless of which source (config or record) supplied the
policy — a team's accumulated spend/quota persists across a source switch.

### 4. `allowed_models` on a team record is a key-creation default, not a hot-path check

A team record's `allowed_models` is stored and can prefill a key-creation
form, but is **not** enforced against a running request — matching today's
`config.TeamConfig.AllowedModels`, which also isn't enforced anywhere. Wiring
per-team model allow-lists into the hot path (`Principal.Allows`) is a
separate, larger decision (it would require the router or ingress handler to
consult team records, not just the key), left for a future ADR if needed.

### 5. Users: derived, read-only, no spend

`GET /admin/users` groups `keystore.Store.List()` by `KeyOptions.Owner`
(`"(unowned)"` for empty). There is no `users` table and no per-user spend
field — audit/analytics events carry a `team` dimension, not an owner
dimension (`internal/analytics`'s `events`/`rollup_day` tables have no owner
column). Adding one is an audit-schema change, out of scope here; the console
states the limitation verbatim rather than approximating it.

### 6. Authorization tiers

- `GET /admin/teams` and `GET /admin/users`: any `AdminAuth`-authenticated
  identity (team-mapped or full admin) — a read of data the identity can
  already reconstruct from `/admin/keys`.
- `PUT`/`DELETE /admin/teams/{name}`: full admin only (`requireAdmin`, the
  same tier as the connection probe and the analytics endpoints). A
  team-mapped identity must not be able to raise its own team's budget via the
  console.
- Deleting a team record does **not** revoke any key — the team simply
  reverts to its config policy (if any) or ungoverned. Conflating "delete the
  governance record" with "revoke access" would be a surprising side effect.

### 7. HA honesty carries over unchanged

The console labels team counters "enforced per gateway instance" per
ADR-013 — a record's RPM/TPM/budget is still an in-memory, per-replica
counter (`internal/limiter`, `internal/budget`); an *N*-replica cluster admits
up to *N×* before blocking, same caveat the per-key limits already carry.

## Consequences

- `teams_records` capability is now unconditionally `true` once the assembly
  wires a `TeamStore` (the keystore always supports it) — same posture as
  `key_governance_fields`.
- `AdminMux`'s signature grew two parameters (`teamStore`, `configTeams`); all
  call sites pass `nil` unless they want the mount (mirrors the optional
  `analyticsQ` pattern already established).
- No change to the audit record schema, the analytics schema, or the
  Postgres-backed Mode B store (`internal/analytics/pgstore`) — teams are a
  keystore concept only.
