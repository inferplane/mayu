# per-key budget alerts (ADR-017 deferred item)

**Date:** 2026-07-12
**Source:** ADR-017 "Deferred" section — "Per-key budget alerts (needs a per-key
destination design; key_id cannot be a label)."

**Key design insight, precisely scoped (plan-gate round 1, codex, corrected the
framing — see below):** "key_id cannot be a label" is a **Prometheus `/metrics`
cardinality rule** (CLAUDE.md: "Metric labels are config-bounded; never label
with raw client input") — it says nothing about a webhook JSON payload body.
`internal/alert.Notifier`/`Fire`/`deliver` never touch Prometheus at all.
`key_id` is already routinely shown elsewhere (admin `/admin/keys`, audit
records) — it's an opaque identifier, not a secret. This much is a correct,
narrow reading of CLAUDE.md's rule, independently confirmed by 3 of 4
plan-gate reviewers against the literal text.

**What this is NOT, corrected per round 1:** this is not merely "discovering
the ADR already permitted per-key alerts all along." ADR-017 §3 explicitly
named the blocker as "a per-key alert would need a per-key destination design
(whose webhook? whose threshold?)" — a real, unanswered design question, not
a misunderstanding. This plan answers it with a **deliberate decision**: reuse
the team's/global configured webhook and threshold list for key-scoped alerts
too (no per-key destination — every key's alert rides the SAME webhook its
team's alerts already use). That is a real design choice worth stating
plainly in the ADR update, not something to imply was always obviously true.

**Zero new config surface.** No new `config.*` fields, no new capability flag
(`configapi.Capabilities.BudgetAlerts` already means "the budget-alerts
subsystem is on," not "team-only" — riding the per-key path on the same flag
is consistent with its existing meaning).

**Confirmed via direct code read:** `Settle`'s per-key debit block
(`governance.go`, `if kp.BudgetMicrosPerMonth > 0 { g.bud.Debit("budget:key:"+keyID, ...) }`)
has no `Spent` read-back today (unlike the team block, which reads back
immediately after its own `Debit` for the utilization gauge) — this plan adds
one, mirroring the team block's exact shape, purely to feed the new hook (no
new metric).

## Task list

### Task 1: `alert.Notifier` gains `ObserveKey` + `Fire.KeyID`

**Files:**
- Modify: `internal/alert/alert.go`
- Test: `internal/alert/alert_test.go`

Steps:
- [ ] Add `KeyID string \`json:"key_id,omitempty"\`` to the `Fire` struct, after `Team`
      (additive — `omitempty` keeps every existing team-only `Fire` JSON byte-identical
      through `GET /admin/alerts/recent`, confirmed the handler serializes `alert.Fire`
      directly with no separate DTO).
- [ ] Add `func (n *Notifier) ObserveKey(team, keyID string, spentMicros, limitMicros int64)`.
      **Plan-gate round 1 CRITICAL-equivalent finding (codex, verified real):** the original
      draft proposed disambiguating team vs. key dedupe state with a `"key:"+keyID` string
      prefix inside the SAME `fired map[string]float64`. Team-name validation
      (`internal/server/adminapi/teams.go`) rejects empty/too-long/`/`/control-chars but
      does **not** forbid a colon — an operator naming a team literally `"key:ik_abc"` would
      collide in that shared map with a real key ID `ik_abc`, corrupting both scopes' dedupe
      state. **Fixed design:** add a second, entirely separate field
      `firedKey map[string]float64` to `Notifier` (alongside the existing `fired` field,
      which stays team-only and untouched) — `ObserveKey`'s dedupe logic is identical to
      `Observe`'s but reads/writes `firedKey[keyID]` instead of `fired[team]`. Two disjoint
      Go maps make a collision structurally impossible, not just improbable — no prefix
      scheme, no shared namespace. Sets `Fire{Team: team, KeyID: keyID, ...}` instead of
      just `Team`. Do not touch `Observe`/`fired` — team-level fires and their dedupe state
      are completely unaffected. **Round-2 nit (opus), must not be skipped:** `New()`
      initializes `fired: map[string]float64{}` today — add the parallel
      `firedKey: map[string]float64{}` there too, or `ObserveKey`'s
      `n.firedKey[keyID] = crossed` write panics on a nil map the first time it runs.
- [ ] `deliver`'s hand-built payload map literal (it does NOT derive from `Fire`'s JSON
      tags today) gains a conditional `"key_id"` entry: only set when `fire.KeyID != ""`,
      so a team-level fire's outbound webhook body is byte-identical to before.
