# Security / 보안 구현 상세

[![English](https://img.shields.io/badge/Language-English-blue)](#english)
[![한국어](https://img.shields.io/badge/Language-한국어-red)](#korean)

<a id="english"></a>
## English

### 1. Overview
Cross-cutting security: virtual-key authentication, team RBAC, key/secret isolation,
inline-secret rejection, optional self-TLS, and metrics that never leak secrets. These
are non-negotiable invariants (see CLAUDE.md → Security mandates).

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Data-plane auth | `internal/server/auth.go` | `KeyAuth` resolves `ik_...` → Principal |
| Admin auth | `internal/server/adminauth.go` | bearer-token guard for `/admin/keys` |
| RBAC | `internal/keystore/keystore.go` | `Principal.Allows()` (team + allowed models) |
| Key hashing | `internal/keystore/sqlite.go` | SHA-256 at rest; plaintext shown once |
| TLS validation | `internal/server/tls.go` | rejects half-specified cert/key pairs |
| Secret refs | `internal/config/config.go` | `env:`/`file:`/`secret:` only; inline `api_key` rejected |
| Metrics safety | `internal/metrics/metrics.go` | no `key_id`/secret labels; `_rejected` sentinel bounds cardinality |

### 3. Key Decisions
- The gateway never forwards the client key upstream and never exposes its upstream keys to clients (§5.2).
- `/metrics` is unauthenticated but carries no secret or `key_id` and bounds label cardinality.
- Pre-resolution 403/404 paths use a sentinel model label so attacker-supplied model strings can't explode Prometheus series.

### 4. Code Pointers
- `internal/server/auth.go` — virtual-key auth, empty-key bypass guard
- `internal/config/config.go` — secret-ref resolution + inline-secret rejection
- `internal/server/anthropicapi/messages.go` / `openaiapi/chat.go` — `_rejected` label on 403/404

### 5. Cross-references
- Related modules: `internal/keystore`, `internal/audit`, `internal/metrics`
- Related ADRs: docs/decisions/ (none yet)
- Related runbooks: docs/runbooks/ ; policy in `SECURITY.md`

<a id="korean"></a>
## 한국어

### 1. 개요
횡단 보안입니다. 가상 키 인증, 팀 RBAC, 키/시크릿 격리, 인라인 시크릿 거부, 선택적
자체 TLS, 그리고 시크릿을 절대 노출하지 않는 메트릭으로 구성됩니다. 이들은 협상
불가능한 불변식입니다(CLAUDE.md → 보안 mandate 참조).

### 2. 구성요소
| 구성요소 | 경로 | 목적 |
|---|---|---|
| 데이터 플레인 auth | `internal/server/auth.go` | `KeyAuth`가 `ik_...` → Principal 해석 |
| 관리 auth | `internal/server/adminauth.go` | `/admin/keys` 베어러 토큰 가드 |
| RBAC | `internal/keystore/keystore.go` | `Principal.Allows()` (팀 + 허용 모델) |
| 키 해싱 | `internal/keystore/sqlite.go` | 저장 시 SHA-256; 평문은 1회만 표시 |
| TLS 검증 | `internal/server/tls.go` | 반쪽만 지정된 cert/key 쌍 거부 |
| 시크릿 ref | `internal/config/config.go` | `env:`/`file:`/`secret:`만; 인라인 `api_key` 거부 |
| 메트릭 안전 | `internal/metrics/metrics.go` | `key_id`/시크릿 레이블 없음; `_rejected` 센티넬로 카디널리티 제한 |

### 3. 주요 결정
- 게이트웨이는 클라이언트 키를 상위로 전달하지 않고, 상위 키를 클라이언트에 노출하지 않음(§5.2).
- `/metrics`는 무인증이지만 시크릿·`key_id`를 담지 않으며 레이블 카디널리티를 제한.
- 사전 해석 403/404 경로는 센티넬 model 레이블을 사용해 공격자 입력 model 문자열이 Prometheus 시리즈를 폭증시키지 못하게 함.

### 4. 코드 포인터
- `internal/server/auth.go` — 가상 키 인증, 빈 키 우회 가드
- `internal/config/config.go` — 시크릿 ref 해석 + 인라인 시크릿 거부
- `internal/server/anthropicapi/messages.go` / `openaiapi/chat.go` — 403/404의 `_rejected` 레이블

### 5. 상호 참조
- 관련 모듈: `internal/keystore`, `internal/audit`, `internal/metrics`
- 관련 ADR: docs/decisions/ (아직 없음)
- 관련 런북: docs/runbooks/ ; 정책은 `SECURITY.md`
