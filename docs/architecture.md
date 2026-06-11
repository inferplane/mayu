# Architecture

<a href="#english"><img src="https://img.shields.io/badge/lang-English-blue.svg" alt="English"></a>
<a href="#korean"><img src="https://img.shields.io/badge/lang-한국어-red.svg" alt="Korean"></a>

---

<a id="english"></a>

# English

## System Overview

inferplane is a single-binary LLM consumption governance gateway that sits between
coding agents (Claude Code, OpenCode) and upstream model providers (Anthropic,
Amazon Bedrock, self-hosted vLLM/Ollama). It authenticates virtual keys, enforces
per-team RBAC, rate limits, quotas, and budgets, forwards each request to a real
provider, and writes a tamper-evident audit record — all with no external SaaS
dependency. It runs as a static, cgo-free binary and is Kubernetes-native.

It is built around two design invariants: a **canonical schema** (an Anthropic-superset
that preserves thinking blocks and `cache_control`) for cross-protocol conversion, and
**verbatim body forwarding** when the ingress protocol matches the upstream protocol, so
prompt-cache hits are never corrupted.

## Components

### Ingress Layer (`internal/server`)
- **Data plane (`:8080`)** -- two ingresses: Anthropic Messages (`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`) and OpenAI Chat Completions (`/v1/chat/completions`, `/v1/models`). `KeyAuth` resolves the virtual key before routing.
- **Admin plane (`:9090`)** -- `/healthz`, `/readyz`, unauthenticated `/metrics`, and the token-authenticated `/admin/keys` API.
- **TLS (`tls.go`)** -- optional self-terminated TLS on the data plane for non-Kubernetes deployments; the admin plane stays plaintext (cluster-internal).

### Governance Layer (`internal/governance`, `limiter`, `budget`, `pricing`)
- **Governor** -- two-phase: `PreCheck` runs BEFORE billing (rate/quota/budget), `Settle` runs AFTER (debits quota tokens and budget microUSD, records cost).
- **Limiter / Budget stores** -- in-memory, two-phase (optimistic check + post-debit), injectable clocks; the v0.2 Redis backend slots behind the same interfaces.
- **Pricing** -- integer microUSD, round-half-even via `math/big`, TTL-tiered cache-write rates, `on_missing: allow` (self-hosted chargeback) | `block`.

### Provider Layer (`providers/*`)
- **anthropic** -- Messages API passthrough; verbatim body, gateway-injected `x-api-key`.
- **bedrock** -- Claude via InvokeModel (native Anthropic body, cache-safe top-level model rewrite, event-stream → Anthropic SSE); non-Claude via Converse. SDK isolated behind invoker/converser interfaces.
- **openaicompat** -- vLLM/Ollama/any OpenAI endpoint; order-preserving model rewrite.
- The `Provider` interface (`Name`, `Models`, `Complete`, `Stream`, optional `TokenCounter`) is the single extension point — a new provider is one package.

### Routing Layer (`internal/router`)
- Resolves model → provider target, walks the priority fallback chain, and guards each provider with a circuit breaker (consecutive-failure → open → backoff → half-open). Failover is **pre-TTFT only**; a mid-stream failure is never retried.

### Persistence Layer (`internal/keystore`, `internal/audit`)
- **Key store** -- SQLite (`modernc.org/sqlite`, cgo-free), Postgres-portable schema; keys SHA-256 hashed at rest behind a `Store` interface.
- **Audit** -- single-writer goroutine, per-instance SHA-256 hash chain, disk-backed WAL (`buffer_then_block`), `audit verify` CLI, ULID record IDs.

### Observability Layer (`internal/metrics`)
- Prometheus registry with OpenTelemetry GenAI semantic-convention naming (`gen_ai_*`) plus `inferplane_*` operational series. Cardinality is config-bounded; a sentinel `_rejected` model label protects pre-resolution 403/404 paths.

### Security Layer (cross-cutting)
- Virtual-key auth + team RBAC (`Principal.Allows`), inline-secret rejection, client/upstream key isolation, no secret leakage on `/metrics`.

## Full Architecture Diagram

