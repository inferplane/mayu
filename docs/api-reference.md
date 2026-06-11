# API Reference

inferplane exposes two HTTP planes: the **data plane** (`:8080`, client traffic) and
the **admin plane** (`:9090`, operations). All data-plane requests authenticate with a
virtual key (`ik_...`).

## Data Plane (`:8080`)

### Authentication
Send the virtual key the way the client protocol expects:
- Anthropic ingress: `x-api-key: ik_...` (Claude Code sends this when `ANTHROPIC_API_KEY=ik_...`).
- OpenAI ingress: `Authorization: Bearer ik_...`.

The gateway resolves the key to a `Principal` (team + allowed models) and never forwards
the client key upstream; it injects the upstream provider credential itself.

### Anthropic ingress

```
POST /v1/messages
POST /v1/messages/count_tokens
GET  /v1/models
```

| Endpoint | Notes |
|----------|-------|
| `POST /v1/messages` | Messages API; streaming via SSE when `"stream": true`. Body forwarded verbatim to an Anthropic-protocol upstream. |
| `POST /v1/messages/count_tokens` | Token counting. **Always returns 200** (a non-200 crashes Claude Code). |
| `GET /v1/models` | Lists models the principal may use (negotiated by `anthropic-version`). |

### OpenAI ingress

```
POST /v1/chat/completions
GET  /v1/models
```

| Endpoint | Notes |
|----------|-------|
| `POST /v1/chat/completions` | Chat Completions; streaming via SSE when `"stream": true`. Converted via the canonical schema when the upstream protocol differs. |
| `GET /v1/models` | Lists models the principal may use. |

## Admin Plane (`:9090`)

### Unauthenticated
```
GET /healthz      # liveness
GET /readyz       # readiness
GET /metrics      # Prometheus exposition (no secret/key_id labels)
```

### Token-authenticated (`/admin/keys`)
Authenticate with the admin token (`Authorization: Bearer <INFERPLANE_ADMIN_TOKEN>`).

```
POST   /admin/keys        # issue a virtual key (plaintext returned once)
GET    /admin/keys        # list key metadata (never plaintext)
DELETE /admin/keys/{id}   # revoke a key
```

## Error Codes

| Code | Meaning |
|------|---------|
| 400 | Bad request (malformed body) |
| 401 | Missing/invalid virtual key (data plane) or admin token (admin plane) |
| 403 | Model not in the principal's allow-list, or governance `block` (quota/budget/rate) |
| 404 | Unknown model (not in the gateway's `models` map) |
| 429 | Rate limit exceeded (`on_exceeded: block`) |
| 5xx | Upstream provider error (teed through) or gateway failure |

Errors are returned in the shape of the ingress protocol (Anthropic error object on the
Anthropic ingress; OpenAI error object on the OpenAI ingress).

## CLI

```
inferplane serve  --config <path>
inferplane keys   create --team <t> --models <csv> --store <path>
inferplane keys   list   --store <path>
inferplane keys   revoke --id <key_id> --store <path>
inferplane audit  verify --file <path>
```
