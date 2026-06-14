# Data / 데이터 구성 상세

[![English](https://img.shields.io/badge/Language-English-blue)](#english)
[![한국어](https://img.shields.io/badge/Language-한국어-red)](#korean)

<a id="english"></a>
## English

### 1. Overview
Persistent and in-memory state: the SQLite virtual-key store, the disk-backed
tamper-evident audit log, and the in-memory two-phase governance stores (limiter,
budget). All persistent stores sit behind interfaces so the v0.2 Postgres/Redis
backends are a swap, not a rewrite.

### 2. Components
| Component | Path | Purpose |
|---|---|---|
| Key store | `internal/keystore/sqlite.go` | SHA-256-hashed virtual keys, Postgres-portable schema |
| Store interface | `internal/keystore/keystore.go` | `Store`, `Principal`, `Allows()` (RBAC) |
| Provider store | `internal/providerstore/sqlite.go` | opt-in DB topology (ADR-008): `providers` (refs only — no secret column), `model_targets` (ordered routes), `meta` (durable `seeded` marker); Postgres-portable TEXT-only DDL |
| Audit writer | `internal/audit/writer.go` | single-writer hash chain, WAL truncation |
| Audit WAL | `internal/audit/wal.go` | disk buffer for `buffer_then_block` durability |
| Audit verify | `internal/audit/verify.go` | per-instance segmented chain verification |
| Limiter store | `internal/limiter/limiter.go` | in-memory token bucket (TPM/RPM), two-phase |
| Budget store | `internal/budget/budget.go` | in-memory microUSD budget, two-phase |
| ULID | `pkg/ulid/ulid.go` | monotonic record IDs (Crockford base32) |

### 3. Key Decisions
- SQLite (`modernc.org/sqlite`, cgo-free) default → static binary, 5-minute boot.
- Per-instance audit hash chain so restarts segment cleanly instead of reading as tampering.
- Admin-plane events (`admin_key_created` / `admin_key_revoked` / `admin_denied`, ADR-004) carry `principal.user` (opaque OIDC `sub` — never email) and `principal.auth_method` (`oidc` | `break_glass`); `auth_method` is appended at the END of `PrincipalRef` so pre-change chains still verify byte-exactly (mixed-version fixture test).
- Two-phase stores (check then debit) so a denied request never charges the team.

### 4. Code Pointers
- `internal/keystore/sqlite.go` — schema + SHA-256 lookup
- `internal/audit/writer.go` — single-writer goroutine, pending-based WAL truncate
- `internal/audit/verify.go` — `audit verify` chain check

### 5. Cross-references
- Related modules: `internal/governance`, `internal/server` (auth)
- Related ADRs: docs/decisions/ (SQLite-vs-Postgres decision — to be recorded)
- Related runbooks: docs/runbooks/ (audit verification, backup)

<a id="korean"></a>
## 한국어

### 1. 개요
영속·인메모리 상태입니다. SQLite 가상 키 스토어, 디스크 백업 변조 감지 감사 로그,
인메모리 2단계 거버넌스 스토어(limiter, budget)로 구성됩니다. 모든 영속 스토어는
인터페이스 뒤에 있어 v0.2 Postgres/Redis 백엔드는 재작성이 아닌 교체입니다.

### 2. 구성요소
| 구성요소 | 경로 | 목적 |
|---|---|---|
| 키 스토어 | `internal/keystore/sqlite.go` | SHA-256 해시 가상 키, Postgres 이식 스키마 |
| Store 인터페이스 | `internal/keystore/keystore.go` | `Store`, `Principal`, `Allows()` (RBAC) |
| Provider 스토어 | `internal/providerstore/sqlite.go` | 옵트인 DB 토폴로지 (ADR-008): `providers`(ref만·시크릿 컬럼 없음), `model_targets`(순서 라우트), `meta`(durable `seeded` 마커); Postgres 이식 TEXT 전용 DDL |
| 감사 writer | `internal/audit/writer.go` | 단일 writer 해시 체인, WAL 절단 |
| 감사 WAL | `internal/audit/wal.go` | `buffer_then_block` 내구성용 디스크 버퍼 |
| 감사 verify | `internal/audit/verify.go` | 인스턴스별 분절 체인 검증 |
| Limiter 스토어 | `internal/limiter/limiter.go` | 인메모리 토큰 버킷(TPM/RPM), 2단계 |
| Budget 스토어 | `internal/budget/budget.go` | 인메모리 microUSD budget, 2단계 |
| ULID | `pkg/ulid/ulid.go` | 단조 증가 레코드 ID(Crockford base32) |

### 3. 주요 결정
- SQLite(`modernc.org/sqlite`, cgo 없음) 기본 → 정적 바이너리, 5분 기동.
- 인스턴스별 감사 해시 체인으로 재시작이 변조로 읽히지 않고 깔끔히 분절.
- 2단계 스토어(검사 후 차감)로 거부된 요청은 팀에 과금하지 않음.

### 4. 코드 포인터
- `internal/keystore/sqlite.go` — 스키마 + SHA-256 조회
- `internal/audit/writer.go` — 단일 writer 고루틴, pending 기반 WAL 절단
- `internal/audit/verify.go` — `audit verify` 체인 검사

### 5. 상호 참조
- 관련 모듈: `internal/governance`, `internal/server`(auth)
- 관련 ADR: docs/decisions/ (SQLite-vs-Postgres 결정 — 기록 예정)
- 관련 런북: docs/runbooks/ (감사 검증, 백업)
