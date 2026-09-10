# Infrastructure

### 1. Overview
Packaging and deployment for the single static binary: a multi-stage Docker build
producing a distroless image, and a Helm chart that renders config into a ConfigMap and
wires an optional IRSA ServiceAccount for Bedrock.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Dockerfile | `Dockerfile` | Multi-stage `CGO_ENABLED=0` build → `distroless/static:nonroot` |
| Docker ignore | `.dockerignore` | Excludes tests/docs/charts from the build context |
| Helm chart | `charts/inferplane/` | Deployment, Service (data+admin), ServiceAccount, ConfigMap, optional policies ConfigMap (`/etc/inferplane/policies`, live-reloaded — ADR-035), optional Ingress, optional PVC (ADR-023), NOTES.txt |
| GovernancePolicy CRD | `deploy/crd/` | kubectl-native schema validation for `inferplane.dev/v1alpha1` documents (structural schema + CEL, K8s 1.25+); controller-watch is a named follow-up (ADR-035) |
| Chart values | `charts/inferplane/values.yaml` | Image, replicaCount (must stay 1, SQLite), existingSecret, IRSA annotation, ingress (data/admin hosts), persistence (opt-in PVC for the key store), commented `config.otel` OTLP-trace example |
| Grafana dashboard | `deploy/grafana/inferplane.json` | 9-panel Prometheus dashboard |

### 3. Key Decisions
- `CGO_ENABLED=0` static binary so the image can be distroless/nonroot with no libc.
- The admin key console's static assets (`internal/server/adminui/static/`) ship inside the binary via `go:embed` — no image, chart, or build-pipeline change (ADR-001).
- **Config hot-reload (ADR-006):** edit config and `kill -HUP <pid>` (K8s: signal PID 1 or roll the pods) to apply provider/model/pricing changes with no restart — the topology is swapped atomically, governance counters/keystore/audit persist, and a bad config rolls back. Listen addrs, TLS, drain, and team policy limits are NOT hot (restart required).
- Single replica **only**, not merely a default (SQLite key store + instance-local governance) — the chart's only `replicaCount != 1` guard is gated on `persistence.enabled` (default `false`), so an operator can render >1 replica with no error today. Multi-replica HA waits for a shared-state backend; maintainer direction as of 2026-08-14 is Postgres-only, recorded in `docs/roadmap.md` pending a real ADR — not the Postgres/Redis split ADR-013 originally designed.
- **Key-store persistence (ADR-023):** `persistence.enabled` (default `false`, breaking-change-free) mounts a PVC (or `existingClaim`) at `/var/lib/inferplane`; without it, that path is an `emptyDir` and the key store/audit WAL are wiped on every pod restart. Enabling it switches the Deployment to `strategy: Recreate` (an RWO volume cannot attach to two pods) and a template guard refuses to render if `replicaCount != 1`. `virtual_keys` in `config` can declare a virtual key from a secret ref (`secrets.existingSecret`) so a client's key survives a restart even without persistence — see ADR-023 for the trade-off between the two.
- The chart references an `existingSecret` and never creates secrets (design §7).
- `Ingress` is off by default (`ingress.enabled: false`); when on, the admin plane
  additionally requires `ingress.admin.enabled: true` to be routed — it carries
  key-issuance/governance actions, so exposing it is an explicit second opt-in, not
  a side effect of turning on the data-plane Ingress.
- **OTel Collector contract — three channels, only one of them OTLP.** A collector
  cannot pick up everything from one receiver, so the split is deliberate:
  - **metrics** — a `prometheus` receiver scrapes `http://<svc>:9090/metrics` (the
    Service's named `admin` port). The Prometheus registry (`internal/metrics`) is the
    single source of truth for metrics; there is deliberately no OTLP metric exporter,
    because a second SDK would double-instrument the same counters and let the two
    drift. `gen_ai_*` names follow OTel GenAI semconv naming, but the transport is
    Prometheus exposition, not OTLP.
  - **traces** — an `otlp` receiver on `:4318` (http) or `:4317` (grpc), pushed by the
    opt-in `config.otel` block (ADR-011). Spans carry GenAI request/response
    attributes plus `inferplane.usage.cache_{read,write_5m,write_1h}_input_tokens`,
    `inferplane.cost.amount_usd_micros` / `inferplane.cost.pricing_missing`, and
    `inferplane.response.partial` on an interrupted stream (span status `Error` even
    though the wire status was already 200).
  - **usage windows** — `POST /v1alpha1/usage` to `inferplaned` (ADR-036). This is
    inferplane's own protocol on its own channel; it is NOT OTLP and no collector
    receiver consumes it.
  Give the collector separate `metrics` and `traces` pipelines, each with a `batch`
  processor. `/metrics` is unauthenticated by design and must stay cluster-internal
  (it is on the admin port, which `ingress.admin.enabled` gates) — it is
  cardinality-bounded and carries no secret or `key_id`, but it is still spend data.
  The chart ships no ServiceMonitor/PodMonitor: those CRDs belong to the operator's
  monitoring stack, and the named `admin` port is all a scrape config needs.
- `NOTES.txt` is the "easy deploy" surface: it prints the actual reachable
  address (Ingress host or a ready-to-paste `kubectl port-forward`), the first-key
  command, and the Claude Code env vars — so `helm install` alone gets an operator
  to working traffic without re-deriving them from `values.yaml`.

### 4. Code Pointers
- `Dockerfile` — build + runtime stages
- `charts/inferplane/templates/deployment.yaml` — pod spec, ports 8080/9090
- `charts/inferplane/templates/configmap.yaml` — rendered `config.json`
- `charts/inferplane/templates/ingress.yaml` — optional data/admin Ingress rules
- `charts/inferplane/templates/pvc.yaml` — optional PVC for the key store (ADR-023)
- `charts/inferplane/templates/NOTES.txt` — post-install quickstart

### 5. Cross-references
- Related modules: [docs/architecture.md](../architecture.md) (Infrastructure section)
- Related ADRs: docs/decisions/ (none yet)
- Related runbooks: docs/runbooks/ (create `deploy-production.md`)

### Routing rollout (ADR-043)

Upgrade all mayu instances, inferplaned, and the GovernancePolicy CRD when used
before activating sensitiveData/context rules. Install routes, explicit pricing,
and verified boundary/capability metadata first. Local policy is revalidated after
effective topology assembly and before listening; CP ApplyWire validates targets
on each data plane. Set `control_plane.require_sync` for privacy from first request,
optionally `max_policy_age` for staleness. Counts stay local/200 while unready/stale.
Use the isolated `examples/policy-routing/governance.yaml`, not the quick-start
`examples/policies/` directory. Shadow applies only to context preferences; privacy
already enforces. No fleet HA or durability improvement is implied. See
[operator guide](../policy-routing.md).
