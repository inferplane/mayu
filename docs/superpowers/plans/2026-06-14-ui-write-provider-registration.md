# Plan: UI-write provider registration (Stage 2)

**Date:** 2026-06-14
**Related:** ADR-008 (this plan implements it), ADR-005 (stage 1 visibility),
ADR-006 (hot-reload `reload()` + `live.Holder`), spec §7 (secret refs), §5.5
(admin actions are audit events)
**Base:** main @ 3102223 · **Produces:** the Stage 2 write path

## Goal

Let an operator register/repoint providers and edit model routes from the admin
plane, with a **DB-authoritative** topology store (opt-in), reusing the ADR-006
`reload()` mechanism — and with **secret values never entering the gateway**
(the UI registers WHICH ref a provider uses; the operator sets the value in
their own secret store). Git export gives the GitOps record.

## Core architecture (from ADR-008; refined after P2 round-1 — codex + gemini)

- **`internal/providerstore`** — new opt-in SQLite store, keystore-pattern,
  Postgres-portable DDL. `providers` table (name, type, base_url, region,
  auth mode/profile, **api_key_ref** — env name or file path; **no api_key
  column**) + `model_targets` table (model, position, provider, model_id, api).
- **Config parse is split from provider-secret resolution** (gate G1, CRITICAL):
  a raw parse (inline-`api_key` reject + admin/oidc validation, unchanged) and a
  separate provider-resolution step. When the store is authoritative, **file
  providers are parsed but NOT resolved** — a stale file provider with an unset
  env ref must NOT crash boot/SIGHUP before the overlay discards it.
- **Effective config = raw file config with `Providers`/`Models` overlaid from
  the DB** when the store is enabled; only the DB-overlaid providers' refs are
  resolved (through the same `config` resolver — inline rejection + env/file
  rules in one place). Empty store on boot is **seeded** from the file in **one
  transaction** ("empty" = zero `providers` rows; gate C5).
- **`reload()` (ADR-006) is extended** to build the effective config (file + DB
  overlay) before `BuildState` + `Swap`. SIGHUP and UI-write **funnel through the
  single `reloadMu`**; `reload()` is split into a public locking entry +
  `reloadLocked()` so the write path holds the lock without reentrant deadlock
  (gate C3).
- **Write path = build-once, swap-once** (gate C2): under `reloadMu`, build the
  candidate effective `live.State` (= the real validation) → persist(txn) → swap
  the **already-validated State** (`+RetainBreakers`), NOT a second `BuildState`.
  No window where the DB holds a row that fails to build. A `400` on validation
  failure leaves the DB untouched.
- **Secret-free by construction**: the write DTO has no `APIKey` field; the
  handler rejects an inline `api_key` (§7 probe); **ref-shape validation** (env
  name `^[A-Za-z_][A-Za-z0-9_]*$`, file path absolute) + **sanitized errors**
  (never echo the caller-supplied ref string in a `400`) close the
  pasted-secret-as-ref leak (gate C1). Provider writes emit secret-free
  admin-action audit events.
- **Opt-in**: store absent → exact v0.2.0 **write** posture (`405`, ADR-005) and
  file topology source. The read-only secret-free `GET /admin/config/export` is
  additive and mounted unconditionally (gate C7).

## Hard safety invariants (the gate's checklist)

- **No secret column, no secret field, no secret in any response or audit
  record** — provider rows/DTOs/events carry only `api_key_ref` (env name / file
  path) and IAM mode. A test populates a resolved key and asserts it never
  appears in any DB row, export, or audit record.
- **No secret leak via a ref**: env refs must match `^[A-Za-z_][A-Za-z0-9_]*$`,
  file refs must be absolute paths — a pasted secret is rejected at write time;
  validation `400`s are sanitized and never echo the caller-supplied ref string
  (gate C1) — pinned by a test that sends `env: "sk-ant-…"` and asserts 400 with
  the value absent from the body.
- **Inline `api_key` in a write body is rejected** (`400`), the same rule as
  `config.Load` — pinned by a test.
