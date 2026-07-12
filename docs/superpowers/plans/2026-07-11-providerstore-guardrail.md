# providerstore guardrail: provider-level default Bedrock Guardrail (ADR-019 deferred item)

**Date:** 2026-07-11
**Source:** ADR-019 "Deferred" section — DB-registered (UI/providerstore) Bedrock
providers get no *provider-level* default Guardrail; only a per-team override
(`keystore.TeamRecord.GuardrailID/GuardrailVersion`) currently reaches a DB-registered
provider. `config.ProviderConfig` (file path) already has `GuardrailID`/`GuardrailVersion`
— validated in `ResolveProviders`, injected into `live.BuildState`'s bedrock `Settings`,
consumed by `providers/bedrock` as `defaultGuardrail`, and serialized by
`configapi/export.go`. **Zero diff in the CONSUMING layers below the row↔config
mapping** (`providers/bedrock`, `internal/live`, `config.ResolveProviders`,
`configapi/export.go` all already handle `GuardrailID`/`GuardrailVersion` generically via
`config.ProviderConfig` and need no changes) — this plan only threads the same two
fields THROUGH the mapping itself (the providerstore row and its write/view/UI surface),
which is exactly Tasks 1-2's job, so a DB-registered provider can carry them too.

**Bonus bug fix (Task 2):** `providerstore/overlay.go`'s `rowFromProviderConfig` does not
carry guardrail fields today, so a config-file-declared bedrock provider's guardrail is
silently dropped the moment `SeedIfEmpty` imports it into the DB (the DB becomes
authoritative with the guardrail gone). Task 2 fixes this as part of the round-trip.

**Invariants (CLAUDE.md / ADR-008, non-negotiable):**
- `providers` table carries NO secret column — guardrail_id/version are not secrets, only
  a Bedrock Guardrail identifier/version, same class as `region`/`auth_mode` already there.
- Inline `api_key` stays rejected (§7) — untouched by this change.
- UI writes go build-once-swap-once through the assembly's `reloadMu` — untouched; this
  plan only adds two carried fields to an existing write path, no new write mechanism.
- Guardrail validation duplicates the existing three call sites' inline pattern
  (`config.go` ResolveProviders, `adminapi/teams.go` validateGuardrailFields) rather than
  extracting a shared helper — matches the codebase's established precedent of inline
  duplication per surface (see `auth_header`'s three independent validations).

**Reused assets:**
- `internal/providerstore/sqlite.go`'s `auth_header` ALTER-TABLE migration pattern
  (`columns` slice at the end of `ensureSchema`) — mirror exactly for the two new columns.
- `internal/providerstore/overlay.go`'s `providerConfigFromRow`/`rowFromProviderConfig` —
  add two more field assignments each, same shape as the existing `AuthHeader` line.
- `internal/config/config.go:697-711`'s guardrail validation (version format, id-without-
  version-only, type-must-be-bedrock) — same three checks, inlined into
  `configapi/write.go`'s `ParseProviderWrite`.
- `internal/server/configapi/config.go`'s `ProviderView`/`ViewFrom` — same pattern as the
  existing `Region` field (non-secret, echoed for console prefill).
- `internal/server/adminui/static/index.html` team-form guardrail inputs (`tf-guardrail-id`
  /`tf-guardrail-version`, lines ~240-241) and `app.js`'s team-form wiring (submit body
  lines ~855-856, prefill lines ~761-762) — mirror into the provider form
  (`pf-guardrail-id`/`pf-guardrail-version`).

---

### Task 1: providerstore schema + Row fields

**Files:**
- Modify: `internal/providerstore/providerstore.go`
- Modify: `internal/providerstore/sqlite.go`
- Modify: `internal/providerstore/models.go`
- Test: `internal/providerstore/sqlite_test.go`

Steps:
- [ ] Add `GuardrailID string` and `GuardrailVersion string` fields to `ProviderRow` in
      `providerstore.go`, doc-commented the same way as `AuthHeader` (mirrors
      `config.ProviderConfig.GuardrailID`/`GuardrailVersion`; meaningful only for
      type "bedrock"; empty version with non-empty id defaults to "DRAFT" at the provider
      layer — validation of that rule happens in Task 3, not here).
