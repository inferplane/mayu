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
| Anthropic ingress | `internal/server/anthropicapi/` | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`. Unknown-model 404 and disallowed-model 403 list the key's allow-filtered available models (ADR-021) |
| OpenAI ingress | `internal/server/openaiapi/` | `/v1/chat/completions`, `/v1/models` (same discoverable 404/403, ADR-021) |
| Usage API | `internal/server/usageapi/usage.go` | `GET /v1/usage` — data-plane, KeyAuth; the caller's own effective governance state (per-key + team budget/quota, integer µUSD, unlimited dims null). Never echoes `key_id` (ADR-021) |
| Admin keys API | `internal/server/adminapi/keys.go` | issue / list / revoke virtual keys (per-team entitlement + admin audit, ADR-004) |
| Admin teams/users API | `internal/server/adminapi/teams.go` | `GET /admin/teams` (any AdminAuth identity); `PUT`/`DELETE /admin/teams/{name}` upsert/delete a team governance record (**full-admin only**, ADR-016 D3) — enforced dynamically in the request hot path via `Governor.SetTeamLookup`, no restart; `GET /admin/users` derived read-only owner projection (no users table, no per-user spend) |
| Whoami API | `internal/server/adminapi/whoami.go` | `GET /admin/whoami` secret-free resolved identity (subject/teams/is_admin/auth_method) for self-service key issuance (ADR-010) |
| Admin key console | `internal/server/adminui/` | `/admin/ui/` embedded static console (data-free, unauthenticated; data via `/admin/keys`, ADR-001) |
| Config view/write API | `internal/server/configapi/` | `GET /admin/config` read-only topology (ADR-005); `PUT`/`DELETE /admin/providers/{name}` + `PUT`/`DELETE /admin/models/{name}` UI-write (ADR-008; 405 unless `provider_store` enabled) — a model write's optional `aliases` field is validated for a within-write duplicate at parse time and for a cross-model collision at the assembly's `writeMutation` (ADR-021 follow-up: providerstore alias support); `GET /admin/config/export` secret-free Git export |
| Connection probe | `internal/server/configapi/probe.go` | `POST /admin/providers/test` — tests a **draft** provider (ProviderWrite body, refs only) by resolving the ref server-side and probing the upstream via the provider's `HealthChecker` (ADR-014). **Full-admin only**; SSRF-guarded (metadata endpoint blocked at dial time, optional `probe.allowed_hosts`); stateless (status cached client-side in memory (no sessionStorage; data-free invariant)). 405 unless `provider_store` enabled. Returns `{ok, latency_ms, detail}` (sanitized) |
| Model catalog | `internal/server/configapi/catalog.go` | `GET /admin/providers/catalog?type=<t>` — embedded known-model ids for the console typeahead (ADR-014); advisory (unknown type ⇒ empty, never blocks a save) |
| Provider store | `internal/providerstore/` | opt-in DB-authoritative provider/model topology (ADR-008); refs only (no secret column), durable seed marker, Postgres-portable DDL; a model's aliases are stored per model (`model_aliases`), not per fallback-chain target, and flow through the same `Router.Canonical`/RBAC-before-canonicalization path as a config-file alias (ADR-021 follow-up) |
| Audit verify API | `internal/server/auditapi/` | `GET /admin/audit/verify` per-sink hash-chain check (ADR-003 #2); complete-prefix, 16 MiB cap |
| Budget alerts API | `internal/server/adminapi/alerts.go` | `GET /admin/alerts/recent` (**full-admin only**, D5b/ADR-017) — recent-fires ring (last 50) of the budget-alert webhook emitter (`internal/alert.Notifier`); per-instance state |
| Provider health API | `internal/server/configapi/health.go` | `GET /admin/providers/health` (**full-admin only**, ADR-014 deferred item) — periodic background prober's current per-provider status snapshot (`configapi.HealthStore`); nil when `provider_health_check` is not configured; on-demand `POST /admin/providers/test` (D2) is unaffected |
| Logs + body API | `internal/server/analyticsapi/logs.go`, `internal/server/adminapi/bodies.go` | `GET /admin/logs` (**full-admin only**, D4/ADR-018) recent request events (id-keyset paginated, `body_ref` marks a captured body); `GET`/`DELETE /admin/bodies/{ref}` (**full-admin only**) fetch/erase a captured body — GET emits `body_accessed` (deduped 5m/viewer), DELETE emits `body_deleted`, both carry `record_ref` never `body_ref`; purged/erased/undecryptable → **410 tombstone**, never 500 |
| Metrics endpoint | `internal/server/metricsapi.go` | unauthenticated Prometheus `/metrics` |
| OpenAI conversion | `internal/openai/convert.go` | OpenAI ⇄ canonical request/response/chunk |

### 3. Key Decisions
- `count_tokens` must always return 200 — a non-200 crashes Claude Code (aliases are canonicalized here too, ADR-021).
- Verbatim body forwarding on protocol match; canonical conversion only on mismatch. Model **aliases** (config `models.<name>.aliases`, ADR-021) are normalized to the canonical name BEFORE RBAC/routing/audit/metrics; on the anthropic verbatim path the only body change is a cache-safe top-level `model` rewrite (nested `cache_control` preserved, HTML escaping off).
- Errors are returned in the ingress protocol's own error shape; the unknown/disallowed-model messages append the allow-filtered available-model list (ADR-021).

### 4. Code Pointers
- `internal/server/anthropicapi/messages.go` — Messages handler, streaming tee, cardinality-safe labels
- `internal/server/openaiapi/chat.go` — Chat Completions handler
- `internal/server/auth.go` — `KeyAuth` virtual-key resolution

### 5. Cross-references
- Related modules: `internal/router`, `internal/governance`, `internal/alert`, `internal/bodystore`, `providers/`
- Related ADRs: docs/decisions/ADR-016-teams-as-keystore-records.md, docs/decisions/ADR-017-budget-alert-webhooks.md, docs/decisions/ADR-018-opt-in-body-logging.md, docs/decisions/ADR-021-ticket-driven-ux-fixes.md
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
| Anthropic 인그레스 | `internal/server/anthropicapi/` | `/v1/messages`, `/v1/messages/count_tokens`, `/v1/models`. 미등록 모델 404·미허용 모델 403은 키의 allow-필터된 사용 가능 모델 목록을 포함 (ADR-021) |
| OpenAI 인그레스 | `internal/server/openaiapi/` | `/v1/chat/completions`, `/v1/models` (동일한 discoverable 404/403, ADR-021) |
| Usage API | `internal/server/usageapi/usage.go` | `GET /v1/usage` — 데이터 플레인, KeyAuth; 호출자 본인의 거버넌스 상태(키별+팀 예산/쿼터, 정수 µUSD, 무제한 차원은 null). `key_id` 절대 미노출 (ADR-021) |
| 관리 키 API | `internal/server/adminapi/keys.go` | 가상 키 발급 / 목록 / 폐기 (팀별 권한 + 관리 감사, ADR-004) |
| 관리 팀/유저 API | `internal/server/adminapi/teams.go` | `GET /admin/teams`(모든 AdminAuth 신원); `PUT`/`DELETE /admin/teams/{name}` 팀 거버넌스 레코드 upsert/삭제(**풀 어드민 전용**, ADR-016 D3) — `Governor.SetTeamLookup`으로 요청 hot path에서 재시작 없이 동적 적용; `GET /admin/users` 파생 읽기 전용 owner 프로젝션(유저 테이블 없음, 유저별 spend 없음) |
| Whoami API | `internal/server/adminapi/whoami.go` | `GET /admin/whoami` 시크릿 무노출 신원(subject/teams/is_admin/auth_method) — 셀프서비스 키 발급용 (ADR-010) |
| 관리 키 콘솔 | `internal/server/adminui/` | `/admin/ui/` 내장 정적 콘솔(데이터 없음·무인증, 데이터는 `/admin/keys` 경유, ADR-001) |
| Config 뷰/쓰기 API | `internal/server/configapi/` | `GET /admin/config` 읽기 전용 토폴로지 (ADR-005); `PUT`/`DELETE /admin/providers/{name}` + `PUT`/`DELETE /admin/models/{name}` UI 쓰기 (ADR-008; `provider_store` 미설정 시 405) — 모델 쓰기의 선택적 `aliases` 필드는 파싱 시 동일 요청 내 중복을 검증하고, 다른 모델과의 충돌은 어셈블리의 `writeMutation`에서 검증(ADR-021 후속: providerstore alias 지원); `GET /admin/config/export` 시크릿 무노출 Git export |
| 연결 프로브 | `internal/server/configapi/probe.go` | `POST /admin/providers/test` — **드래프트** 프로바이더(ProviderWrite 본문, 참조만)를 서버에서 ref 해석 후 `HealthChecker`로 업스트림 연결 시험 (ADR-014). **풀 어드민 전용**; SSRF 가드(메타데이터 차단, 선택적 `probe.allowed_hosts`); 무상태(상태는 클라이언트 인메모리 캐시). `provider_store` 미설정 시 405. `{ok, latency_ms, detail}`(살균) 반환 |
| 모델 카탈로그 | `internal/server/configapi/catalog.go` | `GET /admin/providers/catalog?type=<t>` — 콘솔 typeahead용 내장 모델 ID (ADR-014); 어드바이저리(미지 타입 ⇒ 빈 목록, 저장 차단 안 함) |
| Provider 스토어 | `internal/providerstore/` | 옵트인 DB 권위 프로바이더/모델 토폴로지 (ADR-008); ref만 저장(시크릿 컬럼 없음), durable seed 마커, Postgres 이식 가능 DDL; 모델 alias는 폴백 체인 target이 아니라 모델 단위로 저장(`model_aliases`)되며 config 파일 alias와 동일한 `Router.Canonical`/RBAC-이전-정규화 경로를 탄다 (ADR-021 후속) |
| Audit verify API | `internal/server/auditapi/` | `GET /admin/audit/verify` sink별 해시체인 검증 (ADR-003 #2); 완전 prefix, 16 MiB 캡 |
| 예산 알림 API | `internal/server/adminapi/alerts.go` | `GET /admin/alerts/recent` (**풀 어드민 전용**, D5b/ADR-017) — 예산 알림 웹훅 발신기(`internal/alert.Notifier`)의 최근 발화(최대 50건) 링; 인스턴스별 상태 |
| Provider 헬스 API | `internal/server/configapi/health.go` | `GET /admin/providers/health` (**풀 어드민 전용**, ADR-014 deferred item) — 주기적 백그라운드 프로버의 provider별 현재 상태 스냅샷(`configapi.HealthStore`); `provider_health_check` 미설정 시 nil; 온디맨드 `POST /admin/providers/test`(D2)는 영향 없음 |
| 로그 + 본문 API | `internal/server/analyticsapi/logs.go`, `internal/server/adminapi/bodies.go` | `GET /admin/logs` (**풀 어드민 전용**, D4/ADR-018) 최근 요청 이벤트(id keyset 페이지네이션, `body_ref`는 본문 저장 표시); `GET`/`DELETE /admin/bodies/{ref}` (**풀 어드민 전용**) 저장 본문 조회/삭제 — GET은 `body_accessed`(뷰어별 5분 dedupe), DELETE는 `body_deleted` 발행, 둘 다 `record_ref`만 가지며 `body_ref`는 절대 없음; purge/삭제/복호불가 → **410 톰스톤**, 500 아님 |
| 메트릭 엔드포인트 | `internal/server/metricsapi.go` | 무인증 Prometheus `/metrics` |
| OpenAI 변환 | `internal/openai/convert.go` | OpenAI ⇄ canonical 요청/응답/청크 |

### 3. 주요 결정
- `count_tokens`는 항상 200 반환 — 비-200은 Claude Code를 크래시시킴 (여기서도 alias를 canonical로 정규화, ADR-021).
- 프로토콜 일치 시 본문 verbatim 전달, 불일치 시에만 canonical 변환. 모델 **alias**(config `models.<name>.aliases`, ADR-021)는 RBAC/라우팅/감사/메트릭 이전에 canonical로 정규화; anthropic verbatim 경로의 유일한 본문 변경은 캐시 안전 top-level `model` 재작성(nested `cache_control` 보존, HTML 이스케이프 off).
- 오류는 인그레스 프로토콜 고유의 오류 형태로 반환; 미등록/미허용 모델 메시지는 allow-필터된 사용 가능 모델 목록을 덧붙임 (ADR-021).

### 4. 코드 포인터
- `internal/server/anthropicapi/messages.go` — Messages 핸들러, 스트리밍 tee, 카디널리티 안전 레이블
- `internal/server/openaiapi/chat.go` — Chat Completions 핸들러
- `internal/server/auth.go` — `KeyAuth` 가상 키 해석

### 5. 상호 참조
- 관련 모듈: `internal/router`, `internal/governance`, `internal/alert`, `internal/bodystore`, `providers/`
- 관련 ADR: docs/decisions/ADR-016-teams-as-keystore-records.md, docs/decisions/ADR-017-budget-alert-webhooks.md, docs/decisions/ADR-018-opt-in-body-logging.md, docs/decisions/ADR-021-ticket-driven-ux-fixes.md
- 관련 런북: docs/runbooks/
