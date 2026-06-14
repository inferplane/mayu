# cmd Module

## Role
Binary entrypoints. Currently one binary: `cmd/inferplane`.

## Key Files
- `inferplane/main.go` — CLI dispatch (`serve` / `keys` / `audit` / `report`), wiring of keystore + audit + governor + metrics + router + providers, the TLS branch, and graceful shutdown.

## Rules
- `main` owns the metrics sink (`metrics.New()`) and threads it into the audit writer, router, governor, and ingress handlers.
- New providers are registered here by blank import (`_ "…/providers/<name>"`) — this is the only core file a provider PR may touch.
- Subcommand wiring stays thin; real logic lives in `internal/*`. Keep `main` readable as the system's assembly diagram.
- On any config/provider error, fail fast with a wrapped error and non-zero exit.
