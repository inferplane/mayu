# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Overview

**inferplane** is a governance control plane + node-local data plane for LLM
consumption: virtual keys, team RBAC, quotas, budgets, and tamper-evident audit
logging for Claude Code / OpenCode / Codex traffic to Anthropic, Amazon Bedrock,
and self-hosted vLLM/Ollama. Each of the two binaries (`mayu`, `inferplaned`)
is itself a single static binary, Kubernetes-native, Apache-2.0, no external
SaaS dependency. The project aspires to CNCF Sandbox.

Architecture overview: [docs/architecture.md](docs/architecture.md).
Design history / decisions: [docs/decisions/](docs/decisions/).

## Core Purpose

Every design decision, ADR, and scope call is judged against these five goals.
If a change doesn't serve one of them, it needs a reason that isn't "seemed
useful" — and if it weakens one to serve another, that trade-off must be
stated explicitly (see the HA vs. rate-limit-accuracy tension below).

1. **A single entry point for coding-assistant traffic** — Claude Code,
   OpenCode, and Codex users route through inferplane to reach Anthropic,
   Amazon Bedrock, and OpenAI-compatible (vLLM/Ollama/etc.) providers.
   Codex support is the goal, not yet verified — no Codex-specific code or
   test exists (`docs/roadmap.md` Purpose alignment).
2. **Per-user model choice** — each user can pick which model they talk to.
3. **Cost-driven model substitution** — swap to a cheaper model (e.g.
   Sonnet → GLM) when cost, not just capability, is the deciding factor.
   Enforceable via `routing.budgetTiers` (ADR-041): `router.SubstituteTier`.
   ADR-043 adds independent privacy restrictions and Shadow-default context
   recommendations; opt-in Enforce switches only eligible short requests.
   Task success, total cost including cold-cache/retries, p95 latency, and privacy
   negative cases are rollout gates, not benefits established by unit tests.
4. **Budget control with visibility** — set spend limits per team and per
   individual, block on breach, and always be able to answer "how much have
   we spent."
5. **No SPOF** — control plane and data plane are separate processes; a
   control-plane outage must never stop inference-path request traffic. This
   is specifically about the control plane, not about any one `mayu`
   instance's own availability — see "Current limits" in README for that.

**Known tension:** #5 (no SPOF) pulls against making enforcement accurate.
Running N node-local data planes removes the SPOF, but in-memory
per-instance counters mean rate limits and quotas become up to N× the
configured value unless enforcement is made globally accurate — as ADR-034
did (with bounded, not exact, overshoot) for team budget in control-plane
mode, but not yet for rate/quota or standalone budget — see
`docs/roadmap.md`. Any HA or multi-replica work must close this gap for #4,
not just add replicas.

## Tech Stack

- **Language:** Go 1.25 (module `github.com/inferplane/inferplane`)
- **Build:** single static binary, `CGO_ENABLED=0` (every dependency is pure-Go)
- **Storage:** `modernc.org/sqlite` (cgo-free SQLite) for the key store; disk WAL for audit
- **AWS:** `aws-sdk-go-v2` (`config` + `bedrockruntime`) for the Bedrock provider
- **Policy files:** `sigs.k8s.io/yaml` (pure Go) for CRD-style GovernancePolicy documents (`policies` config key, ADR-033)
- **Observability:** `prometheus/client_golang`; OpenTelemetry GenAI semantic conventions for metric naming
- **Packaging:** multi-stage Dockerfile → `distroless/static:nonroot`; Helm chart in `charts/inferplane`

## Request Flow (the big picture across packages)

One data-plane request touches, in order — this is the spine to keep in mind
when a change spans packages:

1. **KeyAuth** (`internal/server/auth.go`) — `x-api-key` OR `Authorization:
   Bearer`, SHA-256 lookup in `keystore` → `Principal` on the request context.
2. **Ingress parse** (`internal/server/{anthropicapi,openaiapi,bedrockapi}`) —
   protocol-specific; the raw body is kept for verbatim forwarding.
3. **Routing** (`internal/router`) — alias canonicalization → `ResolveModel`
   (config `model_fallbacks` when the requested model has no route) →
   `SubstituteTier` (ADR-041 budget-tier substitution of an ALREADY-routed
   model; narrows-only, never denies — see internal/CLAUDE.md `router/`) →
   `ResolveChain` (priority fallback + circuit breaker).
4. **RBAC re-check** — `FilterModelAllowed`/`FilterRegions` MUST run after
   routing in every ingress handler: a fallback target appended after the
   original allow-list check is otherwise unchecked (see internal/CLAUDE.md
   Invariants).
5. **Policy-aware request routing** (`internal/router.RouteRequest`, ADR-043) —
   inspect original bytes once; intersect sensitive-data rules; compute optional
   context preferences; return the complete safe chain on one topology snapshot.
   Privacy enforces even in Shadow. Original-model RBAC must pass. Passive results
   preserve the input preflight model; requested is resolved pre-tier, selected is
   the policy choice, proposed is observational, actual attempt is separate.
