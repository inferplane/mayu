# Implementation Reference Index

Per-layer implementation detail for inferplane. Each layer doc lists its components,
key decisions, and code pointers. The current architecture overview is
[docs/architecture.md](../architecture.md); the original v0.1 design rationale
(historical, translated and annotated against what shipped) is
[docs/specs/2026-06-10-inferplane-gateway-design.md](../specs/2026-06-10-inferplane-gateway-design.md).

<!-- AUTO-MANAGED:reference-index -->
| Layer | Document | Scope |
|-------|----------|-------|
| Infrastructure | [infrastructure.md](infrastructure.md) | Dockerfile, Helm chart, Grafana dashboard |
| API | [api.md](api.md) | Data/admin planes, ingress handlers, conversion |
| Data | [data.md](data.md) | Key store, audit chain, governance stores |
| Security | [security.md](security.md) | Auth, RBAC, secret isolation, metrics safety |
| Agent · LLM | [agent-llm.md](agent-llm.md) | Provider abstraction, canonical schema |
<!-- /AUTO-MANAGED:reference-index -->

Add a layer with `/add-reference-doc <layer>` (project-init). Available layers not yet
present: data-derived (frontend, ui, iac) — not applicable to this Go gateway.

Operator-facing routing schema, example, limits, and rollout gates: [Policy-aware routing](../policy-routing.md) (ADR-043).
