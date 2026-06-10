# Contributing

- All commits MUST be signed off (`git commit -s`) — [DCO](https://developercertificate.org/).
  CI rejects unsigned commits.
- Provider PRs touch only `providers/<name>/`, `providers/register.go`,
  and `docs/providers/` — zero core diff (design doc §8).
- Run `go test ./...` before submitting.
- Design doc: `docs/specs/2026-06-10-inferplane-gateway-design.md`.
