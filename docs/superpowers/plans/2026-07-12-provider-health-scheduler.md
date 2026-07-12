# periodic provider health probing (ADR-014 deferred item)

**Date:** 2026-07-12
**Source:** ADR-014's "What we deliberately do NOT adopt" section — "Periodic/auto
health checks — v1 is on-demand (D5); background probing is a follow-up (it needs a
scheduler + cardinality-bounded status storage)." This plan is that follow-up.

**Scope decision:** ADR-014 also lists **wildcard model mapping** (`openai/*`) as a
separate out-of-scope item, explicitly called a "router-engine change" distinct from
registration UX. That is a materially different, architecturally sensitive change
(interaction with ADR-021's model aliasing, exact-match routing semantics) — left for
its own future round, not bundled here.

**Design, confirmed against the actual code:**
- The existing on-demand probe (`POST /admin/providers/test`, `configapi/probe.go`)
  is entangled with HTTP request-parsing (`ParseProviderWrite`) because it tests a
  **draft, unsaved** provider a client is submitting — the whole point is testing
  before trusting. A periodic prober is simpler: it iterates **already-registered,
  already-secret-resolved** `providers.Provider` instances from `live.Holder.Load().
  Providers()` (re-`Load()` each tick, so hot-reloads/UI-writes are picked up
  automatically) and just needs the 2-line `prov.(providers.HealthChecker)` + `
  HealthCheck(ctx)` sequence — it bypasses `probe.go`'s SSRF `guardedClient`/ref
  resolution entirely, since those exist specifically for **unsaved draft hosts** an
  operator hasn't registered yet, not for already-trusted registered providers.
- "Cardinality-bounded status storage": the provider topology map is already bounded
  (config-driven, not request-driven — CLAUDE.md's metric-label rule is satisfied by
  construction, since the map is keyed by provider NAME, a config-bounded dimension,
  the same class as `team`/`model`, never raw client input).
- `cmd/inferplane/gateway.go` already runs this exact shape of background worker
  four times (`reloadWorker`, `anchorWorker`, `pgstoreAggregatorWorker`,
  `bodyPurgeWorker`) — one shared `workerCtx`, one ticker per worker, best-effort
  (stderr-logged, never fatal), one `<-done` wait per worker in `serve`'s shutdown
  defer. This plan adds a fifth, `healthProbeWorker`, in exactly that shape.
- Opt-in via a new config block (`provider_health_check.interval`), nil by default —
  matching every other subsystem's "presence enables" convention in this repo
  (`budget_alerts`, `audit.log_bodies`, `analytics.mode_b`) and ADR-014's own D2 risk
  note that upstream probing "must be explicit," now extended to the periodic case.

## Task list

### Task 1: `config.ProviderHealthCheckConfig`

**Files:**
- Modify: `internal/config/config.go`
- Test: `internal/config/config_test.go`

Steps:
- [ ] Add `ProviderHealthCheck *ProviderHealthCheckConfig \`json:"provider_health_check,omitempty"\``
      to `Config` (alongside the existing `BudgetAlerts *BudgetAlertsConfig` field).
- [ ] Define `type ProviderHealthCheckConfig struct { Interval string \`json:"interval,omitempty"\` }`
      (doc-commented: nil block = probing off, the v1-on-demand-only default; no secret
      field, so no inline-secret rejection probe entry needed, unlike `budget_alerts`).
- [ ] Add `func validateProviderHealthCheck(phc *ProviderHealthCheckConfig) error`: nil → nil;
      otherwise `validateDurationString("provider_health_check.interval", phc.Interval,
      time.Millisecond)` — the SAME floor `validateBudgetAlerts` already uses for
      `budget_alerts.timeout` (not a new, stricter `time.Second` floor an earlier draft of
      this plan proposed: that would have rejected Task 3's own e2e test config, which
      deliberately uses a sub-second interval so the test doesn't have to wait a full
      second per tick — plan-gate round 1, kimi-k2.5, a real self-contradiction caught
      before implementation).
- [ ] Wire the call into `LoadRaw`, right after the existing `validateBudgetAlerts(cfg.BudgetAlerts)`
      call.
- [ ] Test: `TestLoadRaw_ProviderHealthCheckValidInterval`, `TestLoadRaw_ProviderHealthCheckMalformedInterval`
      (mirrors the existing budget_alerts timeout validation tests' shape — find them via
      `grep -n BudgetAlerts.*Timeout config_test.go` and mirror exactly), `TestLoadRaw_ProviderHealthCheckNilIsValid`
      (absent block loads fine, `cfg.ProviderHealthCheck == nil`).

### Task 2: `configapi.HealthStore` + `HealthHandler` + capability flag

**Files:**
- Create: `internal/server/configapi/health.go`
- Modify: `internal/server/configapi/probe.go`
- Modify: `internal/server/configapi/capabilities.go`
- Test: `internal/server/configapi/health_test.go`
- Test: `internal/server/configapi/capabilities_test.go`

Steps:
- [ ] `probe.go`: export the existing package-private `probeTimeout` constant as
      `ProbeTimeout` (single rename, one usage site in this same file) — so
      `healthProbeWorker` (Task 3) reuses the identical 8s bound instead of a second,
      independently-drifting constant.
- [ ] `health.go`: define
      ```go
      type HealthRecord struct {
          OK           bool   `json:"ok"`
          LatencyMS    int64  `json:"latency_ms"`
          Detail       string `json:"detail"`
          LastProbedAt string `json:"last_probed_at"`
      }
      type HealthStore struct {
          mu      sync.Mutex
          records map[string]HealthRecord
      }
      func NewHealthStore() *HealthStore { return &HealthStore{records: map[string]HealthRecord{}} }
      func (s *HealthStore) Set(name string, r providers.HealthResult, at time.Time) { ... }
      func (s *HealthStore) Snapshot() map[string]HealthRecord { ... } // returns a copy
      ```
      `Set` locks, writes `records[name] = HealthRecord{OK: r.OK, LatencyMS: r.LatencyMS,
      Detail: r.Detail, LastProbedAt: at.UTC().Format(time.RFC3339Nano)}`. `Snapshot` locks,
      copies the map, returns it (never the live map — same defensive-copy shape as
      `alert.Notifier.Recent()`).
- [ ] `health.go`: `func HealthHandler(snapshot func() map[string]HealthRecord) http.Handler`
      — a closure parameter, NOT a raw `*HealthStore` pointer, mirroring
      `adminapi.AlertsHandler(recent func() []alert.Fire)`'s exact shape (`internal/server/
      adminapi/alerts.go`) so `AdminMux` (Task 3) can wire it identically to `alertFires`
      with zero special-casing. GET-only (405 otherwise), writes `{"providers": snapshot()}`
      as JSON; `snapshot == nil` (feature not configured) → `{"providers": {}}`, never an
      error (matches `AlertsHandler`'s `recent == nil` handling verbatim).
- [ ] `capabilities.go`: add `ProviderAutoHealth bool \`json:"provider_auto_health"\`` to
      `Capabilities`, alongside `BudgetAlerts`.
- [ ] Test (`health_test.go`, mirrors `probe_test.go`'s `doProbe`/direct-`http.Handler`-via-
      `httptest` style, and `alert_test.go`'s `Recent()`-returns-a-copy assertion style):
      `TestHealthStore_SetAndSnapshot` (round-trip, `LastProbedAt` is RFC3339Nano);
      `TestHealthStore_SnapshotIsACopy` (mutating the returned map must not affect a later
      `Snapshot()` call); `TestHealthHandler_GET` (pass `store.Snapshot` as the closure,
      returns the store's current snapshot as `{"providers": {...}}`); `TestHealthHandler_RejectsNonGET`
      (405); `TestHealthHandler_NilSnapshot` (passing a nil func returns `{"providers":{}}`,
      not an error — mirrors `AlertsHandler`'s nil-func handling).
- [ ] Test (`capabilities_test.go`): extend/add a case asserting `Capabilities{ProviderAutoHealth:
      true}` round-trips through `CapabilitiesHandler`'s JSON encoding (mirrors the existing
      generic-capabilities-handler test shape in this file — this file tests JSON plumbing
      only, NOT the config→bool derivation, which Task 3's e2e test covers).

### Task 3: `gateway.healthProbeWorker` + assembly wiring + route

**Files:**
- Modify: `cmd/inferplane/gateway.go`
- Modify: `internal/server/server.go`
- Modify: `internal/server/server_test.go`
- Modify: `internal/server/probe_wire_test.go`
- Test: `cmd/inferplane/health_probe_test.go`
- Test: `cmd/inferplane/e2e_test.go`

Steps:
- [ ] `gateway` struct: add `healthStore *configapi.HealthStore` (nil unless
      `provider_health_check` is configured) and `healthProbeEvery time.Duration`, alongside
      the existing `notifier *alert.Notifier` field.
- [ ] `newGateway`: mirror the existing `budget_alerts` block's exact shape —
      ```go
      var healthStore *configapi.HealthStore
      var healthProbeEvery time.Duration
      if phc := cfg.ProviderHealthCheck; phc != nil {
          healthProbeEvery, _ = time.ParseDuration(phc.Interval) // shape already validated at config load
          healthStore = configapi.NewHealthStore()
          fmt.Println("inferplane: periodic provider health checks enabled")
      }
      ```
      Store both on the `gateway{}` literal.
- [ ] Add `func (g *gateway) healthProbeWorker(ctx context.Context, done chan<- struct{})`,
      mirroring `bodyPurgeWorker`'s exact shape:
      ```go
      func (g *gateway) healthProbeWorker(ctx context.Context, done chan<- struct{}) {
          defer close(done)
          t := time.NewTicker(g.healthProbeEvery)
          defer t.Stop()
          for {
              select {
              case <-ctx.Done():
                  return
              case <-t.C:
                  g.probeAllProviders(ctx)
              }
          }
      }
      func (g *gateway) probeAllProviders(ctx context.Context) {
          for name, p := range g.holder.Load().Providers() {
              hc, ok := p.(providers.HealthChecker)
              if !ok {
                  continue
              }
              pctx, cancel := context.WithTimeout(ctx, configapi.ProbeTimeout)
              res := hc.HealthCheck(pctx)
              cancel()
              g.healthStore.Set(name, res, time.Now())
          }
      }
      ```
      (`probeAllProviders` factored out as its own method specifically so Task 3's test can
      call it directly on a short deadline without waiting for a real ticker tick — same
      reason `anchorWorker`'s tests use a short `anchorEvery` rather than calling an inner
      function directly; either shape is fine, this one is more directly testable.)
- [ ] Wire into `serve(ctx)`'s existing worker block, mirroring the `bodyPurgeDone` pattern
      exactly: `var healthProbeDone chan struct{}` → `if g.healthStore != nil { healthProbeDone
      = make(chan struct{}); go g.healthProbeWorker(workerCtx, healthProbeDone) }` → append
      `if healthProbeDone != nil { <-healthProbeDone }` to the shutdown `defer`.
- [ ] Capabilities literal: add `ProviderAutoHealth: healthStore != nil,`.
- [ ] `server.go`'s `AdminMux` signature gains one new parameter, mirroring the EXACT
      existing shape of `alertFires func() []alert.Fire` (a closure, not a raw stateful
      pointer — consistent with every other optional-subsystem param in this signature):
      `healthSnapshot func() map[string]configapi.HealthRecord`, inserted immediately after
      the existing `alertFires func() []alert.Fire` parameter (before `bodiesRec`, and
      before the trailing `probeAllowedHosts ...string` variadic, which must stay last).
      **Correction from an earlier draft of this plan:** `AdminMux` is NOT called from a
      single site — `grep -c "AdminMux(" internal/server/server_test.go
      internal/server/probe_wire_test.go` returns **23** test call sites, plus the one real
      call in `cmd/inferplane/gateway.go:459`, for **24 total**. Every one of the 23 test
      call sites needs exactly one more positional argument at the matching position — `nil`
      for every existing test (none of them currently exercise the new route, so `nil` is the
      correct "endpoint absent" value, mirroring how they already pass `nil` for `alertFires`
      today). This is mechanical (add one `nil` per call site) but touches two test files
      broadly — call this out explicitly in the task so the implementer doesn't miss any of
      the 23.
      Mount, mirroring the existing `alertFires`-guarded block exactly (`server.go`'s comment
      "nil alertFires → omitted (budget_alerts capability off)" right above it):
      ```go
      // nil healthSnapshot → omitted (provider_health_check capability off).
      if healthSnapshot != nil {
          mux.Handle("GET /admin/providers/health", AdminAuth(adminTokens, verifier, mapping, denied,
              requireAdmin(configapi.HealthHandler(healthSnapshot), emit)))
      }
      ```
      (`HealthHandler` already takes the `healthSnapshot` closure directly per Task 2's
      corrected signature — no adapter needed here.)
      At the real call site (`gateway.go:459`), thread through `var healthSnapshot
      func() map[string]configapi.HealthRecord; if healthStore != nil { healthSnapshot =
      healthStore.Snapshot }` — mirrors `gateway.go:439-441`'s `alertFires`/`notifier.Recent`
      pattern exactly.
      ```
      right after the existing `catalogH` mount (same `GET /admin/providers/...` family).
- [ ] Test (`health_probe_test.go`, mirrors `anchor_test.go`'s `fakeAnchorer`/`newAnchorGateway`/
      poll-with-deadline shape exactly): a fake `providers.Provider` implementing
      `providers.HealthChecker` with a call counter; build a minimal `&gateway{holder: ...,
      healthStore: configapi.NewHealthStore(), healthProbeEvery: 10*time.Millisecond}`; run
      `healthProbeWorker` directly, poll `healthStore.Snapshot()` until non-empty (deadline
      3s, matching `anchor_test.go`'s bound), assert the recorded `HealthRecord` matches the
      fake's configured result; `cancel(); <-done`. A second test: a provider that does NOT
      implement `HealthChecker` is silently skipped (no panic, no entry in the snapshot).
- [ ] Test (`e2e_test.go`), mirroring `TestE2ECapabilitiesReportsTeamsRecords`'s exact shape:
      `TestE2EProviderHealthCheckReportsCapability` — boot a gateway with
      `cfg["provider_health_check"] = map[string]any{"interval": "50ms"}`, GET
      `/admin/capabilities`, assert `provider_auto_health: true`. A second e2e test,
      `TestE2EProviderHealthCheckPopulatesStatus`: same boot, poll `GET
      /admin/providers/health` until it contains the registered provider's name with
      `ok: true` (the real mock/fake upstream from `newAnthropicUpstream` — confirm it
      already serves a `GET /v1/models`-shaped response `providers/anthropic/health.go`
      expects, or extend the test upstream fixture minimally if not).

### Task 4: admin console shows the auto-probed status

**Plan-gate note:** mirrors ADR-014 D5's own stated posture precisely — the page-session
`probeResults` cache (a MANUALLY triggered test) must win over an auto-probed value when
both exist for the same provider (a deliberate, recent action is more trustworthy than a
periodic background sample); the auto-probe is a **fallback for providers the operator
hasn't manually tested this session**, not a replacement.

**Files:**
- Modify: `internal/server/adminui/static/app.js`
- Test: `internal/server/adminui/adminui_test.go`

Steps:
- [ ] In `refreshProviders` (or wherever the providers table render loop lives), when the
      capability `provider_auto_health` is on: `GET /admin/providers/health` once per
      refresh, and for each provider row whose `probeCacheGet(name)` is empty (never
      manually tested this page-session), populate the badge from the fetched auto-probe
      record instead of rendering "○ untested" — do NOT call `probeCacheSet` for these (that
      cache is reserved for manually-triggered tests per the existing D5 design; keep the
      auto-probed values in a separate local variable/map scoped to this render pass, not
      persisted, so a manual test afterward still correctly overrides it on the next render).
- [ ] Test: extend `TestAdminUI_budgetAlertsWired`-adjacent style — a new
      `TestAdminUI_providerAutoHealthWired` asserting `app.js` contains a call to
      `"/admin/providers/health"` (via `api(...)`, never a bare `fetch`, same CSP-adjacent
      convention already enforced for `/admin/alerts/recent`).

## Host-direct doc sync (outside the harness task loop)

- `docs/decisions/ADR-014-provider-registration-ux-litellm-parity.md`: strike the
  "Periodic/auto health checks" line from "What we deliberately do NOT adopt," replace with
  an "implemented" note naming the new config block, `healthProbeWorker`, and the new
  `GET /admin/providers/health` endpoint. Leave the wildcard-model-mapping bullet untouched
  (still genuinely out of scope, per this plan's own Scope decision above).
- `docs/reference/api.md`: add the new `GET /admin/providers/health` endpoint row (mirrors
  the existing `GET /admin/alerts/recent` row's format).
- `internal/CLAUDE.md`: update the `server/` bullet (new endpoint) and add one clause to the
  `configapi`-adjacent description of `probe.go`/health-check machinery.

## Out of scope

- Wildcard model mapping (`openai/*`) — a router-engine change, explicitly separate per
  ADR-014; left for its own future round.
- A Prometheus gauge for provider health — no precedent exists for this in
  `internal/metrics` today (confirmed via direct grep); the admin-console read path (this
  plan) is the only surface, matching the on-demand D2 probe's own posture (no metric
  either). Add later if operators ask for alerting-on-provider-down via Prometheus rules.
- Persisting health status across a gateway restart — `HealthStore` is in-memory,
  per-instance (same posture as `alert.Notifier`'s `fired`/`recent` state, ADR-013 caveat);
  a fresh boot shows "untested" for every provider until the first tick.
- Any change to the on-demand `POST /admin/providers/test` draft-probe flow — untouched.