```
┌──────────────────────────────────────────────────────────────┐
│                         Clients                                │
│   Claude Code (Anthropic API)      OpenCode (OpenAI API)       │
└───────────────┬─────────────────────────────┬─────────────────┘
                │ ik_... virtual key           │ ik_... virtual key
                ▼                             ▼
┌──────────────────────────────────────────────────────────────┐
│                   Data Plane  :8080  (internal/server)         │
│   ┌────────────┐   KeyAuth (RBAC)   ┌────────────────────┐     │
│   │ /v1/messages│──────┐     ┌──────│ /v1/chat/completions│    │
│   └────────────┘       ▼     ▼      └────────────────────┘     │
│                  ┌──────────────────┐                          │
│                  │    Governor       │  PreCheck (rate/quota/  │
│                  │ (governance)      │   budget) BEFORE bill   │
│                  └────────┬─────────┘                          │
│                           ▼                                    │
│                  ┌──────────────────┐                          │
│                  │     Router        │ fallback chain +        │
│                  │ + circuit breaker │ breaker (pre-TTFT)      │
│                  └────────┬─────────┘                          │
└───────────────────────────┼────────────────────────────────────┘
                            ▼
┌──────────────────────────────────────────────────────────────┐
│                   Provider Layer  (providers/*)                │
│   ┌──────────┐    ┌──────────┐    ┌────────────────────┐       │
│   │ anthropic│    │ bedrock   │    │ openai_compatible  │       │
│   └────┬─────┘    └────┬─────┘    └─────────┬──────────┘       │
└────────┼──────────────┼────────────────────┼──────────────────┘
         ▼              ▼                    ▼
   Anthropic API   Amazon Bedrock      vLLM / Ollama
         │              │                    │
         └──────────────┴────────────────────┘
                        │ Settle (debit quota/budget, record cost)
                        ▼
┌──────────────────────────────────────────────────────────────┐
│  Persistence + Observability                                   │
│  ┌───────────┐  ┌──────────────────┐  ┌───────────────────┐   │
│  │ key store │  │ audit hash chain  │  │ Prometheus /metrics│  │
│  │ (SQLite)  │  │ (WAL, verify)     │  │ :9090 (admin plane)│  │
│  └───────────┘  └──────────────────┘  └───────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

## Data Flow Summary

```
Client -> KeyAuth(RBAC) -> Governor.PreCheck -> Router(fallback+breaker) -> Provider -> Upstream
                                                                                  |
                              ┌───────────────────────────────────────────────────┘
                              ▼
                       Governor.Settle -> quota/budget debit + Pricing(microUSD) -> Audit(hash chain) -> /metrics
