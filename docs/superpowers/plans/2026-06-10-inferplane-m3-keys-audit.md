# inferplane M3 — Virtual Key + 감사로그 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** 게이트웨이가 발급한 virtual key로 클라이언트를 인증하고(팀·모델 allow-list RBAC), 모든 요청을 2단계 해시체인 감사로그(변조감지, WAL 내구성)로 기록하며, `audit verify`로 무결성을 검증할 수 있게 한다 (스펙: `docs/specs/2026-06-10-inferplane-gateway-design.md` r4, §5.1·§5.4·§5.5).

**Architecture:** key store는 `Store` 인터페이스 뒤의 SQLite(`modernc.org/sqlite`, pure-Go, cgo 불필요 — co-agent 만장일치 결정; SQL 스키마는 Postgres 호환 형태로 잡아 v0.2 HA 전환을 단순 교체로). 인증 미들웨어 `KeyAuth`가 M2의 `DevKeyAuth`를 교체 — virtual key를 해석해 `Principal`을 요청 컨텍스트에 주입하고, ingress가 모델 allow-list를 집행한다. 감사로그는 **단일 writer 고루틴**이 큐에서 레코드를 받아 `prev_hash`를 계산(인스턴스별 해시 체인)하고 disk-backed WAL에 fsync한 뒤 sink로 flush한다 — 동시 요청의 started/completed 인터리빙에서 `prev_hash` 경합을 막는 유일 구조(r4 구현 노트).

**Tech Stack:** Go 1.23+ 표준 라이브러리 + **첫 외부 의존성** `modernc.org/sqlite`(pure-Go). `crypto/sha256`, `crypto/rand`, `crypto/subtle`. ULID는 자체 구현(`pkg/ulid`). 테스트는 `t.TempDir()` 위의 실제 SQLite + 인메모리 sink. M2 위에 구축.

---

## M3 결정 기록 (승인됨)

- **key store = `Store` 인터페이스 + 기본 SQLite** (`modernc.org/sqlite`, pure-Go). Postgres는 HA용 v0.2. co-agent 3/3 만장일치 — "5분 단일-바이너리 데모(v0.1 게이트) vs 외부 DB 강제"에서 활성화 단순성 우선, 저-write·단일 레플리카 워크로드라 SQLite 적합, 인터페이스 뒤라 교체 자유. **SQL 스키마는 표준 타입만 써 Postgres 호환**으로 (Gemini/Kimi 통찰: 전환을 데이터 마이그레이션이 아닌 config 교체로).
- 키 해시 = **SHA-256** (256-bit 고엔트로피 랜덤 키, bcrypt 불필요 — 빠른 인덱스 조회).
- ULID = **자체 구현** (`pkg/ulid`, crypto/rand + time, Crockford base32, 단조 증가).
- `cost` = **M3에서 nil** (M5 BudgetStore가 채움), `trace_id` = **예약**(v0.2 OTel), `prev_hash` = **M3에서 실제 채움**.
- `DevKeyAuth`(M2 임시) → **`KeyAuth`로 교체** (제거).

## 마일스톤 로드맵 (전체 6개 중 3번)

| M | 범위 | 게이트 |
|---|---|---|
| M1 ✅ | canonical 스키마 + 골든 테스트 | (완료) |
| M2 ✅ | Anthropic ingress ↔ provider 직통 | (완료) |
| **M3 (이 계획)** | virtual key + 감사로그 | 키 발급→Claude Code 인증→전 요청 audit verify 통과 + allow-list 밖 403 |
| M4 | bedrock provider | Claude Code→Bedrock 실연동 + thinking 순서 골든 |
| M5 | rate limit/quota/budget + OpenAI ingress | OpenCode 실연동 + quota block + 비용 필드 |
| M6 | failover/메트릭/Helm/TLS/quickstart | docker run→키 발급→Claude Code 5분 |

---

## 파일 구조

```
go.mod / go.sum                  # 첫 외부 의존성: modernc.org/sqlite
pkg/ulid/
  ulid.go                        # ULID 자체 구현 (시간순, 단조 증가)
  ulid_test.go
internal/keystore/
  keystore.go                    # Store 인터페이스 + Principal + 키 생성/해시 헬퍼
  sqlite.go                      # SQLite 구현 (Postgres 호환 스키마)
  sqlite_test.go
internal/audit/
  record.go                      # Record 스키마 (§5.4) + canonical JSON
  record_test.go
  sink.go                        # Sink 인터페이스 + stdout/file 구현
  sink_test.go
  wal.go                         # disk-backed WAL (append-only + replay)
  wal_test.go
  writer.go                      # 단일 writer 고루틴 + 해시 체인 + buffer_then_block
  writer_test.go
  verify.go                      # 체인 무결성 검증
  verify_test.go
  metrics.go                     # audit 카운터 (전역, /metrics 노출은 M6)
internal/server/
  auth.go (대체)                 # DevKeyAuth → KeyAuth (virtual key 해석)
  context.go                     # principal을 request context에 주입/추출
  adminauth.go                   # admin token 미들웨어 (해시·constant-time·복수)
  adminapi/
    keys.go                      # POST/DELETE/GET /admin/keys
    keys_test.go
  server.go (수정)               # AdminMux에 /admin/keys 추가, DataMux는 KeyAuth로
  server_test.go (수정)
  anthropicapi/
    messages.go (수정)           # principal allow-list 검사 + 2단계 audit 훅
    messages_test.go (수정)
    models.go (수정)             # principal allow-list로 필터
    models_test.go (수정)
internal/config/config.go (수정) # key_store, audit, admin_auth 섹션 추가
cmd/inferplane/
  main.go (수정)                 # serve에 keystore/audit 와이어, KeyAuth 사용
  keys.go                        # `inferplane keys create|revoke|list`
  audit.go                       # `inferplane audit verify`
examples/config.json (수정)      # key_store/audit/admin_auth 추가
```

---

### Task 1: pkg/ulid — 시간순 정렬 ID 자체 구현

**Files:**
- Create: `pkg/ulid/ulid.go`, `pkg/ulid/ulid_test.go`

ULID = 48-bit 밀리초 타임스탬프 + 80-bit 랜덤, Crockford base32 26자, 시간순 정렬. 같은 밀리초 내 단조 증가 보장(랜덤 부분을 마지막 값보다 크게).

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/ulid/ulid_test.go`:
```go
package ulid

import (
	"sort"
	"testing"
	"time"
)

func TestNewIsLexicographicallyTimeOrdered(t *testing.T) {
	t0 := time.UnixMilli(1_000_000_000_000)
	t1 := time.UnixMilli(1_000_000_001_000)
	a := NewAt(t0)
	b := NewAt(t1)
	if !(a < b) {
		t.Fatalf("later timestamp must sort after: %q !< %q", a, b)
	}
	if len(a) != 26 {
		t.Fatalf("ULID must be 26 chars, got %d (%q)", len(a), a)
	}
}

func TestMonotonicWithinSameMillisecond(t *testing.T) {
	ts := time.UnixMilli(1_700_000_000_000)
	g := NewGenerator()
	var ids []string
	for i := 0; i < 1000; i++ {
		ids = append(ids, g.NewAt(ts))
	}
	if !sort.StringsAreSorted(ids) {
		t.Fatal("ids generated in the same millisecond must be monotonically increasing")
	}
	// uniqueness
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Fatalf("duplicate ULID: %s", id)
		}
		seen[id] = true
	}
}

