# inferplane — continuation handoff (as of v0.2.0, 2026-06-14)

This is the cold-start brief for resuming work after a context clear. Read this
first, then continue with the co-agent consensus pipeline.

## Where the project is

- Repo `/home/atomoh/mayu`, Go 1.25, module `github.com/inferplane/inferplane`,
  single static binary (`CGO_ENABLED=0`), pure-Go.
- `main` is at tag **v0.2.0** (annotated, local — never pushed). Tree clean.
  `go test ./...` = 28 packages green under `-race`.
- **v0.2.0 = governance core complete:** virtual keys + team RBAC, two-phase
  quotas/budgets, integer-µUSD pricing, tamper-evident audit, 3 providers
  (anthropic/bedrock/openai_compatible), free OIDC SSO (ADR-004), config
  hot-reload (ADR-006), provider visibility (ADR-005), operator console
  dashboard (ADR-002: Overview/Virtual keys/Providers/Governance/Quickstart),
  governance views + one-click `/admin/audit/verify` (ADR-003 #2), chargeback
  `inferplane report` (ADR-007).
- ECS demo: CloudFormation stack `inferplane-demo` in `ap-northeast-2`
  (CloudFront `d1l7e2xnxvhpkn.cloudfront.net`, Bedrock via task role, admin
  token in SSM `/inferplane-demo/admin-token`). Running an OLDER image —
  redeploy v0.2.0 if you want the demo current. Teardown:
  `aws cloudformation delete-stack --stack-name inferplane-demo --region ap-northeast-2`.

## How we work (do not skip)

Use the **`/co-agent:consensus`** pipeline for each feature: P0 detect/init →
P1 write a plan in `docs/superpowers/plans/<date>-<slug>.md` → **P2 multi-model
design gate** (codex + gemini; iterate ≤2 rounds until no CRITICAL/MAJOR) → P3
TDD implementation (one commit per task, scope-locked to the plan's file list)
→ **P4 multi-model code gate** on the cumulative diff → P5 report. ADRs in
`docs/decisions/` — next is **ADR-008**. Each ADR records the decision +
rejected alternatives.

Panel reality on this machine:
- **codex** (openai.gpt-5.5), **gemini** (3.1-pro), and **kiro** are all usable.
  Use the **`kiro-cli`** binary explicitly (a bare `kiro` is wrong). kiro-cli
  IGNORES piped stdin — embed the review context INSIDE the prompt argument
  (`kiro-cli chat "<PROMPT + CONTEXT>" --no-interactive --trust-tools=read,grep
  --wrap never`), NOT `cat ctx | kiro-cli`. codex/gemini take context via stdin.
- **codex** needs `timeout 600` and a prompt that says *"answer from the
  provided context ONLY; do not explore the filesystem"* — otherwise it times
  out or wanders. It occasionally returns a server stream-error (empty output)
  — just retry it once.
- Chair authority: every panel finding is verified against the actual code
  (`check_citations.py`) before applying; refute with evidence when wrong.
- `AGENTS.md` / `GEMINI.md` carry distilled reviewer context — regenerate with
  `/co-agent:sync-context` whenever `CLAUDE.md` changes.

## Non-negotiable mandates (CLAUDE.md)

- Secrets only via `env:`/`file:` refs — never inline; config rejects inline keys.
- Cost is integer microUSD, never float. `count_tokens` always returns 200.
- Client key never forwarded upstream; gateway key never exposed. `/metrics`
  and `/admin/*` leak no secret/`key_id`.
- Console: vanilla JS `go:embed`, **CSP `default-src 'self'`** (no inline
  `style=`/`onclick=`, no `<style>`; use `<progress>`/CSS classes, set values
  via DOM properties). Token in JS memory only (no localStorage/cookies).
- Provider isolation (§8): a new provider = one package + one blank import in
  `cmd/inferplane/main.go`, zero core diff.
- **Every commit is `git commit -s`** (DCO). Per task, all four must be green:
  `CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane`,
  `go test ./... -race`, `go vet ./... && gofmt -l .`, `bash tests/run-all.sh`.

## Environment gotchas (learned this session)

- `go build ./...` does NOT emit the binary — rebuild `bin/inferplane`
  explicitly before any local `serve` smoke; a stale binary silently runs.
- `rm` is denied in this sandbox — `mv` stray files to `/tmp/...` instead. Keep
  the tree clean (the co-agent state file under `.claude/co-agent-consensus/`
  is gitignored; a stray root `inferplane` binary is not — move it out).
- After `sed`/python inserts of Go imports, run `gofmt -w` (import ordering)
  before committing; `gofmt -l .` is part of the gate.
- The co-agent session binds to a HEAD; if HEAD drifts mid-run, re-`init` the
  session at the new HEAD before `verify`.
- Browser smoke via Playwright MCP writes to `.playwright-mcp/` in the repo —
  `mv` it to `/tmp` before committing.

## Architecture map (where things live)

- `internal/live` — immutable topology generation (providers+models+pricing) in
  an atomic `Holder`; `BuildState` is the topology-only builder. Hot-reload
  swaps it; governance counters/keystore/audit/breaker persist.
- `internal/adminauth` — OIDC verifier + `IsOIDCBearerShape` + groups→team
  `Resolve` (a leaf; never imports config/server).
- `internal/server/{configapi,auditapi,adminui}` — `/admin/config`,
  `/admin/audit/verify`, the embedded console.
- `internal/governance` — `Governor` (PreCheck/Settle); leaf; `Settle` takes
  the `*pricing.Table` per-call from the request's resolved snapshot.
- `cmd/inferplane` — `serve` (gateway.go: assembly + SIGHUP reload worker) /
  `keys` / `audit` / `report`.

## Remaining roadmap (suggested order)

1. **UI-write provider registration (Stage 2)** — **BACKEND DONE** (ADR-008,
   2026-06-14). Shipped behind a 2-round P2 design gate + 2-round P4 code gate
   (codex+gemini; kiro skips — its CLI ignores piped stdin). Commits `a6be00f`
   (T0) → `1fa4dcc` (T9 docs), all on `main`, 28→? pkg green under `-race`.
   - `internal/providerstore` — opt-in SQLite store (`provider_store` config
     block): `providers` (refs only, **no secret column**), `model_targets`,
     `meta` (durable `seeded` marker, NOT row count → deleting all providers
     never resurrects the file topology). `Overlay`/`OverlayFrom` build the
     effective config (file + DB topology), `SeedIfEmpty` one-time file→DB import
     (validates ref shape first).
   - `config.LoadRaw`/`ResolveProviders` split — file providers are parsed but
     NOT resolved when a store is authoritative (G1 boot-crash fix);
     `config.ValidateSecretRef` is the shared ref-shape guard (env name charset /
     absolute file path / not-both) used by BOTH the write path and the seed.
   - Write path: `PUT`/`DELETE /admin/providers/{name}` + `/admin/models/{name}`
     (`configapi.WriteHandler`, behind AdminAuth; 405 when no store). The
     assembly (`cmd/inferplane` gateway `writeMutation`) is **build-once-swap-
     once** under ONE `reloadMu` (split `reload()`/`reloadLocked()`): build the
     candidate `live.State`, validate, persist, swap the validated state.
     Invalid topology → fixed sanitized 400 (detail logged server-side, never
     echoes a ref). Secret-free admin audit events (`provider_*`/`model_route_*`).
   - `GET /admin/config/export` — secret-free Git export (`ProviderConfig.APIKey`
     is `json:"-"`), mounted unconditionally.
   - **T8 console write UI — DONE** (`5b20a66`, gate `56d80ad`): the Providers
     tab has register/edit/delete provider forms + a model-route editor, shown
     only when `provider_store` is enabled (`View.Writable` capability hint;
     `ProviderView.Region` added for edit prefill). CSP `default-src 'self'`
     throughout (addEventListener, textContent, `hidden` property; no inline
     handlers/styles); the form collects the REF only (no secret-value field);
     writes go through the token-gated `api()` (which now surfaces the server's
     sanitized `{"error"}`); a Git-export card renders `/admin/config/export`.
     Verified by a live serve smoke (PUT/DELETE/export, inline + secret-shaped
     ref rejected without echo, delete-with-live-route 400, 401 no-token).
   - **ADR-008 Stage 2 is COMPLETE** (backend + console). Whole feature behind
     a P2 design gate (2 rounds) + P4 code gate (2 rounds) + T8 UI gate (codex+
     gemini PASS; kiro CHANGES-REQUIRED refuted with evidence).
2. **PII masking plugin — DONE** (ADR-009, 2026-06-14; commits `44afb6c`→
   `63d5305`). New top-level **`plugins/`** surface + `internal/filter`
   (RequestFilter interface + registry + `Masking` predicate). `plugins/piimask`
   masks request text (email/phone/card+Luhn/SSN/IPv4 → typed placeholders),
   one-way (no vault, no PII at rest). Opt-in per-team via `plugins` config
   (`buildMasking` resolves the registry; unknown name fails boot; enabling logs
   the cache/cost warning). Wired into `/v1/messages` (masks BOTH RawBody AND
   Parsed — the openai_compat path converts from Parsed; **fail-closed 400** on
   masker error), `count_tokens` (200 + local estimate on error — never 500,
   never leak), and `/v1/chat/completions` (**masked team rejected 400** — the
   OpenAI ingress is not a bypass; audited). Metric
   `inferplane_pii_mask_redactions_total{team}` + audit `pii_masked` (bool only).
   2-round design gate (2 CRITICAL R1 + 1 CRITICAL R2) + P4 code gate, all
   gemini+kiro (codex empty-streamed all session — a codex-CLI issue on this box).
5. **OTel trace spans + self-service key page — DONE** (ADR-010 self-service:
   `GET /admin/whoami` + console team-scoped issuance; ADR-011 OTel: opt-in OTLP
   exporter + GenAI-semconv spans + W3C propagation + audit `trace_id`, no-op
   default, `internal/tracing` + `internal/filter`-style seam). Each behind a
   3-family P2 + P4 gate. OTel deps added (go.opentelemetry.io/otel v1.44,
   pure-Go; CGO=0 build still green).

3. **S3 Object Lock audit anchoring — DONE** (ADR-012; commits 16204f3→c496d13).
   `audit.Writer.HeadHash()` (single-atomic, post-durable) + `Anchorer` interface
   + `internal/audit/s3anchor` (PutObject, optional COMPLIANCE retention, MinIO
   endpoint) + opt-in `audit.anchor` config + periodic worker (retry-on-failure,
   final anchor AFTER writer drain under bounded ctx) + runbook
   (docs/runbooks/audit-anchoring.md, conditional on Object Lock + restricted
   IAM). 3-family P2 + P4 gate. aws-sdk-go-v2/service/s3 added (pure-Go). E2E
   needs real AWS S3 (verified offline via a stub S3 client).

### Still remaining
4. **Multi-replica HA** — Postgres key store, Redis/Valkey quota store +
   distributed rate limit. LARGE; needs Postgres/Redis for live verification —
   recommend interface/migration DESIGN only in this environment.

Start the next session by picking a remaining item and running
`/co-agent:consensus` to plan → gate → implement it.
