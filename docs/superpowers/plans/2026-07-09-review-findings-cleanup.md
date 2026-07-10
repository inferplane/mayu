# Review-findings cleanup (PR #21/#22/#23 MEDIUM/LOW)

**Source:** claude-review comments on merged PRs #21 (D4/ADR-018), #22 (D6/ADR-019),
#23 (D7/ADR-020) — re-fetched verbatim via `gh pr view --json comments` before this
plan was written. Only MEDIUM/LOW findings that have a real, in-scope code fix are
tasked here (per the standing policy: report but don't act on findings without a
concrete fix, and don't touch what the reviewer itself flagged as a documented design
decision). Two items are explicitly OUT of scope — see "Not in this plan" below.

## Scope

Nine independently-testable fixes across body storage, audit records, team-policy
plumbing, and admin-API validation. `harness.parallel_tasks` is `1` in this repo, so
these run through the sequential per-task loop, not as concurrent worktrees — several
tasks share a file (`chat.go`/`messages.go`: Tasks 2/3/4; both `guardrail_wire_test.go`
files: Tasks 3/4; `teams.go`/`teams_test.go`: Tasks 6/8), which is fine sequentially
but would conflict if ever run as parallel waves.

## Global constraints (apply to every task)

- Go 1.25, `gofmt`-clean, `go vet`-clean. Errors wrapped with `%w`.
- No new exported surface beyond what each task states — this is a findings cleanup,
  not a feature PR.
- `internal/audit/record.go` (Task 3): new fields append at the struct **END**,
  `omitempty` pointers, mirroring `AuthMethod`/`BodyRef`/`RecordRef` — a pre-existing
  mixed-version-chain test asserts old records verify byte-identically; do not reorder
  existing fields.
- Task 1 (pgstore) integration test requires `INFERPLANE_TEST_PG_DSN`; skip via `t.Skip`
  when unset (existing pgstore test convention) — the SQL fix itself must still be
  covered by a DSN-independent unit assertion (e.g. a string-constant check on the
  query) so the fix is verifiable without Postgres.

## Task 1: pgstore rebuild no longer clobbers `body_ref` on replay

`internal/analytics/pgstore` re-ingesting a pre-D4 record (`BodyRef=""`) via
`ON CONFLICT(id) DO UPDATE` currently overwrites a previously-captured `body_ref` with
`''`, making a captured body unreachable from `/admin/logs` after a rebuild.

- Modify: `internal/analytics/pgstore/pgstore.go`
- Test: `internal/analytics/pgstore/pgstore_test.go`

Steps:
- [ ] Write a test asserting the `upsertEvent` query text preserves `body_ref` when
      the incoming value is empty (`COALESCE`/`CASE` pattern) — a string-constant
      assertion that runs without Postgres.
- [ ] Write (or extend) an `INFERPLANE_TEST_PG_DSN`-gated integration test: insert an
      event with a non-empty `body_ref`, re-upsert the same `id` with `body_ref=""`,
      assert the stored `body_ref` is unchanged.