```

## Infrastructure

### Deployment
- Container: multi-stage build, `CGO_ENABLED=0` static binary → `distroless/static:nonroot`.
- Kubernetes: Helm chart at `charts/inferplane` (ConfigMap-rendered config, optional IRSA ServiceAccount for Bedrock, `existingSecret` reference — the chart never creates secrets).

### Modules / Resources
| Component | Path | Description |
|-----------|------|-------------|
| Binary | `cmd/inferplane` | serve / keys / audit subcommands |
| Helm chart | `charts/inferplane` | Deployment, Service (data+admin), ServiceAccount, ConfigMap |
| Dashboard | `deploy/grafana/inferplane.json` | 9-panel Prometheus dashboard |

### Deployed Endpoints
- Data plane: `:8080` (`/v1/messages`, `/v1/chat/completions`)
- Admin plane: `:9090` (`/healthz`, `/readyz`, `/metrics`, `/admin/keys`)

## Key Design Decisions

- **Canonical schema = Anthropic-superset, not OpenAI** -- preserves thinking blocks and `cache_control` that the OpenAI shape cannot represent; same-protocol round-trips stay lossless.
- **Verbatim body forwarding on protocol match** -- corrupting `cache_control` turns a 96%-hit prompt cache into a 10× cost regression, so a matching protocol tees `RawBody` byte-for-byte instead of re-serializing.
- **Instance-local governance + SQLite default** -- a single binary boots in 5 minutes with no external DB; the `Store` interface keeps Postgres/Redis HA as a v0.2 swap, not a rewrite.
- **Per-instance segmented audit chain** -- a hash chain per process run survives legitimate restarts without reading as tampering, while remaining verifiable offline.
- **Pre-TTFT-only failover** -- once the first token streams, the response is committed; retrying mid-stream would duplicate or corrupt output.
- **Cost as integer microUSD** -- float accumulation drifts; round-half-even on `math/big` keeps billing exact and overflow-free.

## Operations
- Deployment: see [docs/runbooks/.template.md](runbooks/.template.md) (create `deploy-production.md` from it).
- Decisions: see [docs/decisions/](decisions/).
- Reference: see [docs/reference/INDEX.md](reference/INDEX.md).

---

<a id="korean"></a>

# 한국어

## 시스템 개요

inferplane은 코딩 에이전트(Claude Code, OpenCode)와 상위 모델 공급자(Anthropic,
Amazon Bedrock, 자체 호스팅 vLLM/Ollama) 사이에 위치하는 단일 바이너리 LLM 소비
거버넌스 게이트웨이입니다. 가상 키를 인증하고 팀별 RBAC·rate limit·quota·budget을
적용하며, 각 요청을 실제 공급자로 전달하고 변조 감지(tamper-evident) 감사 레코드를
기록합니다. 외부 SaaS 의존성이 전혀 없으며, cgo 없는 정적 바이너리로 동작하고
Kubernetes-native합니다.

두 가지 설계 불변식을 중심으로 구축됩니다. 교차 프로토콜 변환을 위한 **canonical
schema**(thinking 블록과 `cache_control`을 보존하는 Anthropic 상위집합)와, 인그레스
프로토콜이 상위 프로토콜과 일치할 때의 **본문 verbatim 전달**로, 이를 통해 프롬프트
캐시 적중이 절대 손상되지 않습니다.

## 구성요소

### 인그레스 계층 (`internal/server`)
- **데이터 플레인 (`:8080`)** -- 두 인그레스: Anthropic Messages(`/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`)와 OpenAI Chat Completions(`/v1/chat/completions`, `/v1/models`). 라우팅 전에 `KeyAuth`가 가상 키를 검증합니다.
- **관리 플레인 (`:9090`)** -- `/healthz`, `/readyz`, 무인증 `/metrics`, 그리고 토큰 인증 `/admin/keys` API.
- **TLS (`tls.go`)** -- 비-Kubernetes 배포를 위한 데이터 플레인 자체 TLS 종단(선택); 관리 플레인은 평문 유지(클러스터 내부용).

### 거버넌스 계층 (`internal/governance`, `limiter`, `budget`, `pricing`)
- **Governor** -- 2단계: `PreCheck`는 과금 전(rate/quota/budget), `Settle`는 과금 후(quota 토큰·budget microUSD 차감, 비용 기록)에 실행됩니다.
- **Limiter / Budget 스토어** -- 인메모리 2단계(낙관적 검사 + 사후 차감), 주입 가능한 clock; v0.2 Redis 백엔드는 동일 인터페이스 뒤로 들어갑니다.
- **Pricing** -- 정수 microUSD, `math/big` 기반 round-half-even, TTL 계층별 cache-write 단가, `on_missing: allow`(자체 호스팅 차지백) | `block`.

### 공급자 계층 (`providers/*`)
- **anthropic** -- Messages API 패스스루; 본문 verbatim, 게이트웨이가 `x-api-key` 주입.
- **bedrock** -- Claude는 InvokeModel(네이티브 Anthropic 본문, 캐시 안전한 최상위 model 재작성, event-stream → Anthropic SSE), non-Claude는 Converse. SDK는 invoker/converser 인터페이스 뒤로 격리.
- **openaicompat** -- vLLM/Ollama/모든 OpenAI 엔드포인트; 순서 보존 model 재작성.
- `Provider` 인터페이스(`Name`, `Models`, `Complete`, `Stream`, 선택 `TokenCounter`)가 단일 확장 지점 — 새 공급자는 패키지 하나입니다.

### 라우팅 계층 (`internal/router`)
- model → 공급자 타깃을 해석하고, 우선순위 폴백 체인을 순회하며, 각 공급자를 서킷 브레이커(연속 실패 → open → 백오프 → half-open)로 보호합니다. 폴백은 **TTFT 이전에만** 수행하며, 스트림 중간 실패는 재시도하지 않습니다.

### 영속 계층 (`internal/keystore`, `internal/audit`)
- **키 스토어** -- SQLite(`modernc.org/sqlite`, cgo 없음), Postgres 이식 가능 스키마; 키는 `Store` 인터페이스 뒤에서 SHA-256으로 저장.
- **감사** -- 단일 writer 고루틴, 인스턴스별 SHA-256 해시 체인, 디스크 백업 WAL(`buffer_then_block`), `audit verify` CLI, ULID 레코드 ID.

### 관측 계층 (`internal/metrics`)
- OpenTelemetry GenAI 시맨틱 컨벤션 네이밍(`gen_ai_*`)과 `inferplane_*` 운영 시리즈를 갖춘 Prometheus 레지스트리. 카디널리티는 config로 제한되며, 사전 해석 단계의 403/404 경로는 센티넬 `_rejected` model 레이블로 보호됩니다.

### 보안 계층 (횡단)
- 가상 키 인증 + 팀 RBAC(`Principal.Allows`), 인라인 시크릿 거부, 클라이언트/상위 키 격리, `/metrics` 시크릿 비노출.

## 전체 아키텍처 다이어그램

```
┌──────────────────────────────────────────────────────────────┐
│                         Clients                                │
│   Claude Code (Anthropic API)      OpenCode (OpenAI API)       │
└───────────────┬─────────────────────────────┬─────────────────┘
                │ ik_... virtual key           │ ik_... virtual key
                ▼                             ▼
┌──────────────────────────────────────────────────────────────┐
│                   Data Plane  :8080  (internal/server)         │
│   ┌────────────┐   KeyAuth (RBAC)   ┌────────────────────┐     │
│   │ /v1/messages│──────┐     ┌──────│ /v1/chat/completions│    │
│   └────────────┘       ▼     ▼      └────────────────────┘     │
│                  ┌──────────────────┐                          │
│                  │    Governor       │  PreCheck (rate/quota/  │
│                  │ (governance)      │   budget) 과금 이전     │
│                  └────────┬─────────┘                          │
│                           ▼                                    │
│                  ┌──────────────────┐                          │
│                  │     Router        │ 폴백 체인 +             │
│                  │ + circuit breaker │ 브레이커 (TTFT 이전)    │
│                  └────────┬─────────┘                          │
└───────────────────────────┼────────────────────────────────────┘
                            ▼
┌──────────────────────────────────────────────────────────────┐
│                   Provider Layer  (providers/*)                │
│   ┌──────────┐    ┌──────────┐    ┌────────────────────┐       │
│   │ anthropic│    │ bedrock   │    │ openai_compatible  │       │
│   └────┬─────┘    └────┬─────┘    └─────────┬──────────┘       │
└────────┼──────────────┼────────────────────┼──────────────────┘
         ▼              ▼                    ▼
   Anthropic API   Amazon Bedrock      vLLM / Ollama
         │              │                    │
         └──────────────┴────────────────────┘
                        │ Settle (quota/budget 차감, 비용 기록)
                        ▼
┌──────────────────────────────────────────────────────────────┐
│  Persistence + Observability                                   │
│  ┌───────────┐  ┌──────────────────┐  ┌───────────────────┐   │
│  │ key store │  │ audit hash chain  │  │ Prometheus /metrics│  │
│  │ (SQLite)  │  │ (WAL, verify)     │  │ :9090 (admin plane)│  │
│  └───────────┘  └──────────────────┘  └───────────────────┘   │
└──────────────────────────────────────────────────────────────┘
```

## 데이터 흐름 요약

```
Client -> KeyAuth(RBAC) -> Governor.PreCheck -> Router(폴백+브레이커) -> Provider -> Upstream
                                                                                  |
                              ┌───────────────────────────────────────────────────┘
                              ▼
                       Governor.Settle -> quota/budget 차감 + Pricing(microUSD) -> Audit(해시 체인) -> /metrics
```

## 인프라

### 배포
- 컨테이너: 멀티스테이지 빌드, `CGO_ENABLED=0` 정적 바이너리 → `distroless/static:nonroot`.
- Kubernetes: `charts/inferplane` Helm 차트(ConfigMap 렌더링 config, Bedrock용 선택 IRSA ServiceAccount, `existingSecret` 참조 — 차트는 시크릿을 생성하지 않음).

### 모듈 / 리소스
| 구성요소 | 경로 | 설명 |
|-----------|------|-------------|
| 바이너리 | `cmd/inferplane` | serve / keys / audit 서브커맨드 |
| Helm 차트 | `charts/inferplane` | Deployment, Service(data+admin), ServiceAccount, ConfigMap |
| 대시보드 | `deploy/grafana/inferplane.json` | 9패널 Prometheus 대시보드 |

### 배포 엔드포인트
- 데이터 플레인: `:8080` (`/v1/messages`, `/v1/chat/completions`)
- 관리 플레인: `:9090` (`/healthz`, `/readyz`, `/metrics`, `/admin/keys`)

## 주요 설계 결정

- **canonical schema = Anthropic 상위집합, OpenAI 아님** -- OpenAI 형태로는 표현할 수 없는 thinking 블록과 `cache_control`을 보존하며, 동일 프로토콜 왕복은 무손실 유지.
- **프로토콜 일치 시 본문 verbatim 전달** -- `cache_control`이 손상되면 96% 적중 프롬프트 캐시가 10배 비용 퇴행으로 바뀌므로, 프로토콜이 일치하면 재직렬화 없이 `RawBody`를 바이트 단위로 그대로 전달.
- **인스턴스 로컬 거버넌스 + SQLite 기본** -- 단일 바이너리가 외부 DB 없이 5분 안에 기동; `Store` 인터페이스가 Postgres/Redis HA를 재작성이 아닌 v0.2 교체로 유지.
- **인스턴스별 분절 감사 체인** -- 프로세스 실행마다의 해시 체인이 정상 재시작을 변조로 읽지 않으면서 오프라인 검증 가능 유지.
- **TTFT 이전 전용 폴백** -- 첫 토큰이 스트리밍되면 응답은 확정; 스트림 중간 재시도는 출력을 중복·손상시킴.
- **비용은 정수 microUSD** -- float 누적은 드리프트하므로, `math/big` round-half-even으로 과금을 정확하고 오버플로 없이 유지.

## 운영
- 배포: [docs/runbooks/.template.md](runbooks/.template.md) 참조(여기서 `deploy-production.md` 작성).
- 결정: [docs/decisions/](decisions/) 참조.
- 레퍼런스: [docs/reference/INDEX.md](reference/INDEX.md) 참조.
