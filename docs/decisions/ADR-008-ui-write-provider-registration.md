# ADR-008: UI-write provider registration — DB-authoritative topology store with Git export

**Date:** 2026-06-14
**Status:** Accepted — design gate passed (Stage 2 of ADR-005). 2-round
multi-model gate (codex gpt-5.5 + gemini 3.1-pro; kiro opus-4.8 skipped — its CLI
ignored the piped context). Round 1: 9 findings (2 CRITICAL/secret-safety + 5
MAJOR) folded in. Round 2: all 9 confirmed CLOSED; 2 new findings — a CRITICAL
seed-resurrection bug (durable marker, below) and a MAJOR call-chain ambiguity
(explicit chain, below) — folded in; 1 file-ref MINOR chair-downgraded with code
evidence. **Implemented (Stage B): backend T0–T7** behind 2-round P4 code gate
(codex + gemini) — round 1 found a CRITICAL seed-path ref-validation gap + a
MAJOR un-sanitized 400, both fixed (shared `config.ValidateSecretRef`, fixed
client message + server-side log); round 2 PASS. Console write UI (T8) is a
tracked follow-up.
**Related:** ADR-005 (provider visibility; UI-write deferred to this ADR),
ADR-006 (config hot-reload — the `reload()` + `live.Holder` mechanism this builds
on), ADR-003 (policy-as-code differentiator), spec §7 (secret-ref mandate, inline
keys rejected), §4.5/§5.4 (reload semantics), 부록 A (secret-ref mandate)

## Context

ADR-005 shipped read-only provider visibility (`GET /admin/config`, secret-free)
and explicitly deferred **UI-write registration** to a separate ADR, naming two
unresolved prerequisites: a runtime-mutable config (delivered by ADR-006's
hot-reload) and a **source-of-truth decision**. ADR-006 recorded that decision —
**DB-authoritative with Git export** — and left the write path + DB store layer
"on top later". This is that layer.

The requirement that constrains every option: **secret values never enter the
gateway.** Rival gateways (LiteLLM et al.) let an operator paste an upstream API
key into a UI and persist it in their DB, making the gateway a secret store and a
breach target. Our stated compliance posture (§7, ADR-003) is that the gateway
holds only *references* to secrets (`env:`/`file:`), resolved at load from the
operator's platform secret store. UI-write must preserve this exactly: the UI
registers **which ref** a provider authenticates with — an env var name or a file
path — and guides the operator to set the value in their secret store out of band.
The gateway never sees, stores, transmits, or logs the value.

## Decision

**A DB-authoritative topology store (providers + model routes), opt-in via
config, that reuses the ADR-006 hot-reload mechanism for the write path.**

### 1. Opt-in store; file remains for everything else

A new optional config block enables the store:

```json
"provider_store": { "type": "sqlite", "path": "providers.db" }
```

- **Absent (default):** behavior is exactly v0.2.0 — providers/models come from
  the config file, writes return `405` (ADR-005 stage 1 unchanged). Stage 2 is
  strictly additive; existing deployments are untouched.
- **Present:** the DB is **authoritative for `providers` and `models`** (the
  reloadable topology — exactly what `live.State` already swaps). Everything else
  (server, teams, audit, key_store, **pricing**) stays file-sourced. On first boot
  with an **empty** store, the file's `providers`/`models` are **seeded** into the
  DB once, so a file-config deployment that flips on the store loses nothing and
  transparently becomes DB-authoritative. Thereafter the file's `providers`/
  `models` blocks are ignored for topology (a divergence is logged, not silently
  dropped).

  **Config-load must not resolve ignored file providers** (gate G1, CRITICAL):
  today `config.Load` resolves *every* file provider's secret ref and hard-fails
  if any is unset — so a deployment that flipped to DB-authoritative but left
  stale file providers (whose env refs are now unset) would crash at boot / on
  SIGHUP *before* the overlay could discard them. Therefore config parsing is
  split from provider-secret resolution: a raw parse (inline-`api_key` reject +
  admin/oidc validation, unchanged) and a separate provider-secret resolution
  step. When the store is authoritative, **file providers are parsed but never
  resolved** (their names/refs are still readable for the divergence log); only
  the effective (DB-overlaid) providers are resolved.

  **The assembly call chain is explicit** (round-2 MAJOR): when the store is
  enabled, boot (`newGateway`) and `reloadLocked()` must call `config.LoadRaw`
  (parse + inline-`api_key` reject + admin-token resolve + OIDC validation, but
  **no provider-secret resolution**) → `SeedIfEmpty` → `Overlay` (DB topology) →
  `config.ResolveProviders` on the *overlaid* set → `BuildState`. Continuing to
  call `config.Load` (which resolves file providers internally) would re-trigger
  the G1 crash at the call site. When the store is absent, the path is unchanged
  (`config.Load`).

  **Seed-once is tracked by a durable marker, in one transaction** (gates C5 +
  round-2 CRITICAL): seeding is guarded by a `meta` row (`seeded = 1`), **not a
  row count**. A row-count anchor would *resurrect* providers an operator
  deliberately deleted — delete every provider via the API (0 rows) and the next
  restart would re-seed them from the file. Instead: if `meta['seeded']` is
  absent, import the file `providers` + `model_targets` **and** set
  `meta['seeded']=1` in a **single transaction** (all-or-nothing — no half-seeded
  state observable); once set, the gateway never seeds again regardless of row
  count. (`meta` is a portable `(key TEXT PRIMARY KEY, value TEXT)` table.)

