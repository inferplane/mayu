# inferplane — LLM Consumption Governance Gateway 설계 문서

- 상태: Draft (승인 대기)
- 날짜: 2026-06-10
- 라이선스: Apache 2.0
- 언어: Go
- 대상 릴리스: v0.1

---

## 1. 포지셔닝 및 비전

### 1.1 한 문장 정의

> Envoy 계열 게이트웨이는 플랫폼팀의 인프라 프로젝트를 요구하고,
> LiteLLM/Bifrost는 거버넌스 핵심을 유료화했다.
> **inferplane은 AI팀이 5분 안에 띄우는 단일 바이너리에
> RBAC·quota·budget·변조방지 audit를 전부 무료로 담는다.**

inferplane은 데이터 플레인 성능이나 추론 최적화로 경쟁하지 않는다.
**LLM 소비(consumption)의 거버넌스** — 누가, 어떤 모델을, 얼마나,
얼마의 비용으로 쓰는지 통제하고 기록하는 레이어 — 가 본체다.

### 1.2 경쟁 분석 요약 (2026-06 기준)

| 프로젝트 | 포지션 | 거버넌스 기능의 한계 |
|---|---|---|
| LiteLLM | Python LLM 프록시, 사실상 표준 | SSO·audit log·project-level RBAC가 Enterprise 유료 |
| Bifrost | Go LLM 게이트웨이, 가장 직접적 경쟁자 | virtual key/budget은 무료, RBAC·SSO·immutable audit는 Enterprise-gated |
| Higress | CNCF Sandbox (2026-03), 엔터프라이즈 AI Gateway | standalone 버전은 공식적으로 "프로덕션 미검증, 로컬/테스트용" 명시 |
| kgateway | CNCF Sandbox, Envoy 기반 | Envoy 제어 평면 도입이 전제 — 플랫폼팀 단위 인프라 프로젝트 필요 |
| Envoy AI Gateway | Envoy Gateway 공식 AI 확장 | 동일 — Envoy 운영 역량 전제 |
| llm-d | CNCF Sandbox (2026-03), 분산 추론 프레임워크 | 경쟁이 아닌 **백엔드** — inferplane의 self-hosted upstream 후보 |
| Inference Gateway | Sandbox 심사 중 (cncf/sandbox#486) | 멀티 프로바이더 통합 중심, 거버넌스 깊이 없음 |

**시장 관찰:** Envoy 계열은 기능 매트릭스상 강하지만 시장 가시성이
낮다. 구매자가 다르기 때문이다 — LLM gateway 수요의 대부분은 AI팀의
"오늘 오후에 팀 키 묶기" bottom-up 수요이고, 경쟁 기준은 Envoy가
아니라 **LiteLLM/Bifrost의 도입 경험**이다. 따라서 단일 바이너리는
차별화 포인트가 아니라 **시장 진입의 전제조건**으로 취급한다.

**비어 있는 자리:** "거버넌스 전부 무료 + 변조방지 audit."
이것이 inferplane의 승부처다.

### 1.3 CNCF 전략

- 최종 목표: CNCF Sandbox (현실적 타임라인: 첫 릴리스 후 12~18개월).
- 포지션은 기존 Sandbox 프로젝트와 **보완 관계**로 명시한다:
  "kgateway/Higress는 추론 트래픽을 라우팅하고, inferplane은 LLM
  소비를 거버닝한다. llm-d/vLLM은 inferplane의 백엔드다."
- 첫 커밋부터: DCO 강제, Apache 2.0, GOVERNANCE.md(벤더 중립,
  특정 회사 언급 없음), MAINTAINERS.md, SECURITY.md,
  CODE_OF_CONDUCT.md, OTel GenAI semantic conventions 네이밍.
- 공개 시점: 법무/정책 확인 완료 전까지 public 공개 및 외부 홍보
  보류. 설계와 코드는 공개 가능한 상태로 유지한다.

### 1.4 타깃 클라이언트와 v0.1 성공 기준

1차 타깃은 AI 코딩 툴 트래픽이다:

- **Claude Code**: Anthropic Messages API만 사용 (`ANTHROPIC_BASE_URL`은
  호스트만 교체, 본문은 Anthropic Messages 그대로). OpenAI 포맷 미지원.
- **OpenCode 등**: openai-compatible (`/v1/chat/completions`)이 가장 안정적.

**v0.1 성공 기준:** Claude Code와 OpenCode가 inferplane을 통해
가상 키로 인증하고, 팀 쿼터가 집계되고, 모든 요청이 감사로그에
남으며, **prompt cache hit율이 직결 대비 저하되지 않는** 것.

---

## 2. 아키텍처 개요

### 2.1 요청 흐름

```
 Claude Code                OpenCode
     │ Anthropic Messages       │ OpenAI Chat Completions
     ▼                          ▼
┌─────────────────────────────────────────────────────────────┐
│ Ingress Adapters                                            │
│   /v1/messages, /v1/messages/count_tokens   /v1/chat/...,   │
│   (Anthropic shape)                         /v1/models      │
│   → canonical 변환 (무손실 불변식, §2.2)                      │
├─────────────────────────────────────────────────────────────┤
│ Governance Pipeline (타입드 Filter 체인)                     │
│   ① 요청ID 부여                                              │
│   ② 인증: virtual key → principal(team) 해석                 │
│   ③ 모델 해석: 모델명 → 타깃 체인 (allow-list 검사 포함)      │
│   ④ rate limit (TPM/RPM, 로컬 카운터, 사전 차단)             │
│   ⑤ quota 사전 체크 (낙관적)                                 │
│   ⑥ (v0.2+) 플러그인 필터: PII 마스킹 등                     │
├─────────────────────────────────────────────────────────────┤
│ Router                                                      │
│   우선순위 폴백 체인 + circuit breaker (패시브 헬스체크)     │
├─────────────────────────────────────────────────────────────┤
│ Provider Layer                                              │
│   anthropic │ bedrock (invoke/converse/mantle) │ openai_compat│
│   → canonical 청크 iterator 반환, SSE 직렬화는 코어가 담당   │
├─────────────────────────────────────────────────────────────┤
│ 응답 경로 (스트림 중계와 동시에)                              │
│   ⑦ egress: canonical → ingress 프로토콜 형식으로 직렬화     │
│   ⑧ usage 확정 → quota 사후 차감, budget 집계                │
│   ⑨ audit record 기록 (응답 완료 후)                         │
│   ⑩ 메트릭 갱신                                              │
└─────────────────────────────────────────────────────────────┘
     │                    │                    │
     ▼                    ▼                    ▼
 Anthropic API      Amazon Bedrock      vLLM / Ollama / llm-d
```

핵심 설계 원칙:

- **쿼터는 2단계**: 사전 낙관 체크 + 응답 후 실제 토큰으로 사후 차감.
- **감사로그는 응답 완료 후** 기록 (usage 확정 필요).
- **게이트웨이는 stateless**: 세션 없음. 상태는 quota 스토어와
  키/팀 메타데이터 스토어에만 존재.

### 2.2 Canonical 스키마 — 결정 사항

초기 전제는 "canonical = OpenAI 형식"이었으나, 다음 요구사항과
양립하지 않아 수정한다:

- thinking / redacted_thinking / tool_use / tool_result 블록 보존
- `cache_control` 블록 무변형 통과 (§4.4)
- `anthropic-beta` 등 프로토콜 고유 메타데이터 보존

**결정:** canonical 스키마는 `pkg/schema`의 독자 Go 타입으로,
OpenAI와 Anthropic 양쪽 프로토콜을 덮는 **프로토콜 중립 superset**이다.

불변식:

1. **동일 프로토콜 왕복 무손실**: Anthropic ingress → Anthropic 계열
   provider 경로에서 content 블록·블록 순서·cache_control은
   의미적으로 동일하게 재직렬화된다. (Claude Code 경로의 생명선)
2. **교차 프로토콜 변환은 best-effort**: OpenAI ingress → Claude
   provider 등은 매핑 가능한 범위만 변환하고, 손실 항목은 문서화한다
   (§3.3 변환 충실도 매트릭스).
3. 알 수 없는 provider 필드는 버리지 않고 `x_provider_extensions`
   네임스페이스로 보존한다.

### 2.3 HTTP 스택

- 표준 `net/http` + Go 1.22+ 내장 `ServeMux`. 프레임워크 없음.
  (Fiber 금지 — fasthttp는 `http.Flusher` 기반 SSE, net/http
  미들웨어 생태계와 비호환.)
- 데이터 평면(`:8080`)과 관리 평면(`:9090` — 관리 API, `/metrics`,
  헬스체크) 리스너 분리.

### 2.4 Filter 체인 인터페이스

플러그인과 내장 거버넌스 단계가 공유하는 확장점. raw HTTP가 아닌
**파싱된 canonical 요청/응답 위에서** 동작한다.

```go
// pkg/plugin
type Filter interface {
    Name() string
    // 변형 또는 거부. 거부 시 typed error 반환.
    OnRequest(ctx *RequestContext, req *schema.ChatRequest) error
    // 스트리밍 청크 검사/변형. cache 안전성 주의: 요청 prefix는 불변.
    OnResponseChunk(ctx *RequestContext, chunk *schema.ChatChunk) error
    // usage 확정 후 호출. 차감/집계/로깅용.
    OnComplete(ctx *RequestContext, usage *schema.Usage)
}
```

- 플러그인은 컴파일드인 Go 인터페이스 + 레지스트리 (CoreDNS 패턴).
  `plugins/<name>/` + `init()` 등록.
- 플러그인 ABI(외부 프로세스/Wasm) 동결은 v1.0까지 보류.

---

## 3. Ingress 스펙

전체 스펙 호환은 목표가 아니다. **타깃 클라이언트가 실제로 쓰는
범위를 충실하게** 구현한다.

### 3.1 Anthropic ingress — v0.1 필수 범위

| 엔드포인트 | 요구사항 |
|---|---|
| `POST /v1/messages` | 비스트리밍 + SSE 스트리밍. Anthropic SSE 이벤트 구조 (`message_start`, `content_block_start/delta/stop`, `message_delta`, `message_stop`, `ping`, `error`) 그대로 재현 |
| `POST /v1/messages/count_tokens` | **반드시 정상 응답.** 단순 501/403 반환 시 Claude Code가 크래시한 버그 이력 있음 (truncated JSON 크래시). 절대 5xx/501로 끝내지 않는다 |

count_tokens 처리 전략:

- 해석된 타깃이 Anthropic Direct → upstream count_tokens로 전달.
- 타깃이 Bedrock → Bedrock CountTokens API 사용 (가용 시).
- 그 외 / upstream 실패 → 보수적 추정값으로 응답 (전략은 §10
  미해결 질문). 어떤 경우에도 유효한 JSON 응답을 반환한다.

콘텐츠 보존 (불변식 §2.2-1에 의해 보장):

- `tool_use` / `tool_result` / `thinking` / `redacted_thinking`
  블록을 순서 포함 무손실 보존.
- `cache_control` 블록 무변형 통과 (§4.4 설계 제약).

헤더 처리:

- `anthropic-version`, `anthropic-beta`: upstream으로 패스스루.
- `x-api-key` / `Authorization`: 게이트웨이 virtual key로 해석
  (upstream에는 게이트웨이 소유 자격증명 사용, §5.2).

### 3.2 OpenAI ingress — v0.1 범위

| 엔드포인트 | 요구사항 |
|---|---|
| `POST /v1/chat/completions` | 비스트리밍 + SSE 스트리밍 (`data: {...}` / `data: [DONE]`), tool calling 포함 |
| `GET /v1/models` | 해당 virtual key의 **allow-list에 있는 모델만** 반환 — OpenCode 모델 피커가 사용 |

### 3.3 교차 프로토콜 변환 충실도

| 경로 | 충실도 |
|---|---|
| Anthropic ingress → Anthropic 계열 provider (Direct, Bedrock invoke/mantle) | 무손실 (불변식) |
| OpenAI ingress → openai_compatible provider | 무손실 |
| OpenAI ingress → Anthropic 계열 | best-effort: messages/tools 매핑, thinking 미노출 |
| Anthropic ingress → openai_compatible | best-effort: thinking 블록 drop을 문서화, cache_control 무시(경고 로그) |

v0.1 문서에 이 매트릭스를 그대로 게재한다.

---

## 4. Provider 레이어

### 4.1 Provider 인터페이스

```go
// providers
type Provider interface {
    Name() string
    Models() []schema.ModelInfo
    Complete(ctx context.Context, req *schema.ChatRequest) (*schema.ChatResponse, error)
    // canonical 청크 iterator. SSE 직렬화는 코어(egress)가 담당.
    Stream(ctx context.Context, req *schema.ChatRequest) (iter.Seq2[*schema.ChatChunk, error], error)
}
```

- `iter.Seq2` (Go 1.23+)로 고루틴/채널 누수를 언어 차원에서 차단.
- 인터페이스에서 raw SSE(`io.Reader`)를 노출하지 않는다 — 토큰
  카운팅·감사로그·필터가 전부 canonical 청크 위에서 동작한다.
- 새 프로바이더 추가 = `providers/<name>/` 패키지 +
  `providers/register.go` blank import 1줄 + 문서. **코어 diff 0.**

### 4.2 v0.1 프로바이더 3종

1. `anthropic` — Anthropic API Direct.
2. `bedrock` — Amazon Bedrock (§4.3).
3. `openai_compatible` — vLLM / Ollama / llm-d 등 OpenAI 호환 서버.

### 4.3 Bedrock 전략 — "Claude는 native, 나머지는 Converse"

| 모델군 | 기본 경로 | 근거 |
|---|---|---|
| Claude | **InvokeModel** (native Anthropic Messages shape) | ① Claude Code 자체가 Bedrock에서 Invoke만 사용 (공식 문서 명시) ② Converse가 thinking block 순서를 깨뜨린 사례 존재 (LiteLLM #21128 — Anthropic 프로토콜 위반) ③ `anthropic_beta` 필드 보존이 native 경로에서 안전 |
| Claude (대안) | **Bedrock Mantle** (`bedrock-mantle.{region}.api.aws/anthropic/v1/messages`, 표준 Anthropic Messages shape) | 변환 자체가 사라지는 우선 검토 옵션. 리전 가용성/기능 패리티 검증 필요 (§10) |
| 비-Claude (Kimi, GLM, Nova 등) | **Converse** | 단일 스키마로 N개 모델 커버. 모델 고유 파라미터는 `additionalModelRequestFields`로 전달 |

config에서 모델 단위 오버라이드:
`api: invoke_model | converse | mantle`.

인증: IRSA / Pod Identity / static credentials / profile.
클라이언트 IAM identity는 Bedrock으로 전파하지 않는다 (§5.2).

### 4.4 Prompt caching pass-through — 설계 제약 (v0.1 필수)

**게이트웨이는 프롬프트 prefix를 변형하지 않는다.**

- `cache_control` (Anthropic) / `cachePoint` (Bedrock Converse)
  블록 무변형 통과.
- 금지 사항: 메시지 재정렬, system prompt 수정/주입, 요청 본문에
  영향을 주는 메타데이터 삽입. (HTTP 헤더 추가는 무방 — 캐시 키는
  본문 prefix 기준.)
- 근거: Claude Code 트래픽은 cache hit율 ~96%. 캐시가 깨지면
  사용자 비용이 최대 10배 폭증한다 (cache read = base input의 10%,
  5분 write = 1.25배).
- 이 제약은 Filter 체인에도 적용된다: v0.2+ PII 마스킹 등 요청
  변형 플러그인은 캐시 파괴를 명시 opt-in으로만 허용하고 문서에
  비용 영향을 경고한다.

### 4.5 라우팅 / 폴백 / 서킷브레이커

- 모델 매핑은 **정적 config + 명시적 우선순위 폴백 체인**.
  자동 디스커버리·스마트 라우팅 없음 — 디버깅 가능성이 우선.
- 패시브 헬스체크: 연속 N회 실패 (기본 5) → circuit open →
  지수 백오프 half-open.
- 폴백 트리거: config로 지정 (`rate_limited`, `server_error`,
  `timeout`).
- 폴백 발생 시: 응답 헤더(`x-inferplane-fallback`) + 감사로그 +
  메트릭에 기록.

---

## 5. 거버넌스 레이어

### 5.1 RBAC — Identity / Principal / Policy 3계층

| 계층 | 정의 | 소유 |
|---|---|---|
| **Identity** (누구인가) | 사람의 신원. 직접 만들지 않는다 — OIDC 위임 (Dex/Keycloak/Okta). v0.2 | 외부 IdP |
| **Principal** (게이트웨이 내부 주체) | `user` / `team` / `virtual key`(service account). OIDC `groups` claim → team 매핑 규칙만 게이트웨이가 소유 | inferplane |
| **Policy** (무엇을 할 수 있나) | team × model × action 매트릭스. v0.1은 model allow-list. OPA 연동은 로드맵 | inferplane |

v0.1 구현 범위:

- Virtual API key 발급/폐기 + CLI (`inferplane keys create --team x`).
- 키 → team 바인딩, team → model allow-list + quota/budget.
- 키는 해시로만 저장 (생성 시 1회 표시).

### 5.2 Upstream 인증과 client 인증의 분리 원칙

- 클라이언트는 **게이트웨이 virtual key로만** 인증한다. 실제
  프로바이더 자격증명은 클라이언트에 절대 노출되지 않는다.
- Bedrock 호출은 게이트웨이 자신의 자격(IRSA/Pod Identity)으로
  수행한다. 클라이언트 IAM identity의 SigV4 전파는 하지 않는다.
- 책임추적성은 감사로그의 `principal → virtual key → upstream call`
  체인으로 확보한다.

### 5.3 Rate limit / Quota / Budget — 3분리

비슷해 보이지만 시간 축과 목적이 다르므로 **별도 개념으로 설계**한다.

| 개념 | 시간 축 | 목적 | 집행 방식 |
|---|---|---|---|
| **rate limit** | 초/분 (TPM/RPM) | 보호 | 사전 차단, 로컬 카운터 |
| **quota** | 일/월 (tokens/day 등) | 정책 | 사전 낙관 체크 + 사후 차감 (2단계) |
| **budget** | 비용($) | 재무 | 토큰 × 모델별 단가 테이블로 집계 |

스토어 추상화:

```go
// internal/quota
type LimiterStore interface {
    // 사전 체크 (낙관적). 분산 환경에서 수 % 오버슈트 허용.
    Check(ctx context.Context, key string, estimated int64) (Decision, error)
    // 응답 후 실제 사용량 차감 (비동기 허용).
    Debit(ctx context.Context, key string, actual int64) error
}
```

- 기본 구현: 인메모리 (단일 레플리카).
- HA: Redis/Valkey opt-in. 사전 체크는 로컬 캐시 + 주기 동기화,
  사후 차감은 비동기. **정확한 전역 일관성은 보장하지 않으며
  수 % 오버슈트가 가능함을 문서에 명시한다** — 토큰 쿼터는
  응답 후에야 확정되므로 본질적 한계다.
- 후회 방지: DB 트랜잭션/분산 락 기반 정확한 전역 쿼터는
  핫패스 레이턴시를 파괴하므로 채택하지 않는다.

Budget 구현 제약:

- **단가 테이블을 게이트웨이가 소유**한다. 모델별
  `input / output / cache_read / cache_write` 단가 구분.
  번들 YAML + 사용자 오버라이드, `pricing_version` 필드로 추적.
- usage의 `cache_read_input_tokens`를 **반드시 구분 집계**한다.
  base input 단가로 계산하면 비용이 10배 과대계상된다.
- 스트리밍 중단 시: upstream의 마지막 usage 청크 기준 정산.
  못 받으면 출력 청크 기반 추정 + 감사로그에 `estimated: true`.

### 5.4 감사로그

v0.1 범위: **append-only JSONL + 구조화 레코드.** 레코드 스키마에
`prev_hash` 필드만 예약하고 hash chain 구현은 v0.2.

레코드 스키마 (v0.1):

```json
{
  "schema_version": 1,
  "id": "01J...",                      // ULID, 시간순 정렬 가능
  "ts": "2026-06-10T12:34:56.789Z",
  "instance": "inferplane-7d4f-abc12", // 다중 레플리카 식별
  "principal": {
    "key_id": "ik_5f2...",             // 키 해시 prefix, 원문 금지
    "team": "platform-eng",
    "user": null                       // OIDC 도입(v0.2) 후 채움
  },
  "request": {
    "ingress": "anthropic",            // anthropic | openai
    "model_requested": "claude-sonnet-4-6",
    "model_resolved": "anthropic.claude-sonnet-4-6-v1:0",
    "provider": "bedrock-us",
    "provider_api": "invoke_model",
    "stream": true
  },
  "outcome": {
    "status": 200,
    "fallback_used": false,
    "fallback_chain": [],
    "error": null
  },
  "usage": {
    "input_tokens": 1200,
    "output_tokens": 850,
    "cache_read_input_tokens": 45000,
    "cache_creation_input_tokens": 0,
    "estimated": false
  },
  "cost": {
    "currency": "USD",
    "amount": 0.031,
    "pricing_version": "2026-06-01"
  },
  "latency": { "ttft_ms": 420, "total_ms": 9800 },
  "prev_hash": null                    // v0.1: 예약. v0.2: 해시 체인
}
```

- 프롬프트/응답 본문은 기본 미기록 (메타데이터만). 본문 로깅은
  v0.2+ 플러그인(`prompt-log`)의 명시 opt-in.
- 싱크: `stdout` (K8s 표준 — Fluent Bit/Loki 수거) / `file` / `s3`
  / `webhook`. 포맷 옵션: raw JSONL 또는 CloudEvents 봉투.
- v0.2: hash chain (각 레코드에 이전 레코드 해시) + N분 주기 체인
  헤드 외부 앵커링 (S3 Object Lock 등) + 검증 CLI
  (`inferplane audit verify`).
- 정직한 한계를 문서에 명시: 같은 디스크에 쓰는 한 완전한 변조
  불가는 없다. 해시 체인 + 외부 앵커가 소프트웨어 레벨의 상한이다.

---

## 6. 관측가능성

### 6.1 원칙

- v0.1: **Prometheus 메트릭만.** OTel trace는 v0.2.
- 단, 메트릭/속성 네이밍은 **첫 커밋부터 OTel GenAI semantic
  conventions를 따른다** (`gen_ai.request.model`,
  `gen_ai.usage.input_tokens` 등) — 나중에 trace를 추가해도
  속성 체계가 일치하도록.

### 6.2 v0.1 메트릭 목록

| 메트릭 (Prometheus) | 타입 | 레이블 | GenAI 컨벤션 매핑 |
|---|---|---|---|
| `gen_ai_client_token_usage_total` | counter | `type`(input\|output\|cache_read\|cache_write), `model`, `provider`, `team` | `gen_ai.usage.{input,output}_tokens` |
| `gen_ai_server_request_duration_seconds` | histogram | `model`, `provider`, `ingress`, `status` | `gen_ai.server.request.duration` |
| `gen_ai_server_time_to_first_token_seconds` | histogram | `model`, `provider` | `gen_ai.server.time_to_first_token` |
| `inferplane_requests_total` | counter | `ingress`, `model`, `provider`, `team`, `status` | — |
| `inferplane_fallback_total` | counter | `model`, `from_provider`, `to_provider`, `reason` | — |
| `inferplane_circuit_state` | gauge | `provider` (0=closed,1=half,2=open) | — |
| `inferplane_quota_utilization_ratio` | gauge | `team`, `window` | — |
| `inferplane_budget_spend_usd_total` | counter | `team`, `model`, `cost_type` | — |
| `inferplane_audit_write_failures_total` | counter | `sink` | — |

- 카디널리티 가드: `team`·`model` 레이블은 config에 선언된 값만
  허용 (요청 입력값을 레이블로 직접 사용하지 않음).
- Grafana 대시보드 JSON을 레포에 동봉 (`deploy/grafana/`).

### 6.3 v0.2 trace 스팬 구조 (예고)

`ingress 변환 → 거버넌스 파이프라인 → 라우팅/폴백 → upstream 호출`
단위로 스팬 분리, GenAI conventions 속성 부착.

---

## 7. Config 스키마 전체 예시

```yaml
server:
  listen: :8080
  admin_listen: :9090          # 관리 API + /metrics + 헬스체크

providers:
  anthropic-direct:
    type: anthropic
    api_key_ref: { env: ANTHROPIC_API_KEY }    # env: | file: | secret: 만 허용
  bedrock-us:
    type: bedrock
    region: us-west-2
    auth: { mode: irsa }                       # irsa | pod_identity | static | profile
  local-vllm:
    type: openai_compatible
    base_url: http://vllm.gpu.svc:8000/v1
    api_key_ref: { secret: { name: vllm-key, key: token } }   # optional

models:
  claude-sonnet-4-6:
    targets:                                   # 우선순위 = 배열 순서
      - provider: anthropic-direct
        model: claude-sonnet-4-6
      - provider: bedrock-us
        model: anthropic.claude-sonnet-4-6-v1:0
        api: invoke_model                      # invoke_model | converse | mantle
    fallback:
      on: [rate_limited, server_error, timeout]
      circuit_break_after: 5
  kimi-k2:
    targets:
      - provider: bedrock-us
        model: moonshot.kimi-k2-v1:0
        api: converse
        model_fields: { top_k: 40 }            # additionalModelRequestFields
  qwen-coder:
    targets:
      - { provider: local-vllm, model: Qwen/Qwen2.5-Coder-32B }

teams:
  platform-eng:
    allowed_models: ["claude-sonnet-4-6", "qwen-coder"]
    rate_limit:  { requests_per_minute: 300, tokens_per_minute: 2_000_000 }
    quota:       { tokens_per_day: 50_000_000 }
    budget:      { usd_per_month: 5_000, on_exceeded: block }   # block | warn
  data-science:
    allowed_models: ["*"]
    quota: { tokens_per_day: 200_000_000 }

pricing:
  source: bundled                              # 번들 단가 테이블
  overrides:                                   # 사용자 오버라이드
    claude-sonnet-4-6:
      input_per_mtok: 3.00
      output_per_mtok: 15.00
      cache_read_per_mtok: 0.30
      cache_write_5m_per_mtok: 3.75

plugins: []                                    # v0.1: 내장 거버넌스만. v0.2: pii-mask 등

audit:
  sinks:
    - { type: stdout, format: jsonl }
    - { type: s3, bucket: llm-audit, prefix: gw/, format: jsonl }
  # v0.2: hash_chain: true, anchor: { type: s3_object_lock, interval: 5m }

quota_store:                                   # 생략 시 in-memory (단일 레플리카)
  type: redis
  addr: redis.infra.svc:6379

key_store:                                     # virtual key / team 메타데이터
  type: sqlite                                 # sqlite (기본) | postgres (HA)
  path: /var/lib/inferplane/keys.db
```

제약:

- **비밀값 직접 기입 금지.** `api_key: sk-...` 형태는 config 파싱
  단계에서 거부한다. `env:` / `file:` / `secret:`(K8s) ref만 허용.
- Helm: `values.yaml`에는 비밀이 아닌 설정만. 인증은 전부
  `existingSecret` 참조. Bedrock은 IRSA 경로를 1급 지원.

---

## 8. 프로젝트 구조

```
cmd/inferplane/main.go           # 단일 바이너리 (serve / keys / audit 서브커맨드)
api/                             # (예약) CRD 타입 — config 스키마 안정 후 v1alpha1
pkg/                             # ★ 공개 API는 이 두 패키지뿐
  schema/                        #   canonical 타입 (ChatRequest/Chunk/Usage...)
  plugin/                        #   Filter 인터페이스
internal/
  server/
    anthropicapi/                # /v1/messages, count_tokens ingress
    openaiapi/                   # /v1/chat/completions, /v1/models ingress
    adminapi/                    # 키 관리 API (관리 리스너)
  pipeline/                      # 거버넌스 Filter 체인 실행기
  router/                        # 모델 해석, 폴백, 서킷브레이커
  auth/                          # virtual key, principal 해석
  quota/                         # LimiterStore + inmemory/redis 구현
  budget/                        # 단가 테이블, 비용 집계
  pricing/                       # 번들 단가 데이터 + 오버라이드 병합
  audit/                         # 레코드 빌더 + sinks (stdout/file/s3/webhook)
  config/                        # 로딩, 검증, secret ref 해석
providers/                       # ★ 코어 밖 — 프로바이더 PR은 여기만
  registry.go                    #   Register(name, factory)
  anthropic/
  bedrock/                       #   invoke_model / converse / mantle 경로
  openaicompat/
  testing/mockprovider/          #   결정적 mock (테스트 전용)
plugins/                         # 내장 플러그인 (v0.2: piimask/, promptlog/)
charts/inferplane/               # Helm chart
deploy/grafana/                  # 대시보드 JSON
docs/
  specs/                         # 본 문서
  providers/                     # 프로바이더별 문서
hack/                            # 개발 스크립트
```

원칙:

- `pkg/schema`와 `pkg/plugin`만 import 안정성을 약속. 나머지는
  전부 `internal/`. (전부 `pkg/`에 두면 외부 의존 → SemVer 부채.)
- 프로바이더 PR이 건드리는 곳: `providers/<name>/` +
  `providers/register.go` 1줄 + `docs/providers/<name>.md` +
  config 예시. CI에서 "프로바이더 PR은 코어 diff 0" 검사.

테스트 전략 (3층):

1. **골든 파일 변환 테스트**: 프로토콜 변환별 req/resp 페어를
   `testdata/`에. 무손실 불변식(§2.2-1)을 골든 파일로 검증 —
   특히 thinking 블록 순서, cache_control 위치.
2. **httptest 가짜 upstream**: 프로바이더 통합 테스트.
   실 API 키 없이 CI 통과 필수.
3. **mockprovider E2E**: 라우팅·폴백·쿼터·감사로그 시나리오.

---

## 9. 로드맵

### v0.1 — "Claude Code가 5분 안에 붙는다"

기본기 (없으면 신뢰 상실, 차별화 아님):

- [ ] 이중 ingress (§3 범위: messages + count_tokens + chat/completions + models)
- [ ] Provider 3종: anthropic / bedrock / openai_compatible
- [ ] Virtual key 발급/폐기 + CLI (`inferplane keys create --team x`)
- [ ] 팀 기반 토큰 quota (2단계 집행)
- [ ] Provider failover (명시적 우선순위 + circuit breaker)
- [ ] Prometheus 메트릭 (GenAI conventions 네이밍) + Grafana 대시보드 JSON
- [ ] 단일 바이너리 + Helm chart
- [ ] Prompt caching pass-through 보장 (골든 테스트 포함)

차별화 기능 (여기에 승부):

- [ ] 감사로그: append-only JSONL + 구조화 레코드 (`prev_hash` 예약)
- [ ] RBAC: team × model allow-list
- [ ] rate limit / quota / budget 3분리 + cache 토큰 구분 비용 집계

거버넌스 파일 (첫 커밋부터): DCO, GOVERNANCE.md(벤더 중립),
MAINTAINERS.md, SECURITY.md, CODE_OF_CONDUCT.md, 공개 로드맵.

### v0.2 — 거버넌스 완성

- 감사로그 hash chain + 외부 앵커링 + `audit verify` CLI
- OIDC SSO (Dex/Keycloak/Okta) — Identity 계층 연결, groups → team 매핑
- OTel trace (GenAI conventions)
- 키 발급 셀프서비스 페이지 (최소 UI — 로그인 → 내 키 발급)
- PII 마스킹 플러그인 (캐시 파괴 명시 opt-in + 비용 경고)
- Redis/Valkey quota 스토어 HA 검증, Postgres key store
- OpenSSF Best Practices 배지

### Phase 3 — CNCF Sandbox 신청 (첫 릴리스 후 8~14개월)

- CRD v1alpha1 (`ModelRoute`, `TeamQuota`, `Provider`) — config
  스키마 안정 후 동일 스키마 승격
- 보안 셀프 어세스먼트
- OPA 연동 (Policy 계층 외부화 옵션)
- Wasm 플러그인: **검토만** (wazero 기반, ABI 동결 비용 vs 수요 평가)
- Sandbox 신청서: kgateway/Higress/llm-d와의 보완 관계 1문단,
  cncf/sandbox#486 심사 코멘트 추적 반영
- 제외 유지: MCP 게이트웨이 (Higress/Envoy AI GW가 강한 영역,
  차별점 아님)

커뮤니티 트랙 (공개 시점 이후):

- CNCF Slack 채널, TAG Workloads Foundation 발표, KubeCon CFP
- ADOPTERS.md — 외부 사용자 확보 전략 (조직 internal adopter 가정
  없음), good-first-issue 운영, 타 조직 메인테이너 1명 목표

### Sandbox 신청 시 핵심 메시지

> "kgateway/Higress는 추론 트래픽을 라우팅하고, inferplane은 LLM
> 소비를 거버닝한다. llm-d/vLLM은 inferplane의 백엔드다."

---

## 10. 미해결 질문

| # | 질문 | 결정 시한 |
|---|---|---|
| 1 | **count_tokens 비-Anthropic 타깃 추정 전략**: 보수적 휴리스틱(문자수/4 등) vs 로컬 토크나이저 동봉(바이너리 크기 영향) | v0.1 구현 전 |
| 2 | **Bedrock Mantle 검증**: 리전 가용성, count_tokens/beta 헤더 패리티, IRSA 인증 경로 — 실측 후 기본 경로 승격 여부 | v0.1 중 spike |
| 3 | **단가 테이블 유지 정책**: 번들 YAML 갱신 주기, 모델 신규 출시 시 fallback 동작 (단가 미정 모델 = budget 미집계 + 경고?) | v0.1 구현 전 |
| 4 | **다중 레플리카 감사로그 순서**: 인스턴스별 독립 체인(v0.2 hash chain 시) vs 중앙 시퀀서 — 인스턴스별 체인 + `instance` 필드가 유력 | v0.2 설계 |
| 5 | **스트리밍 중단 비용 추정 정확도**: 출력 청크 기반 추정의 오차 허용 범위와 `estimated` 레코드의 budget 반영 방식 | v0.1 구현 중 |
| 6 | **프로젝트명 "inferplane" 상표/중복 검사**: CNCF 제출 전 필수 (기존 프로젝트·상표 충돌 확인) | 공개 전 |
| 7 | **OpenAI ingress → Claude provider의 tool calling 매핑 상세**: parallel tool calls, tool_choice 옵션별 변환 규칙 | v0.1 구현 중 |
| 8 | **회사 법무/정책 확인**: public 공개 및 외부 홍보 가능 시점 | 외부 의존 |

---

## 부록 A. 명시적 "후회 방지" 결정 기록

| 결정 | 기각한 대안 | 이유 |
|---|---|---|
| net/http + ServeMux | Fiber | fasthttp의 SSE/h2/미들웨어 비호환, CNCF 기여자 이질감 |
| 타입드 Filter 체인 | raw http.Handler 미들웨어 | 플러그인마다 body 재파싱, 스트림 buffering 지옥 |
| iter.Seq2 청크 iterator | io.Reader (raw SSE) | SSE 방언이 코어로 누출, 필터/집계 불가 |
| Claude = InvokeModel | Converse 통일 | thinking 순서 파괴 사례, anthropic_beta 손실, Claude Code 관행과 불일치 |
| 비-Claude = Converse | 모델별 InvokeModel 변환기 N개 | LiteLLM의 변환 지옥 재현 |
| 정적 라우팅 + 명시 폴백 | 자동 디스커버리/스마트 라우팅 | 디버깅 불가능한 라우팅은 운영팀의 적 |
| 쿼터 오버슈트 허용 | 분산 락 정확 집행 | 핫패스 동기 라운드트립이 레이턴시 파괴 |
| 컴파일드인 플러그인 | v0.x에서 Wasm/gRPC ABI 공개 | 스키마 유동기에 ABI 동결 = 진화 차단 |
| 비밀값 ref 강제 | config 평문 허용 | ConfigMap 평문 키 유출 사고 + 보안 감사 평판 |
| pkg 2개만 공개 | 전부 pkg/ | 외부 의존 누적 → SemVer 부채 (k8s staging 반면교사) |
| 파일 config 먼저 | CRD 먼저 | 스키마 유동기 API 마이그레이션 비용 |
| Gateway API 독립+호환 | GatewayClass 구현체 | conformance 유지가 풀타임 업무, kgateway와 정면 경쟁 구도 |
| UI 분리 (셀프서비스만 v0.2) | 코어에 풀 UI | 소수 인원 프로젝트의 프론트 유지보수 세금 |
