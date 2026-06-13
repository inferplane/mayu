# inferplane

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Version](https://img.shields.io/badge/Version-0.1.0--pre-green.svg)]()
[![Go](https://img.shields.io/badge/Go-1.25-00ADD8.svg)](go.mod)
<a href="#english"><img src="https://img.shields.io/badge/lang-English-blue.svg" alt="English"></a>
<a href="#korean"><img src="https://img.shields.io/badge/lang-한국어-red.svg" alt="Korean"></a>

LLM consumption governance gateway — virtual keys, team RBAC, quotas, budgets, and tamper-evident audit logging for Claude Code / OpenCode traffic. | Claude Code / OpenCode 트래픽을 위한 가상 키·팀 RBAC·쿼터·예산·변조 감지 감사 로깅 LLM 소비 거버넌스 게이트웨이.

---

<a id="english"></a>

# English

## Overview

inferplane sits between your coding agent and the upstream model APIs (Anthropic,
Amazon Bedrock, self-hosted vLLM/Ollama). You point Claude Code / OpenCode at the
gateway with a **virtual key** (`ik_...`); the gateway authenticates the key, enforces
the team's quota/budget, forwards the request to a real provider, and writes a
tamper-evident audit record. Single static binary, Kubernetes-native, Apache-2.0,
**no external SaaS dependency**.

> **Project status: v0.1 pre-release, not yet announced.** APIs, config schema, and
> metric names may still change before the first tagged release.

Design: [docs/specs/2026-06-10-inferplane-gateway-design.md](docs/specs/2026-06-10-inferplane-gateway-design.md) ·
Architecture: [docs/architecture.md](docs/architecture.md)

## Features

- **Virtual keys + team RBAC** — issue `ik_...` keys scoped to a team and an allowed-model list; keys are SHA-256 hashed at rest and shown once.
- **Quotas, budgets, rate limits** — per-team tokens/day, USD/month, and TPM/RPM, enforced two-phase (pre-check before billing, settle after).
- **Tamper-evident audit** — per-instance SHA-256 hash chain with a disk WAL and an offline `audit verify` command.
- **Multi-provider routing** — Anthropic, Amazon Bedrock (Claude via InvokeModel, others via Converse), and any OpenAI-compatible endpoint, with priority fallback and per-provider circuit breakers.
- **Cache-safe forwarding** — verbatim request-body passthrough on protocol match keeps prompt-cache hits intact.
- **OpenTelemetry GenAI metrics** — Prometheus `/metrics` using GenAI semantic conventions, plus a ready-made Grafana dashboard.

## Prerequisites

- Go 1.25+ (to build from source) **or** Docker (to run the image)
- Upstream credentials for the providers you enable (e.g. `ANTHROPIC_API_KEY`)
- For Kubernetes: Helm 3 and kubectl

## Installation

```bash
# Clone the repository
git clone https://github.com/inferplane/mayu.git
cd mayu

# Build the static binary
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane
```

## Usage

### 1. Write a `config.json`

This declares the providers it can reach and the model names clients may request. The
example wires Anthropic direct, an OpenAI-compatible endpoint, and Amazon Bedrock, and
gives a `demo` team a 10M-token/day quota and a $50/month budget:

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
    "anthropic-direct": { "type": "anthropic", "base_url": "https://api.anthropic.com", "api_key_ref": { "env": "ANTHROPIC_API_KEY" } },
    "local-vllm": { "type": "openai_compatible", "base_url": "http://vllm.internal:8000/v1", "api_key_ref": { "env": "VLLM_API_KEY" } },
    "bedrock-us": { "type": "bedrock", "region": "us-west-2", "auth": { "mode": "irsa" } }
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
      "anthropic-direct": { "claude-sonnet-4-6": { "input_per_mtok": 3.0, "output_per_mtok": 15.0 } }
    }
  }
}
```

Provider kinds: `anthropic` (Messages API, Claude Code's native protocol),
`openai_compatible` (any OpenAI Chat Completions endpoint), and `bedrock` (prefer
`auth.mode: irsa` on EKS so no static key is stored). Secrets are referenced from the
environment — inferplane never persists them.

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
(`/healthz`, `/readyz`, unauthenticated `/metrics`, the `/admin/keys` API, and the
minimal admin key console at `http://localhost:9090/admin/ui/` — ADR-001).

