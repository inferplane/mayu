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
- Only **codex** (openai.gpt-5.5) and **gemini** (3.1-pro) are installed
  (`kiro-cli` is NOT). Quorum guard: gemini-only rounds are single-opinion.
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

1. **UI-write provider registration (Stage 2)** — the decided design:
   **DB-authoritative source of truth, Git export** (ADR-005/006 deferred this).
   Builds on the hot-reload `reload()` + `live.Holder`: a write endpoint
   (`POST/PUT /admin/config` or `/admin/providers`) persists to a DB store, then
   triggers the SAME `reload()`. **Secrets never enter the gateway** — the UI
   registers WHICH ref a provider uses (env var name / file path), not the
   value; guide the operator to set it in their platform secret store. Needs a
   new ADR for the DB store + write contract + how it reconciles with the file
   config (DB authoritative, file/Git as export). This is the largest item.
2. **PII masking plugin (#4)** — opt-in, with explicit cache-destruction +
   cost-increase warning (per spec; honest trade-off vs silent-masking rivals).
3. **S3 Object Lock audit anchoring (#5)** — upgrades tamper-EVIDENT → tamper-
   RESISTANT; periodic external anchoring of the hash chain.
4. **Multi-replica HA** — Postgres key store, Redis/Valkey quota store +
   distributed rate limit (summed enforcement across replicas).
5. **OTel trace spans** (v0.2 GenAI conventions) and the **self-service key
   page** (OIDC login → issue my own key) on the existing console.

Start the next session by picking item 1 (or whichever the user names) and
running `/co-agent:consensus` to plan → gate → implement it.