- [ ] In `sqlite.go`'s `schema` const, add `guardrail_id TEXT NOT NULL DEFAULT ''` and
      `guardrail_version TEXT NOT NULL DEFAULT ''` columns to `CREATE TABLE providers`
      (same reasoning comment as `auth_header`: a fresh DB gets the canonical shape in one
      DDL, `ensureSchema`'s migration list below still runs for a pre-existing DB).
- [ ] Add two entries to the `columns` migration slice in `ensureSchema` (right after
      `auth_header`): `{"guardrail_id", ALTER TABLE providers ADD COLUMN guardrail_id TEXT NOT NULL DEFAULT ''}`
      and `{"guardrail_version", ...}` likewise.
- [ ] Update `UpsertProvider`'s INSERT column list, VALUES placeholders, ON CONFLICT SET
      clause, and positional args to include `guardrail_id`/`guardrail_version` (append
      after `auth_header`, matching order everywhere).
- [ ] Update `GetProvider`'s SELECT column list and `Scan` target list the same way.
- [ ] Update `ListProviders`'s SELECT column list and `Scan` target list the same way.
- [ ] In `models.go`'s `Seed` method, update the providers INSERT column list, VALUES
      placeholders, and positional args the same way (mirrors the existing `auth_header`
      column already there).
- [ ] Write `TestMigrationAddsGuardrailColumns` in `sqlite_test.go`, mirroring
      `TestMigrationAddsAuthHeaderColumn`: hand-create a DB with the OLD schema (columns
      through `auth_header`, no guardrail columns), insert a pre-existing row, close, then
      `OpenSQLite` on it and assert (a) the pre-existing row's `GuardrailID`/
      `GuardrailVersion` both default to `""` via `GetProvider`, and (b) a fresh
      `UpsertProvider` with both fields set round-trips correctly via `GetProvider`.
- [ ] Extend `TestProviderRoundTrip` (or add `TestProviderRoundTripGuardrail` mirroring
      `TestProviderRoundTripAuthHeader`) to cover `UpsertProvider` → `GetProvider` →
      `ListProviders` carrying non-empty `GuardrailID`/`GuardrailVersion`.
- [ ] `TestNoSecretColumn` needs NO change (guardrail_id/version aren't in its banned-name
      switch) — confirm it still passes; do not add them to the banned list (they are not
      secrets).

---

### Task 2: overlay mapping (fixes the file→DB seed guardrail-loss bug)

**Files:**
- Modify: `internal/providerstore/overlay.go`
- Test: `internal/providerstore/overlay_test.go`

Steps:
- [ ] In `providerConfigFromRow`, add `GuardrailID: p.GuardrailID, GuardrailVersion:
      p.GuardrailVersion` to the returned `config.ProviderConfig` literal (same line as
      the existing `AuthHeader: p.AuthHeader`).
- [ ] In `rowFromProviderConfig`, add `GuardrailID: pc.GuardrailID, GuardrailVersion:
      pc.GuardrailVersion` to the returned `ProviderRow` literal (same line as the existing
      `AuthHeader: pc.AuthHeader`). **This is the bug fix** — today this function drops
      both fields, so `SeedIfEmpty` silently loses a config-file-declared bedrock
      provider's guardrail the moment it's imported into the DB.
- [ ] Write `TestGuardrailRoundTripsThroughRowConversion` mirroring
      `TestAuthHeaderRoundTripsThroughRowConversion`: build a `config.ProviderConfig{Type:
      "bedrock", GuardrailID: "gr-abc", GuardrailVersion: "3"}`, run it through
      `rowFromProviderConfig` then `providerConfigFromRow`, assert both fields survive the
      round trip.
- [ ] Extend `TestSeedIfEmptySeedsOnceFromFile`'s `fileCfg()` fixture (or add a sibling
      test `TestSeedIfEmptyPreservesGuardrail`) so the seeded file provider has
      `Type: "bedrock", GuardrailID: "gr-seed", GuardrailVersion: "DRAFT"` set; after
      `SeedIfEmpty`, assert `ListProviders` returns a row with both fields intact (this is
      the regression test for the bug — it must FAIL against the current
      `rowFromProviderConfig` before the fix, and PASS after).

---

### Task 3: configapi write-path validation

**Files:**
- Modify: `internal/server/configapi/write.go`
- Test: `internal/server/configapi/write_test.go`