- **Config-load does not resolve ignored file providers** (gate G1): with the
  store enabled, a file provider whose env ref is unset does NOT crash boot or
  SIGHUP — pinned by a test.
- **Build-once-swap-once** (gate C2): a write builds the candidate `live.State`,
  persists, then swaps THAT state — never a second `BuildState`. A write that
  fails to build returns `400` and leaves the DB byte-for-byte unchanged — pinned
  by a test.
- **Stateful components untouched** (ADR-006): a UI write reloads topology only;
  governor counters / keystore / audit / breakers persist (pointer-identity
  test, reusing the ADR-006 pattern).
- **Opt-in back-compat**: with no `provider_store` block, providers/models come
  from the file and every write returns `405` — pinned by a test.
- **Seed-once via a durable marker, atomic** (gate C5 + round-2 CRITICAL):
  seeding is gated by a `meta['seeded']` marker, NOT a row count — so deleting
  every provider via the API does NOT resurrect the file topology on the next
  restart. The first enable imports `providers` + `model_targets` **and** sets
  the marker in **one transaction**. Pinned by a "delete-all then re-init → no
  resurrection" test.
- **Writes + SIGHUP serialized through one `reloadMu`** (gate C3): a write and a
  concurrent SIGHUP never interleave; no reentrant deadlock (`reload()` splits
  into locking + `reloadLocked()`). Pinned by a race test covering write-vs-SIGHUP.
- **Referential integrity on delete** (gate G3): `DELETE /admin/providers/{name}`
  with a model still routing to it returns `400` (the dry-run `BuildState`
  rejects it) — pinned by a test.
- **`live` stays a leaf** (ADR-006): `internal/live` still imports only
  config/providers/pricing and gains NO new import. The overlay lives in
  `internal/providerstore` (which may import `config` — allowed; `live` does not
  import `providerstore`) and the write wiring in the assembly layer (`cmd`) /
  `configapi`. `internal/governance` stays a leaf. Import-guard test still passes
  (gate C6 — wording corrected: the invariant is specifically that `live` and
  `governance` gain no new imports).

## Tasks

Each task: failing test first → minimal code → refactor; one `git commit -s`;
all four gates green (`build`, `test -race`, `vet`+`gofmt -l`, `tests/run-all.sh`).

### Backend (shippable without the console UI)

- [ ] **T0 — config: `provider_store` block + split parse/resolve (G1, G2).**
  Add `ProviderStoreConfig{Type, Path}` and `Config.ProviderStore
  *ProviderStoreConfig` (gate G2). Split `config.Load` so parsing (inline-
  `api_key` reject + **admin-token resolve + OIDC validation**) is separate from
  **provider-secret** resolution: add `LoadRaw(path)` (everything EXCEPT the
  provider-secret loop) + exported `ResolveProviders(*Config) error` /
  `ResolveSecretRef(*SecretRef)`. Existing `Load` = `LoadRaw` + `ResolveProviders`
  (back-compat, byte-identical behavior when no store). Tests: block parses;
  `Load` unchanged for file-only; **`LoadRaw` does NOT resolve provider secrets**
  so an unset file ref does NOT error (gate G1); inline-reject + OIDC validation
  still fire in `LoadRaw`.
  *Files:* `internal/config/config.go`, `internal/config/config_test.go`.

- [ ] **T1 — providerstore: providers table (CRUD).**
  New `internal/providerstore/{providerstore.go,sqlite.go,sqlite_test.go}`.
  `Store` interface + `SQLiteStore` (Postgres-portable TEXT/INTEGER DDL, WAL +
  busy_timeout, `SetMaxOpenConns(1)` — keystore pattern). `ProviderRow{Name,
  Type, BaseURL, Region, AuthMode, AuthProfile, APIKeyRefEnv, APIKeyRefFile}` —
  **no APIKey field**. `Upsert/Get/List/Delete`. Tests: round-trip; refs
  persisted; **assert no api_key column exists** (`PRAGMA table_info`); delete
  missing → `ErrNotFound`.
  *Files:* `internal/providerstore/*` (new pkg).

