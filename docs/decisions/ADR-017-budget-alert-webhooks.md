# ADR-017: Budget-alert webhooks (D5b)

**Date:** 2026-07-08
**Status:** Accepted (implemented).
**Related:** §6.7/§8 D5b of `docs/superpowers/specs/2026-06-26-admin-console-litellm-ux-redesign-design.md`
(the console UX spec's nominal ADR-018 slot — see numbering note below);
ADR-013 (multi-replica HA — the per-instance counter caveat this ADR's
alerting state repeats); `internal/governance/` (the Settle choke point this
hooks into); `internal/budget/` (extended with a read method).

**Numbering note:** the spec's §14 table nominally assigns budget alerts
ADR-018 and body logging ADR-017. This PR landed first (per the user-confirmed
PR order: alerts before body logging), and per CLAUDE.md's rule ("find the
highest number, increment by 1") it takes the next available slot — ADR-017.
Body logging becomes ADR-018 when it lands.

## Context

Before this change, a team's monthly budget was enforced (block/warn at the
configured limit, `internal/governance.Governor.Settle`) but there was no way
to know a team was *approaching* its limit short of watching `/metrics` or the
console's cumulative budget-spend table. The spec (§6.7, "#17 Budget alert")
calls for threshold-based notification (e.g. 80%/100%) to a destination
(webhook/SNS/Slack), which did not exist in any form — `internal/budget` only
supported `Check`/`Debit`, with no way to read current spend, and there was no
notification/webhook code anywhere in the repo.

User-confirmed scope (2026-07-08):
1. **Generic webhook only** — no native SNS or Slack SDK integration. A Slack
   incoming-webhook URL and an SNS HTTPS-subscription endpoint are both plain
   HTTP(S) POST targets, so one webhook emitter covers all three destinations
   the spec names without adding an AWS SDK dependency for SNS.

## Decision

### 1. `budget.BudgetStore.Spent` — a read method, mirroring the quota gauge precedent

`internal/limiter.LimiterStore` already exposes `QuotaUsed(key, window) int64`
specifically so `Governor.Settle` could compute and publish a quota
utilization *ratio*, not just enforce a binary block. `budget.BudgetStore` had
no equivalent — `Memory.win.spent` existed internally but nothing read it back.
Adding `Spent(key, window) int64` (returning 0 for a missing/elapsed window,
identical semantics to `QuotaUsed`) closes that gap with the same shape,
enabling both a new `inferplane_budget_utilization_ratio{team}` gauge and the
alert threshold evaluation below. `budget.Memory`'s single implementation
locks and returns `cur(key, window).spent`.

### 2. A new leaf package, `internal/alert`, not folded into `governance` or `budget`

`internal/alert.Notifier` knows nothing about governance, budget storage, or
config. It exposes one entry point, `Observe(team string, spentMicros,
limitMicros int64)`, called synchronously from `Governor.Settle` right after
the existing team-budget debit. The math (compute ratio, compare against a
sorted threshold list, decide whether a *new* threshold was crossed) is cheap
and runs inline; delivery (the actual HTTP POST) happens on a spawned
goroutine so a slow or unreachable webhook never adds request latency to the
billing hot path — the same "cheap synchronous decision, async side effect"
shape as the existing PII-mask/metrics hooks.

`Governor` gains a single optional field, `notifyBudget func(team string,
spentMicros, limitMicros int64)`, installed via `SetBudgetNotify` — the exact
pattern `SetTeamLookup` (ADR-016) established: a startup-only assignment, no
synchronization, nil (default) reproduces today's behavior exactly.

### 3. Scope: team budgets only, never per-key

Settle's per-key budget debit (`"budget:key:"+keyID`) is deliberately **not**
observed by the notify hook or the new utilization gauge. A `key_id` must
never become a metric or alert-destination label (CLAUDE.md's cardinality
rule, already the reason `AddBudgetSpend` carries no key dimension) — a
per-key alert would need a per-key destination design (whose webhook? whose
threshold?) that is out of scope here. Per-key budget alerting is deferred.

### 4. Dedupe: highest-threshold-once, ratio-drop re-arms

Given thresholds `[0.8, 1.0]`, a team's spend rising past 0.8 fires once (not
on every subsequent Settle call while still between 0.8 and 1.0); rising past
1.0 fires again (once). `Notifier` tracks the highest threshold already fired
per team in `fired[team]`. Re-arming — allowing 0.8 to fire again — happens
when the observed ratio drops below the last-fired threshold, which can only
happen when the 30-day budget window rolled over (or an admin raised the
limit). This is a **heuristic**: `BudgetStore` exposes no `windowEnd`, so a
ratio drop is inferred rather than observed directly.