Steps:
- [ ] Add `GuardrailID string \`json:"guardrail_id,omitempty"\`` and `GuardrailVersion
      string \`json:"guardrail_version,omitempty"\`` fields to `ProviderWrite`.
- [ ] In `ParseProviderWrite`, insert an inline guardrail validation block directly
      before the `row := providerstore.ProviderRow{...}` construction (i.e. after the
      existing `auth_header` validation block, which itself sits before that same
      construction). Duplicate **six** checks, not `config.go`'s three alone — plan-gate
      round 1 found the team-level path (`adminapi/teams.go:220
      validateGuardrailFields`) enforces three additional id-shape checks that a
      config.go-only port would miss, and the provider write path should not be a weaker
      guardrail-registration surface than the team path:
      (a) if `len(w.GuardrailID) > 2048` → reject ("guardrail_id exceeds 2048 bytes") —
          mirror `adminapi/teams.go`'s `maxGuardrailIDBytes` constant/limit,
      (b) if `w.GuardrailID` contains any `unicode.IsControl` rune → reject,
      (c) if `w.GuardrailID != "" && strings.TrimSpace(w.GuardrailID) == ""` → reject
          ("guardrail_id must not be whitespace-only"),
      (d) if `w.GuardrailVersion != "" && w.GuardrailID == ""` → reject
          ("guardrail_version set without guardrail_id"),
      (e) if `w.GuardrailVersion != "" && w.GuardrailVersion != "DRAFT"` → parse with
          `strconv.Atoi`, reject unless `n >= 1 && strconv.Itoa(n) == w.GuardrailVersion`
          (rejects leading zeros/signs/non-numeric),
      (f) if `w.GuardrailID != "" && w.Type != "bedrock"` → reject ("guardrail_id is only
          meaningful for type \"bedrock\"") — this check has no team-level analog (teams
          have no `Type`), keep it provider-side only.
      Import `strconv` (not yet imported in this file); `unicode` and `strings` may
      already be imported — check before adding.
- [ ] Carry `GuardrailID`/`GuardrailVersion` into the returned `providerstore.ProviderRow`
      literal.
- [ ] Write four tests in `write_test.go` mirroring the
      `TestParseProviderWriteCarriesAuthHeader`/`RejectsAuthHeaderOnNonAnthropicType`/
      `RejectsInvalidAuthHeader` trio:
      - `TestParseProviderWriteCarriesGuardrail`: valid bedrock body with both fields set
        → row carries both.
      - `TestParseProviderWriteRejectsGuardrailVersionWithoutID`: `guardrail_version` set,
        `guardrail_id` empty → error.
      - `TestParseProviderWriteRejectsMalformedGuardrailVersion`: table test covering a
        few invalid versions (`"0"`, `"01"`, `"+1"`, `"abc"`) all rejected, `"DRAFT"` and
        `""` and `"3"` all accepted (mirrors `config_test.go`'s
        `TestLoadRejectsMalformedGuardrailVersion`/`TestLoadAcceptsValidGuardrailVersion`
        table shape).
      - `TestParseProviderWriteRejectsGuardrailOnNonBedrockType`: `type: "anthropic"` +
        `guardrail_id` set → error.
      Also extend the malformed-version table test (or add a case) covering an
      oversized/control-character/whitespace-only `guardrail_id`, mirroring
      `adminapi/teams_test.go:182`'s equivalent cases for the new checks (a)-(c).

---

### Task 4: ProviderView echo-back

**Files:**
- Modify: `internal/server/configapi/config.go`
- Test: `internal/server/configapi/config_test.go`
- Test: `internal/server/configapi/export_test.go`

Steps:
- [ ] Add `GuardrailID string \`json:"guardrail_id,omitempty"\`` and `GuardrailVersion
      string \`json:"guardrail_version,omitempty"\`` fields to `ProviderView` (same
      non-secret category as the existing `Region` field — doc comment should say so).
- [ ] In `ViewFrom`, add `GuardrailID: p.GuardrailID, GuardrailVersion:
      p.GuardrailVersion` to the `ProviderView` literal built per provider (same line as
      the existing `Region: p.Region`).
