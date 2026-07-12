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
| Key store | `internal/keystore/sqlite.go` | SHA-256-hashed virtual keys, Postgres-portable schema; also owns the `teams` table (D3, ADR-016): `name` (PK), `allowed_models`, `rpm`, `tpm`, `tokens_per_day`, `quota_on_exceeded`, `budget_usd_micros`, `budget_on_exceeded`, `guardrail_id`, `guardrail_version` (D6, ADR-019 — per-team Bedrock Guardrail override), `allowed_regions` (D7, ADR-020 — per-team region lock), `created_at`, `updated_at`. `guardrail_id`/`guardrail_version`/`allowed_regions` are `teams`' ALTER-TABLE migrations (it shipped as a brand-new table under D3, so no pre-existing rows needed catching up until D6); `ensureSchema` shares a small `existingColumns`/`applyMigrations` helper pair between `keys` and `teams` instead of duplicating the PRAGMA-scan loop. |
| Store interface | `internal/keystore/keystore.go` | `Store`, `Principal`, `Allows()` (RBAC); `TeamStore` (`UpsertTeam`/`GetTeam`/`ListTeams`/`DeleteTeam`, D3) is a separate interface so the existing `Store` fakes in `internal/server`'s tests are unaffected |
| Provider store | `internal/providerstore/sqlite.go` | opt-in DB topology (ADR-008): `providers` (refs only — no secret column; `guardrail_id`/`guardrail_version` TEXT columns, D6/ADR-019 — a DB-registered Bedrock provider's own default Guardrail, ALTER-TABLE migrations mirroring `auth_header`), `model_targets` (ordered routes), `model_aliases` (`model`, `alias` PK — ADR-021 follow-up: a model's alias→canonical names, group-level so a multi-target fallback chain can't duplicate them; a brand-new `CREATE TABLE IF NOT EXISTS`, no ALTER-TABLE needed), `meta` (durable `seeded` marker); Postgres-portable TEXT-only DDL |
| Audit writer | `internal/audit/writer.go` | single-writer hash chain, WAL truncation |
| Audit WAL | `internal/audit/wal.go` | disk buffer for `buffer_then_block` durability |
| Audit verify | `internal/audit/verify.go` | per-instance segmented chain verification |
| Audit anchoring | `internal/audit/s3anchor/` | opt-in WORM (S3 Object Lock) chain-head anchoring → tamper-resistant (ADR-012); refs/PII-free anchor objects |
| Limiter store | `internal/limiter/limiter.go` | in-memory token bucket (TPM/RPM), two-phase |
| Budget store | `internal/budget/budget.go` | in-memory microUSD budget, two-phase |
| Body store | `internal/bodystore/` | opt-in captured-body store (D4, ADR-018), OUTSIDE the audit chain: `bodies` table (`ref` PK, `record_id`, `team`, `created_ts`, `expires_ts`, `size`, `wrapped_key_nonce`/`wrapped_key_ct`, `req_nonce`/`req_ct`, `resp_nonce`/`resp_ct` — BLOB/BYTEA ciphertext; `resp_*` nullable = streaming request-only). Envelope AEAD (per-record data key wrapped by a config-ref master key). Two backends (`sqlite.go`/`postgres.go`), TTL + size-cap `Purge`, hard-deletable per-row (GDPR erasure). Key rotation: `inferplane bodies rewrap-key` (ADR-018 deferred item) rewraps `wrapped_key_*` only, via `Store.ListWrappedKeys`/`UpdateWrappedKey` (CAS) — never reads or rewrites `req_*`/`resp_*` |
| Analytics index | `internal/analytics/` | derived usage read-model; `events` table gained `ts` + `body_ref` columns (D4, ADR-018) via ALTER-if-missing (SQLite) / `ADD COLUMN IF NOT EXISTS` (Postgres); backs `GET /admin/logs` |
| ULID | `pkg/ulid/ulid.go` | monotonic record IDs (Crockford base32) |

### 3. Key Decisions
- SQLite (`modernc.org/sqlite`, cgo-free) default → static binary, 5-minute boot.
- Per-instance audit hash chain so restarts segment cleanly instead of reading as tampering.
- Admin-plane events (`admin_key_created` / `admin_key_revoked` / `admin_denied`, ADR-004) carry `principal.user` (opaque OIDC `sub` — never email) and `principal.auth_method` (`oidc` | `break_glass`); `auth_method` is appended at the END of `PrincipalRef` so pre-change chains still verify byte-exactly (mixed-version fixture test).
- Two-phase stores (check then debit) so a denied request never charges the team.
- Prompt/response bodies are NEVER in the audit chain (ADR-003 content-free
  invariant preserved): opt-in `audit.log_bodies` (D4, ADR-018) captures them
  into a separate, encrypted, deletable body store; the chain carries only an
  opaque `body_ref`. `Record.body_ref`/`record_ref` are appended at the END of
  the struct (omitempty pointers), so mixed-version chains verify byte-exactly.
- Team governance policy has two sources — the config file and a `teams` DB
  record — with the DB record winning when both name the same team (D3,
  ADR-016); `internal/governance.Governor` resolves this via a per-request
  keystore lookup (`SetTeamLookup`), not a cache, so a console edit enforces
  on the very next request with no restart.
- Per-team region lock (D7, ADR-020): `TeamRecord.AllowedRegions` restricts a
  team to providers labeled with one of these regions; an UNLABELED provider
  is always dropped for a restricted team (fail-closed). A config-declared
  team with no DB record still gets its config `allowed_regions` enforced (the
  one case `TeamRecord` is synthesized from config rather than read from a
  row) — but a DB record, once it exists, wins wholesale over that config
  policy, same ADR-016 precedence as every other team field.

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
| 키 스토어 | `internal/keystore/sqlite.go` | SHA-256 해시 가상 키, Postgres 이식 스키마; `teams` 테이블도 소유(D3, ADR-016): `name`(PK), `allowed_models`, `rpm`, `tpm`, `tokens_per_day`, `quota_on_exceeded`, `budget_usd_micros`, `budget_on_exceeded`, `guardrail_id`, `guardrail_version`(D6, ADR-019 — 팀별 Bedrock Guardrail 오버라이드), `allowed_regions`(D7, ADR-020 — 팀별 region 제한), `created_at`, `updated_at`. `guardrail_id`/`guardrail_version`/`allowed_regions`은 `teams`의 ALTER-TABLE 마이그레이션들(D3 때는 신규 테이블이라 불필요했으나 D6부터 필요해짐); `ensureSchema`는 `keys`/`teams`가 `existingColumns`/`applyMigrations` 헬퍼를 공유하도록 정리됨 |
| Store 인터페이스 | `internal/keystore/keystore.go` | `Store`, `Principal`, `Allows()` (RBAC); `TeamStore`(`UpsertTeam`/`GetTeam`/`ListTeams`/`DeleteTeam`, D3)는 별도 인터페이스로 분리되어 `internal/server` 테스트의 기존 `Store` fake들에 영향 없음 |
| Provider 스토어 | `internal/providerstore/sqlite.go` | 옵트인 DB 토폴로지 (ADR-008): `providers`(ref만·시크릿 컬럼 없음; `guardrail_id`/`guardrail_version` TEXT 컬럼, D6/ADR-019 — DB 등록 Bedrock provider 자체의 기본 Guardrail, `auth_header`를 미러링한 ALTER-TABLE 마이그레이션), `model_targets`(순서 라우트), `model_aliases`(`model`, `alias` PK — ADR-021 후속: 모델의 alias→canonical 이름, 멀티 타겟 폴백 체인에서도 중복되지 않도록 모델(그룹) 단위로 분리; 신규 테이블이라 `CREATE TABLE IF NOT EXISTS`만으로 충분, ALTER-TABLE 불필요), `meta`(durable `seeded` 마커); Postgres 이식 TEXT 전용 DDL |
| 감사 writer | `internal/audit/writer.go` | 단일 writer 해시 체인, WAL 절단 |
| 감사 WAL | `internal/audit/wal.go` | `buffer_then_block` 내구성용 디스크 버퍼 |
| 감사 verify | `internal/audit/verify.go` | 인스턴스별 분절 체인 검증 |
| Limiter 스토어 | `internal/limiter/limiter.go` | 인메모리 토큰 버킷(TPM/RPM), 2단계 |
| Budget 스토어 | `internal/budget/budget.go` | 인메모리 microUSD budget, 2단계 |
| Body 스토어 | `internal/bodystore/` | 옵트인 본문 저장소(D4, ADR-018), 감사 체인 바깥: `bodies` 테이블(`ref` PK, `record_id`, `team`, `created_ts`, `expires_ts`, `size`, `wrapped_key_*`, `req_*`, `resp_*` — BLOB/BYTEA 암호문; `resp_*` nullable = 스트리밍 요청만). 엔벨로프 AEAD(레코드별 데이터키를 config-ref 마스터키로 wrap). 두 백엔드(`sqlite.go`/`postgres.go`), TTL+사이즈캡 `Purge`, 행별 하드 삭제(GDPR 소거). 키 로테이션: `inferplane bodies rewrap-key`(ADR-018 deferred item)가 `Store.ListWrappedKeys`/`UpdateWrappedKey`(CAS)로 `wrapped_key_*`만 재래핑 — `req_*`/`resp_*`는 읽거나 쓰지 않음 |
| 분석 인덱스 | `internal/analytics/` | 파생 사용량 read-model; `events` 테이블에 `ts`+`body_ref` 컬럼 추가(D4, ADR-018) — ALTER-if-missing(SQLite)/`ADD COLUMN IF NOT EXISTS`(Postgres); `GET /admin/logs` 백엔드 |
| ULID | `pkg/ulid/ulid.go` | 단조 증가 레코드 ID(Crockford base32) |

### 3. 주요 결정
- SQLite(`modernc.org/sqlite`, cgo 없음) 기본 → 정적 바이너리, 5분 기동.
- 인스턴스별 감사 해시 체인으로 재시작이 변조로 읽히지 않고 깔끔히 분절.
- 2단계 스토어(검사 후 차감)로 거부된 요청은 팀에 과금하지 않음.
- 프롬프트/응답 본문은 감사 체인에 절대 들어가지 않음(ADR-003 content-free 불변
  유지): 옵트인 `audit.log_bodies`(D4, ADR-018)가 별도 암호화·삭제 가능 body
  스토어에 캡처하고, 체인은 opaque `body_ref`만 보유. `Record.body_ref`/
  `record_ref`는 구조체 끝에 append(omitempty 포인터)되어 혼합 버전 체인도
  byte-exact 검증됨.
- 팀 거버넌스 정책은 config 파일과 `teams` DB 레코드 두 소스를 가지며, 같은 팀
  이름이 양쪽에 있으면 DB 레코드가 승리합니다(D3, ADR-016).
  `internal/governance.Governor`는 캐시가 아니라 요청당 keystore 조회
  (`SetTeamLookup`)로 이를 해결하므로, 콘솔에서의 수정이 재시작 없이 바로 다음
  요청부터 적용됩니다.
- 팀별 region 제한(D7, ADR-020): `TeamRecord.AllowedRegions`는 팀을 지정된
  region으로 라벨링된 provider로만 제한합니다; region 라벨이 없는 provider는
  제한된 팀에서 항상 제외됩니다(fail-closed). DB 레코드가 없는 config 선언
  팀도 config의 `allowed_regions`가 적용됩니다(`TeamRecord`가 config로부터
  합성되는 유일한 경우) — 다만 DB 레코드가 생성되는 순간 그 config 정책을
  전체적으로 대체합니다(다른 모든 팀 필드와 동일한 ADR-016 우선순위).

### 4. 코드 포인터
- `internal/keystore/sqlite.go` — 스키마 + SHA-256 조회
- `internal/audit/writer.go` — 단일 writer 고루틴, pending 기반 WAL 절단
- `internal/audit/verify.go` — `audit verify` 체인 검사

### 5. 상호 참조
- 관련 모듈: `internal/governance`, `internal/server`(auth)
- 관련 ADR: docs/decisions/ (SQLite-vs-Postgres 결정 — 기록 예정)
- 관련 런북: docs/runbooks/ (감사 검증, 백업)