```go
// ponytail: ratio-drop heuristic instead of exposing windowEnd from
// BudgetStore; widen the interface if a real edge case needs it.
```

The alternative — exposing `windowEnd` on `BudgetStore` — was rejected as
premature: no caller needs it except this heuristic's edge case (a limit
increase mid-window without a ratio drop, which would delay re-arming rather
than mis-fire), and it would grow the `BudgetStore` interface for every
implementation (`Memory` today) for a case with no observed failure.

### 5. Webhook payload and delivery

One HTTP POST, JSON body:
```json
{"event":"budget_alert","team":"acme","threshold":0.8,"ratio":0.83,
 "spent_usd_micros":830000,"limit_usd_micros":1000000,"ts":"2026-07-08T12:00:00Z"}
```
No retry — a missed alert is not re-delivered; the next threshold crossing (or
the next window) will fire again. `http.Client` has a configurable timeout
(default 5s) so an unreachable destination cannot leak goroutines indefinitely.
Each delivery goroutine is tracked by a `sync.WaitGroup`; `Notifier.Close()`
(called on the graceful-shutdown path after the data plane drains) waits for
in-flight deliveries so a rolling deploy's last-window alerts are not silently
abandoned — the wait is bounded by the per-delivery client timeout.

**Why this client has no `DialContext` metadata-IP guard, unlike the connection
probe** (`internal/server/configapi/probe.go`): the probe's guard exists
because its target is **request-time, admin-API-driven** — any authenticated
admin session can direct the gateway to dial an arbitrary host on demand,
repeatedly, which is a classic confused-deputy/SSRF surface. The webhook
destination is **boot-time, config-file/env-var-driven** — the same trust
level as the OTel exporter endpoint and the S3 anchor endpoint, neither of
which carries this guard either. Whoever can set the gateway's config/env
already controls the process. Additionally, unlike the probe (whose result is
returned to the caller), the webhook's delivery outcome exposed via
`GET /admin/alerts/recent` is a **fixed classification string**
(`Fire.Error`, e.g. `"webhook delivery failed"`) — never the raw response
body or error text — so even a misdirected request cannot exfiltrate a
metadata-endpoint response through the admin API.

### 6. The webhook URL is a secret reference, never inline

A Slack incoming-webhook URL and an SNS HTTPS-subscription URL both embed a
capability token in the URL path/query — the same trust level as an API key.
`config.BudgetAlertsConfig.WebhookURLRef` follows the existing `SecretRef`
(env/file) contract (§7); an inline `webhook_url` key is rejected at
`LoadRaw`, mirroring the `analytics.mode_b.dsn` guard. Delivery-failure error
strings are a fixed classification (`"webhook delivery failed"` /
`"webhook delivery timed out"`), never the raw `error.Error()` text — a
`*url.Error` embeds the destination URL, and `Fire.Error` is served over
`GET /admin/alerts/recent`.

### 7. Recent-fires ring + admin endpoint, full-admin only

`Notifier` keeps an in-memory ring of the last 50 fires (`Fire{TS, Team,
Threshold, Ratio, SpentMicros, LimitMicros, Delivered, Error}`), read via
`GET /admin/alerts/recent`. Mounted `requireAdmin` (full-admin only) — a fire
carries cross-team spend figures, the same posture as the analytics summary
endpoints (Phase 1a). A new `Capabilities.BudgetAlerts` bool flips true when a
`Notifier` is built (i.e. `budget_alerts` is configured).

### 8. Per-instance state (multi-replica caveat)

`Notifier`'s `fired`/`recent` state is in-memory, per gateway instance — the
same limitation ADR-013 documents for the rate/budget counters themselves. On
a multi-replica deployment, each instance evaluates `Observe` independently
against its own view of the (shared, if using a shared budget store) spend, so
a single threshold crossing may fire once per replica rather than once
globally. This is stated in the console's alerts card, not hidden.

### 9. A budget crossing 100% under `on_exceeded: block` still fires

Governance blocks a request whose *pre-check* shows accumulated spend already
over the limit; the request that pushes spend *from* under the limit *to*
over it is itself allowed through (§5.3's known overshoot behavior) and
settles normally — so the 1.0 threshold does fire even in block mode, on the
crossing request. This is intentional: the alert should still tell the
operator the limit was reached, regardless of enforcement mode.

## Deferred

- Per-key budget alerts (needs a per-key destination design; key_id cannot be
  a label).
- A delivery-failure metric/counter (stderr logging is the interim
  observability; add if drops are observed in practice).
- Retry/backoff on delivery failure.
- Native SNS/Slack SDK integration (a webhook URL covers both without an
  extra dependency).