6. **Filters and admission** — explicit masking (`internal/filter`, `plugins/piimask`)
   runs after privacy inspection; context preflight uses the returned topology.
   Governance PreCheck (`internal/governance`, `internal/proxy.LeaseTable`) runs
   before provider billing; body capture cannot precede the routing decision.
7. **Provider** (`providers/<name>`) — verbatim `RawBody` when protocols
   match (the cache invariant); Bedrock routes Claude models via
   InvokeModel and every other model family (GPT, GLM, …) via Converse
   with schema translation.
8. **Settle** (`internal/governance`) — actual usage debits quota (cache
   tiers included), trues up TPM against the PreCheck estimate (ADR-039),
   computes integer-µUSD cost.
9. **Audit + analytics** (`internal/audit`, `internal/analytics`) —
   hash-chained `request_started`/`request_completed` records; the
   analytics index feeds `GET /admin/logs` and the console.

Control-plane attach (optional): `internal/proxy.Syncer` heartbeats
`/v1alpha1/sync` (policy + budget leases, ADR-034), `UsagePusher` pushes
telemetry (ADR-036), and `CredentialFetcher` pulls short-lived Bedrock
credentials (ADR-040) — three separate channels sharing only base URL and,
except the broker, the bearer token.

## Project Structure

```
cmd/mayu/          - Data plane binary (node-local proxy): serve / keys / audit / report / bodies / pricing / login / token / logout
cmd/inferplaned/   - Control plane binary: policy distribution + budget leases (ADR-034); credential broker (ADR-040, opt-in via INFERPLANED_BROKER_ROLE_ARN)
api/v1alpha1/      - Versioned config API wire types (CRD-style shape, gRPC/HTTP delivery)
internal/          - Private packages (gateway internals)
  policy/          - Rule + lease schema shared by both binaries (the single truth, ADR-031); loader/store + sync wire types (ADR-033/034)
  policystore/     - Postgres-authoritative GovernancePolicy store for inferplaned (ADR-038)
  controlplane/    - inferplaned distribution core: sync heartbeat, lease ledger, dataplane view (ADR-034)
  proxy/ cache/ telemetry/ - proxy/ owns the control-plane Syncer + LeaseTable (ADR-034) and the UsagePusher (ADR-036); telemetry/ is live (ADR-036): usage wire types, window collector, memory/postgres/durable aggregators; cache/ owns VolatileStore (unimplemented, ADR-031 consolidation target)
  server/          - HTTP data plane + admin plane, ingress handlers
  router/          - Model→provider resolution, fallback chain, circuit breaker; SubstituteTier applies ADR-041 budget-tier substitution
  sensitivity/     - Stdlib-only original-byte inspection; finite detector categories and request-shape signals (ADR-043)
  tier/            - ADR-041 budget-tier substitution: per-team Table, window-latched activation, shared by controlplane/ and proxy/
  governance/      - Rate / quota / budget enforcement (PreCheck + Settle)
  keystore/        - Virtual-key store (SQLite), Principal + RBAC
  providerstore/   - Opt-in DB-authoritative provider/model topology (ADR-008)
  audit/           - Tamper-evident hash-chain audit writer, WAL, verify
  bodystore/       - Opt-in encrypted request/response body capture (ADR-018)
  analytics/       - Derived usage read-model backing GET /admin/logs
  alert/           - Budget-alert webhook emitter (ADR-017)
  pricing/         - microUSD cost computation (round-half-even)
  limiter/ budget/ - In-memory two-phase governance stores
  metrics/         - Prometheus registry + GenAI collectors
  live/            - Reloadable topology generation behind an atomic pointer (ADR-006)
  filter/          - Request-transform filter seam (ADR-009); concrete filters under plugins/
  tracing/         - Opt-in OpenTelemetry seam, no-op default (ADR-011)
  adminauth/       - Admin-plane OIDC identity leaf (ADR-004)
  openai/          - OpenAI ⇄ canonical conversion
  config/ principal/ - Config loading; request-scoped principal context
providers/         - Upstream provider implementations (the extension surface)
  anthropic/ bedrock/ openaicompat/ - One package per provider; testing/ has mocks
pkg/               - Public packages: schema/ (canonical types), ulid/
plugins/           - Concrete filter implementations (piimask/, ADR-009)
docs/              - decisions (ADRs), runbooks, reference, architecture
charts/inferplane/ - Helm chart (incl. policies ConfigMap channel, ADR-035)
deploy/crd/        - GovernancePolicy CustomResourceDefinition (ADR-035)
deploy/grafana/    - Grafana dashboard
.claude/           - Claude settings, hooks, skills, commands, agents
tests/             - Harness tests (hooks, secret patterns, structure) — bash, not Go
```

## Conventions

