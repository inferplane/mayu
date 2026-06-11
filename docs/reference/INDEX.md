# Implementation Reference Index

Per-layer implementation detail for inferplane. Each layer doc lists its components,
key decisions, and code pointers. The design source of truth remains
[docs/specs/2026-06-10-inferplane-gateway-design.md](../specs/2026-06-10-inferplane-gateway-design.md);
the high-level view is [docs/architecture.md](../architecture.md).

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