`internal/providerstore` is a new SQLite store mirroring the `keystore` pattern:
TEXT/INTEGER-only, Postgres-portable DDL (the v0.2 HA path swaps the backend), a
`providers` table and a `model_targets` table (ordered targets with the optional
`api` field). **No column can hold a secret** — a provider row stores its
`api_key_ref` (env var name or file path), `type`, `base_url`, `region`, and auth
`mode`/`profile`; never an `api_key`. This is the same structural guarantee as
`configapi.ProviderView`: the absence of the field is the defense.

### 2. Write contract: validate → persist → reload

Granular REST resources behind the existing `AdminAuth` (same guard as the
mutating `/admin/keys`):

- `PUT /admin/providers/{name}` — register/replace a provider
- `DELETE /admin/providers/{name}`
- `PUT /admin/models/{name}` — set a model's ordered target chain
- `DELETE /admin/models/{name}`

The request DTO **has no `APIKey` field** and the handler **rejects an inline
`api_key`** in the JSON body (the same probe `config.Load` runs, §7) — a write
carrying a secret value is a `400`, structurally and explicitly. Auth is
specified as a reference: `{ "api_key_ref": { "env": "ANTHROPIC_KEY" } }` or
`{ "file": "/secrets/key" }`; bedrock providers carry only IAM `mode`/`profile`.

**Ref-shape validation + sanitized errors** (gate C1, MAJOR): a `ref` field is
supposed to hold a *name*, not a value — but nothing stops a confused operator
pasting the secret into `env`. Two structural guards close this: (a) the env ref
must match `^[A-Za-z_][A-Za-z0-9_]*$` and a file ref must be an absolute path —
most real secrets (`sk-ant-…`, mixed case, dashes) fail this and are rejected at
write time, never persisted/exported/audited; (b) the validation error returned
to the client is **sanitized** — a secret-resolution failure maps to a fixed
message (e.g. "api_key_ref env var is unset") and **never echoes the
caller-supplied ref string back**, so even a pasted-secret-as-ref cannot leak via
the `400` body. (Today `config.resolveSecret` returns `fmt.Errorf("env %s is
empty", ref.Env)` — that raw error must not reach an HTTP response.)

A `file` ref accepts any absolute path, so a panel reviewer asked whether a
path-shaped secret could slip through and be persisted (round-2 MINOR, codex).
Two facts close it: (1) a file *path* is non-secret operational data **by
design** — ADR-005 already displays `file:/secrets/key` in the auth string — so
a path in the DB/export/audit is not a leak; (2) **validation resolves the ref**
(build-once: `ResolveProviders` reads the file) *before* persist, so a
path-shaped value that is not a readable file is rejected `400` and never
persisted/exported/audited. A value that is simultaneously a real readable file
*and* the secret itself is self-contradictory. A test pins "file ref to a
nonexistent path → 400, nothing persisted".

Each write is **serialized through the single `reloadMu`** that already guards
SIGHUP reload (gate C3, MAJOR — a separate write mutex would let SIGHUP interleave
a write's validate/persist/swap, and reusing `reloadMu` then calling the existing
`reload()` would deadlock reentrantly). `reload()` is therefore split into a
public locking entry and a `reloadLocked()` helper; both the write path and the
SIGHUP worker funnel through `reloadMu`. Within that lock the write is processed
**build-once, swap-once** (gate C2, MAJOR — eliminating the persist→reload
TOCTOU):

1. **Validate (dry-run = the real build):** build the *candidate* effective
   `live.State` (file config + the DB topology as it would be after this write,
   refs resolved through the same `config` resolver). This resolves every ref,
   builds every provider, validates every route; on failure (unknown type,
   ref-shape reject, unresolvable ref, route to a missing provider) return a
   sanitized `400` and **leave the DB untouched**.
2. **Persist:** write the row(s) in a transaction.
3. **Swap the already-validated `State`** (+ `RetainBreakers`) — *not* a second
   `BuildState`. The generation published is byte-for-byte the one just
   validated, so there is no window where the DB holds a row that fails to build.
   (A *later, independent* SIGHUP still rebuilds from file+DB and is
   validate-then-swap fail-safe; if the env changed underneath since this write,
   that SIGHUP fails safe exactly as a file-based env change does today.)

