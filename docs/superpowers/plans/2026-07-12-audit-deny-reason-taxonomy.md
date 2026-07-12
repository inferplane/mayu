# audit deny-reason taxonomy (ADR-020 deferred item)

**Date:** 2026-07-12
**Source:** ADR-020 "Deferred" section — `region_blocked` is set as a plain string on
`audit.OutcomeRef.Error`; "a broader enum/taxonomy across all deny reasons (allow-list,
quota, budget, region) is a larger, separate cleanup." This plan is that cleanup.

**Scope finding (from direct code read):** of the four categories, only region-block
today actually populates `OutcomeRef.Error` (`"region_blocked"`, duplicated verbatim in
both ingress handlers). Allow-list deny and all governance denies (quota + budget, 7
sub-cases across team/key × rate/token-rate/quota/budget) leave `Error` nil today — the
human-readable reason exists only in `governance.GovDecision.Reason`, which reaches the
HTTP response body via `writeErr` but never the audit record. This plan is therefore not
a rename — it adds a deny-reason value to audit records that don't carry one today, for
3 of the 4 categories.

**Wire-format invariant (non-negotiable):** `docs/specs/2026-06-10-inferplane-gateway-design.md`
documents the audit record schema (v0.1) with `"error": null` as a bare nullable string.
`OutcomeRef.Error` stays `*string` — unchanged JSON shape. The "taxonomy" is a closed Go
`type DenyReason string` with named constants; the only new invariant is that every value
ever written into `Error` is now one of those constants, not that the wire shape changes.

**Zero-cycle check (confirmed via `go list`):** `internal/audit`, `internal/governance`,
`internal/router` currently import zero `internal/*` packages from each other — adding
`internal/governance` → `internal/audit` (for the `DenyReason` type) introduces no cycle.

**Test-debt finding:** no existing test asserts on `OutcomeRef.Error`'s string value
anywhere in the repo, and no `governance_test.go` test asserts on `.Reason`. The only
cosmetic risk is `region_wire_test.go`'s `t.Fatalf` message text mentioning `region_blocked`
— untouched by this plan (still exactly `"region_blocked"`).