- [ ] Fix the `ON CONFLICT(id) DO UPDATE SET` in `upsertEvent` (`pgstore.go`) so
      `body_ref` only updates when the incoming value is non-empty. Referencing the
      target table's own name (`events`) alongside `excluded` inside `DO UPDATE SET`
      is standard Postgres syntax (the same pattern as the official docs' `distributors
      ON CONFLICT ... SET dname = EXCLUDED.dname || ... || distributors.dname`
      example) — not a syntax error. Guard NULL as well as `''` (the column is
      `NOT NULL DEFAULT ''` today, but `NULLIF` costs nothing and removes the
      dependency on that column default never changing):
      `body_ref = COALESCE(NULLIF(excluded.body_ref, ''), events.body_ref)`.

## Task 2: OpenAI ingress captures the bytes actually sent to the client

`internal/server/openaiapi/chat.go`'s `serveComplete` writes
`openai.ResponseFromCanonical(resp.Parsed)` to the client when routing through a
non-openai-wire provider, but captures `resp.RawBody` (the upstream's native wire
format) for body logging — the two diverge, so the body drawer shows the wrong shape.

- Modify: `internal/server/openaiapi/chat.go`
- Test: `internal/server/openaiapi/body_capture_wire_test.go`

Steps:
- [ ] Write a test routing an anthropic-wire (or bedrock) fake provider through the
      OpenAI ingress with `log_bodies` on, asserting the captured response body
      (fetched back from the recorder) is the OpenAI-shaped JSON actually written to
      the client. Assert this with a POSITIVE shape check (e.g. the captured bytes
      parse as an OpenAI `chat.completion` object) AND a NEGATIVE check that the
      captured bytes are NOT byte-equal to the fake provider's anthropic-wire
      `resp.RawBody` — a test that only checks "captured == written" would pass
      trivially if both sides were still wrongly using `RawBody`.
- [ ] In `serveComplete`, compute the client-facing response bytes once (the existing
      `openai.ResponseFromCanonical(resp.Parsed)` / `resp.RawBody` branch) into a local
      variable, `w.Write` that variable, and pass the SAME variable to
      `h.bodies.Capture(...)` instead of `resp.RawBody`.

## Task 3: applied guardrail recorded in the audit chain

Bedrock guardrail overrides (team or provider default) are applied but never recorded
on the audit record, so the tamper-evident log cannot later prove which policy governed
a given request — and a team's `guardrail_id` is mutable, so this is not otherwise
reconstructible after the fact.

- Modify: `internal/audit/record.go`
- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/openaiapi/chat.go`
- Test: `internal/audit/record_test.go`
- Test: `internal/server/anthropicapi/guardrail_wire_test.go`
- Test: `internal/server/openaiapi/guardrail_wire_test.go`

Exact contract — appended at the struct END, after `RecordRef`:

```go
// GuardrailID and GuardrailVersion (D6/D7 cleanup) record the Bedrock guardrail
// actually stamped onto the request (team override else provider default), so the
// audit chain can prove after the fact which policy governed a request even though
// a team's guardrail_id is mutable. Empty/omitted for non-Bedrock requests.
GuardrailID      *string `json:"guardrail_id,omitempty"`
GuardrailVersion *string `json:"guardrail_version,omitempty"`
```

Steps:
- [ ] Write/extend the mixed-version-chain fixture test (same pattern as
      `TestMixedVersionChainVerifies_BodyRef`) asserting an old record without these
      fields still verifies byte-identically, and Canonical() of an old fixture is
      unchanged.
- [ ] Add `GuardrailID`/`GuardrailVersion` to `audit.Record` per the contract above.
- [ ] In `messages.go`/`chat.go`, thread the effective `pr.GuardrailID`/
      `pr.GuardrailVersion` (the same values stamped onto `ProxyRequest`) into
      `auditCompleted`'s call so a request routed through Bedrock with a guardrail
      applied gets non-nil `GuardrailID`/`GuardrailVersion` on its
      `request_completed` record. **Nil-vs-empty is load-bearing**: only take the
      address of `pr.GuardrailID`/`pr.GuardrailVersion` when the string is
      non-empty (`omitempty` on a non-nil pointer-to-`""` still serializes as
      `"guardrail_id":""` — it does NOT omit) — a small helper
      (e.g. `func strPtrOrNil(s string) *string`) returning `nil` for `""` is
      the simplest correct shape; non-Bedrock / no-guardrail requests leave both
      nil on the record.
- [ ] Write a wire test per ingress asserting the audit record captures the stamped
      guardrail id/version when one is applied, and both are `nil` (not
      pointer-to-`""`) when none is applied.

## Task 4: `messages.go`/`chat.go` stop discarding `teamPolicy`'s `ok`

`count_tokens.go` correctly gates on `teamPolicy`'s `ok` return; `messages.go` and
`chat.go` discard it (`teamRec, _ = h.teamPolicy(p.Team)`), which is harmless with
today's single `teamPolicy` implementation but a latent hazard for any future
implementation that returns a non-zero record with `ok=false`.

- Modify: `internal/server/anthropicapi/messages.go`
- Modify: `internal/server/openaiapi/chat.go`
- Test: `internal/server/anthropicapi/guardrail_wire_test.go`
- Test: `internal/server/openaiapi/guardrail_wire_test.go`

Steps:
- [ ] Write a test with a fake `teamPolicy` returning `(TeamRecord{GuardrailID: "gr-x",
      AllowedRegions: []string{"eu"}}, false)` — assert the request is NEITHER
      guardrail-stamped NOR region-filtered (i.e. `ok=false` is honored), matching
      `count_tokens.go`'s existing behavior.
- [ ] In both handlers, replace `teamRec, _ = h.teamPolicy(p.Team)` with
      `if rec, ok := h.teamPolicy(p.Team); ok { teamRec = rec }`.

## Task 5: config-file `guardrail_version` format validated at load time

The admin API rejects a `guardrail_version` that isn't `""`/`"DRAFT"`/numeric; the
config-file loader only checks version-without-id, so a config file with
`"guardrail_version": "latest"` loads cleanly and then fails every Bedrock request at
runtime with an AWS `ValidationException`.

- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

**Canonical version rule (shared verbatim with Task 6 — do not let the two diverge):**
a version is valid iff it is `""`, or `"DRAFT"`, or parses via `strconv.Atoi` to `n >= 1`
**and** `strconv.Itoa(n) == version` (the round-trip check is what actually rejects
leading zeros and a leading `+` — `strconv.Atoi("01")` returns `(1, nil)`, not an
error, so an `err != nil || n < 1` check alone accepts `"01"`; verified directly:
`Atoi("01")→(1,nil)`, `Atoi("+1")→(1,nil)`, `Atoi("0")→(0,nil)`. `Itoa(1)` is `"1"`,
which doesn't match `"01"` or `"+1"`, so the round-trip check rejects both while still
accepting `"1"`/`"42"`).

Steps:
- [ ] Write a test: `ResolveProviders` (or `Load`) rejects a bedrock provider config
      with `guardrail_version: "latest"`, `"0"`, `"01"`, and `"+1"`; accepts `""`,
      `"DRAFT"`, `"1"`, `"42"`.
- [ ] In `ResolveProviders` (near the existing `GuardrailVersion`/`GuardrailID` checks,
      `config.go` ~line 676), add a format check applying the canonical version rule
      above — reject anything that fails it with a
      `config: provider %q guardrail_version …` error.

## Task 6: adminapi teams validation — whitespace id, `"0"` version, empty region

Three related shape gaps in `internal/server/adminapi/teams.go`'s validators, each
letting a value through that fails at Bedrock/round-trip time instead of at the PUT:

- Modify: `internal/server/adminapi/teams.go`
- Test: `internal/server/adminapi/teams_test.go`

Steps:
- [ ] Write test cases: `validateGuardrailFields(" ", "")` rejected (whitespace-only
      id); `validateGuardrailFields("gr-x", "0")`, `"01"`, and `"+1"` all rejected;
      `validateGuardrailFields("gr-x", "1")` and `"42"` accepted;
      `validateAllowedRegions([]string{""})` rejected;
      `validateAllowedRegions([]string{"eu", ""})` rejected;
      `validateAllowedRegions([]string{" "})` rejected (whitespace-only).
- [ ] `validateGuardrailFields`: after the length/control-char guard, add
      `if strings.TrimSpace(id) == "" && id != "" { return fmt.Errorf(...) }` — reject
      a non-empty id that is whitespace-only (empty stays valid: "no override").
- [ ] `validateGuardrailFields`: replace the numeric-digit loop with the canonical
      version rule from Task 5 — `n, err := strconv.Atoi(version)`, reject when
      `err != nil || n < 1 || strconv.Itoa(n) != version`. The round-trip
      (`Itoa(n) != version`) is required, not optional: `strconv.Atoi("01")` and
      `strconv.Atoi("+1")` both return `(1, nil)` — a bare `n < 1` check alone
      accepts both; comparing back against `strconv.Itoa(n)` ("1") is what actually
      rejects the non-canonical forms while still accepting `"1"`/`"42"`.
- [ ] `validateAllowedRegions`: in the per-entry loop, add
      `if strings.TrimSpace(region) == "" { return fmt.Errorf("allowed_regions entries must not be empty") }`
      before the comma/control-char checks (rejects both `""` and whitespace-only
      entries).

## Task 7: bodies handler defense-in-depth + SQLite writer serialization

Two independent hardening fixes from the D4 review, both low-risk/low-blast-radius:

- Modify: `internal/server/adminapi/bodies.go`
- Modify: `internal/bodystore/sqlite.go`
- Test: `internal/server/adminapi/bodies_test.go`

Steps:
- [ ] Write a test: a request context carrying an admin identity with `IsAdmin=false`
      gets 403 from `BodiesHandler.ServeHTTP` (today it only checks context-presence).
- [ ] In `bodies.go`'s `ServeHTTP`, change `if !ok {` to `if !ok || !id.IsAdmin {`
      (mirrors `requireAdmin`'s exact invariant) — defense-in-depth in case this
      handler is ever mounted without the `requireAdmin` wrapper.
- [ ] In `bodystore/sqlite.go`'s `OpenSQLite`, add `db.SetMaxOpenConns(1)` right after
      `sql.Open` succeeds (same call as `keystore/sqlite.go`'s existing
      `db.SetMaxOpenConns(1)`) — idiomatic single-writer guard for this codebase's
      SQLite stores; no new test needed (behavior is a driver-level serialization
      knob, not independently observable without a flaky concurrency test).

## Task 8: config-only team regions surfaced so the console can pre-fill them

`TeamsHandler.configTeams` returns names only (`func() []string`), so
`GET /admin/teams`'s `"source":"config"` rows carry no `allowed_regions`. The console's
`fillTeamForm` therefore always pre-fills an empty regions field for a config-only
team, so the admin's first-ever PUT on that team (e.g. just to set RPM) silently
erases the config-declared region restriction — ADR-020 documents this as a known
sharp edge; this task closes it by surfacing the config value so it round-trips.

- Modify: `internal/server/adminapi/teams.go`
- Modify: `internal/server/server.go`
- Modify: `cmd/inferplane/gateway.go`
- Modify: `internal/server/adminui/static/index.html`
- Test: `internal/server/adminapi/teams_test.go`
- Test: `internal/server/server_test.go`
- Test: `internal/server/adminui/adminui_test.go`

Steps:
- [ ] Write a test: `NewTeamsHandler(store, configTeamsFn, nil)` where `configTeamsFn`
      returns a `keystore.TeamRecord{Name: "eu-team", AllowedRegions: []string{"eu"}}`
      with no matching DB record — assert `GET /admin/teams`'s `"source":"config"` row
      for `eu-team` includes `"allowed_regions":["eu"]`.
- [ ] Change `TeamsHandler.configTeams` field and `NewTeamsHandler`'s parameter type
      from `func() []string` to `func() []keystore.TeamRecord` (each entry needs only
      `Name` + `AllowedRegions` populated — other fields stay zero, config has no
      other per-team values). Update `list()` to iterate the returned records directly
      (`teamView(rec, "config")` instead of synthesizing `keystore.TeamRecord{Name: name}`).
      In `teamView`, the `if source != "record" { return v }` early-return currently
      returns before adding anything past `name`/`source` — change that branch to
      `v["allowed_regions"] = t.AllowedRegions; return v` (still returning
      immediately after, still skipping every other field) so a config-sourced row
      gains exactly one extra key and nothing else.
- [ ] Update `server.go`'s `AdminMux` parameter type
      (`configTeams func() []string` → `func() []keystore.TeamRecord`) and its
      `server_test.go` call sites.
- [ ] Update `gateway.go`'s `configTeams` closure to build
      `[]keystore.TeamRecord{{Name: name, AllowedRegions: tc.AllowedRegions}, ...}`
      from `cfg.Teams` instead of names only.
- [ ] Add a one-line hint to the team form in `index.html` near `tf-regions`:
      submitting this form replaces any config-declared region policy for that team
      unless the field is filled in (the console now pre-fills it automatically per
      the fix above, so this is a backstop notice, not the primary mitigation).
      Cover with the existing adminui static-asset wire-test pattern
      (`adminui_test.go`) asserting the hint text is present.

## Task 9: Logs view degrades gracefully when `logs_bodies` is on but `analytics_index` is off

`app.js`'s `refreshLogsView` hides the entire Logs view whenever `analytics_index` is
off, even if `logs_bodies` is independently on — an operator who enables body logging
without the analytics index has no path to discover captured bodies via the console.

- Modify: `internal/server/adminui/static/app.js`
- Test: `internal/server/adminui/adminui_test.go`

Steps:
- [ ] Write a wire test (existing pattern) asserting `app.js` contains a
      degraded-message branch for `logs_bodies` on / `analytics_index` off.
- [ ] In `refreshLogsView` (`app.js` ~line 1050), change the early return
      `if (!capOn("analytics_index")) { content.hidden = true; return; }` to: when
      `capOn("logs_bodies")` is also true, show `content` with a short message
      instead of hiding it — word it as an actionable explanation, not just a status
      ("Body logging is on, but the analytics index is off, so captured bodies
      aren't browsable here. Enable analytics_index to browse them in the console."),
      then `return` so the branch doesn't fall through into analytics-index queries
      that would error while the index is off; otherwise keep the existing hidden
      behavior.

## Not in this plan (explicitly deferred)

- **#23 MEDIUM — keystore fail-open bypasses region enforcement during a store
  outage.** The reviewer itself classified this as "a documented design decision, not
  an oversight," consistent with the project's existing governance fail-open posture;
  a configurable fail-closed mode needs its own ADR, not a findings-cleanup patch.
- The stray 45 MB `inferplane` binary committed in PR #21 and the `existingColumns`
  PRAGMA-concatenation comment are handled directly by the host outside this plan (no
  failing test applies to either — one is a `git rm`, the other is a code comment with
  no behavior change).