- [ ] Extend `TestViewFromNeverLeaksSecrets` or add a case asserting a provider with
      `GuardrailID`/`GuardrailVersion` set produces a view carrying both non-secret values
      unchanged (mirrors how `Region` is already asserted, if it is; otherwise add a
      minimal new assertion following `TestViewAuthStrings`'s fixture shape).
- [ ] Add a test in `internal/server/configapi/export_test.go` asserting a
      `config.ProviderConfig{Type: "bedrock", GuardrailID: ..., GuardrailVersion: ...}`
      passed through `ExportDocFrom` and re-marshaled/unmarshaled round-trips both
      fields. This is genuinely zero-code-diff in `export.go` itself (it serializes
      `config.ProviderConfig` directly, which already carries the json-tagged fields) —
      the test exists to pin that behavior against a future refactor, not because
      `export.go` needs a change (plan-gate round 1 finding).

---

### Task 5: admin console provider form

**Files:**
- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Test: `internal/server/adminui/adminui_test.go`

Steps:
- [ ] In `index.html`'s provider form (`<form id="provider-form">`, inside the
      `provider-write` card), add two inputs right after `pf-authmode`:
      `<input id="pf-guardrail-id" type="text" placeholder="guardrail ID override (optional, bedrock)">`
      and
      `<input id="pf-guardrail-version" type="text" placeholder="guardrail version (optional, default DRAFT)">`
      — same placeholder wording style as the team-form `tf-guardrail-*` inputs.
- [ ] In `app.js`'s `PROVIDER_FIELDS.bedrock` array, add `"pf-guardrail-id"` and
      `"pf-guardrail-version"` (they should only show for the bedrock type, matching the
      write-path's type=="bedrock" requirement from Task 3). Add both ids to
      `PROVIDER_FIELD_IDS` too, so `applyProviderTypeFields` hides them for non-bedrock types.
- [ ] In `app.js`'s `providerFormBody()`, add: read `pf-guardrail-id`/`pf-guardrail-version`
      trimmed values, and if the id is non-empty set `body.guardrail_id` (and
      `body.guardrail_version` if non-empty) — same `if (val) body.x = val` shape as the
      existing region/authmode lines.
- [ ] In `app.js`'s `fillProviderForm(p)`, add prefill lines
      `$("pf-guardrail-id").value = p.guardrail_id || "";` and the version equivalent,
      using the `ProviderView` fields Task 4 added.
- [ ] Write `TestAdminUI_providerGuardrailFieldsWired` in `adminui_test.go`, mirroring
      `TestAdminUI_guardrailFieldsWired` (the team-form version): assert `index.html`
      contains `id="pf-guardrail-id"` and `id="pf-guardrail-version"`, assert no inline
      event-handler/style attributes were introduced (same CSP banned-attribute loop),
      and assert `app.js` contains the submit-body line
      (`body.guardrail_id = $("pf-guardrail-id")...`) and the prefill line
      (`$("pf-guardrail-id").value = p.guardrail_id`).

---

## Verification

- Unit: each task's red→green as specified above.
- Integration: `go test ./... -race` full green — especially
  `internal/providerstore/*_test.go` (schema/migration/overlay/seed), `TestNoSecretColumn`,
  `internal/server/configapi/*_test.go` (write/view), `internal/server/adminui/adminui_test.go`,
  and the existing bedrock guardrail wire tests (`guardrail_wire_test.go`, both ingresses) —
  these must stay green unchanged since this plan touches no bedrock-consumption code path.
- Manual/E2E scenario (record in the harness report): register a bedrock provider via
  `PUT /admin/providers/{name}` with `guardrail_id`/`guardrail_version` set → confirm
  `GET /admin/config` echoes both back → confirm `GET /admin/config/export` includes both
  in the provider block → after a reload, a request routed to that provider with no team
  override present should carry the provider's default guardrail. `guardrail_wire_test.go`
  (both ingresses) is the WRONG reference for this last check — it only captures per-TEAM
  overrides on `ProxyRequest`. The correct existing pattern for a provider-level DEFAULT is
  `providers/bedrock/guardrail_test.go`'s fake invoker/converser test (plan-gate round 1
  finding) — verify against that fake's captured request instead.
- Seed-bug regression: `TestSeedIfEmptyPreservesGuardrail` (Task 2) must demonstrably have
  failed against pre-fix `rowFromProviderConfig` and pass after.
