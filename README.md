# inferplane

LLM consumption governance gateway — virtual keys, team RBAC, quotas,
budgets, and tamper-evident audit logging for Claude Code / OpenCode
traffic to Anthropic, Amazon Bedrock, and self-hosted vLLM/Ollama.
Single binary, Kubernetes-native, Apache-2.0.

> **Project status: v0.1 pre-release, not yet announced.** APIs, config schema,
> and metric names may still change before the first tagged release.

Design: [docs/specs/2026-06-10-inferplane-gateway-design.md](docs/specs/2026-06-10-inferplane-gateway-design.md)

## Quickstart (5 minutes)

inferplane sits between your coding agent and the upstream model APIs. You point
Claude Code / OpenCode at the gateway with a **virtual key** (`ik_...`); the
gateway authenticates the key, enforces the team's quota/budget, forwards the
request to a real provider, and writes a tamper-evident audit record.

### 1. Write a `config.json`

This declares the providers it can reach and the model names clients may request.
The example below wires up all three provider kinds — Anthropic direct, an
OpenAI-compatible endpoint (vLLM/Ollama/any OpenAI API), and Amazon Bedrock —
and gives a `demo` team a 10M-token/day quota and a $50/month budget:

```json
{
  "server": {
    "listen": ":8080",
    "admin_listen": ":9090",
    "admin_auth": { "token_refs": [ { "env": "INFERPLANE_ADMIN_TOKEN" } ] }
  },
  "key_store": { "type": "sqlite", "path": "/var/lib/inferplane/keys.db" },
  "audit": {
    "failure_mode": "buffer_then_block",
    "buffer": { "path": "/var/lib/inferplane/audit.wal" },
    "sinks": [ { "type": "stdout" }, { "type": "file", "path": "/var/lib/inferplane/audit.jsonl" } ]
  },
  "providers": {
    "anthropic-direct": {
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key_ref": { "env": "ANTHROPIC_API_KEY" }
    },
    "local-vllm": {
      "type": "openai_compatible",
      "base_url": "http://vllm.internal:8000/v1",
      "api_key_ref": { "env": "VLLM_API_KEY" }
    },
    "bedrock-us": {
      "type": "bedrock",
      "region": "us-west-2",
      "auth": { "mode": "irsa" }
    }
  },
  "models": {
    "claude-sonnet-4-6": { "targets": [ { "provider": "anthropic-direct", "model": "claude-sonnet-4-6" } ] },
    "claude-sonnet-4-6-bedrock": { "targets": [ { "provider": "bedrock-us", "model": "anthropic.claude-sonnet-4-6-v1:0", "api": "invoke_model" } ] },
    "llama-3.1-70b": { "targets": [ { "provider": "local-vllm", "model": "meta-llama/Llama-3.1-70B-Instruct" } ] }
  },
  "teams": {
    "demo": {
      "allowed_models": ["*"],
      "quota": { "tokens_per_day": 10000000, "on_exceeded": "block" },
      "budget": { "usd_per_month": 50.0, "on_exceeded": "block" }
    }
  },
  "pricing": {
    "on_missing": "allow",
    "overrides": {
      "anthropic-direct": {
        "claude-sonnet-4-6": { "input_per_mtok": 3.0, "output_per_mtok": 15.0 }
      }
    }
  }
}
```

Provider kinds:

- **`anthropic`** — Anthropic's Messages API (Claude Code's native protocol).
- **`openai_compatible`** — any OpenAI Chat Completions endpoint (self-hosted
  vLLM, Ollama, or a managed OpenAI-compatible service).
- **`bedrock`** — Amazon Bedrock; prefer `auth.mode: irsa` on EKS so no static
  key is stored.

Secrets (`ANTHROPIC_API_KEY`, `VLLM_API_KEY`, `INFERPLANE_ADMIN_TOKEN`) are
referenced from the environment — inferplane never persists them.

### 2. Run it

```bash
docker run -d --name inferplane \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e INFERPLANE_ADMIN_TOKEN=admin-secret \
  -v "$PWD/config.json":/etc/inferplane/config.json \
  -v inferplane-data:/var/lib/inferplane \
  -p 8080:8080 -p 9090:9090 \
  inferplane:0.1.0
```

Port `8080` is the **data plane** (client traffic); `9090` is the **admin plane**
(`/healthz`, `/readyz`, unauthenticated `/metrics`, and the `/admin/keys` API).

### 3. Issue a virtual key

```bash
docker exec inferplane inferplane keys create \
  --team demo --models '*' \
  --store /var/lib/inferplane/keys.db
# → prints ik_... once (never recoverable — copy it now)
```

### 4. Point your coding agent at the gateway

Claude Code (Anthropic protocol):

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=ik_...
claude
```

OpenCode / any OpenAI client: set the OpenAI base URL to
`http://localhost:8080/v1` and use the same `ik_...` key as the API key.

### 5. Verify audit + metrics

```bash
# Audit hash chain is intact and tamper-evident:
docker exec inferplane inferplane audit verify --file /var/lib/inferplane/audit.jsonl

# Prometheus exposition (OTel GenAI naming) — counter rises after each request:
curl -s localhost:9090/metrics | grep inferplane_requests_total
```

## Operating inferplane

- **Metrics:** `/metrics` on the admin plane exposes Prometheus metrics using
  OpenTelemetry GenAI semantic conventions
  (`gen_ai_client_token_usage_total`, `gen_ai_server_request_duration_seconds`,
  `gen_ai_server_time_to_first_token_seconds`) plus `inferplane_*` operational
  series (requests, fallbacks, circuit state, budget spend, audit failures,
  pricing misses). A ready-made dashboard ships at
  [deploy/grafana/inferplane.json](deploy/grafana/inferplane.json).
  > `inferplane_budget_spend_usd_total` is an **observability approximation** —
  > the settlement source of truth is the microUSD budget store, not this metric.
- **Kubernetes:** install the Helm chart in [charts/inferplane](charts/inferplane).
  The chart renders `config.json` into a ConfigMap, wires an optional IRSA
  ServiceAccount for Bedrock, and references (never creates) a Secret for keys.
  v0.1 runs a single replica (SQLite key store + instance-local governance);
  multi-replica HA lands with the Postgres backend in v0.2.
- **TLS:** for non-Kubernetes deployments set `server.tls.cert_file` /
  `server.tls.key_file` to terminate TLS on the data plane directly. On
  Kubernetes, terminate at the ingress or service mesh instead.

## CLI

```
inferplane serve  --config <path>
inferplane keys   create --team <t> --models <csv> --store <path>
inferplane keys   list   --store <path>
inferplane keys   revoke --id <key_id> --store <path>
inferplane audit  verify --file <path>
```