- [ ] **T2 — providerstore: model_targets + `meta` seed marker (C5 + round-2 CRIT).**
  Add `model_targets(model, position, provider, model_id, api)` + `SetModel(name,
  []Target)` (replace-all in a txn, ordered), `ListModels()`, `DeleteModel`. Add
  a portable `meta(key TEXT PRIMARY KEY, value TEXT)` table + `Seeded() bool` /
  `MarkSeeded(tx)` — seeding is gated by the **durable marker, NOT a row count**
  (round-2 CRITICAL: a row-count anchor resurrects deliberately-deleted providers
  on the next restart). Tests: ordering; replace-all; api round-trip; **`Seeded`
  stays true after all providers are deleted** (no resurrection).
  *Files:* `internal/providerstore/{models.go,models_test.go}`.

- [ ] **T3 — overlay + seed-once (durable marker, atomic) (C5 + round-2).**
  New `internal/providerstore/overlay.go`: `Overlay(rawFileCfg *config.Config,
  store) (*config.Config, error)` → a copy of the raw file config with
  `Providers`/`Models` from the DB; server/teams/audit/pricing kept from file;
  logs a divergence when the file still declares providers. **Resolution is the
  caller's job** (the assembly runs `config.ResolveProviders` on the overlaid set
  — keeps Overlay free of secret material). `SeedIfEmpty(store, rawFileCfg)` — if
  **`!store.Seeded()`**, import file providers + models **and** `MarkSeeded` in
  **one transaction** (all-or-nothing); a populated-then-emptied store is NOT
  re-seeded. Tests: overlay replaces topology only; **no resolved key ever
  written to the DB**; seed runs once; **deleting all providers does not
  re-seed** on the next `SeedIfEmpty`; ignored file providers are not resolved
  (composes with G1).
  *Files:* `internal/providerstore/{overlay.go,overlay_test.go}`.

- [ ] **T4 — write DTO + validation (inline reject, ref-shape, sanitized, dry-run).**
  New `internal/server/configapi/write.go`: `ProviderWrite` DTO (**no APIKey
  field**), inline-`api_key` probe → `400` (mirror `config.Load`); **ref-shape
  validation** (env `^[A-Za-z_][A-Za-z0-9_]*$`, file absolute) → `400` (gate C1);
  `ModelWrite` DTO. A `Validator` that builds the candidate effective `live.State`
  and returns a **sanitized** error (never echoing the caller's ref string; gate
  C1). Tests: inline secret → 400; **`env:"sk-ant-…"` → 400 with the value absent
  from the body**; **`file` ref to a nonexistent path → 400, nothing persisted**
  (round-2 MINOR — validation reads the file, so a path-shaped secret that is not
  a readable file never reaches DB/export/audit); unknown type → 400; route to
  missing provider → 400; **delete-provider-with-live-route → 400** (gate G3);
  valid → ok.
  *Files:* `internal/server/configapi/{write.go,write_test.go}`.

- [ ] **T5 — write handlers + build-once-swap-once + single-mutex serialization.**
  Handlers for `PUT/DELETE /admin/providers/{name}` and `PUT/DELETE
  /admin/models/{name}`, taking an injected `Writer` callback — same DI shape as
  `configView func() View`. The assembly callback runs **under `reloadMu`**:
  build candidate state → persist(txn) → swap that state + `RetainBreakers` (gate
  C2, build-once-swap-once). Split `gateway.reload()` into a locking entry +
  `reloadLocked()` so the write path and the SIGHUP worker funnel through one
  `reloadMu` without reentrant deadlock (gate C3). **Explicit store-enabled call
  chain** (round-2 MAJOR — both boot `newGateway` and `reloadLocked`):
  `LoadRaw` → `SeedIfEmpty` → `Overlay` → `ResolveProviders`(overlaid) →
  `BuildState` → swap; store-absent keeps `config.Load` unchanged. Tests: PUT
  persists + new topology visible via the view; DELETE;
  **write-vs-SIGHUP serialized (race test)**; store-absent → `405`; stateful
  components survive (pointer-identity).
  *Files:* `internal/server/configapi/write.go`, `internal/server/server.go`
  (mux), `cmd/inferplane/gateway.go` (callback + reloadLocked + DB overlay),
  `internal/server/server_test.go`, `cmd/inferplane/gateway_test.go`,
  `cmd/inferplane/reload_test.go`.

