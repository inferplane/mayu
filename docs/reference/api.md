# API / API 구성 상세

[![English](https://img.shields.io/badge/Language-English-blue)](#english)
[![한국어](https://img.shields.io/badge/Language-한국어-red)](#korean)

<a id="english"></a>
## English

### 1. Overview
The HTTP surface: a data plane with two ingresses (Anthropic Messages and OpenAI Chat
Completions) and an admin plane (health, metrics, key management). Full endpoint
contract is in [docs/api-reference.md](../api-reference.md).

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Data/admin mux | `internal/server/server.go` | DataMux / AdminMux wiring (auth, governance, metrics) |
| Anthropic ingress | `internal/server/anthropicapi/` | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models` |
| OpenAI ingress | `internal/server/openaiapi/` | `/v1/chat/completions`, `/v1/models` |
| Admin keys API | `internal/server/adminapi/keys.go` | issue / list / revoke virtual keys (per-team entitlement + admin audit, ADR-004) |
| Admin key console | `internal/server/adminui/` | `/admin/ui/` embedded static console (data-free, unauthenticated; data via `/admin/keys`, ADR-001) |
| Config view API | `internal/server/configapi/` | `GET /admin/config` read-only secret-free provider/model topology (ADR-005); writes 405 |
| Audit verify API | `internal/server/auditapi/` | `GET /admin/audit/verify` per-sink hash-chain check (ADR-003 #2); complete-prefix, 16 MiB cap |
| Metrics endpoint | `internal/server/metricsapi.go` | unauthenticated Prometheus `/metrics` |
| OpenAI conversion | `internal/openai/convert.go` | OpenAI ⇄ canonical request/response/chunk |

### 3. Key Decisions
- `count_tokens` must always return 200 — a non-200 crashes Claude Code.
- Verbatim body forwarding on protocol match; canonical conversion only on mismatch.
- Errors are returned in the ingress protocol's own error shape.

### 4. Code Pointers
- `internal/server/anthropicapi/messages.go` — Messages handler, streaming tee, cardinality-safe labels
- `internal/server/openaiapi/chat.go` — Chat Completions handler
- `internal/server/auth.go` — `KeyAuth` virtual-key resolution

### 5. Cross-references
- Related modules: `internal/router`, `internal/governance`, `providers/`
- Related ADRs: docs/decisions/ (none yet)
- Related runbooks: docs/runbooks/

<a id="korean"></a>
## 한국어

### 1. 개요
HTTP 표면입니다. 두 인그레스(Anthropic Messages, OpenAI Chat Completions)를 갖춘
데이터 플레인과 관리 플레인(헬스, 메트릭, 키 관리)으로 구성됩니다. 전체 엔드포인트
계약은 [docs/api-reference.md](../api-reference.md)에 있습니다.

### 2. 구성요소
| 구성요소 | 경로 | 목적 |
|---|---|---|
| 데이터/관리 mux | `internal/server/server.go` | DataMux / AdminMux 배선(auth, governance, metrics) |
| Anthropic 인그레스 | `internal/server/anthropicapi/` | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models` |
| OpenAI 인그레스 | `internal/server/openaiapi/` | `/v1/chat/completions`, `/v1/models` |
| 관리 키 API | `internal/server/adminapi/keys.go` | 가상 키 발급 / 목록 / 폐기 (팀별 권한 + 관리 감사, ADR-004) |
| 관리 키 콘솔 | `internal/server/adminui/` | `/admin/ui/` 내장 정적 콘솔(데이터 없음·무인증, 데이터는 `/admin/keys` 경유, ADR-001) |
| Config 뷰 API | `internal/server/configapi/` | `GET /admin/config` 읽기 전용 시크릿 무노출 프로바이더/모델 토폴로지 (ADR-005); 쓰기 405 |
| Audit verify API | `internal/server/auditapi/` | `GET /admin/audit/verify` sink별 해시체인 검증 (ADR-003 #2); 완전 prefix, 16 MiB 캡 |
| 메트릭 엔드포인트 | `internal/server/metricsapi.go` | 무인증 Prometheus `/metrics` |
| OpenAI 변환 | `internal/openai/convert.go` | OpenAI ⇄ canonical 요청/응답/청크 |

### 3. 주요 결정
- `count_tokens`는 항상 200 반환 — 비-200은 Claude Code를 크래시시킴.
- 프로토콜 일치 시 본문 verbatim 전달, 불일치 시에만 canonical 변환.
- 오류는 인그레스 프로토콜 고유의 오류 형태로 반환.

### 4. 코드 포인터
- `internal/server/anthropicapi/messages.go` — Messages 핸들러, 스트리밍 tee, 카디널리티 안전 레이블
- `internal/server/openaiapi/chat.go` — Chat Completions 핸들러
- `internal/server/auth.go` — `KeyAuth` 가상 키 해석

### 5. 상호 참조
- 관련 모듈: `internal/router`, `internal/governance`, `providers/`
- 관련 ADR: docs/decisions/ (아직 없음)
- 관련 런북: docs/runbooks/
