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

### 4. Code Pointers
- `providers/provider.go` — the interface every provider implements
- `providers/bedrock/invoke.go` — InvokeModel body build + SSE re-serialization
- `pkg/schema/extra.go` — unknown-field preservation + case-collision rejection

### 5. Cross-references
- Related modules: `internal/router` (resolution/fallback), `internal/openai` (conversion)
- Related ADRs: docs/decisions/ (none yet)
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

### 4. 코드 포인터
- `providers/provider.go` — 모든 공급자가 구현하는 인터페이스
- `providers/bedrock/invoke.go` — InvokeModel 본문 구성 + SSE 재직렬화
- `pkg/schema/extra.go` — 미지 필드 보존 + 대소문자 충돌 거부

### 5. 상호 참조
- 관련 모듈: `internal/router`(해석/폴백), `internal/openai`(변환)
- 관련 ADR: docs/decisions/ (아직 없음)
- 관련 런북: docs/runbooks/
