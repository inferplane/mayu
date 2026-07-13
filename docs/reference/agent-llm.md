# Agent · LLM / Agent · LLM 구현 상세

[![English](https://img.shields.io/badge/Language-English-blue)](#english)
[![한국어](https://img.shields.io/badge/Language-한국어-red)](#korean)

<a id="english"></a>
## English

### 1. Overview
The LLM-facing core: the provider abstraction that talks to Anthropic, Amazon Bedrock,
and OpenAI-compatible upstreams, plus the canonical schema used to convert between
client and upstream protocols without losing thinking blocks or `cache_control`.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Provider interface | `providers/provider.go` | `Name`/`Models`/`Complete`/`Stream`, optional `TokenCounter` |
| Registry | `providers/registry.go` | `Register`/`New` factory by type string |
| Anthropic provider | `providers/anthropic/` | Messages passthrough, verbatim body, byte-exact SSE |
| Bedrock provider | `providers/bedrock/` | Claude→InvokeModel, non-Claude→Converse; SDK isolated |
| OpenAI-compatible | `providers/openaicompat/` | vLLM/Ollama; order-preserving model rewrite |
| Mock provider | `providers/testing/mockprovider/` | deterministic provider for unit tests |
| Canonical schema | `pkg/schema/` | Anthropic-superset types, Extra preservation, SSE writer |
| Filter chain | `internal/filter/` | `RequestFilter` interface + registry (the spec's filter chain ⑥, ADR-009) |
| PII mask filter | `plugins/piimask/` | opt-in regex+Luhn PII masking → typed placeholders; one-way (no vault); masks messages text only |
| Tracing | `internal/tracing/` | opt-in OTel: OTLP exporter + GenAI-semconv spans + W3C propagation + trace_id in audit; no-op default (ADR-011) |

### 3. Key Decisions
- One package per provider; adding a provider is one package + a blank import (zero core diff, §8).
- Canonical schema is an Anthropic-superset so thinking blocks and `cache_control` survive conversion.
- Bedrock Claude uses InvokeModel with a cache-safe top-level-only model rewrite; the event stream is re-serialized to Anthropic SSE.
- PII masking is OPT-IN per team: it re-serializes the body (cache loss, ~10× cost — warned, not silent), updates both RawBody and Parsed (so the openai_compatible Parsed-conversion path can't leak), masks text only (never system/tool/cache_control), and fails CLOSED (ADR-009).
- Bedrock Guardrails (D6, ADR-019) are applied on the DATA PLANE — every InvokeModel/InvokeModelWithResponseStream/Converse/ConverseStream call — not just surfaced in the console: a provider-level default (config `guardrail_id`/`guardrail_version`) plus an optional per-team override (`teams.guardrail_id`/`guardrail_version`), with the override winning but no per-team opt-out (a team can pick a different guardrail, never remove the default).
- Bedrock upstream errors (e.g. a throttled model) are classified into their real HTTP status (`providers/bedrock/errors.go`) and returned as a `providers.UpstreamError`, so the client sees the actual 429/4xx/5xx instead of a generic 502 — applied on non-streaming calls and the pre-first-byte error of a stream open; a mid-stream error (after the first SSE event is already committed) cannot change the HTTP status and is left as-is (existing truncated-stream handling).

### 4. Code Pointers
- `providers/provider.go` — the interface every provider implements; `ProxyRequest.GuardrailID`/`GuardrailVersion` (D6) is the narrow provider-isolation exception carrying a per-team override
- `providers/bedrock/invoke.go` — InvokeModel body build + SSE re-serialization
- `providers/bedrock/client.go` — `Guardrail` type, `buildGuardrailConfig`/`buildGuardrailStreamConfig`
- `providers/bedrock/errors.go` — AWS SDK error → HTTP status classification, synthesized Anthropic-shaped error body
- `pkg/schema/extra.go` — unknown-field preservation + case-collision rejection

### 5. Cross-references
- Related modules: `internal/router` (resolution/fallback), `internal/openai` (conversion), `internal/keystore` (team-record guardrail override)
- Related ADRs: docs/decisions/ADR-019-bedrock-guardrails-data-plane.md
- Related runbooks: docs/runbooks/

<a id="korean"></a>
## 한국어

### 1. 개요
LLM 대면 코어입니다. Anthropic·Amazon Bedrock·OpenAI 호환 상위와 통신하는 공급자
추상화와, thinking 블록이나 `cache_control`을 잃지 않고 클라이언트/상위 프로토콜을
변환하는 canonical schema로 구성됩니다.

### 2. 구성요소
| 구성요소 | 경로 | 목적 |
|---|---|---|
| Provider 인터페이스 | `providers/provider.go` | `Name`/`Models`/`Complete`/`Stream`, 선택 `TokenCounter` |
| 레지스트리 | `providers/registry.go` | 타입 문자열 기반 `Register`/`New` 팩토리 |
| Anthropic 공급자 | `providers/anthropic/` | Messages 패스스루, 본문 verbatim, 바이트 정확 SSE |
| Bedrock 공급자 | `providers/bedrock/` | Claude→InvokeModel, non-Claude→Converse; SDK 격리 |
| OpenAI 호환 | `providers/openaicompat/` | vLLM/Ollama; 순서 보존 model 재작성 |
| Mock 공급자 | `providers/testing/mockprovider/` | 단위 테스트용 결정적 공급자 |
| Canonical schema | `pkg/schema/` | Anthropic 상위집합 타입, Extra 보존, SSE writer |
| 필터 체인 | `internal/filter/` | `RequestFilter` 인터페이스 + 레지스트리 (spec 필터 체인 ⑥, ADR-009) |
| PII 마스크 필터 | `plugins/piimask/` | 옵트인 regex+Luhn PII 마스킹 → 타입 placeholder; 단방향(vault 없음); 메시지 텍스트만 |
| 트레이싱 | `internal/tracing/` | 옵트인 OTel: OTLP exporter + GenAI semconv 스팬 + W3C 전파 + audit trace_id; 미설정 시 no-op (ADR-011) |

### 3. 주요 결정
- 공급자당 패키지 하나; 공급자 추가는 패키지 하나 + blank import(코어 무수정, §8).
- canonical schema는 Anthropic 상위집합이라 변환에서 thinking 블록·`cache_control` 보존.
- Bedrock Claude는 캐시 안전한 최상위 전용 model 재작성으로 InvokeModel 사용; event stream은 Anthropic SSE로 재직렬화.
- Bedrock Guardrails(D6, ADR-019)는 데이터플레인에서 적용됩니다 — InvokeModel/InvokeModelWithResponseStream/Converse/ConverseStream 모든 호출 경로에서 실제로 적용되며 콘솔 표시로만 그치지 않습니다: provider 기본값(config `guardrail_id`/`guardrail_version`) + 선택적 팀별 오버라이드(`teams.guardrail_id`/`guardrail_version`), 오버라이드가 우선하지만 팀별 opt-out은 없습니다(다른 가드레일 선택만 가능, 기본값 제거는 불가).
- Bedrock의 upstream 에러(예: 모델 스로틀링)는 실제 HTTP 상태로 분류되어(`providers/bedrock/errors.go`) `providers.UpstreamError`로 반환됩니다 — 클라이언트는 뭉개진 502가 아니라 실제 429/4xx/5xx를 봅니다. non-streaming 호출과 스트림이 열리기 전(첫 바이트 이전) 에러에 적용되며, mid-stream 에러(이미 첫 SSE 이벤트가 커밋된 뒤)는 그 시점에 HTTP 상태를 바꿀 수 없어 기존 truncated-stream 처리를 그대로 유지합니다.

### 4. 코드 포인터
- `providers/provider.go` — 모든 공급자가 구현하는 인터페이스; `ProxyRequest.GuardrailID`/`GuardrailVersion`(D6)은 팀별 오버라이드를 전달하는 좁은 provider-isolation 예외
- `providers/bedrock/invoke.go` — InvokeModel 본문 구성 + SSE 재직렬화
- `providers/bedrock/client.go` — `Guardrail` 타입, `buildGuardrailConfig`/`buildGuardrailStreamConfig`
- `providers/bedrock/errors.go` — AWS SDK 에러 → HTTP 상태 분류, Anthropic 형태 에러 바디 합성
- `pkg/schema/extra.go` — 미지 필드 보존 + 대소문자 충돌 거부

### 5. 상호 참조
- 관련 모듈: `internal/router`(해석/폴백), `internal/openai`(변환), `internal/keystore`(팀 레코드 가드레일 오버라이드)
- 관련 ADR: docs/decisions/ADR-019-bedrock-guardrails-data-plane.md
- 관련 런북: docs/runbooks/
