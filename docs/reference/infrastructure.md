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
| Chart values | `charts/inferplane/values.yaml` | Image, replicaCount (local=1; shared Postgres supports multiple replicas), existingSecret, IRSA annotation, ingress (data/admin hosts), persistence (opt-in PVC for the key store), commented `config.otel` OTLP-trace example |
| Grafana dashboard | `deploy/grafana/inferplane.json` | 9-panel Prometheus dashboard |

### 3. Key Decisions
- `CGO_ENABLED=0` static binary so the image can be distroless/nonroot with no libc.
- The admin key console's static assets (`internal/server/adminui/static/`) ship inside the binary via `go:embed` — no image, chart, or build-pipeline change (ADR-001).
- **Config hot-reload (ADR-006):** edit config and `kill -HUP <pid>` (K8s: signal PID 1 or roll the pods) to apply provider/model/pricing changes with no restart — the topology is swapped atomically, governance counters/keystore/audit persist, and a bad config rolls back. Listen addrs, TLS, drain, and team policy limits are NOT hot (restart required).
- Default/local mode is single-replica. ADR-046 shared Postgres mode supports multiple gateways; the chart rejects unsafe local replication. Persistent shared mode renders a StatefulSet with separate PVCs, required node anti-affinity and a disruption budget. See [shared governance](../shared-governance.md).
- **Persistence:** local mode retains its existing Deployment/PVC behavior. Shared mode stores keys and counters in Postgres; per-replica PVCs hold independent audit segments. Do not share one audit WAL between replicas.
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

ADR-044 keeps adaptive decisions and bounded affinity state node-local, with no
central classifier/cache dependency. Backend failover remains inside approved
privacy/cost constraints. Already installed rules remain usable during a control
plane outage while required authority is valid; expired hard leases still deny.
ADR-045 adds opt-in Postgres global monetary authority with interchangeable
control-plane replicas and private durable node journals. Its readiness endpoint
checks the database; data planes retain only finite previously committed credit
during outages. Deploy a replicated database and a stable control-plane endpoint.
This does not remove the shared-gateway key/rate/quota limits above.
See [deployment and failure behavior](../durable-budgets.md).
