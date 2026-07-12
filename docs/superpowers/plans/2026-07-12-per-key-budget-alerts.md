# per-key budget alerts (ADR-017 deferred item)

**Date:** 2026-07-12
**Source:** ADR-017 "Deferred" section — "Per-key budget alerts (needs a per-key
destination design; key_id cannot be a label)."

**Key design insight (resolves both blockers, verified against the actual code):**
"key_id cannot be a label" is a **Prometheus `/metrics` cardinality rule**
(CLAUDE.md: "Metric labels are config-bounded; never label with raw client
input") — it says nothing about a webhook JSON payload body. `governance.go`'s
own `SetBudgetNotify` doc comment invokes this rule to justify skipping the
per-key debit, but the underlying concern it's citing (`Settle`'s comment:
"Key-level spend is deliberately NOT added to `/metrics`") is about the
Prometheus gauge specifically — `internal/alert.Notifier`/`Fire`/`deliver`
never touch Prometheus at all. `key_id` is already routinely shown elsewhere
(admin `/admin/keys`, audit records) — it's an opaque identifier, not a secret.
This plan therefore does **not** need a "per-key destination" at all: it
reuses the **exact same webhook `Notifier`** already wired for team alerts —
a key's alert rides the team's configured webhook, with `key_id` added to the
payload body (never a metric label, still true and untouched by this plan).

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
- [ ] Add `func (n *Notifier) ObserveKey(team, keyID string, spentMicros, limitMicros int64)`:
      same threshold-crossing/dedupe logic as `Observe`, but keyed in the `fired` map on
      `"key:"+keyID` (not `team` — a distinct namespace prefix, so a team name and a key ID
      can never collide in the same map even though both are plain strings) and setting
      `Fire{Team: team, KeyID: keyID, ...}` instead of just `Team`. Do not touch `Observe`
      or its dedupe key shape — team-level fires are unaffected.
- [ ] `deliver`'s hand-built payload map literal (it does NOT derive from `Fire`'s JSON
      tags today) gains a conditional `"key_id"` entry: only set when `fire.KeyID != ""`,
      so a team-level fire's outbound webhook body is byte-identical to before.
- [ ] Test (mirrors the existing in-package `Observe` tests — same `httptest.NewServer` +
      `waitForFires` harness): `TestObserveKey_CrossesThresholdFiresWithKeyID` (payload
      body decoded from the fake webhook server contains `"key_id"`; `Recent()`'s `Fire`
      has `KeyID` set, `Team` also set); `TestObserveKey_DedupeAndRearm` (mirrors
      `TestObserve_RatioDropRearms`'s shape, but for the `"key:"` -prefixed map entry);
      `TestObserveKey_DoesNotCollideWithTeamFiredMap` (call `Observe("acme", ...)` past a
      threshold, then `ObserveKey("acme", "acme", ...)` — a pathological but possible
      string collision between a team name and a key ID — both must fire independently,
      since the map keys are `"acme"` vs `"key:acme"`); `TestDeliver_KeyIDOmittedForTeamFire`
      (an `Observe`-triggered fire's outbound JSON body has no `"key_id"` key at all, not
      even `"key_id":""` — proves the conditional-inclusion guard, not just an empty tag).

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

### Task 3: wire `Notifier.ObserveKey` at assembly time

**Files:**
- Modify: `cmd/inferplane/gateway.go`

Steps:
- [ ] Immediately after the existing `gov.SetBudgetNotify(notifier.Observe)` call (inside
      the same `if ba := cfg.BudgetAlerts; ba != nil { ... }` block — same `notifier`
      instance, no new config read, no new `alert.New(...)` call), add
      `gov.SetKeyBudgetNotify(notifier.ObserveKey)`.
- [ ] No test file for this task (assembly wiring only, `test_required:false`) — covered
      end-to-end by Task 1/2's unit tests plus the existing `TestAlertsHandler_GET_returnsFires`-
      style admin-API test already exercising the real `Notifier`/`Fire` shape through
      `GET /admin/alerts/recent`.

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-017-budget-alert-webhooks.md`: strike the "Per-key budget alerts"
  Deferred bullet, replace with an "implemented" note explaining the key design insight
  (webhook payload ≠ metric label) and naming `Notifier.ObserveKey`/`Governor.SetKeyBudgetNotify`.
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