Provider/model writes are **admin-action audit events** (§5.5, like key
create/revoke): `provider_registered` / `provider_updated` / `provider_deleted`
and the model equivalents, emitted **secret-free** (ref name only).

### 3. Git export

`GET /admin/config/export` renders the current effective topology
(`providers` + `models`) as a **secret-free config JSON fragment** — refs only,
no resolved values — for the operator to commit to Git. This is the "export" half
of "DB-authoritative with Git export": the DB is the live source of truth; Git is
the reviewable, auditable record and disaster-recovery seed. (A future
`inferplane config export` CLI can wrap the same renderer.)

Export is **read-only and secret-free, so it is mounted unconditionally** (gate
C7): with the store absent it simply exports the file topology — useful and safe.
The "exact v0.2.0 behavior" of the absent-store case (decision §1) refers to the
**write** posture (`405`, ADR-005) and the **topology source** (file); additive
read-only endpoints like export do not violate it.

### 4. Console write UI

The "Providers" tab gains register/edit/delete forms and a model-route editor,
under the existing CSP `default-src 'self'` discipline (ADR-001): no inline
`style=`/`onclick=`, values set via DOM properties, the admin token in JS memory
only. The form collects the **ref name**, never a secret, and shows the operator
the exact secret-store step to perform out of band.

## Alternatives considered

1. **Git-authoritative — the UI opens a PR instead of writing a DB.** Rejected as
   the primary path: it requires a Git host integration + credentials and adds
   latency (a merge round-trip) to "add a provider," and ADR-006 already committed
   to DB-authoritative. Git export (decision §3) keeps the GitOps benefit
   (review, audit, DR) without coupling the gateway to a Git host. A PR-opening
   exporter could layer on later.
2. **Store the upstream key in the gateway DB (LiteLLM model).** Rejected — a
   direct §7 violation (inline secrets rejected at load) that makes the gateway a
   secret store and breach target. The secret-ref posture is a stated
   differentiator (ADR-003), not a limitation to "fix." The store has no key
   column by construction.
3. **Make the DB authoritative for everything (teams, pricing, audit, server).**
   Rejected for this stage — those interact with live governance counters
   (ADR-006 deferred team-limit reload for exactly this reason) and the audit
   chain; scope-creep with no UI-write demand. Only the already-reloadable
   topology (providers/models) moves to the DB; pricing stays file-sourced, so a
   UI-registered provider with no file pricing override bills at the `on_missing`
   policy (documented; an operator adds a pricing override in the file when
   needed).
4. **A single `PUT /admin/config` replacing the whole config.** Rejected —
   coarse, race-prone, and conflates the reloadable topology with the static file
   config. Granular `/admin/providers` + `/admin/models` resources map to the
   console's per-row actions and to per-resource audit events.
5. **A new admin entitlement tier (only `admin_groups` may write providers).**
   Considered. Deferred: `/admin/keys` is already a mutating admin operation
   behind the same `AdminAuth`, so provider writes match the existing bar without
   a new authz tier. If operators want provider-write segregated from key-write,
   that is a follow-up that extends `adminauth`, not a blocker here.
6. **`reload()` keeps reading only the file.** Rejected — it cannot surface UI
   writes. `reload()` is extended to build the *effective* config (file + DB
   overlay) so the write path and SIGHUP converge on one rebuild-and-swap.

## Consequences

- Operators register/repoint providers and edit routes from the console with no
  downtime and **no upstream secret ever touching the gateway** — the UI takes a
  ref name and points the operator at their secret store.
- The write path reuses ADR-006 end to end: one atomic generation swap,
  validate-then-swap fail-safe, stateful components (governor counters, keystore,
  audit, breakers) untouched across the reload.
- DB is the live source of truth; `GET /admin/config/export` produces the
  Git-committable record. A divergent file `providers` block is logged, not
  silently honored, so operators are never confused about what is authoritative.
- The store is opt-in: zero behavior change for file-only / GitOps-only
  deployments, which keep the 405-on-write posture from ADR-005.
- Postgres-portable DDL keeps the v0.2 HA path open (shared topology store across
  replicas) without a rewrite.
- New audit events (`provider_*`) extend the §5.5 admin-action coverage;
  downstream audit consumers gain provider-lifecycle visibility.
- **File pricing overrides are keyed by provider/model *name*** (gate C4): they
  apply to the DB-overlaid topology by name, regardless of the endpoint the DB
  row now points at. Repointing a provider to a *different-cost* upstream while
  keeping its name keeps the old override — by design when the logical provider's
  cost is unchanged, but an operator who switches cost basis (e.g. a `vllm-prod`
  name re-pointed from a hosted to a self-hosted endpoint) must update the file
  pricing override. This is documented operator guidance, not silent: pricing is
  not in the DB (alt. 3), so a name-keyed override is the only coupling and it is
  visible in the file.
