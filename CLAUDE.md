# Project Context

## Overview

**inferplane** is an LLM consumption governance gateway: virtual keys, team RBAC,
quotas, budgets, and tamper-evident audit logging for Claude Code / OpenCode
traffic to Anthropic, Amazon Bedrock, and self-hosted vLLM/Ollama. Single static
binary, Kubernetes-native, Apache-2.0, no external SaaS dependency. The project
aspires to CNCF Sandbox.

Design source of truth: [docs/specs/2026-06-10-inferplane-gateway-design.md](docs/specs/2026-06-10-inferplane-gateway-design.md).
Architecture overview: [docs/architecture.md](docs/architecture.md).

## Tech Stack

- **Language:** Go 1.25 (module `github.com/inferplane/inferplane`)
- **Build:** single static binary, `CGO_ENABLED=0` (every dependency is pure-Go)
- **Storage:** `modernc.org/sqlite` (cgo-free SQLite) for the key store; disk WAL for audit
- **AWS:** `aws-sdk-go-v2` (`config` + `bedrockruntime`) for the Bedrock provider
- **Observability:** `prometheus/client_golang`; OpenTelemetry GenAI semantic conventions for metric naming
- **Packaging:** multi-stage Dockerfile → `distroless/static:nonroot`; Helm chart in `charts/inferplane`

## Project Structure

```
cmd/inferplane/    - Binary entrypoint: serve / keys / audit / report subcommands
internal/          - Private packages (gateway internals)
  server/          - HTTP data plane + admin plane, ingress handlers
  router/          - Model→provider resolution, fallback chain, circuit breaker
  governance/      - Rate / quota / budget enforcement (PreCheck + Settle)
  keystore/        - Virtual-key store (SQLite), Principal + RBAC
  audit/           - Tamper-evident hash-chain audit writer, WAL, verify
  pricing/         - microUSD cost computation (round-half-even)
  limiter/ budget/ - In-memory two-phase governance stores
  metrics/         - Prometheus registry + GenAI collectors
  openai/          - OpenAI ⇄ canonical conversion
  config/ principal/ - Config loading; request-scoped principal context
providers/         - Upstream provider implementations (the extension surface)
  anthropic/ bedrock/ openaicompat/ - One package per provider; testing/ has mocks
pkg/               - Public packages: schema/ (canonical types), ulid/
docs/              - specs, decisions (ADRs), runbooks, reference, architecture
charts/inferplane/ - Helm chart
deploy/grafana/    - Grafana dashboard
.claude/           - Claude settings, hooks, skills, commands, agents
tests/             - Harness tests (hooks, secret patterns, structure) — bash, not Go
```

## Conventions

- **Go style:** `gofmt`-clean (tabs), `go vet`-clean. Package comments on exported packages. Errors wrapped with `%w`.
- **Provider isolation (design §8):** a new provider adds **one package** under `providers/<name>/` plus a blank-import line in `cmd/inferplane/main.go`. Provider PRs touch only `providers/<name>/` and provider docs — **zero core diff**.
- **Canonical schema invariant (§2.2):** same-protocol round-trip is lossless. Pipeline-interpreted fields are typed; everything else is preserved verbatim (`Extra map[string]json.RawMessage`). Streaming-frame string fields are `*string` so empty values survive.
- **Cache invariant (§4.4):** when provider protocol == ingress protocol, forward the request body **verbatim** (`RawBody`) so `cache_control` and prompt-cache hits are never corrupted.
- **Two-phase governance:** pre-check BEFORE billing, settle AFTER. `on_exceeded` is `block` | `warn` (block wins on tie).
- **Cost is integer microUSD** — never float. Round-half-even via `math/big`.

### Security mandates (non-negotiable)

- Secrets are referenced only via `env:` / `file:` / `secret:` refs — **never inline** in config (config rejects inline `api_key`, §7).
- Virtual keys are SHA-256 hashed at rest; the plaintext `ik_...` is shown **once** and is never recoverable.
- The client never sees the gateway's upstream provider key; the gateway never forwards the client's key (§5.2).
- `/metrics` is unauthenticated but must leak **no** secret or `key_id` (cardinality-bounded labels only).
- `count_tokens` must **never** return a non-200 (a non-200 crashes Claude Code).
- Every commit is DCO signed off (`git commit -s`). License: Apache-2.0.

## Key Commands

```bash
# Build the static binary
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane

# Test (race detector) / vet / format check
go test ./... -race
go vet ./...
gofmt -l .

# Run the gateway
go run ./cmd/inferplane serve --config examples/config.json

# Issue a virtual key / verify the audit chain
go run ./cmd/inferplane keys create --team demo --models '*' --store keys.db
go run ./cmd/inferplane audit verify --file audit.jsonl
go run ./cmd/inferplane report --file audit.jsonl --by team,model

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

---

## Auto-Sync Rules

Rules below are applied automatically after Plan mode exit and on major code changes.

### Post-Plan Mode Actions
After exiting Plan mode (`/plan`), before starting implementation:

1. **Architecture decision made** -> Update `docs/architecture.md`
2. **Technical choice/trade-off made** -> Create `docs/decisions/ADR-NNN-title.md`
3. **New module added** -> Create `CLAUDE.md` in that module directory
4. **Operational procedure defined** -> Create runbook in `docs/runbooks/`
5. **Changes needed in this file** -> Update relevant sections above

### Code Change Sync Rules
- New top-level package under `internal/`, `providers/`, or `pkg/` -> update that area's `CLAUDE.md`
- New provider added -> update `providers/CLAUDE.md` and `docs/reference/agent-llm.md`
- Ingress endpoint added/changed -> update `internal/CLAUDE.md` and `docs/reference/api.md`
- Key store / audit schema changed -> update `docs/reference/data.md`
- Infrastructure changed (Dockerfile, charts) -> update `docs/architecture.md` and `docs/reference/infrastructure.md`

### ADR Numbering
Find the highest number in `docs/decisions/ADR-*.md` and increment by 1.
Format: `ADR-NNN-concise-title.md`