For a self-hosted-only setup (Ollama/vLLM, no cloud key), start from
[`examples/config.selfhosted.json`](examples/config.selfhosted.json).

### 3. Issue a virtual key

```bash
docker exec inferplane inferplane keys create \
  --team demo --models '*' \
  --store /var/lib/inferplane/keys.db
# → prints ik_... once (never recoverable — copy it now)
```

Or use the web console: open `http://localhost:9090/admin/ui/`, paste the admin
token, and issue/revoke keys from the page (the token stays in page memory only).
The **Providers** tab shows which providers, endpoints, and auth modes are
wired and the model routing/fallback order (read-only; secrets never shown —
registration stays in config, ADR-005).

**Add a provider without downtime:** edit the config file and send `SIGHUP`
(`kill -HUP <pid>`; on Kubernetes signal PID 1 or roll the pods). The gateway
re-reads config and atomically swaps the provider/model/pricing topology —
in-flight requests, team quotas/budgets, and the audit chain are unaffected; a
bad config rolls back and keeps the old generation serving (ADR-006). Listen
addresses, TLS, and team rate/quota/budget limits still need a restart.

### 4. Point your coding agent at the gateway

```bash
# Claude Code (Anthropic protocol)
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=ik_...
claude
```

OpenCode / any OpenAI client: set the OpenAI base URL to `http://localhost:8080/v1`
and use the same `ik_...` key as the API key.

### 5. Verify audit + metrics

```bash
# Audit hash chain is intact and tamper-evident:
docker exec inferplane inferplane audit verify --file /var/lib/inferplane/audit.jsonl

# Prometheus exposition (OTel GenAI naming) — counter rises after each request:
curl -s localhost:9090/metrics | grep inferplane_requests_total
```

## Configuration

Secrets are referenced, never inlined. Operational environment variables:

| Variable | Description | Default |
|----------|-------------|---------|
| `ANTHROPIC_API_KEY` | Upstream Anthropic key (referenced by an `api_key_ref`) | — |
| `VLLM_API_KEY` | Key for an OpenAI-compatible upstream | — |
| `INFERPLANE_ADMIN_TOKEN` | Bearer token for the `/admin/keys` API (break-glass; opaque — JWT-shaped values are rejected when OIDC is enabled) | — |

### Optional: OIDC SSO for the admin plane (free, ADR-004)

Admins can authenticate with an ID token from your IdP (Dex/Keycloak/Okta)
instead of the static token — the gateway validates it offline against the
IdP's JWKS and maps the `groups` claim to teams. The static token keeps
working as break-glass even with the IdP down.

```json
"admin_auth": {
  "token_refs": [ { "env": "INFERPLANE_ADMIN_TOKEN" } ],
  "oidc": {
    "issuer": "https://idp.example.com/realms/dev",
    "client_id": "inferplane-admin",
    "admin_groups": ["platform-admins"],
    "group_mappings": [ { "group": "team-alpha", "teams": ["alpha"] } ]
  }
}
```

Members mapped to a team can issue/revoke keys for that team only;
`admin_groups` members for any team. Every admin action — including denied
attempts — lands in the tamper-evident audit chain with the opaque OIDC
`sub` (never email).
| `CLAUDE_NOTIFY_WEBHOOK` | Optional webhook for dev notifications | unset |

TLS: for non-Kubernetes deployments set `server.tls.cert_file` / `server.tls.key_file`
to terminate TLS on the data plane directly. On Kubernetes, terminate at the ingress or
service mesh instead.

## Project Structure