func TestOnlyCrockfordAlphabet(t *testing.T) {
	id := New()
	const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"
	for _, c := range id {
		found := false
		for _, a := range alphabet {
			if c == a {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("char %q not in Crockford base32", c)
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/ulid/ -v`
Expected: FAIL — `undefined: NewAt`

- [ ] **Step 3: 구현**

`pkg/ulid/ulid.go`:
```go
// Package ulid implements time-ordered, lexicographically-sortable 128-bit IDs
// (48-bit millisecond timestamp + 80-bit randomness, Crockford base32, 26
// chars). Used for audit record IDs so the on-disk chain is naturally
// time-ordered. Self-implemented to keep the dependency footprint minimal.
package ulid

import (
	"crypto/rand"
	"sync"
	"time"
)

const encoding = "0123456789ABCDEFGHJKMNPQRSTVWXYZ" // Crockford base32

// Generator produces monotonic ULIDs: within the same millisecond, the random
// component strictly increases so IDs remain sortable and unique.
type Generator struct {
	mu       sync.Mutex
	lastMS   int64
	lastRand [10]byte
}

func NewGenerator() *Generator { return &Generator{} }

var defaultGen = NewGenerator()

// New returns a ULID for the current time.
func New() string { return defaultGen.NewAt(time.Now()) }

// NewAt returns a ULID for t (default generator).
func NewAt(t time.Time) string { return defaultGen.NewAt(t) }

// NewAt returns a ULID for t, monotonic within a millisecond.
func (g *Generator) NewAt(t time.Time) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	ms := t.UnixMilli()
	var r [10]byte
	if ms == g.lastMS {
		// same ms: increment the previous random value to stay monotonic
		r = g.lastRand
		for i := len(r) - 1; i >= 0; i-- {
			r[i]++
			if r[i] != 0 {
				break
			}
		}
	} else {
		if _, err := rand.Read(r[:]); err != nil {
			panic("ulid: crypto/rand failed: " + err.Error())
		}
		g.lastMS = ms
	}
	g.lastRand = r
	return encode(ms, r)
}

// encode packs the 48-bit ms + 80-bit random into 26 Crockford base32 chars.
func encode(ms int64, r [10]byte) string {
	var b [16]byte // 128 bits
	b[0] = byte(ms >> 40)
	b[1] = byte(ms >> 32)
	b[2] = byte(ms >> 24)
	b[3] = byte(ms >> 16)
	b[4] = byte(ms >> 8)
	b[5] = byte(ms)
	copy(b[6:], r[:])

	// 128 bits → 26 base32 chars (130 bits, top 2 bits zero-padded).
	out := make([]byte, 26)
	out[0] = encoding[(b[0]&224)>>5]
	out[1] = encoding[b[0]&31]
	out[2] = encoding[(b[1]&248)>>3]
	out[3] = encoding[((b[1]&7)<<2)|((b[2]&192)>>6)]
	out[4] = encoding[(b[2]&62)>>1]
	out[5] = encoding[((b[2]&1)<<4)|((b[3]&240)>>4)]
	out[6] = encoding[((b[3]&15)<<1)|((b[4]&128)>>7)]
	out[7] = encoding[(b[4]&124)>>2]
	out[8] = encoding[((b[4]&3)<<3)|((b[5]&224)>>5)]
	out[9] = encoding[b[5]&31]
	out[10] = encoding[(b[6]&248)>>3]
	out[11] = encoding[((b[6]&7)<<2)|((b[7]&192)>>6)]
	out[12] = encoding[(b[7]&62)>>1]
	out[13] = encoding[((b[7]&1)<<4)|((b[8]&240)>>4)]
	out[14] = encoding[((b[8]&15)<<1)|((b[9]&128)>>7)]
	out[15] = encoding[(b[9]&124)>>2]
	out[16] = encoding[((b[9]&3)<<3)|((b[10]&224)>>5)]
	out[17] = encoding[b[10]&31]
	out[18] = encoding[(b[11]&248)>>3]
	out[19] = encoding[((b[11]&7)<<2)|((b[12]&192)>>6)]
	out[20] = encoding[(b[12]&62)>>1]
	out[21] = encoding[((b[12]&1)<<4)|((b[13]&240)>>4)]
	out[22] = encoding[((b[13]&15)<<1)|((b[14]&128)>>7)]
	out[23] = encoding[(b[14]&124)>>2]
	out[24] = encoding[((b[14]&3)<<3)|((b[15]&224)>>5)]
	out[25] = encoding[b[15]&31]
	return string(out)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/ulid/ -v`
Expected: PASS (3 tests). 주의: `TestMonotonicWithinSameMillisecond`는 1000개 증가가 10바이트 공간에서 오버플로 없이 정렬 유지됨을 확인.

- [ ] **Step 5: 커밋**

```bash
git add pkg/ulid/
git commit -s -m "feat(ulid): self-contained time-ordered monotonic ULID"
```

---

### Task 2: go.mod에 modernc.org/sqlite 추가

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 의존성 추가**

```bash
cd /home/atomoh/mayu
go get modernc.org/sqlite@latest
```
주의: 네트워크가 필요하다. 실패 시 BLOCKED 보고. 성공 시 `go.mod`에 `require modernc.org/sqlite vX.Y.Z` 추가됨.

- [ ] **Step 2: 컴파일 가능 확인 (드라이버 import smoke)**

임시 파일로 드라이버 등록 확인 — `internal/keystore/sqlite.go`를 Task 3에서 만들 때 `_ "modernc.org/sqlite"` blank import + `sql.Open("sqlite", path)`로 검증된다. 이 태스크는 의존성 확보만.

```bash
go build ./... && echo "build ok"
```
Expected: build ok (아직 sqlite 사용처 없으니 통과). `go.sum`에 체크섬 기록됨.

- [ ] **Step 3: 커밋**

```bash
git add go.mod go.sum
git commit -s -m "build: add modernc.org/sqlite (pure-Go, first external dep)

co-agent panel unanimous: embedded SQLite behind a Store interface
preserves the single-binary / 5-minute-demo gate; Postgres is the HA
path (v0.2). Pure-Go (no cgo) keeps the true-single-binary promise."
```

---

### Task 3: keystore — Store 인터페이스 + Principal + SQLite 구현

**Files:**
- Create: `internal/keystore/keystore.go`, `internal/keystore/sqlite.go`, `internal/keystore/sqlite_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/keystore/sqlite_test.go`:
```go
package keystore

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
)

func openTest(t *testing.T) *SQLiteStore {
	t.Helper()
	s, err := OpenSQLite(filepath.Join(t.TempDir(), "keys.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateResolveRoundTrip(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, p, err := s.Create(ctx, "platform-eng", []string{"claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(plaintext, "ik_") {
		t.Fatalf("key should be prefixed ik_: %q", plaintext)
	}
	if p.Team != "platform-eng" || len(p.AllowedModels) != 1 {
		t.Fatalf("principal: %+v", p)
	}
	got, err := s.Resolve(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if got.KeyID != p.KeyID || got.Team != "platform-eng" {
		t.Fatalf("resolve mismatch: %+v vs %+v", got, p)
	}
}

func TestResolveUnknownKeyErrors(t *testing.T) {
	s := openTest(t)
	if _, err := s.Resolve(context.Background(), "ik_does_not_exist"); err == nil {
		t.Fatal("expected error for unknown key")
	}
}

func TestRevokeInvalidatesKey(t *testing.T) {
	s := openTest(t)
	ctx := context.Background()
	plaintext, p, _ := s.Create(ctx, "t", []string{"*"})
	if err := s.Revoke(ctx, p.KeyID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Resolve(ctx, plaintext); err == nil {
		t.Fatal("revoked key must not resolve")
	}
}

func TestPlaintextNeverStored(t *testing.T) {
	// The raw key must not be recoverable from the store — only its SHA-256.
	s := openTest(t)
	ctx := context.Background()
	plaintext, _, _ := s.Create(ctx, "t", []string{"*"})
	list, err := s.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range list {
		if strings.Contains(p.KeyID, plaintext) {
			t.Fatal("plaintext leaked into key_id")
		}
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/keystore/ -v`
Expected: FAIL — `undefined: OpenSQLite`

- [ ] **Step 3: 인터페이스 + Principal (keystore.go)**

`internal/keystore/keystore.go`:
```go
// Package keystore stores virtual API keys (SHA-256 hashed) and the team /
// model-allow-list metadata behind them. Store is the swappable backend
// interface; M3 ships SQLite, Postgres is the HA path (v0.2). Only the key
// HASH is persisted — the plaintext is shown once at Create and never stored.
package keystore

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"strings"
)

// Principal is the resolved identity behind a virtual key (M3: service-account
// + team; user/OIDC is M5). It rides in the request context after KeyAuth.
type Principal struct {
	KeyID         string   // "ik_" + 12-char prefix of the key id; logged, never the secret
	Team          string
	AllowedModels []string // "*" allows all; else explicit allow-list (§5.1 policy)
}

// Allows reports whether this principal may use the given model.
func (p Principal) Allows(model string) bool {
	for _, m := range p.AllowedModels {
		if m == "*" || m == model {
			return true
		}
	}
	return false
}

type Store interface {
	Create(ctx context.Context, team string, allowedModels []string) (plaintext string, p Principal, err error)
	Resolve(ctx context.Context, plaintext string) (Principal, error)
	Revoke(ctx context.Context, keyID string) error
	List(ctx context.Context) ([]Principal, error)
	Close() error
}

var b32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// generateKey returns a high-entropy virtual key ("ik_" + 32 random bytes in
// lowercase base32) and its SHA-256 hash (hex). The key id is a 12-char prefix
// of the hash — safe to log, not reversible to the secret.
func generateKey() (plaintext, hashHex, keyID string, err error) {
	var raw [32]byte
	if _, err = rand.Read(raw[:]); err != nil {
		return "", "", "", err
	}
	plaintext = "ik_" + b32.EncodeToString(raw[:])
	sum := sha256.Sum256([]byte(plaintext))
	hashHex = hex.EncodeToString(sum[:])
	keyID = "ik_" + hashHex[:12]
	return plaintext, hashHex, keyID, nil
}

// hashKey returns the SHA-256 hex of a presented plaintext key for lookup.
func hashKey(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

func joinModels(models []string) string  { return strings.Join(models, ",") }
func splitModels(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(s, ",")
}
```

- [ ] **Step 4: SQLite 구현 (sqlite.go)**

`internal/keystore/sqlite.go`:
```go
package keystore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// SQLiteStore is the M3 default Store. Schema uses only portable SQL types so
// the same DDL maps cleanly onto Postgres in v0.2 (co-agent guidance).
type SQLiteStore struct{ db *sql.DB }

// schema — TEXT/INTEGER only, no SQLite-specific types, for Postgres portability.
const schema = `
CREATE TABLE IF NOT EXISTS keys (
    key_id        TEXT PRIMARY KEY,
    key_hash      TEXT NOT NULL UNIQUE,
    team          TEXT NOT NULL,
    allowed_models TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    revoked       INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_keys_hash ON keys(key_hash) WHERE revoked = 0;
`

func OpenSQLite(path string) (*SQLiteStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	// SQLite is single-writer; cap to 1 open conn to serialize writes cleanly.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("keystore: schema: %w", err)
	}
	return &SQLiteStore{db: db}, nil
}

func (s *SQLiteStore) Create(ctx context.Context, team string, allowedModels []string) (string, Principal, error) {
	plaintext, hashHex, keyID, err := generateKey()
	if err != nil {
		return "", Principal{}, err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO keys (key_id, key_hash, team, allowed_models, created_at) VALUES (?,?,?,?,?)`,
		keyID, hashHex, team, joinModels(allowedModels), nowRFC3339())
	if err != nil {
		return "", Principal{}, fmt.Errorf("keystore: insert: %w", err)
	}
	return plaintext, Principal{KeyID: keyID, Team: team, AllowedModels: allowedModels}, nil
}

var ErrKeyNotFound = errors.New("keystore: key not found")

func (s *SQLiteStore) Resolve(ctx context.Context, plaintext string) (Principal, error) {
	h := hashKey(plaintext)
	var p Principal
	var models string
	err := s.db.QueryRowContext(ctx,
		`SELECT key_id, team, allowed_models FROM keys WHERE key_hash = ? AND revoked = 0`, h).
		Scan(&p.KeyID, &p.Team, &models)
	if errors.Is(err, sql.ErrNoRows) {
		return Principal{}, ErrKeyNotFound
	}
	if err != nil {
		return Principal{}, err
	}
	p.AllowedModels = splitModels(models)
	return p, nil
}

func (s *SQLiteStore) Revoke(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE keys SET revoked = 1 WHERE key_id = ?`, keyID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrKeyNotFound
	}
	return nil
}

func (s *SQLiteStore) List(ctx context.Context) ([]Principal, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key_id, team, allowed_models FROM keys WHERE revoked = 0 ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Principal
	for rows.Next() {
		var p Principal
		var models string
		if err := rows.Scan(&p.KeyID, &p.Team, &models); err != nil {
			return nil, err
		}
		p.AllowedModels = splitModels(models)
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *SQLiteStore) Close() error { return s.db.Close() }
```

`internal/keystore/keystore.go`에 `nowRFC3339` 헬퍼 추가 (테스트 가능하게 하되 M3는 단순):
```go
import "time"

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339Nano) }
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/keystore/ -v`
Expected: PASS (4 tests). `go vet ./...`, `gofmt -l .` clean.

- [ ] **Step 6: 커밋**

```bash
git add internal/keystore/
git commit -s -m "feat(keystore): Store interface + SQLite (SHA-256 keys, Postgres-portable schema)"
```

---

### Task 4: audit Record 스키마 + canonical JSON

**Files:**
- Create: `internal/audit/record.go`, `internal/audit/record_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/audit/record_test.go`:
```go
package audit

import (
	"encoding/json"
	"testing"
)

func TestRecordCanonicalIsDeterministic(t *testing.T) {
	r := Record{
		SchemaVersion: 1, Event: "request_completed", ID: "01J", TS: "2026-06-10T00:00:00Z",
		Instance: "inst-1",
		Principal: PrincipalRef{KeyID: "ik_abc", Team: "platform-eng"},
		Request:   RequestRef{Ingress: "anthropic", ModelRequested: "claude-sonnet-4-6", Provider: "anthropic-direct", Stream: true},
		PrevHash:  "sha256:00",
	}
	a, err := r.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := r.Canonical()
	if string(a) != string(b) {
		t.Fatal("canonical form must be byte-stable across calls")
	}
	// must be valid JSON and contain the event
	var m map[string]any
	if err := json.Unmarshal(a, &m); err != nil {
		t.Fatalf("canonical not valid JSON: %v", err)
	}
	if m["event"] != "request_completed" {
		t.Fatalf("event missing: %v", m["event"])
	}
}

func TestStartedRecordOmitsCompletionFields(t *testing.T) {
	r := Record{SchemaVersion: 1, Event: "request_started", ID: "01J", TS: "t", Instance: "i",
		Principal: PrincipalRef{KeyID: "ik", Team: "t"}, Request: RequestRef{Ingress: "anthropic"}}
	b, _ := r.Canonical()
	s := string(b)
	if contains(s, `"usage"`) || contains(s, `"outcome"`) {
		t.Fatalf("started record must omit usage/outcome: %s", s)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/audit/ -run TestRecord -v` (also TestStarted)
Expected: FAIL — `undefined: Record`

- [ ] **Step 3: 구현**

`internal/audit/record.go`:
```go
// Package audit implements inferplane's tamper-evident audit log: two-phase
// records (request_started / request_completed), an instance-local SHA-256
// hash chain, a disk-backed WAL, and pluggable sinks (§5.4). cost is nil in M3
// (filled by the M5 BudgetStore); trace_id is reserved (v0.2 OTel); prev_hash
// is computed for real in M3.
package audit

import "encoding/json"

type PrincipalRef struct {
	KeyID string  `json:"key_id"`
	Team  string  `json:"team"`
	User  *string `json:"user,omitempty"` // OIDC, M5
}

type RequestRef struct {
	Ingress        string `json:"ingress"`         // "anthropic" | "openai"
	ModelRequested string `json:"model_requested"`
	ModelResolved  string `json:"model_resolved,omitempty"`
	Provider       string `json:"provider,omitempty"`
	ProviderAPI    string `json:"provider_api,omitempty"`
	Stream         bool   `json:"stream"`
}

type OutcomeRef struct {
	Status       int      `json:"status"`
	FallbackUsed bool     `json:"fallback_used"`
	FallbackChain []string `json:"fallback_chain,omitempty"`
	Partial      bool     `json:"partial"`
	Error        *string  `json:"error"`
}

type UsageRef struct {
	InputTokens               int64 `json:"input_tokens"`
	OutputTokens              int64 `json:"output_tokens"`
	CacheReadInputTokens      int64 `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens  int64 `json:"cache_creation_input_tokens,omitempty"`
	Estimated                 bool  `json:"estimated"`
}

type LatencyRef struct {
	TTFTMs  int64 `json:"ttft_ms,omitempty"`
	TotalMs int64 `json:"total_ms"`
}

// Record is one audit entry. Field order here defines the canonical JSON used
// for hashing (encoding/json marshals struct fields in declaration order).
type Record struct {
	SchemaVersion int           `json:"schema_version"`
	Event         string        `json:"event"` // request_started | request_completed
	ID            string        `json:"id"`    // ULID
	TS            string        `json:"ts"`
	Instance      string        `json:"instance"`
	Principal     PrincipalRef  `json:"principal"`
	Request       RequestRef    `json:"request"`
	Outcome       *OutcomeRef   `json:"outcome,omitempty"`
	Usage         *UsageRef     `json:"usage,omitempty"`
	Cost          *struct{}     `json:"cost,omitempty"` // M3: always nil (M5 fills)
	Latency       *LatencyRef   `json:"latency,omitempty"`
	TraceID       *string       `json:"trace_id"`   // reserved (v0.2 OTel)
	PrevHash      string        `json:"prev_hash"`
}

// Canonical returns the deterministic JSON used both for the on-disk record and
// as the input to the next record's prev_hash. encoding/json emits struct
// fields in declaration order, so this is byte-stable.
func (r Record) Canonical() ([]byte, error) { return json.Marshal(r) }
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/audit/ -run 'TestRecord|TestStarted' -v`
Expected: PASS. (started 레코드는 Usage/Outcome가 nil → omitempty로 생략됨.)

- [ ] **Step 5: 커밋**

```bash
git add internal/audit/record.go internal/audit/record_test.go
git commit -s -m "feat(audit): two-phase Record schema with canonical JSON for hashing"
```

---

### Task 5: audit Sink (stdout/file)

**Files:**
- Create: `internal/audit/sink.go`, `internal/audit/sink_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/audit/sink_test.go`:
```go
package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriterSinkAppendsLines(t *testing.T) {
	var buf bytes.Buffer
	s := NewWriterSink("test", &buf, false)
	if err := s.Write([]byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if err := s.Write([]byte(`{"a":2}`)); err != nil {
		t.Fatal(err)
	}
	if buf.String() != "{\"a\":1}\n{\"a\":2}\n" {
		t.Fatalf("sink should append newline-delimited JSONL: %q", buf.String())
	}
	if s.Required() {
		t.Fatal("test sink declared required=false")
	}
}

func TestFileSinkPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	s, err := NewFileSink(path, true)
	if err != nil {
		t.Fatal(err)
	}
	s.Write([]byte(`{"x":1}`))
	s.Close()
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), `{"x":1}`) {
		t.Fatalf("file sink did not persist: %q", data)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/audit/ -run 'TestWriterSink|TestFileSink' -v`
Expected: FAIL — `undefined: NewWriterSink`

- [ ] **Step 3: 구현**

`internal/audit/sink.go`:
```go
package audit

import (
	"io"
	"os"
)

// Sink consumes serialized audit records (one JSON object per Write, emitted
// as a newline-delimited line). Required sinks gate the failure policy (§5.4);
// non-required sinks (e.g. stdout) are best-effort.
type Sink interface {
	Write(rec []byte) error
	Name() string
	Required() bool
	Close() error
}

// WriterSink writes JSONL to any io.Writer (used for stdout and tests).
type WriterSink struct {
	name     string
	w        io.Writer
	required bool
}

func NewWriterSink(name string, w io.Writer, required bool) *WriterSink {
	return &WriterSink{name: name, w: w, required: required}
}

func (s *WriterSink) Write(rec []byte) error {
	if _, err := s.w.Write(rec); err != nil {
		return err
	}
	_, err := s.w.Write([]byte{'\n'})
	return err
}
func (s *WriterSink) Name() string   { return s.name }
func (s *WriterSink) Required() bool  { return s.required }
func (s *WriterSink) Close() error    { return nil }

// NewStdoutSink is the default best-effort sink (§5.4: stdout required:false).
func NewStdoutSink() *WriterSink { return NewWriterSink("stdout", os.Stdout, false) }

// FileSink appends JSONL to a file (required by default).
type FileSink struct {
	name     string
	f        *os.File
	required bool
}

func NewFileSink(path string, required bool) (*FileSink, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &FileSink{name: "file", f: f, required: required}, nil
}

func (s *FileSink) Write(rec []byte) error {
	if _, err := s.f.Write(rec); err != nil {
		return err
	}
	if _, err := s.f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return s.f.Sync() // durability for the required audit sink
}
func (s *FileSink) Name() string  { return s.name }
func (s *FileSink) Required() bool { return s.required }
func (s *FileSink) Close() error   { return s.f.Close() }
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/audit/ -run 'TestWriterSink|TestFileSink' -v` → PASS.
```bash
git add internal/audit/sink.go internal/audit/sink_test.go
git commit -s -m "feat(audit): stdout/file sinks (JSONL, required flag)"
```

---

### Task 6: audit WAL (disk-backed buffer + replay)

**Files:**
- Create: `internal/audit/wal.go`, `internal/audit/wal_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/audit/wal_test.go`:
```go
package audit

import (
	"path/filepath"
	"testing"
)

func TestWALAppendAndReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.wal")
	w, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	w.Append([]byte(`{"a":1}`))
	w.Append([]byte(`{"a":2}`))
	w.Close()

	// reopen and replay
	w2, err := OpenWAL(path)
	if err != nil {
		t.Fatal(err)
	}
	defer w2.Close()
	var got []string
	if err := w2.Replay(func(rec []byte) error { got = append(got, string(rec)); return nil }); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != `{"a":1}` || got[1] != `{"a":2}` {
		t.Fatalf("replay mismatch: %v", got)
	}
}

func TestWALTruncateAfterFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.wal")
	w, _ := OpenWAL(path)
	defer w.Close()
	w.Append([]byte(`{"a":1}`))
	if err := w.Truncate(); err != nil {
		t.Fatal(err)
	}
	var n int
	w.Replay(func([]byte) error { n++; return nil })
	if n != 0 {
		t.Fatalf("truncate should empty the WAL, got %d records", n)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/audit/ -run TestWAL -v`
Expected: FAIL — `undefined: OpenWAL`

- [ ] **Step 3: 구현**

`internal/audit/wal.go`:
```go
package audit

import (
	"bufio"
	"os"
	"sync"
)

// WAL is a disk-backed, append-only buffer for audit records that a required
// sink failed to accept. Records survive a crash/restart and are replayed on
// reopen, so buffer_then_block never loses a record to an in-memory-only
// buffer (§5.4). Newline-delimited, same framing as the sinks.
type WAL struct {
	mu   sync.Mutex
	f    *os.File
	path string
}

func OpenWAL(path string) (*WAL, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	return &WAL{f: f, path: path}, nil
}

func (w *WAL) Append(rec []byte) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Write(rec); err != nil {
		return err
	}
	if _, err := w.f.Write([]byte{'\n'}); err != nil {
		return err
	}
	return w.f.Sync()
}

// Replay invokes fn for each buffered record in order.
func (w *WAL) Replay(fn func(rec []byte) error) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.f.Seek(0, 0); err != nil {
		return err
	}
	sc := bufio.NewScanner(w.f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		cp := append([]byte(nil), line...)
		if err := fn(cp); err != nil {
			return err
		}
	}
	return sc.Err()
}

// Truncate empties the WAL after its records have been durably flushed to sinks.
func (w *WAL) Truncate() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if err := w.f.Truncate(0); err != nil {
		return err
	}
	_, err := w.f.Seek(0, 0)
	return err
}

func (w *WAL) Close() error { return w.f.Close() }
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/audit/ -run TestWAL -v` → PASS.
```bash
git add internal/audit/wal.go internal/audit/wal_test.go
git commit -s -m "feat(audit): disk-backed WAL with crash-replay (durable buffer)"
```

---

### Task 7: audit Writer — 단일 writer 고루틴 + 해시 체인 + buffer_then_block

**Files:**
- Create: `internal/audit/writer.go`, `internal/audit/metrics.go`, `internal/audit/writer_test.go`

- [ ] **Step 1: metrics.go (전역 카운터)**

`internal/audit/metrics.go`:
```go
package audit

import "sync/atomic"

// M3 keeps audit counters as atomics; the Prometheus /metrics endpoint that
// exposes them is M6 (§6.2). These let M3 tests assert failure/buffer state.
var (
	writeFailuresTotal atomic.Int64 // inferplane_audit_write_failures_total
	bufferedRecords    atomic.Int64 // backs inferplane_audit_buffer_utilization_ratio
)

func WriteFailuresTotal() int64 { return writeFailuresTotal.Load() }
func BufferedRecords() int64    { return bufferedRecords.Load() }
```

- [ ] **Step 2: 실패하는 테스트 작성**

`internal/audit/writer_test.go`:
```go
package audit

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
)

func TestWriterChainsPrevHash(t *testing.T) {
	var buf bytes.Buffer
	wal := filepath.Join(t.TempDir(), "a.wal")
	w, err := NewWriter("inst-1", wal, []Sink{NewWriterSink("buf", &buf, true)})
	if err != nil {
		t.Fatal(err)
	}
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A", Instance: "inst-1"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B", Instance: "inst-1"})
	w.Close() // flushes the queue

	lines := splitLines(buf.String())
	if len(lines) != 2 {
		t.Fatalf("expected 2 records, got %d", len(lines))
	}
	var first, second Record
	json.Unmarshal([]byte(lines[0]), &first)
	json.Unmarshal([]byte(lines[1]), &second)
	// second.prev_hash must equal sha256 of the first record's canonical bytes
	sum := sha256.Sum256([]byte(lines[0]))
	want := "sha256:" + hex.EncodeToString(sum[:])
	if second.PrevHash != want {
		t.Fatalf("chain broken:\n got: %s\nwant: %s", second.PrevHash, want)
	}
	if first.PrevHash == "" {
		t.Fatal("first record should carry the genesis prev_hash, not empty")
	}
}

func TestWriterSerializesConcurrentAppends(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	sink := NewWriterSink("buf", &lockedWriter{w: &buf, mu: &mu}, true)
	w, _ := NewWriter("i", filepath.Join(t.TempDir(), "a.wal"), []Sink{sink})
	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func(n int) { defer wg.Done(); w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "x", Instance: "i"}) }(i)
	}
	wg.Wait()
	w.Close()
	// every record's prev_hash must equal the hash of the literally-preceding
	// line — proving a single writer serialized them with no race.
	lines := splitLines(buf.String())
	for i := 1; i < len(lines); i++ {
		sum := sha256.Sum256([]byte(lines[i-1]))
		want := "sha256:" + hex.EncodeToString(sum[:])
		var rec Record
		json.Unmarshal([]byte(lines[i]), &rec)
		if rec.PrevHash != want {
			t.Fatalf("record %d prev_hash not chained to previous line", i)
		}
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func splitLines(s string) []string {
	var out []string
	for _, l := range bytes.Split([]byte(s), []byte{'\n'}) {
		if len(l) > 0 {
			out = append(out, string(l))
		}
	}
	return out
}
```

- [ ] **Step 3: 실패 확인**

Run: `go test ./internal/audit/ -run TestWriter -v`
Expected: FAIL — `undefined: NewWriter`

- [ ] **Step 4: 구현**

`internal/audit/writer.go`:
```go
package audit

import (
	"crypto/sha256"
	"encoding/hex"
)

const genesisHash = "sha256:genesis"

// Writer is the SINGLE writer goroutine for the audit chain. Handlers enqueue
// records via Append (non-blocking); the goroutine assigns prev_hash, persists
// to the WAL, and flushes to sinks — strictly serialized so concurrent
// started/completed records can't race on prev_hash (r4 implementation note).
type Writer struct {
	instance string
	queue    chan Record
	done     chan struct{}
	wal      *WAL
	sinks    []Sink
	prevHash string
}

func NewWriter(instance, walPath string, sinks []Sink) (*Writer, error) {
	wal, err := OpenWAL(walPath)
	if err != nil {
		return nil, err
	}
	w := &Writer{
		instance: instance,
		queue:    make(chan Record, 1024),
		done:     make(chan struct{}),
		wal:      wal,
		sinks:    sinks,
		prevHash: genesisHash,
	}
	go w.loop()
	return w, nil
}

// Append enqueues a record. Non-blocking unless the queue is full (back-pressure).
func (w *Writer) Append(rec Record) { w.queue <- rec }

func (w *Writer) loop() {
	defer close(w.done)
	for rec := range w.queue {
		rec.Instance = w.instance
		rec.PrevHash = w.prevHash
		canon, err := rec.Canonical()
		if err != nil {
			writeFailuresTotal.Add(1)
			continue
		}
		// chain advances on the canonical bytes actually emitted
		sum := sha256.Sum256(canon)
		w.prevHash = "sha256:" + hex.EncodeToString(sum[:])

		// durability first: WAL, then sinks. A required-sink failure leaves the
		// record in the WAL for replay (buffer_then_block; §5.4).
		_ = w.wal.Append(canon)
		flushedAll := true
		for _, s := range w.sinks {
			if err := s.Write(canon); err != nil {
				if s.Required() {
					writeFailuresTotal.Add(1)
					bufferedRecords.Add(1)
					flushedAll = false
				}
			}
		}
		if flushedAll {
			_ = w.wal.Truncate() // all required sinks took it; clear the buffer
		}
	}
}

// Close drains the queue and closes the WAL.
func (w *Writer) Close() error {
	close(w.queue)
	<-w.done
	return w.wal.Close()
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/audit/ -run TestWriter -v`
Expected: PASS. `TestWriterSerializesConcurrentAppends`는 50개 동시 Append가 단일 writer로 직렬화되어 각 레코드의 `prev_hash`가 직전 라인 해시와 일치함을 확인 — race detector로도: `go test ./internal/audit/ -race -run TestWriter`.

- [ ] **Step 6: 커밋**

```bash
git add internal/audit/writer.go internal/audit/metrics.go internal/audit/writer_test.go
git commit -s -m "feat(audit): single-writer goroutine, hash chain, WAL-backed buffer_then_block"
```

---

### Task 8: audit verify (체인 무결성 검증)

**Files:**
- Create: `internal/audit/verify.go`, `internal/audit/verify_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`internal/audit/verify_test.go`:
```go
package audit

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func writeChain(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	w, _ := NewWriter("inst-1", filepath.Join(t.TempDir(), "a.wal"), []Sink{NewWriterSink("buf", &buf, true)})
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01A"})
	w.Append(Record{SchemaVersion: 1, Event: "request_completed", ID: "01B"})
	w.Append(Record{SchemaVersion: 1, Event: "request_started", ID: "01C"})
	w.Close()
	return &buf
}

func TestVerifyAcceptsIntactChain(t *testing.T) {
	buf := writeChain(t)
	res, err := Verify(strings.NewReader(buf.String()))
	if err != nil {
		t.Fatal(err)
	}
	if !res.OK || res.Records != 3 {
		t.Fatalf("intact chain rejected: %+v", res)
	}
}

func TestVerifyDetectsTampering(t *testing.T) {
	buf := writeChain(t)
	lines := strings.Split(strings.TrimRight(buf.String(), "\n"), "\n")
	// tamper: flip a byte in the first record's team-less body by replacing the ID
	lines[1] = strings.Replace(lines[1], `"01B"`, `"XXX"`, 1)
	tampered := strings.Join(lines, "\n") + "\n"
	res, _ := Verify(strings.NewReader(tampered))
	if res.OK {
		t.Fatal("tampering with a chained record must fail verification")
	}
	if res.BrokenAt != 2 {
		t.Fatalf("expected break detected at record 2 (the one after the tampered one), got %d", res.BrokenAt)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/audit/ -run TestVerify -v`
Expected: FAIL — `undefined: Verify`

- [ ] **Step 3: 구현**

`internal/audit/verify.go`:
```go
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
)

type VerifyResult struct {
	OK       bool
	Records  int
	BrokenAt int    // 1-based index of the first record whose prev_hash mismatched (0 if OK)
	Reason   string
}

// Verify reads a JSONL audit stream and checks the hash chain: each record's
// prev_hash must equal sha256 of the PRECEDING record's exact line bytes, and
// the first record must carry the genesis hash. A tampered record changes its
// bytes, so the NEXT record's prev_hash no longer matches — that's the break.
func Verify(r io.Reader) (VerifyResult, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var prevLine []byte
	var n int
	expectedPrev := genesisHash
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		if len(line) == 0 {
			continue
		}
		n++
		var rec Record
		if err := json.Unmarshal(line, &rec); err != nil {
			return VerifyResult{OK: false, Records: n, BrokenAt: n, Reason: "unparseable record"}, nil
		}
		if rec.PrevHash != expectedPrev {
			return VerifyResult{OK: false, Records: n, BrokenAt: n,
				Reason: "prev_hash mismatch — chain broken or record tampered"}, nil
		}
		sum := sha256.Sum256(line)
		expectedPrev = "sha256:" + hex.EncodeToString(sum[:])
		prevLine = line
	}
	_ = prevLine
	if err := sc.Err(); err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{OK: true, Records: n}, nil
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/audit/ -run TestVerify -v` → PASS.
주의: 변조 테스트는 record 1(`01B`)을 변조하면 record 2(`01C`)의 prev_hash가 record1의 새 바이트와 불일치 → BrokenAt=2. (record1 자신의 prev_hash는 record0 해시라 여전히 일치하므로, 변조는 다음 레코드에서 검출된다 — 해시 체인의 본질.)
```bash
git add internal/audit/verify.go internal/audit/verify_test.go
git commit -s -m "feat(audit): hash-chain verification (tamper detection)"
```

---

### Task 9: principal 컨텍스트 + KeyAuth (DevKeyAuth 교체)

**Files:**
- Create: `internal/server/context.go`
- Replace: `internal/server/auth.go` (DevKeyAuth → KeyAuth), update `internal/server/auth_test.go`

- [ ] **Step 1: context.go (principal 주입/추출)**

`internal/server/context.go`:
```go
package server

import (
	"context"

	"github.com/inferplane/inferplane/internal/keystore"
)

type ctxKey int

const principalKey ctxKey = 0

// WithPrincipal returns a context carrying the authenticated principal.
func WithPrincipal(ctx context.Context, p keystore.Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

// PrincipalFrom extracts the principal injected by KeyAuth.
func PrincipalFrom(ctx context.Context) (keystore.Principal, bool) {
	p, ok := ctx.Value(principalKey).(keystore.Principal)
	return p, ok
}
```

- [ ] **Step 2: 실패하는 테스트 — replace auth_test.go**

`internal/server/auth_test.go` (전체 교체):
```go
package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

// stubStore is a minimal keystore.Store for auth tests.
type stubStore struct {
	key string
	p   keystore.Principal
}

func (s stubStore) Create(context.Context, string, []string) (string, keystore.Principal, error) {
	return "", keystore.Principal{}, nil
}
func (s stubStore) Resolve(_ context.Context, plaintext string) (keystore.Principal, error) {
	if plaintext == s.key {
		return s.p, nil
	}
	return keystore.Principal{}, keystore.ErrKeyNotFound
}
func (s stubStore) Revoke(context.Context, string) error          { return nil }
func (s stubStore) List(context.Context) ([]keystore.Principal, error) { return nil, nil }
func (s stubStore) Close() error                                   { return nil }

func TestKeyAuthResolvesPrincipal(t *testing.T) {
	store := stubStore{key: "ik_good", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	var gotTeam string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := PrincipalFrom(r.Context())
		if !ok {
			t.Fatal("principal not injected")
		}
		gotTeam = p.Team
		w.WriteHeader(200)
	})
	h := KeyAuth(store, next)

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"valid x-api-key", "x-api-key", "ik_good", 200},
		{"valid bearer", "Authorization", "Bearer ik_good", 200},
		{"wrong key", "x-api-key", "ik_bad", 401},
		{"missing", "", "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			if c.header != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d want %d", rec.Code, c.want)
			}
		})
	}
	if gotTeam != "platform-eng" {
		t.Fatalf("principal team not propagated: %q", gotTeam)
	}
}
```

- [ ] **Step 3: 실패 확인**

Run: `go test ./internal/server/ -run TestKeyAuth -v`
Expected: FAIL — `undefined: KeyAuth`

- [ ] **Step 4: auth.go 교체 (DevKeyAuth 제거, KeyAuth 추가)**

`internal/server/auth.go` (전체 교체):
```go
package server

import (
	"net/http"
	"strings"

	"github.com/inferplane/inferplane/internal/keystore"
)

// KeyAuth resolves the client's virtual API key (x-api-key or Authorization:
// Bearer) to a Principal via the key store and injects it into the request
// context. Replaces M2's DevKeyAuth. The upstream provider key is never the
// client's (§5.2). Resolution failure → 401 with an Anthropic-shaped error;
// the store's hash lookup is itself constant-time-ish (SHA-256 + indexed DB
// lookup), and we don't distinguish unknown-key from other failures to the
// client.
func KeyAuth(store keystore.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("x-api-key")
		if key == "" {
			key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if key == "" {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "missing API key")
			return
		}
		p, err := store.Resolve(r.Context(), key)
		if err != nil {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, WithPrincipal(r.Context(), p))
	})
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/server/ -v`
Expected: PASS (TestKeyAuth). 주의: M2의 `TestDevKeyAuth*`는 교체되어 사라짐 — `auth_test.go` 전체를 위 내용으로 대체했으므로 DevKeyAuth 참조가 없어야 한다. `writeAnthropicError`(errors.go)는 그대로 사용.

- [ ] **Step 6: 커밋**

```bash
git add internal/server/auth.go internal/server/auth_test.go internal/server/context.go
git commit -s -m "feat(server): KeyAuth replaces DevKeyAuth — resolve virtual key to principal"
```

---

### Task 10: ingress allow-list 집행 + 2단계 audit 훅

**Files:**
- Modify: `internal/server/anthropicapi/messages.go`, `internal/server/anthropicapi/models.go` + 테스트

- [ ] **Step 1: 실패하는 테스트 — messages allow-list + audit**

`internal/server/anthropicapi/messages_test.go`에 추가 (기존 testRouter 유지):
```go
import (
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/server"
)

func TestMessagesEnforcesAllowList(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	// principal allows only qwen-coder, requests claude-sonnet-4-6 → 403
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := server.WithPrincipal(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"qwen-coder"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 403 {
		t.Fatalf("allow-list violation must be 403, got %d", rec.Code)
	}
}

func TestMessagesAllowsListedModel(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := server.WithPrincipal(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	if rec.Code != 200 {
		t.Fatalf("listed model should pass, got %d: %s", rec.Code, rec.Body.String())
	}
}
```

> 참고: MessagesHandler가 audit Writer와 principal 추출 기능을 갖도록 시그니처가 바뀐다. `NewMessagesHandler(r)`는 유지하되, 핸들러가 `server.PrincipalFrom`을 호출하고 (principal 없으면 — 즉 KeyAuth 미적용 테스트 경로 — allow-list 검사를 건너뛰지 않고 401? 아니다, 정상 경로에선 KeyAuth가 항상 principal을 주입하므로 핸들러는 principal 존재를 가정. 단, 직접 테스트를 위해 principal 없으면 403 "no principal").

import cycle 주의: `anthropicapi`가 `server`를 import하면 `server`가 `anthropicapi`를 import(server.go의 DataMux)하므로 **순환**이 된다. 해결: `PrincipalFrom`/`WithPrincipal`를 `server`가 아니라 별도 패키지 `internal/principal`에 둔다.

**계획 수정 (import cycle 회피):** Task 9의 `context.go`를 `internal/server`가 아니라 신규 패키지 `internal/principal/principal.go`로 만든다. `server.KeyAuth`와 `anthropicapi.MessagesHandler` 둘 다 `internal/principal`을 import (단방향). Task 9의 context.go 내용을 `package principal`로 옮기고, auth.go는 `principal.WithPrincipal` 사용, 위 테스트도 `principal.WithPrincipal`/`principal.Principal`… 아니다 Principal은 keystore에 있다. `principal.With(ctx, keystore.Principal)` / `principal.From(ctx)`.

- [ ] **Step 2: principal 패키지 생성 (Task 9 context.go 재배치)**

`internal/principal/principal.go`:
```go
// Package principal carries the authenticated Principal across the request
// context. Separate from internal/server to avoid an import cycle
// (server imports anthropicapi; anthropicapi needs the principal accessor).
package principal

import (
	"context"

	"github.com/inferplane/inferplane/internal/keystore"
)

type ctxKey int

const key ctxKey = 0

func With(ctx context.Context, p keystore.Principal) context.Context {
	return context.WithValue(ctx, key, p)
}

func From(ctx context.Context) (keystore.Principal, bool) {
	p, ok := ctx.Value(key).(keystore.Principal)
	return p, ok
}
```
Delete `internal/server/context.go`; update `internal/server/auth.go` to use `principal.With`; update `auth_test.go` to use `principal.From`. Re-run `go test ./internal/server/` → PASS.

- [ ] **Step 3: messages.go 수정 — allow-list + audit 훅**

`MessagesHandler`에 audit Writer를 주입한다. 시그니처 변경:
```go
type MessagesHandler struct {
	r   *router.Router
	aud *audit.Writer // may be nil in unit tests that don't assert audit
}

func NewMessagesHandler(r *router.Router) *MessagesHandler { return &MessagesHandler{r: r} }
func NewMessagesHandlerWithAudit(r *router.Router, aud *audit.Writer) *MessagesHandler {
	return &MessagesHandler{r: r, aud: aud}
}
```
`ServeHTTP`에서 parse 직후, Resolve 전에:
```go
	p, hasPrincipal := principal.From(req.Context())
	if !hasPrincipal {
		writeErr(w, 401, "authentication_error", "no principal")
		return
	}
	if !p.Allows(parsed.Model) {
		h.auditStarted(req, p, parsed.Model, "", 403)
		writeErr(w, 403, "permission_error", "model not allowed for this key: "+parsed.Model)
		return
	}
```
Resolve 성공 후 `request_started` 기록, 응답 완료 후 `request_completed` 기록. audit 훅 헬퍼:
```go
func (h *MessagesHandler) auditStarted(req *http.Request, p principalRef, model, provider string, denyStatus int) {
	if h.aud == nil {
		return
	}
	rec := audit.Record{
		SchemaVersion: 1, Event: "request_started", ID: ulid.New(), TS: time.Now().UTC().Format(time.RFC3339Nano),
		Principal: audit.PrincipalRef{KeyID: p.KeyID, Team: p.Team},
		Request:   audit.RequestRef{Ingress: "anthropic", ModelRequested: model, Provider: provider},
	}
	if denyStatus != 0 {
		rec.Outcome = &audit.OutcomeRef{Status: denyStatus}
	}
	h.aud.Append(rec)
}
```
(principalRef = keystore.Principal; import keystore, audit, pkg/ulid, time, internal/principal.)
완료 단계: `serveComplete`/`serveStream`에서 usage 관찰 후 `request_completed` Append (usage 채움). 비스트리밍은 `resp.Parsed.Usage`, 스트리밍은 message_delta의 `ev.Chunk.Usage`를 누적.

> 이 태스크는 다파일·다개념이라 구현자가 import cycle·nil-audit·usage 추출을 신중히 다뤄야 한다. 핵심 단언: (1) allow-list 위반 403, (2) principal 없으면 401, (3) audit Writer 주입 시 started/completed 2건 기록.

- [ ] **Step 4: models.go 수정 — allow-list 필터**

`ModelsHandler.ServeHTTP`에서 `principal.From`로 principal을 꺼내, `AllModels()` 결과를 `p.Allows(name)`로 필터. principal 없으면 전체(테스트 호환) 또는 빈 목록 — 정상 경로는 KeyAuth가 주입하므로 필터. 테스트 `TestModelsListShape`는 principal 주입 버전으로 갱신하거나, principal 없을 때 전체 반환으로 기존 테스트 호환 유지(권장: 없으면 전체, 있으면 필터).

`models_test.go`에 추가:
```go
func TestModelsFiltersByAllowList(t *testing.T) {
	h := NewModelsHandler(testRouter()) // testRouter has claude-sonnet-4-6
	req := httptest.NewRequest("GET", "/v1/models", nil)
	ctx := principal.With(req.Context(), keystore.Principal{AllowedModels: []string{"other-only"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	var out struct{ Data []map[string]any `json:"data"` }
	json.Unmarshal(rec.Body.Bytes(), &out)
	if len(out.Data) != 0 {
		t.Fatalf("allow-list should filter out non-listed models: %+v", out.Data)
	}
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/server/... -v`, `go test ./... -race -run 'TestMessages|TestModels|TestWriter'`
Expected: PASS. import cycle 없음 (`go build ./...` 통과).

- [ ] **Step 6: 커밋**

```bash
git add internal/principal/ internal/server/anthropicapi/ internal/server/auth.go internal/server/auth_test.go
git rm internal/server/context.go 2>/dev/null || true
git commit -s -m "feat(ingress): enforce model allow-list (403) + two-phase audit hooks"
```

---

### Task 11: admin token 미들웨어 + /admin/keys API

**Files:**
- Create: `internal/server/adminauth.go`, `internal/server/adminapi/keys.go`, `internal/server/adminapi/keys_test.go`

- [ ] **Step 1: adminauth 실패 테스트**

`internal/server/adminauth_test.go`:
```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminTokenAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth([]string{"tok-a", "tok-b"}, next) // multiple tokens (rotation)
	cases := []struct {
		tok  string
		want int
	}{
		{"tok-a", 200}, {"tok-b", 200}, {"wrong", 401}, {"", 401},
	}
	for _, c := range cases {
		req := httptest.NewRequest("POST", "/admin/keys", nil)
		if c.tok != "" {
			req.Header.Set("Authorization", "Bearer "+c.tok)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != c.want {
			t.Fatalf("tok %q: got %d want %d", c.tok, rec.Code, c.want)
		}
	}
}

func TestAdminTokenAuthRejectsEmptyConfig(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	h := AdminTokenAuth(nil, next) // no tokens configured → all denied
	req := httptest.NewRequest("POST", "/admin/keys", nil)
	req.Header.Set("Authorization", "Bearer anything")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("empty token config must deny all: got %d", rec.Code)
	}
}
```

- [ ] **Step 2: 실패 확인 → 구현 adminauth.go**

`internal/server/adminauth.go`:
```go
package server

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
	"strings"
)

// AdminTokenAuth guards the admin plane. Tokens are compared by SHA-256 +
// constant-time (so length/content don't leak via timing), and multiple tokens
// are accepted for rotation (§5.5). An empty token set denies everything
// (defense-in-depth, like KeyAuth). Separate credential from data-plane keys.
func AdminTokenAuth(tokens []string, next http.Handler) http.Handler {
	hashes := make([][32]byte, len(tokens))
	for i, t := range tokens {
		hashes[i] = sha256.Sum256([]byte(t))
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if got == "" || len(hashes) == 0 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "admin token required")
			return
		}
		gh := sha256.Sum256([]byte(got))
		ok := false
		for _, h := range hashes {
			if subtle.ConstantTimeCompare(gh[:], h[:]) == 1 {
				ok = true
			}
		}
		if !ok {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid admin token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
```
Run `go test ./internal/server/ -run TestAdminToken -v` → PASS.

- [ ] **Step 3: /admin/keys 실패 테스트**

`internal/server/adminapi/keys_test.go`:
```go
package adminapi

import (
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
)

func newTestStore(t *testing.T) *keystore.SQLiteStore {
	s, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateKeyReturnsPlaintextOnce(t *testing.T) {
	h := NewKeysHandler(newTestStore(t))
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"team":"platform-eng","allowed_models":["claude-sonnet-4-6"]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Plaintext, "ik_") || out.KeyID == "" {
		t.Fatalf("expected plaintext+key_id: %+v", out)
	}
}

func TestListKeysOmitsSecrets(t *testing.T) {
	store := newTestStore(t)
	store.Create(nil, "t", []string{"*"}) // nil ctx ok for sqlite test
	h := NewKeysHandler(store)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "ik_") && strings.Contains(rec.Body.String(), "plaintext") {
		t.Fatalf("list must not expose plaintext: %s", rec.Body.String())
	}
}
```
주의: `store.Create(nil, ...)` — context nil은 database/sql에서 패닉할 수 있으니 `context.Background()` 사용으로 수정. 테스트에서 `context` import.

- [ ] **Step 4: 구현 keys.go**

`internal/server/adminapi/keys.go`:
```go
// Package adminapi implements the admin-plane key management endpoints,
// guarded by AdminTokenAuth (§5.5). Create returns the plaintext key ONCE.
package adminapi

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/inferplane/inferplane/internal/keystore"
)

type KeysHandler struct{ store keystore.Store }

func NewKeysHandler(store keystore.Store) *KeysHandler { return &KeysHandler{store: store} }

func (h *KeysHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.Method == http.MethodPost:
		h.create(w, r)
	case r.Method == http.MethodGet:
		h.list(w, r)
	case r.Method == http.MethodDelete:
		h.revoke(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *KeysHandler) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Team          string   `json:"team"`
		AllowedModels []string `json:"allowed_models"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Team == "" {
		http.Error(w, `{"error":"team required"}`, http.StatusBadRequest)
		return
	}
	plaintext, p, err := h.store.Create(r.Context(), body.Team, body.AllowedModels)
	if err != nil {
		http.Error(w, `{"error":"create failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"key_id": p.KeyID, "team": p.Team, "allowed_models": p.AllowedModels, "plaintext": plaintext})
}

func (h *KeysHandler) list(w http.ResponseWriter, r *http.Request) {
	ps, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	out := make([]map[string]any, 0, len(ps))
	for _, p := range ps {
		out = append(out, map[string]any{"key_id": p.KeyID, "team": p.Team, "allowed_models": p.AllowedModels})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

func (h *KeysHandler) revoke(w http.ResponseWriter, r *http.Request) {
	keyID := strings.TrimPrefix(r.URL.Path, "/admin/keys/")
	if keyID == "" || keyID == r.URL.Path {
		http.Error(w, `{"error":"key_id required in path"}`, http.StatusBadRequest)
		return
	}
	if err := h.store.Revoke(r.Context(), keyID); err != nil {
		http.Error(w, `{"error":"revoke failed"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
```

- [ ] **Step 5: 통과 + 커밋**

Run: `go test ./internal/server/... -v` → PASS (adminauth + keys; keys_test.go의 nil ctx를 context.Background()로 수정).
```bash
git add internal/server/adminauth.go internal/server/adminauth_test.go internal/server/adminapi/
git commit -s -m "feat(admin): admin-token auth (rotation) + /admin/keys CRUD"
```

---

### Task 12: config 확장 + server 와이어

**Files:**
- Modify: `internal/config/config.go`, `internal/server/server.go` + 테스트

- [ ] **Step 1: config 실패 테스트**

`internal/config/config_test.go`에 추가:
```go
func TestLoadKeyStoreAuditAdmin(t *testing.T) {
	t.Setenv("ADMIN_TOK", "secret-admin")
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{
	  "server": {"listen":":8080","admin_listen":":9090","admin_auth":{"token_refs":[{"env":"ADMIN_TOK"}]}},
	  "key_store": {"type":"sqlite","path":"/tmp/keys.db"},
	  "audit": {"failure_mode":"buffer_then_block","buffer":{"path":"/tmp/audit.wal"},"sinks":[{"type":"stdout"},{"type":"file","path":"/tmp/audit.jsonl"}]}
	}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyStore.Type != "sqlite" || cfg.KeyStore.Path != "/tmp/keys.db" {
		t.Fatalf("key_store: %+v", cfg.KeyStore)
	}
	if len(cfg.Server.AdminAuth.TokenRefs) != 1 || cfg.Server.AdminAuth.Tokens[0] != "secret-admin" {
		t.Fatalf("admin tokens not resolved: %+v", cfg.Server.AdminAuth)
	}
	if cfg.Audit.FailureMode != "buffer_then_block" || len(cfg.Audit.Sinks) != 2 {
		t.Fatalf("audit: %+v", cfg.Audit)
	}
}
```

- [ ] **Step 2: config.go 확장**

`internal/config/config.go`에 타입 추가 + Load에서 admin token ref 해석:
```go
type AdminAuth struct {
	TokenRefs []SecretRef `json:"token_refs,omitempty"`
	Tokens    []string    `json:"-"` // resolved at load
}

type KeyStoreConfig struct {
	Type string `json:"type"` // "sqlite" (M3); "postgres" (v0.2)
	Path string `json:"path"`
}

type AuditSink struct {
	Type string `json:"type"` // "stdout" | "file"
	Path string `json:"path,omitempty"`
}

type AuditBuffer struct {
	Path string `json:"path"`
}

type AuditConfig struct {
	FailureMode string      `json:"failure_mode"` // buffer_then_block (default)
	Buffer      AuditBuffer `json:"buffer"`
	Sinks       []AuditSink `json:"sinks"`
}
```
`ServerConfig`에 `AdminAuth AdminAuth json:"admin_auth"` 추가; `Config`에 `KeyStore KeyStoreConfig json:"key_store"`, `Audit AuditConfig json:"audit"` 추가. `Load`에서 providers secret 해석 루프 뒤에:
```go
	for _, ref := range cfg.Server.AdminAuth.TokenRefs {
		tok, err := resolveSecret(&ref)
		if err != nil {
			return nil, fmt.Errorf("config: admin token: %w", err)
		}
		cfg.Server.AdminAuth.Tokens = append(cfg.Server.AdminAuth.Tokens, tok)
	}
```

- [ ] **Step 3: server.go 와이어 (DataMux KeyAuth, AdminMux /admin/keys)**

`DataMux` 시그니처 변경: `DataMux(r *router.Router, store keystore.Store) http.Handler` — `DevKeyAuth(devKey, mux)` → `KeyAuth(store, mux)`. `AdminMux` 시그니처 변경: `AdminMux(store keystore.Store, adminTokens []string) http.Handler` — `/admin/keys`와 `/admin/keys/{key_id}`를 `AdminTokenAuth`로 감싸 추가, `/healthz`·`/readyz`는 무인증 유지.
```go
func DataMux(r *router.Router, store keystore.Store, aud *audit.Writer) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", anthropicapi.NewMessagesHandlerWithAudit(r, aud))
	mux.Handle("POST /v1/messages/count_tokens", anthropicapi.NewCountTokensHandler(r))
	mux.Handle("GET /v1/models", anthropicapi.NewModelsHandler(r))
	return KeyAuth(store, mux)
}

func AdminMux(store keystore.Store, adminTokens []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	keys := adminapi.NewKeysHandler(store)
	mux.Handle("/admin/keys", AdminTokenAuth(adminTokens, keys))
	mux.Handle("/admin/keys/", AdminTokenAuth(adminTokens, keys))
	return mux
}
```
`server_test.go` 갱신: `DataMux`/`AdminMux` 새 시그니처 + KeyAuth용 stubStore 사용. health 무인증 유지 확인, /admin/keys는 토큰 없으면 401.

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/... -v`, `go build ./...`
Expected: PASS. (cmd/inferplane는 Task 13에서 새 시그니처에 맞춰 갱신 — 이 시점에 build가 깨지면 main.go도 함께 고쳐야 하므로 Task 13과 묶일 수 있음; 구현자 판단으로 main.go 최소 수정해 build 유지.)
```bash
git add internal/config/ internal/server/server.go internal/server/server_test.go
git commit -s -m "feat(config): key_store/audit/admin_auth sections + server wiring (KeyAuth, /admin/keys)"
```

---

### Task 13: main.go 와이어 + keys/audit CLI

**Files:**
- Modify: `cmd/inferplane/main.go`
- Create: `cmd/inferplane/keys.go`, `cmd/inferplane/audit.go`
- Modify: `examples/config.json`

- [ ] **Step 1: main.go serve 와이어**

`run()`에서: config 로드 → `keystore.OpenSQLite(cfg.KeyStore.Path)` → audit sinks 구성(stdout/file) → `audit.NewWriter(instance, cfg.Audit.Buffer.Path, sinks)` → `server.DataMux(r, store, aud)` + `server.AdminMux(store, cfg.Server.AdminAuth.Tokens)`. `INFERPLANE_DEV_KEY` 의존 제거(이제 virtual key). instance = hostname 또는 env. 종료 시 `aud.Close()`, `store.Close()`.

- [ ] **Step 2: keys 서브커맨드 (cmd/inferplane/keys.go)**

`inferplane keys create --team <t> --models <csv> --store <path>` (로컬 부트스트랩: SQLite 직접 쓰기, 서버 기동 전 전용), `keys list --store <path>`, `keys revoke --id <key_id> --store <path>`. create는 plaintext를 stdout에 1회 출력. **로컬 모드도 audit 기록**(§5.5): create 시 audit 파일에 키 발급 레코드 1건 append(간단히 file sink로 직접 또는 stderr 경고). M3 최소: create 시 stderr에 "audit: key <id> created for team <t>" 기록 + audit 파일 있으면 append.

- [ ] **Step 3: audit verify 서브커맨드 (cmd/inferplane/audit.go)**

`inferplane audit verify --file <path>` → `audit.Verify(file)` → OK면 "chain OK (N records)" exit 0, 깨지면 "chain BROKEN at record M: <reason>" exit 1.

- [ ] **Step 4: main.go 서브커맨드 디스패치**

`os.Args[1]` switch: `serve` | `keys` | `audit`. keys/audit는 별도 파일 함수 호출.

- [ ] **Step 5: examples/config.json 갱신**

key_store/audit/admin_auth 섹션 추가:
```json
{
  "server": {
    "listen": ":8080", "admin_listen": ":9090",
    "admin_auth": { "token_refs": [ { "env": "INFERPLANE_ADMIN_TOKEN" } ] }
  },
  "key_store": { "type": "sqlite", "path": "/var/lib/inferplane/keys.db" },
  "audit": {
    "failure_mode": "buffer_then_block",
    "buffer": { "path": "/var/lib/inferplane/audit.wal" },
    "sinks": [ { "type": "stdout" }, { "type": "file", "path": "/var/lib/inferplane/audit.jsonl" } ]
  },
  "providers": {
    "anthropic-direct": { "type": "anthropic", "base_url": "https://api.anthropic.com", "api_key_ref": { "env": "ANTHROPIC_API_KEY" } }
  },
  "models": {
    "claude-sonnet-4-6": { "targets": [ { "provider": "anthropic-direct", "model": "claude-sonnet-4-6" } ] },
    "claude-opus-4-8": { "targets": [ { "provider": "anthropic-direct", "model": "claude-opus-4-8" } ] }
  }
}
```

- [ ] **Step 6: 빌드 + 전체 검증 + 바이너리 스모크**

```bash
go build ./... && go test ./... && go vet ./... && gofmt -l .
go build -o /tmp/ip-m3 ./cmd/inferplane
# keys create (로컬 부트스트랩) → audit verify 왕복 스모크
/tmp/ip-m3 keys create --team demo --models '*' --store /tmp/ip-keys.db   # prints ik_...
/tmp/ip-m3 keys list --store /tmp/ip-keys.db                              # shows the key_id
rm -f /tmp/ip-m3 /tmp/ip-keys.db
```
Expected: build/test/vet/fmt 클린, keys create가 `ik_...` 출력, list가 key_id 표시.

- [ ] **Step 7: 커밋**

```bash
git add cmd/inferplane/ examples/config.json
git commit -s -m "feat(cmd): wire keystore+audit into serve; keys/audit subcommands"
```

---

### Task 14: M3 게이트 — 실제 Claude Code 연동 (수동)

자동 테스트가 아닌 게이트 검증.

- [ ] **Step 1: 빌드 + 부트스트랩 키 발급**

```bash
go build -o bin/inferplane ./cmd/inferplane
export ANTHROPIC_API_KEY=sk-ant-...
export INFERPLANE_ADMIN_TOKEN=admin-secret
# 서버 기동 전 로컬 부트스트랩으로 첫 키 발급
./bin/inferplane keys create --team platform-eng --models '*' --store /var/lib/inferplane/keys.db
# → ik_xxxxx... (이 값을 클라이언트가 사용)
./bin/inferplane serve --config examples/config.json
```

- [ ] **Step 2: Claude Code 연결**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=ik_xxxxx...    # inferplane virtual key
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
claude
```

- [ ] **Step 3: 게이트 체크리스트 (모두 통과해야 M3 완료)**

- [ ] virtual key 인증: 발급한 `ik_` 키로 대화 동작; 잘못된 키는 401.
- [ ] allow-list: `--models '*'` 키는 모든 모델 허용; 제한된 키로 미허용 모델 요청 시 403.
- [ ] 2단계 감사로그: 각 요청이 `request_started` + `request_completed` 2건으로 audit.jsonl에 기록.
- [ ] **체인 검증**: `./bin/inferplane audit verify --file /var/lib/inferplane/audit.jsonl` → "chain OK (N records)".
- [ ] 변조 검출: audit.jsonl의 한 레코드를 손으로 수정 → `audit verify`가 BROKEN 보고.
- [ ] admin API: `curl -H "Authorization: Bearer admin-secret" -X POST localhost:9090/admin/keys -d '{"team":"x","allowed_models":["claude-sonnet-4-6"]}'` → key_id+plaintext; 토큰 없으면 401.
- [ ] 캐시 hit율 M2 대비 유지 (audit/auth 추가가 prefix를 건드리지 않음 — verbatim 전달 유지).

- [ ] **Step 4: 게이트 통과 기록** — 전부 통과 시 M3 완료.

---

## Self-Review 결과

- **스펙 커버리지**: §5.1 RBAC(virtual key principal + team×model allow-list) → Task 3/9/10. §5.4 감사로그(2단계, 해시 체인 단일 writer, WAL, sink 실패정책, verify) → Task 4~8. §5.5 admin token + 부트스트랩 → Task 11/13. §6.2 audit 카운터 → Task 7 metrics.go(노출은 M6). key store SQLite 결정 → Task 2/3. ULID 자체구현 → Task 1. ✓
- **플레이스홀더**: Task 10/13은 다파일·통합이라 일부 "구현자 판단" 여지가 있으나(import cycle 해소, usage 추출, CLI 플래그 파싱), 핵심 코드·테스트 단언·시그니처는 명시. Task 10의 import cycle 위험을 `internal/principal` 패키지 분리로 사전 해결책 제시. ✓
- **타입 일관성**: `keystore.Principal{KeyID,Team,AllowedModels}`·`Store`·`ErrKeyNotFound`(Task 3) → Task 9/10/11/12 사용처 일치. `audit.Record`/`Writer`/`Sink`/`WAL`/`Verify`(Task 4~8) → Task 10/13 사용처 일치. `principal.With/From`(Task 10 Step 2) → auth.go/messages.go/models.go 일치. `ulid.New`(Task 1) → Task 10/13. config 새 타입(Task 12) → Task 13. ✓
- **알려진 한계 (의도)**: M3 cost=nil(M5), OIDC 없음(principal=virtual key만), S3/webhook·외부앵커링 없음(M5+), /metrics 미노출(M6, 카운터만), 단일 레플리카 전제(SQLite, HA=Postgres v0.2). 로컬 부트스트랩 audit은 M3에서 최소(stderr+옵션 파일 append) — 완전한 audit writer 경유는 서버 런타임 경로.