- [ ] Test (mirrors the existing in-package `Observe` tests — same `httptest.NewServer` +
      `waitForFires` harness): `TestObserveKey_CrossesThresholdFiresWithKeyID` (payload
      body decoded from the fake webhook server contains `"key_id"`; `Recent()`'s `Fire`
      has `KeyID` set, `Team` also set); `TestObserveKey_DedupeAndRearm` (mirrors
      `TestObserve_RatioDropRearms`'s shape, but against `firedKey`); `TestObserveKey_TeamNamedLikeKeyIDDoesNotCollide`
      (the actual round-1 regression case: `Observe("ik_abc", ...)` — a team literally named
      like a key ID — past a threshold, then `ObserveKey("other-team", "ik_abc", ...)` with a
      DIFFERENT key ID that happens to equal the team's name; both must fire independently
      and `n.fired`/`n.firedKey` must each hold exactly the entry that belongs to them, proving
      the two maps never share state); `TestDeliver_KeyIDOmittedForTeamFire` (an
      `Observe`-triggered fire's outbound JSON body has no `"key_id"` key at all, not even
      `"key_id":""` — proves the conditional-inclusion guard, not just an empty tag).

### Task 2: `governance.Governor` gains `SetKeyBudgetNotify` + `Settle` wiring

**Files:**
- Modify: `internal/governance/governance.go`
- Test: `internal/governance/governance_test.go`

Steps:
- [ ] Add a field `notifyKeyBudget func(team, keyID string, spentMicros, limitMicros int64)`
      to `Governor`, alongside the existing `notifyBudget` field.
- [ ] Add `func (g *Governor) SetKeyBudgetNotify(f func(team, keyID string, spentMicros, limitMicros int64))`,
      doc-commented: mirrors `SetTeamLookup`/`SetBudgetNotify`'s startup-only-assignment
      posture (nil default, no synchronization); explicitly notes this is the per-key
      ALERT path — `key_id` reaching a webhook payload is fine (unlike a Prometheus label,
      the existing metrics-cardinality rule this package's `Settle` comment already cites
      is untouched: `/metrics` still never carries `key_id`).
- [ ] Update `SetBudgetNotify`'s existing doc comment: it currently says "per-key budgets
      are not observed here" — narrow this to "per-key budgets are not observed by the
      TEAM hook or by `/metrics`" and point at the new `SetKeyBudgetNotify` for the
      key-scoped alert path, so the two comments don't contradict each other.
- [ ] In `Settle`'s per-key debit block (`if kp.BudgetMicrosPerMonth > 0 { ... }`), after
      `g.bud.Debit("budget:key:"+keyID, costMicros, 30*24*time.Hour)`, add a `Spent`
      read-back (`spent := g.bud.Spent("budget:key:"+keyID, 30*24*time.Hour)`, mirroring
      the team block's exact shape) and, if `g.notifyKeyBudget != nil`, call
      `g.notifyKeyBudget(team, keyID, spent, kp.BudgetMicrosPerMonth)`. No new metric —
      this read-back exists only to feed the hook.
- [ ] Update `TestGovernorSettleBudgetNotify_KeyBudgetExcluded`'s comment (currently "Per-key
      budgets must never reach the notify hook" — true only for the TEAM hook now) to say
      "must never reach the TEAM notify hook (a dedicated per-key hook exists separately,
      see TestGovernorSettleKeyBudgetNotify)" — the test body/assertion itself is unchanged
      and still correct (it only asserts the TEAM hook's call count).
- [ ] Test: `TestGovernorSettleKeyBudgetNotify` (mirrors `TestGovernorSettleBudgetNotify`
      exactly, but via `SetKeyBudgetNotify`/`kp.BudgetMicrosPerMonth`, asserting the hook
      receives `(team, keyID, spent, limit)`); `TestGovernorSettleKeyBudgetNotify_UnbudgetedKeySkipped`
      (mirrors `..._UnbudgetedTeamSkipped`, a `KeyPolicy{}` with no budget must not call the
      key hook); a table-style dual-assertion test confirming BOTH hooks fire independently
      when a request has both a team budget AND a key budget configured (team hook gets
      team figures, key hook gets key figures — no cross-talk).

### Task 3: wire `Notifier.ObserveKey` at assembly time + e2e proof

**Plan-gate round 1 finding (codex, real coverage gap):** the original draft marked this
task `test_required:false` on the theory that Task 1/2's unit tests cover the behavior. They
don't: if the one-line `gov.SetKeyBudgetNotify(notifier.ObserveKey)` wiring is simply omitted
from `gateway.go`, every Task 1/2 unit test still passes (they test `Notifier`/`Governor` in
isolation, never the real assembled gateway) — nothing would catch the regression. Fixed by
adding a real e2e test mirroring the existing team-alert one.

**Files:**
- Modify: `cmd/inferplane/gateway.go`
- Test: `cmd/inferplane/e2e_test.go`

Steps:
- [ ] Immediately after the existing `gov.SetBudgetNotify(notifier.Observe)` call (inside
      the same `if ba := cfg.BudgetAlerts; ba != nil { ... }` block — same `notifier`
      instance, no new config read, no new `alert.New(...)` call), add
      `gov.SetKeyBudgetNotify(notifier.ObserveKey)`.
- [ ] Test: `TestE2EKeyBudgetAlertFires`, mirroring `TestE2EBudgetAlertFires`'s exact
      structure (same fake webhook `httptest.NewServer`, same `bootGateway`/config shape,
      same poll-with-deadline pattern) but: the team carries NO team budget (proves the
      fire is genuinely key-scoped, not a team fire that happens to also have a key_id);
      the key is created with `budget_usd_micros` set in the create-key POST body (the
      key-create handler's DTO already accepts `"budget_usd_micros"` —
      `internal/server/adminapi/keys.go`, confirmed field name) sized so one request
      crosses the 0.5 threshold, mirroring the existing test's $15-cost-against-$20-budget
      ratio math. **Round-2 note (opus):** the existing `createKey(t, adminURL, team,
      models)` test helper only marshals `{team, allowed_models}` — it has no budget
      parameter. Don't extend that shared helper (other e2e tests call it and shouldn't
      grow an unused param); this new test builds its own inline `POST /admin/keys` body
      with `budget_usd_micros` included, same auth/decode pattern as `createKey`'s body.
      Assert the received webhook payload's decoded JSON map contains `"key_id"` equal to
      the created key's ID, and that `GET /admin/alerts/recent` also returns a fire with a
      non-empty `key_id`.

### Task 4: admin console shows the key on a key-scoped fire

**Plan-gate round 1 finding (codex, real user-facing gap):** the Alerts card's table
(`internal/server/adminui/static/index.html`'s `#alerts-table`, rendered by `app.js`) has
exactly 5 columns — time/team/threshold/ratio/delivered — and renders every fire's `f.team`
into the "team" column. Without this task, a key-scoped fire would render **indistinguishable
from a real team-level budget alert**: same table, same columns, `Team` still populated
(a key's alert carries its team name too) — an operator would see "team acme crossed 80%"
when it was actually one specific key under acme, not the team's aggregate spend. This is a
console-correctness bug this plan would otherwise ship, not a cosmetic gap.

**Files:**
- Modify: `internal/server/adminui/static/index.html`
- Modify: `internal/server/adminui/static/app.js`
- Test: `internal/server/adminui/adminui_test.go`

Steps:
- [ ] `index.html`: add a 6th `<th>key</th>` to `#alerts-table`'s `<thead>` (after `team`);
      update the two `colspan="5"` references in this card (the "connect to load…" initial
      row and the pattern `emptyRow(5, ...)` call sites in `app.js` below) to `colspan="6"`.
- [ ] `app.js`: in the `#alerts-table` render loop, insert a new `<td>` between the team and
      threshold cells: `tr.appendChild(td(f.key_id || "—"))` — an em dash for a genuine
      team-level fire (no key), the key ID string for a key-scoped one. Update both
      `emptyRow(5, ...)` call sites in this block to `emptyRow(6, ...)`.
- [ ] Test: extend `TestAdminUI_budgetAlertsWired` (or add a sibling test) asserting
      `index.html` contains `<th>key</th>` inside the alerts-table block and `app.js`
      contains `f.key_id`. Mirrors this file's existing string-presence assertion style —
      no new test infrastructure.

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-017-budget-alert-webhooks.md`: strike the "Per-key budget alerts"
  Deferred bullet, replace with an "implemented" note that states this PLAINLY as a design
  decision — key-scoped alerts ride the team's/global webhook and threshold list (no
  per-key destination), answering the ADR's own "whose webhook? whose threshold?" question
  with "the same one" — not as a mere Prometheus-rule clarification (plan-gate round 1,
  codex: the original framing overstated this). Name `Notifier.ObserveKey`/
  `Governor.SetKeyBudgetNotify`/the admin-console key column.
- `internal/CLAUDE.md`: fix two now-stale team-only claims flagged in plan-gate round 1
  (codex) — the `governance/` bullet's `SetBudgetNotify` description and the `alert/`
  bullet's `Notifier` description both currently read as team-exclusive; update both to
  mention the parallel per-key path.
- `docs/reference/api.md` (if it documents the `GET /admin/alerts/recent` response shape):
  add the optional `key_id` field to the documented `Fire` JSON shape.
- `docs/reference/agent-llm.md` or wherever the webhook payload's JSON shape is documented
  for operators (check first — ADR-017 §5 has the canonical example): add a second example
  payload showing a key-scoped fire with `key_id` present.

## Out of scope

- A per-key alert destination distinct from the team's webhook — deliberately not needed;
  see the design insight above. If a real future need for key-specific destinations
  appears (e.g. a customer-facing per-key alert UI), that is a genuinely separate feature.
- Any change to `/metrics` — `AddBudgetSpend`/`SetBudgetUtilization` remain team-only,
  untouched by this plan; the cardinality rule they exist to satisfy is still fully intact.
- A capability flag distinct from `configapi.Capabilities.BudgetAlerts` — the existing flag's
  meaning ("the budget-alerts subsystem is configured") already covers this.