```
cmd/inferplane/    # Binary entrypoint: serve / keys / audit
internal/          # Gateway internals (server, router, governance, keystore, audit, ...)
providers/         # Upstream providers (anthropic, bedrock, openaicompat) — the extension surface
pkg/               # Public packages: schema (canonical types), ulid
charts/inferplane/ # Helm chart
deploy/grafana/    # Grafana dashboard
docs/              # specs, decisions, runbooks, reference, architecture
```

## Testing

```bash
go test ./... -race      # full Go suite with the race detector
go vet ./...             # static checks
gofmt -l .               # must print nothing
bash tests/run-all.sh    # harness tests (hooks, secret patterns, structure)
```

## API Documentation

The gateway exposes a data plane (`:8080`) and an admin plane (`:9090`). Full endpoint
contract, authentication, and error codes are in
[docs/api-reference.md](docs/api-reference.md).

```
inferplane serve  --config <path>
inferplane keys   create --team <t> --models <csv> --store <path>
inferplane keys   list   --store <path>
inferplane keys   revoke --id <key_id> --store <path>
inferplane audit  verify --file <path>
```

## Contributing

1. Fork the repository
2. Create your branch (`git checkout -b feat/amazing-feature`)
3. Commit changes, **signed off** (`git commit -s -m 'feat: add amazing feature'`) — CI rejects unsigned commits ([DCO](https://developercertificate.org/))
4. Push to the branch (`git push origin feat/amazing-feature`)
5. Open a Pull Request

Provider PRs touch only `providers/<name>/`, the blank-import line in
`cmd/inferplane/main.go`, and provider docs — zero core diff (design §8). See
[CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache-2.0 — see [LICENSE](LICENSE).

## Contact

- Maintainer: [@atomoh](https://github.com/atomoh)
- Issues: https://github.com/inferplane/mayu/issues
- Security: ojs0106@gmail.com (see [SECURITY.md](SECURITY.md))

---

<a id="korean"></a>

# 한국어

## 개요

inferplane은 코딩 에이전트와 상위 모델 API(Anthropic, Amazon Bedrock, 자체 호스팅
vLLM/Ollama) 사이에 위치합니다. **가상 키**(`ik_...`)로 Claude Code / OpenCode를
게이트웨이에 연결하면, 게이트웨이가 키를 인증하고 팀의 쿼터/예산을 적용하며 요청을
실제 공급자로 전달하고 변조 감지 감사 레코드를 기록합니다. 단일 정적 바이너리,
Kubernetes-native, Apache-2.0, **외부 SaaS 의존성 없음**.

> **프로젝트 상태: v0.1 사전 릴리스, 아직 미공개.** 첫 태그 릴리스 전까지 API, config
> 스키마, 메트릭 이름이 변경될 수 있습니다.

설계: [docs/specs/2026-06-10-inferplane-gateway-design.md](docs/specs/2026-06-10-inferplane-gateway-design.md) ·
아키텍처: [docs/architecture.md](docs/architecture.md)

## 주요 기능

- **가상 키 + 팀 RBAC** — 팀과 허용 모델 목록으로 범위가 지정된 `ik_...` 키 발급; 키는 저장 시 SHA-256 해시되고 1회만 표시됩니다.
- **쿼터·예산·rate limit** — 팀별 tokens/day, USD/month, TPM/RPM을 2단계로 적용(과금 전 사전 검사, 이후 정산).
- **변조 감지 감사** — 디스크 WAL과 오프라인 `audit verify` 명령을 갖춘 인스턴스별 SHA-256 해시 체인.
- **다중 공급자 라우팅** — Anthropic, Amazon Bedrock(Claude는 InvokeModel, 그 외 Converse), 모든 OpenAI 호환 엔드포인트를 우선순위 폴백과 공급자별 서킷 브레이커로.
- **캐시 안전 전달** — 프로토콜 일치 시 요청 본문 verbatim 패스스루로 프롬프트 캐시 적중 유지.
- **OpenTelemetry GenAI 메트릭** — GenAI 시맨틱 컨벤션 Prometheus `/metrics`와 준비된 Grafana 대시보드.

## 사전 요구 사항

- Go 1.25+ (소스 빌드 시) **또는** Docker (이미지 실행 시)
- 활성화하는 공급자의 상위 자격 증명(예: `ANTHROPIC_API_KEY`)
- Kubernetes 사용 시: Helm 3, kubectl

## 설치 방법

```bash
# 저장소 클론
git clone https://github.com/inferplane/mayu.git
cd mayu

# 정적 바이너리 빌드
CGO_ENABLED=0 go build -trimpath -o bin/inferplane ./cmd/inferplane
```

## 사용법

### 1. `config.json` 작성

도달 가능한 공급자와 클라이언트가 요청할 수 있는 모델 이름을 선언합니다. 아래 예시는
Anthropic 직접, OpenAI 호환 엔드포인트, Amazon Bedrock을 연결하고 `demo` 팀에 일
1000만 토큰 쿼터와 월 $50 예산을 부여합니다:

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
    "anthropic-direct": { "type": "anthropic", "base_url": "https://api.anthropic.com", "api_key_ref": { "env": "ANTHROPIC_API_KEY" } },
    "local-vllm": { "type": "openai_compatible", "base_url": "http://vllm.internal:8000/v1", "api_key_ref": { "env": "VLLM_API_KEY" } },
    "bedrock-us": { "type": "bedrock", "region": "us-west-2", "auth": { "mode": "irsa" } }
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
      "anthropic-direct": { "claude-sonnet-4-6": { "input_per_mtok": 3.0, "output_per_mtok": 15.0 } }
    }
  }
}
```

공급자 종류: `anthropic`(Messages API, Claude Code의 네이티브 프로토콜),
`openai_compatible`(모든 OpenAI Chat Completions 엔드포인트), `bedrock`(EKS에서는
정적 키를 저장하지 않도록 `auth.mode: irsa` 권장). 시크릿은 환경에서 참조되며,
inferplane은 이를 저장하지 않습니다.

### 2. 실행

```bash
docker run -d --name inferplane \
  -e ANTHROPIC_API_KEY=sk-ant-... \
  -e INFERPLANE_ADMIN_TOKEN=admin-secret \
  -v "$PWD/config.json":/etc/inferplane/config.json \
  -v inferplane-data:/var/lib/inferplane \
  -p 8080:8080 -p 9090:9090 \
  inferplane:0.1.0
```

포트 `8080`은 **데이터 플레인**(클라이언트 트래픽), `9090`은 **관리 플레인**
(`/healthz`, `/readyz`, 무인증 `/metrics`, `/admin/keys` API, 그리고
`http://localhost:9090/admin/ui/`의 최소 관리 키 콘솔 — ADR-001)입니다.

셀프호스팅 전용 구성(Ollama/vLLM, 클라우드 키 불필요)은
[`examples/config.selfhosted.json`](examples/config.selfhosted.json)에서 시작하세요.

### 3. 가상 키 발급

```bash
docker exec inferplane inferplane keys create \
  --team demo --models '*' \
  --store /var/lib/inferplane/keys.db
# → ik_... 를 1회만 출력 (복구 불가 — 지금 복사)
```

**Providers** 탭에서 어떤 프로바이더·endpoint·인증 모드가 연결돼 있는지와
모델 라우팅/폴백 순서를 볼 수 있습니다 (읽기 전용·시크릿 미표시 — 등록은
config, ADR-005).

웹 콘솔도 가능합니다: `http://localhost:9090/admin/ui/`를 열고 관리자 토큰을
붙여넣은 뒤 페이지에서 키 발급/폐기 (토큰은 페이지 메모리에만 유지됩니다).

### 4. 코딩 에이전트를 게이트웨이에 연결

```bash
# Claude Code (Anthropic 프로토콜)
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=ik_...
claude
```

OpenCode / 모든 OpenAI 클라이언트: OpenAI base URL을 `http://localhost:8080/v1`로
설정하고 동일한 `ik_...` 키를 API 키로 사용합니다.

### 5. 감사 + 메트릭 검증

```bash
# 감사 해시 체인이 온전하고 변조 감지 가능한지 확인:
docker exec inferplane inferplane audit verify --file /var/lib/inferplane/audit.jsonl

# Prometheus 노출(OTel GenAI 네이밍) — 요청마다 카운터 증가:
curl -s localhost:9090/metrics | grep inferplane_requests_total
```

## 환경 설정

시크릿은 참조되며 인라인되지 않습니다. 운영 환경 변수:

| 변수 | 설명 | 기본값 |
|----------|-------------|---------|
| `ANTHROPIC_API_KEY` | 상위 Anthropic 키(`api_key_ref`로 참조) | — |
| `VLLM_API_KEY` | OpenAI 호환 상위용 키 | — |
| `INFERPLANE_ADMIN_TOKEN` | `/admin/keys` API용 베어러 토큰 | — |
| `CLAUDE_NOTIFY_WEBHOOK` | 개발 알림용 선택 웹훅 | 미설정 |

TLS: 비-Kubernetes 배포에서는 `server.tls.cert_file` / `server.tls.key_file`을
설정해 데이터 플레인에서 직접 TLS를 종단합니다. Kubernetes에서는 ingress 또는
서비스 메시에서 종단하십시오.

## 프로젝트 구조

```
cmd/inferplane/    # 바이너리 엔트리포인트: serve / keys / audit
internal/          # 게이트웨이 내부 (server, router, governance, keystore, audit, ...)
providers/         # 상위 공급자 (anthropic, bedrock, openaicompat) — 확장 표면
pkg/               # 공개 패키지: schema(canonical 타입), ulid
charts/inferplane/ # Helm 차트
deploy/grafana/    # Grafana 대시보드
docs/              # specs, decisions, runbooks, reference, architecture
```

## 테스트

```bash
go test ./... -race      # 레이스 검출기 포함 전체 Go 스위트
go vet ./...             # 정적 검사
gofmt -l .               # 아무것도 출력하지 않아야 함
bash tests/run-all.sh    # 하네스 테스트 (훅, 시크릿 패턴, 구조)
```

## API 문서

게이트웨이는 데이터 플레인(`:8080`)과 관리 플레인(`:9090`)을 노출합니다. 전체
엔드포인트 계약, 인증, 오류 코드는 [docs/api-reference.md](docs/api-reference.md)에
있습니다.

```
inferplane serve  --config <path>
inferplane keys   create --team <t> --models <csv> --store <path>
inferplane keys   list   --store <path>
inferplane keys   revoke --id <key_id> --store <path>
inferplane audit  verify --file <path>
```

## 기여 방법

1. 저장소를 Fork 합니다
2. 브랜치를 생성합니다 (`git checkout -b feat/amazing-feature`)
3. 변경 사항을 **sign-off하여** 커밋합니다 (`git commit -s -m 'feat: add amazing feature'`) — CI는 미서명 커밋을 거부합니다 ([DCO](https://developercertificate.org/))
4. 브랜치에 Push 합니다 (`git push origin feat/amazing-feature`)
5. Pull Request 를 엽니다

공급자 PR은 `providers/<name>/`, `cmd/inferplane/main.go`의 blank-import 라인,
공급자 문서만 건드립니다 — 코어 무수정(설계 §8). [CONTRIBUTING.md](CONTRIBUTING.md)
참조.

## 라이선스

Apache-2.0 — [LICENSE](LICENSE) 참조.

## 연락처

- 메인테이너: [@atomoh](https://github.com/atomoh)
- 이슈: https://github.com/inferplane/mayu/issues
- 보안: ojs0106@gmail.com ([SECURITY.md](SECURITY.md) 참조)