- **Go style:** `gofmt`-clean (tabs), `go vet`-clean. Package comments on exported packages. Errors wrapped with `%w`.
- **Provider isolation:** a new provider adds **one package** under `providers/<name>/` plus a blank-import line in `cmd/mayu/main.go`. Provider PRs touch only `providers/<name>/` and provider docs — **zero core diff**.
- **Canonical schema invariant:** same-protocol round-trip is lossless. Pipeline-interpreted fields are typed; everything else is preserved verbatim (`Extra map[string]json.RawMessage`). Streaming-frame string fields are `*string` so empty values survive.
- **Cache invariant:** when provider protocol == ingress protocol, forward the request body **verbatim** (`RawBody`) so `cache_control` and prompt-cache hits are never corrupted.
- **Policy-aware routing (ADR-043):** privacy can deny; optional context preferences
  cannot loosen the safe route. All attempts require RBAC/regions; privacy and
  selected alternatives also require boundary/transport checks. Alternatives need
  declared context/capabilities and pricing. Boundary labels are operator assertions;
  finite detectors do not guarantee universal PII detection. No session pinning or
  Responses ingress is added. Upgrade binaries/CRD before activating new rules.
- **Policy assembly:** install `live.Holder.RoutedAndPriced` on Store after effective
  topology construction; revalidate local policy before listeners; future ApplyWire
  validates too. Rejected sensitive generations fail closed until valid recovery.
  CP privacy from first request requires `require_sync`; both count APIs use local
  HTTP-200 estimates while unready/stale or privacy-denied.
- **Two-phase governance:** pre-check BEFORE billing, settle AFTER. `on_exceeded` is `block` | `warn` (block wins on tie).
- **Cost is integer microUSD** — never float. Round-half-even via `math/big`.
- **`mayu` runs standalone by design** — a control plane is optional. `policies` (local file channel) and `control_plane` (ADR-034 heartbeat to `inferplaned`) are mutually exclusive config: one policy source at a time.
- **`AGENTS.md` is generated** (marker in line 1, distilled from this file for the external AI review panel) — edit this file, then regenerate; a hand-edit without updating the `claude-md-sha` marker will be flagged stale and overwritten.

### Security mandates (non-negotiable)

- Secrets are referenced only via `env:` / `file:` / `secret:` refs — **never inline** in config (config rejects inline `api_key`).
- Virtual keys are SHA-256 hashed at rest; the plaintext `ik_...` is shown **once** and is never recoverable.
- The client never sees the gateway's upstream provider key; the gateway never forwards the client's key.
- `/metrics` is unauthenticated but must leak **no** secret or `key_id` (cardinality-bounded labels only).
- `count_tokens` must **never** return a non-200 (a non-200 crashes Claude Code).
- Every commit is DCO signed off (`git commit -s`). License: Apache-2.0.

## Key Commands

```bash
# Build both static binaries
CGO_ENABLED=0 go build -trimpath -o bin/mayu ./cmd/mayu
CGO_ENABLED=0 go build -trimpath -o bin/inferplaned ./cmd/inferplaned

# Test (race detector) / vet / format check
go test ./... -race
go vet ./...
gofmt -l .

# Run a single package's tests, or a single test by name
go test ./internal/policy/... -run TestModelAllowed -v

# Run the gateway (data plane :8080, admin plane + console :9090/admin/ui/)
go run ./cmd/mayu serve --config examples/config.json

# Run the control plane (health :7601; policy sync needs --policies; env-only config, no file)
INFERPLANED_TOKEN=dev go run ./cmd/inferplaned --policies examples/policies/

# Issue a virtual key / verify the audit chain
go run ./cmd/mayu keys create --team demo --models '*' --store keys.db
go run ./cmd/mayu audit verify --file audit.jsonl
go run ./cmd/mayu report --file audit.jsonl --by team,model

# Verify every configured model has a pricing rate (ADR-030 CI guard; exit 1 if not)
INFERPLANE_ADMIN_TOKEN=lint go run ./cmd/mayu pricing check --config examples/config.json

# Harness tests (hooks/structure)
bash tests/run-all.sh
```

## Implementation References

<!-- AUTO-MANAGED:references -->
Per-layer implementation detail lives in [docs/reference/](docs/reference/INDEX.md):

| Layer | Document |
|-------|----------|
| Infrastructure | [docs/reference/infrastructure.md](docs/reference/infrastructure.md) |
| API | [docs/reference/api.md](docs/reference/api.md) |
| Data | [docs/reference/data.md](docs/reference/data.md) |
| Security | [docs/reference/security.md](docs/reference/security.md) |
| Agent · LLM | [docs/reference/agent-llm.md](docs/reference/agent-llm.md) |
<!-- /AUTO-MANAGED:references -->

When a change touches a layer above, update its reference doc and the
matching module `CLAUDE.md` in the same commit — that's the whole sync
rule; there is no separate ceremony to follow.
