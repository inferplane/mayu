# Developer Onboarding

## Quick Start

### 1. Prerequisites
- [ ] Go 1.25+ installed (`go version`)
- [ ] Repository access granted
- [ ] (Optional) Docker for container builds; Helm + kubectl for cluster deploys
- [ ] Upstream credentials for local testing (e.g. `ANTHROPIC_API_KEY`)

### 2. Setup
```bash
# Clone and fetch dependencies
git clone https://github.com/inferplane/mayu.git
cd mayu
go mod download

# Build the static binary
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane
```

### 3. Verify
```bash
go test ./... -race      # full suite, race detector
go vet ./...             # static checks
gofmt -l .               # must print nothing

# Run against the example config
ANTHROPIC_API_KEY=sk-ant-... INFERPLANE_ADMIN_TOKEN=dev \
  go run ./cmd/inferplane serve --config examples/config.json
```

## Project Overview
- Read `CLAUDE.md` for project context and conventions.
- Read `docs/architecture.md` for system design.
- Read `docs/specs/2026-06-10-inferplane-gateway-design.md` for the full design (source of truth).
- Browse `docs/reference/INDEX.md` for per-layer implementation detail.
- Review `docs/decisions/` for architectural decisions.

## Development Workflow
- Branch naming: `feat/`, `fix/`, `docs/`, `refactor/`, `chore/`.
- Commit convention: Conventional Commits, **DCO signed off** (`git commit -s`). CI rejects unsigned commits.
- Provider PRs touch only `providers/<name>/`, the blank-import line in `cmd/inferplane/main.go`, and provider docs — zero core diff (design §8).
- Run `go test ./... -race` and `bash tests/run-all.sh` before submitting.

## Key Concepts
- **Virtual key (`ik_...`)** -- the client credential; SHA-256 hashed at rest, shown once.
- **Canonical schema** -- Anthropic-superset used for cross-protocol conversion.
- **Verbatim forwarding** -- byte-for-byte body passthrough when protocols match (cache safety).
- **Two-phase governance** -- PreCheck before billing, Settle after.
- **Tamper-evident audit** -- per-instance SHA-256 hash chain, offline-verifiable.

## Troubleshooting
- `401` from the gateway: the virtual key is wrong/revoked, or the admin token is missing for `/admin/keys`.
- `count_tokens` must always return 200 — a non-200 crashes Claude Code; never change that contract.
- Cache hit rate dropped: a conversion path is corrupting `cache_control`; confirm verbatim forwarding on the matching-protocol path.
- Audit verify reports a break: confirm you are verifying per-instance segments, not one continuous chain across restarts.

## Resources
- Design doc: `docs/specs/2026-06-10-inferplane-gateway-design.md`
- Grafana dashboard: `deploy/grafana/inferplane.json`
- Helm chart: `charts/inferplane`