**Coverage gap found alongside (fold in, don't defer again):** `governance_test.go` has
no dedicated team-RPM test, and the team-TPM/key-TPM/team-budget tests only assert
`.Allowed`, not `.Status`. Task 2 closes these gaps as part of adding `.Code` assertions —
same file, same table, near-zero extra cost, avoids leaving a second half-covered enum.

## Task list

### Task 1: `audit.DenyReason` taxonomy type

**Files:**
- Create: `internal/audit/deny_reason.go`
- Test: `internal/audit/deny_reason_test.go`

Steps:
- [ ] Add `type DenyReason string` to `internal/audit/deny_reason.go` with a doc comment
      explaining the wire-format invariant (values are the only strings ever written to
      `OutcomeRef.Error`; the field itself stays `*string`, no JSON shape change).
- [ ] Define 9 exported constants covering all four categories: `DenyModelNotAllowed`
      (allow-list), `DenyTeamRateLimited`, `DenyTeamTokenRateLimited`,
      `DenyTeamQuotaExceeded`, `DenyKeyRateLimited`, `DenyKeyTokenRateLimited` (quota),
      `DenyTeamBudgetExceeded`, `DenyKeyBudgetExceeded` (budget), `DenyRegionBlocked`
      (region — value stays the literal `"region_blocked"` string already in the audit
      log today, so existing audit history doesn't change meaning retroactively).
- [ ] Add a `func (d DenyReason) Ptr() *string` helper (`s := string(d); return &s`) —
      every call site needs a `*string`, not a `DenyReason`, to assign into
      `OutcomeRef.Error`.
- [ ] Test: pin the exact string value of each of the 9 constants (a rename here is a
      breaking change to any external audit-log consumer coding against the taxonomy, so
      this must fail loudly) and one case asserting `Ptr()` returns a non-nil pointer to
      the correct string.

### Task 2: thread `Code` through `governance.GovDecision`

**Files:**
- Modify: `internal/governance/governance.go`
- Test: `internal/governance/governance_test.go`

Steps:
- [ ] Import `github.com/inferplane/inferplane/internal/audit` in `governance.go`.
- [ ] Add `Code audit.DenyReason` field to the `GovDecision` struct (alongside the
      existing `Status`/`Reason`/`Allowed` fields).
- [ ] Set `Code` at each of the 7 deny-returning sites in `PreCheck`, pairing with the
      existing `Reason` string on the same line: team RPM → `audit.DenyTeamRateLimited`,
      team TPM → `audit.DenyTeamTokenRateLimited`, team quota → `audit.DenyTeamQuotaExceeded`,
      team budget → `audit.DenyTeamBudgetExceeded`, key RPM → `audit.DenyKeyRateLimited`,
      key TPM → `audit.DenyKeyTokenRateLimited`, key budget → `audit.DenyKeyBudgetExceeded`.
      The allowed (non-deny) return path sets no `Code` (zero value, never read).
- [ ] Test: add a `.Code` assertion to each of the 7 existing/new sub-case tests. For the
      3 that currently only assert `.Allowed` (`TestGovernorTPMBlocks`,
      `TestGovernorKeyTPMBlocks`, `TestGovernorCountersIndependentOfTable`), also add the
      missing `.Status` assertion while there (closes the coverage gap noted above — same
      test function, two extra lines, not a new test).
- [ ] Test: add a new `TestGovernorTeamRPMBlocks` (no dedicated team-RPM test exists
      today) mirroring `TestGovernorKeyRPMBlocksIndependentlyOfTeam`'s shape — repeated
      `PreCheck` calls past the team's configured `RatePerMin`/`RateBurst`, assert
      `Status == 429` and `Code == audit.DenyTeamRateLimited`.

### Task 3: wire the taxonomy into the Anthropic ingress

**Files:**
- Modify: `internal/server/anthropicapi/messages.go`
- Test: `internal/server/anthropicapi/deny_reason_wire_test.go`

Steps:
- [ ] Model-not-allowed site (`h.audit(p, model, "", &audit.OutcomeRef{Status: 403}, ...)`,
      the block right after `if !h.r.Allows(p, model) {`): add
      `Error: audit.DenyModelNotAllowed.Ptr()` to the `OutcomeRef` literal.
- [ ] Region-blocked site (`if len(teamRec.AllowedRegions) > 0 { if filtered := ...
      len(filtered) == 0 {`): delete the local `errMsg := "region_blocked"` var; use
      `Error: audit.DenyRegionBlocked.Ptr()` directly in the `OutcomeRef` literal.
- [ ] Governance-deny site (`if h.gov != nil { dec := h.gov.PreCheck(...); if
      !dec.Allowed {`): add `Error: dec.Code.Ptr()` to the `OutcomeRef` literal (replacing
      the current `&audit.OutcomeRef{Status: dec.Status}` with no `Error`).
- [ ] Test (new file, mirrors `region_wire_test.go`'s `audit.NewWriter` +
      `audit.NewWriterSink(&buf)` + `strings.Contains`/`json.Unmarshal` pattern from
      `messages_test.go:442-446`): table-driven, one case per category exercised through
      the real handler — model-not-allowed (key with a restrictive `AllowedModels`),
      team-quota-exceeded (a `Governor` primed past its team quota), key-budget-exceeded
      (a key primed past its budget), region-blocked (existing fixture from
      `region_wire_test.go` reused). Assert the captured record's `Outcome.Error` decodes
      to exactly the expected `DenyReason` string for each case.

### Task 4: wire the taxonomy into the OpenAI-compatible ingress

**Files:**
- Modify: `internal/server/openaiapi/chat.go`
- Test: `internal/server/openaiapi/deny_reason_wire_test.go`

Steps:
- [ ] Mirror all three Task 3 edits at `chat.go`'s equivalent three sites (model-not-
      allowed, region-blocked, governance-deny — same field, same constants).
- [ ] Test: mirror Task 3's wire test (same four cases, through `ChatHandler` instead of
      `MessagesHandler`), reusing `region_wire_test.go`'s existing OpenAI-side fixture for
      the region case.

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-020-per-team-region-locking.md`: strike the "Audit deny-reason
  taxonomy" Deferred bullet, replace with an "implemented" note pointing at this plan doc
  and naming the 9 `audit.DenyReason` constants.
- `docs/specs/2026-06-10-inferplane-gateway-design.md`'s record-schema (v0.1) example:
  add a short note next to the `"error": null` example line that non-null values are one
  of a closed set of string codes (list them), not free text — no JSON shape change.
- `docs/reference/data.md`: if it documents `OutcomeRef`/audit record fields, add one line
  noting `error` is now taxonomy-constrained (reference `internal/audit/deny_reason.go`).

## Out of scope

- The 404 (unresolvable model), 400 (malformed body / PII-mask reject), and `admin_denied`
  paths — these either don't use `OutcomeRef` at all (`admin_denied`) or share the same
  nil-`Error` shape but are not governance-policy denies; ADR-020's bullet names
  allow-list/quota/budget/region specifically. Not touched here.
- `count_tokens`'s region-block "gap" (ADR-020 §7, by design never returns non-200 and
  never audits) — unrelated to how `Error` is labeled when audited elsewhere.
- Re-deriving old audit log entries — this only changes what NEW records look like; old
  `"region_blocked"` entries already match the new constant's value, everything else
  simply gains a value it didn't have (no migration needed, no old record becomes wrong).