- [ ] **T6 — admin-action audit events (secret-free).**
  Emit `provider_registered`/`provider_updated`/`provider_deleted` and
  `model_route_updated`/`model_route_deleted` via the existing audit emit hook
  (like key create/revoke). Tests: event emitted on each write; **record carries
  no resolved secret** (ref name present, value absent even when a key is set in
  env).
  *Files:* `internal/server/configapi/write.go`, `cmd/inferplane/gateway.go`
  (thread emit), tests.

- [ ] **T7 — Git export endpoint (mounted unconditionally; C7).**
  `GET /admin/config/export` → secret-free config JSON fragment
  (`providers`+`models`, refs only) from the current effective topology, behind
  `AdminAuth`, mounted whether or not the store is enabled (read-only + secret-
  free). Reuse the ADR-005 secret-free projection guarantee. Tests: round-trips
  refs; **resolved key never serialized**; output re-parses as valid config;
  works with store absent (exports file topology).
  *Files:* `internal/server/configapi/{export.go,export_test.go}`,
  `internal/server/server.go` (mux), `cmd/inferplane/gateway.go` (wire).

### Console UI (final; may follow in a separate PR if scope runs long)

- [ ] **T8 — console "Providers" tab: write forms (CSP-compliant).**
  Register/edit/delete provider form + model-route editor in
  `internal/server/adminui/`. CSP `default-src 'self'`: no inline
  `style=`/`onclick=`, values set via DOM properties, token in JS memory only.
  Form collects the **ref name**, never a secret, and shows the operator the
  out-of-band secret-store step. Tests: harness secret-pattern + structure
  (`bash tests/run-all.sh`); a DOM/asset test asserting no inline handlers and no
  secret input field name.
  *Files:* `internal/server/adminui/*` (assets + handler test).

### Docs (final task)

- [ ] **T9 — docs sync.** Update `internal/CLAUDE.md` (new `providerstore` pkg),
  `docs/reference/api.md` (new endpoints), `docs/reference/data.md` (new store
  schema), `docs/architecture.md` (DB-authoritative topology), and mark ADR-008
  **Accepted** with the gate verdict. Regenerate `AGENTS.md`/`GEMINI.md` via
  `/co-agent:sync-context` if `CLAUDE.md` changed.
  *Files:* docs + ADR-008 status line.

## File scope (scope_guard allow-list)

```
docs/decisions/ADR-008-ui-write-provider-registration.md
docs/superpowers/plans/2026-06-14-ui-write-provider-registration.md
internal/providerstore/providerstore.go
internal/providerstore/sqlite.go
internal/providerstore/sqlite_test.go
internal/providerstore/models.go
internal/providerstore/models_test.go
internal/providerstore/overlay.go
internal/providerstore/overlay_test.go
internal/config/config.go
internal/config/config_test.go
internal/server/configapi/config.go
internal/server/configapi/write.go
internal/server/configapi/write_test.go
internal/server/configapi/export.go
internal/server/configapi/export_test.go
internal/server/server.go
internal/server/server_test.go
cmd/inferplane/gateway.go
cmd/inferplane/gateway_test.go
cmd/inferplane/reload_test.go
internal/server/adminui/app.js
internal/server/adminui/index.html
internal/server/adminui/adminui.go
internal/server/adminui/adminui_test.go
internal/CLAUDE.md
docs/reference/api.md
docs/reference/data.md
docs/architecture.md
examples/config.json
```

## Out of scope (explicit)

- Pricing in the DB (stays file-sourced; UI-registered provider with no file
  pricing override bills at `on_missing` — ADR-008 §3 alt.3).
- Team/quota/budget reload or DB authority (ADR-006 alt.3 deferred).
- Postgres backend (DDL is portable; the HA path is roadmap #4).
- A Git-host PR-opening exporter (ADR-008 alt.1 — export-to-stdout/file only).
- A new admin-write entitlement tier (ADR-008 alt.5 — same AdminAuth as keys).
