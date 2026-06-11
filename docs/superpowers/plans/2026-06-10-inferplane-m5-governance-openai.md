# inferplane M5 — 거버넌스 완성 + OpenAI ingress 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** rate limit·quota·budget 3분리 거버넌스를 ingress 파이프라인에 통합하고(정수 µUSD 비용 정산 + 감사로그 cost 채움), OpenAI ingress(`/v1/chat/completions`)와 openai_compatible provider(vLLM/Ollama/llm-d)를 추가하며, provider 우선순위 폴백 + circuit breaker를 구현한다 (스펙: §5.3·§3.2·§3.3·§4.5).

**Architecture:** 거버넌스는 `LimiterStore`(rate+quota)/`BudgetStore`(µUSD) 인터페이스 뒤(기본 인메모리; Redis는 v0.2). ingress 파이프라인이 principal·model 해석 후 **provider 호출 전 사전 체크**, 응답 후 **사후 차감**한다. 프로토콜 변환은 "프로토콜 일치 시 RawBody verbatim 전달(무손실·cache-safe), 불일치 시 canonical 경유 변환(best-effort §3.3)" 원칙 — `ProxyRequest`에 ingress 프로토콜 태그를 추가한다. 폴백은 router의 우선순위 체인 + 인스턴스-로컬 circuit breaker(pre-TTFT 한정).

**Tech Stack:** Go 1.25+, 표준 라이브러리만(Redis 미구현 — 인터페이스만). M4 위에 구축.

---

## M5 결정 기록 (승인된 설계 r4)

- **3분리** (§5.3): rate limit(인스턴스-로컬 토큰버킷, TPM/RPM 사전 차단) / quota(일·월 토큰, 2단계 낙관 Check+사후 Debit, `LimiterStore`, failure_mode 기본 fail_open) / budget(`BudgetStore`, µUSD). `on_exceeded: block|warn`, 동시 초과 block 우선.
- **비용 = 정수 µUSD**: 단가 키 `(provider, model)`, TTL별 cache write(5m=1.25×/1h=2×), cache_read 구분, 요청 단위 1회 round-half-even. `pricing.on_missing: allow`(기본)|`block`, allow 시 비용 0 + `pricing_missing` 마킹 + 메트릭. self-hosted chargeback 단가. 감사로그 cost 채움(M3 nil이던 것).
- **프로토콜 매칭 원칙**: provider 프로토콜 == ingress 프로토콜 → RawBody verbatim(무손실, M2/M4 캐시 불변식 유지). 불일치 → canonical 경유 best-effort 변환(§3.3 매트릭스). canonical은 Anthropic-superset.
- **failover** (§4.5): 명시 우선순위 폴백, 패시브 circuit breaker(연속 N 실패 open→지수백오프 half-open), **pre-TTFT 한정** 폴백, mid-stream 에러 이벤트 종료, `x-inferplane-fallback` 헤더+감사로그.
- **M4 carry-over**: Converse 샘플링 파라미터(temperature/top_p/stop_sequences) 패스스루.

## 마일스톤 로드맵 (6개 중 5번)

| M | 범위 | 게이트 |
|---|---|---|
| M1✅~M4✅ | (완료) | |
| **M5 (이 계획)** | governance + OpenAI ingress + failover | OpenCode 실연동(openai_compatible)+quota block+비용 필드 |
| M6 | 메트릭/Helm/TLS/quickstart | docker run→5분 |

---

## 파일 구조 (3 그룹)

```
# ── Group A: 거버넌스 ──
internal/pricing/
  pricing.go        # (provider,model) 단가 테이블 + µUSD 계산 (round-half-even, TTL별 cache, on_missing)
  pricing_test.go
  bundled.go        # 번들 기본 단가 (Anthropic/Bedrock Claude 등)
internal/limiter/
  limiter.go        # LimiterStore 인터페이스 + 인메모리 토큰버킷(rate) + 윈도우 카운터(quota)
  limiter_test.go
internal/budget/
  budget.go         # BudgetStore 인터페이스 + 인메모리 µUSD 누적
  budget_test.go
internal/config/config.go (수정)  # teams(rate_limit/quota/budget/on_exceeded), pricing(on_missing/overrides)
internal/audit/record.go (수정)   # CostRef{AmountUSDMicros, PricingMissing, PricingVersion} (M3 nil → 실제)
internal/server/anthropicapi/messages.go (수정)  # 거버넌스 사전체크+사후차감 통합, audit cost 채움

# ── Group B: OpenAI ingress + 변환 ──
internal/openai/
  convert.go        # OpenAI ↔ canonical(Anthropic) 요청/응답/청크 변환
  convert_test.go
internal/server/openaiapi/
  chat.go           # POST /v1/chat/completions (비스트리밍 + SSE data:[DONE])
  chat_test.go
  models.go         # GET /v1/models (OpenAI 형식, allow-list)
  models_test.go
providers/openaicompat/
  openaicompat.go   # vLLM/Ollama/llm-d (OpenAI 호환); 프로토콜 매칭 전달
  openaicompat_test.go
providers/ProxyRequest (수정 — providers/provider.go)  # IngressProtocol 필드 추가
internal/server/server.go (수정)  # OpenAI ingress mux 등록

# ── Group C: failover ──
internal/router/router.go (수정)  # 우선순위 폴백 체인 + circuit breaker
internal/router/breaker.go        # 인스턴스-로컬 circuit breaker
internal/router/breaker_test.go

# ── carry-over ──
providers/bedrock/converse.go (수정)  # 샘플링 파라미터 패스스루
```

> 범위가 크므로 그룹 순서로 실행: A(거버넌스) → B(OpenAI) → C(failover) → carry-over. 각 그룹은 독립적으로 빌드·테스트 green.

═══════════════════════════════════════════════════════════
# GROUP A — 거버넌스 (pricing / limiter / budget / 파이프라인 통합)
═══════════════════════════════════════════════════════════

### Task A1: pricing — (provider,model) 단가 테이블 + µUSD 계산

**Files:** Create `internal/pricing/pricing.go`, `internal/pricing/bundled.go`, `internal/pricing/pricing_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/pricing/pricing_test.go`:
```go
package pricing

import "testing"

func TestCostUSDMicrosRoundHalfEven(t *testing.T) {
	tbl := New(OnMissingAllow, map[Key]Rate{
		{"anthropic-direct", "claude-sonnet-4-6"}: {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CacheReadPerMTok: 300_000, CacheWrite5mPerMTok: 3_750_000, CacheWrite1hPerMTok: 6_000_000},
	})
	// 1000 input, 500 output, 45000 cache_read, 1024 cache_write(5m)
	u := Usage{Input: 1000, Output: 500, CacheRead: 45000, CacheWrite5m: 1024}
	cost, missing := tbl.CostUSDMicros("anthropic-direct", "claude-sonnet-4-6", u)
	if missing {
		t.Fatal("rate present, should not be missing")
	}
	// input 1000*3_000_000/1e6=3000; output 500*15_000_000/1e6=7500;
	// cache_read 45000*300_000/1e6=13500; cache_write5m 1024*3_750_000/1e6=3840
	want := int64(3000 + 7500 + 13500 + 3840)
	if cost != want {
		t.Fatalf("cost = %d µUSD, want %d", cost, want)
	}
}

func TestOnMissingAllowReturnsZeroAndMissing(t *testing.T) {
	tbl := New(OnMissingAllow, nil)
	cost, missing := tbl.CostUSDMicros("p", "unknown-model", Usage{Input: 100})
	if cost != 0 || !missing {
		t.Fatalf("missing model: cost=%d missing=%v (want 0,true)", cost, missing)
	}
}

func TestOnMissingBlock(t *testing.T) {
	tbl := New(OnMissingBlock, nil)
	if tbl.OnMissing() != OnMissingBlock {
		t.Fatal("on_missing policy not stored")
	}
}

func TestCacheWriteTTLTiers(t *testing.T) {
	tbl := New(OnMissingAllow, map[Key]Rate{
		{"p", "m"}: {CacheWrite5mPerMTok: 1_250_000, CacheWrite1hPerMTok: 2_000_000},
	})
	c5, _ := tbl.CostUSDMicros("p", "m", Usage{CacheWrite5m: 1_000_000})
	c1h, _ := tbl.CostUSDMicros("p", "m", Usage{CacheWrite1h: 1_000_000})
	if c5 != 1_250_000 || c1h != 2_000_000 {
		t.Fatalf("ttl tiers: 5m=%d 1h=%d", c5, c1h)
	}
}
```

- [ ] **Step 2: 실패 확인** Run: `go test ./internal/pricing/ -v` → `undefined: New`

- [ ] **Step 3: 구현 pricing.go**

```go
// Package pricing computes per-request cost in integer micro-USD (µUSD) from a
// (provider, model)-keyed rate table. Money is NEVER float (design §5.3) — all
// rates are µUSD-per-million-tokens (int64) and the per-request cost is a
// single round-half-even division. cache write is TTL-tiered (5m vs 1h) and
// cache read is billed separately.
package pricing

import "math/big"

type Key struct {
	Provider string
	Model    string
}

// Rate holds µUSD per 1,000,000 tokens for each token class.
type Rate struct {
	InputPerMTok        int64
	OutputPerMTok       int64
	CacheReadPerMTok    int64
	CacheWrite5mPerMTok int64
	CacheWrite1hPerMTok int64
}

type Usage struct {
	Input        int64
	Output       int64
	CacheRead    int64
	CacheWrite5m int64
	CacheWrite1h int64
}

type OnMissing int

const (
	OnMissingAllow OnMissing = iota // cost 0 + missing=true (default; self-hosted chargeback unknown)
	OnMissingBlock
)

type Table struct {
	onMissing OnMissing
	rates     map[Key]Rate
	Version   string
}

func New(onMissing OnMissing, rates map[Key]Rate) *Table {
	if rates == nil {
		rates = map[Key]Rate{}
	}
	return &Table{onMissing: onMissing, rates: rates, Version: "bundled"}
}

func (t *Table) OnMissing() OnMissing { return t.onMissing }

// CostUSDMicros returns the request cost in µUSD and whether the (provider,
// model) rate was missing. Cost is computed once over the full token totals
// (never per-chunk) with round-half-even on the /1e6 division.
func (t *Table) CostUSDMicros(provider, model string, u Usage) (cost int64, missing bool) {
	r, ok := t.rates[Key{provider, model}]
	if !ok {
		return 0, true
	}
	total := int64(0)
	total += mulDivRoundHalfEven(u.Input, r.InputPerMTok)
	total += mulDivRoundHalfEven(u.Output, r.OutputPerMTok)
	total += mulDivRoundHalfEven(u.CacheRead, r.CacheReadPerMTok)
	total += mulDivRoundHalfEven(u.CacheWrite5m, r.CacheWrite5mPerMTok)
	total += mulDivRoundHalfEven(u.CacheWrite1h, r.CacheWrite1hPerMTok)
	return total, false
}

// mulDivRoundHalfEven computes tokens * perMTok / 1_000_000 with banker's
// rounding, using math/big to avoid int64 overflow on large token counts.
func mulDivRoundHalfEven(tokens, perMTok int64) int64 {
	if tokens == 0 || perMTok == 0 {
		return 0
	}
	num := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(perMTok))
	denom := big.NewInt(1_000_000)
	q := new(big.Int)
	rem := new(big.Int)
	q.QuoRem(num, denom, rem)
	// round half to even
	twice := new(big.Int).Mul(rem, big.NewInt(2))
	cmp := twice.CmpAbs(denom)
	if cmp > 0 || (cmp == 0 && q.Bit(0) == 1) {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64()
}
```

`internal/pricing/bundled.go`:
```go
package pricing

// Bundled returns the default rate table (µUSD per 1M tokens). Operators
// override via config; self-hosted models supply their own chargeback rates.
func Bundled() map[Key]Rate {
	return map[Key]Rate{
		{"anthropic-direct", "claude-sonnet-4-6"}: {InputPerMTok: 3_000_000, OutputPerMTok: 15_000_000, CacheReadPerMTok: 300_000, CacheWrite5mPerMTok: 3_750_000, CacheWrite1hPerMTok: 6_000_000},
		{"anthropic-direct", "claude-opus-4-8"}:   {InputPerMTok: 5_000_000, OutputPerMTok: 25_000_000, CacheReadPerMTok: 500_000, CacheWrite5mPerMTok: 6_250_000, CacheWrite1hPerMTok: 10_000_000},
	}
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/pricing/ -v` → PASS.
```bash
git add internal/pricing/
git commit -s -m "feat(pricing): (provider,model) rate table, integer µUSD round-half-even"
```

---

### Task A2: limiter — LimiterStore(rate token-bucket + quota window)

**Files:** Create `internal/limiter/limiter.go`, `internal/limiter/limiter_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/limiter/limiter_test.go`:
```go
package limiter

import (
	"testing"
	"time"
)

func TestRateLimitBlocksOverBurst(t *testing.T) {
	l := NewMemory()
	// 60 rpm = 1/s, burst 2. clock injected for determinism.
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:rps"
	if !l.AllowRate(key, 1, 60, 2) { // burst 2 → first allowed
		t.Fatal("first request should be allowed")
	}
	if !l.AllowRate(key, 1, 60, 2) {
		t.Fatal("second (within burst) should be allowed")
	}
	if l.AllowRate(key, 1, 60, 2) {
		t.Fatal("third should be blocked (burst exhausted, no refill yet)")
	}
	// advance 1s → 1 token refilled at 1/s
	now = now.Add(time.Second)
	if !l.AllowRate(key, 1, 60, 2) {
		t.Fatal("after 1s refill, should be allowed")
	}
}

func TestQuotaTwoPhase(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:daily"
	limit := int64(1000)
	// optimistic check with estimate 800 → ok (0 used)
	if d := l.CheckQuota(key, 800, limit, 24*time.Hour); d != Allow {
		t.Fatalf("first check: %v", d)
	}
	l.DebitQuota(key, 800, 24*time.Hour) // actual 800 used
	// next check estimate 300 → 800+300=1100 > 1000 → Block
	if d := l.CheckQuota(key, 300, limit, 24*time.Hour); d != Block {
		t.Fatalf("over-limit check should block: %v", d)
	}
	// estimate 100 → 800+100=900 ≤ 1000 → Allow
	if d := l.CheckQuota(key, 100, limit, 24*time.Hour); d != Allow {
		t.Fatalf("under-limit check should allow: %v", d)
	}
}

func TestQuotaWindowResets(t *testing.T) {
	l := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	key := "team:win"
	l.DebitQuota(key, 1000, time.Hour)
	if d := l.CheckQuota(key, 1, 1000, time.Hour); d != Block {
		t.Fatal("at limit, should block")
	}
	now = now.Add(2 * time.Hour) // window elapsed
	if d := l.CheckQuota(key, 500, 1000, time.Hour); d != Allow {
		t.Fatal("after window reset, should allow")
	}
}
```

- [ ] **Step 2: 실패 확인** → `undefined: NewMemory`

- [ ] **Step 3: 구현 limiter.go**

```go
// Package limiter implements instance-local rate limiting (token bucket, TPM/
// RPM, pre-block) and quota windows (daily/monthly, two-phase optimistic check
// + post-debit). LimiterStore is the swappable backend; M5 ships in-memory,
// Redis (shared, multi-replica) is v0.2. Per §5.3 the in-memory limiter is
// per-instance, so multi-replica effective limits scale with replica count —
// documented, not hidden.
package limiter

import (
	"sync"
	"time"
)

type Decision int

const (
	Allow Decision = iota
	Block
)

type LimiterStore interface {
	// AllowRate token-bucket: cost tokens against ratePerMin (refill) with the
	// given burst. Returns false when the bucket lacks `cost` tokens.
	AllowRate(key string, cost, ratePerMin, burst int64) bool
	// CheckQuota optimistic: would used+estimate exceed limit in the window?
	CheckQuota(key string, estimate, limit int64, window time.Duration) Decision
	// DebitQuota records actual usage in the current window.
	DebitQuota(key string, actual int64, window time.Duration)
}

type bucket struct {
	tokens float64
	last   time.Time
}

type quotaWin struct {
	used      int64
	windowEnd time.Time
}

type Memory struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	quotas  map[string]*quotaWin
	now     func() time.Time
}

func NewMemory() *Memory {
	return &Memory{buckets: map[string]*bucket{}, quotas: map[string]*quotaWin{}, now: time.Now}
}

func (m *Memory) AllowRate(key string, cost, ratePerMin, burst int64) bool {
	if ratePerMin <= 0 {
		return true // unlimited
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.now()
	b := m.buckets[key]
	if b == nil {
		b = &bucket{tokens: float64(burst), last: t}
		m.buckets[key] = b
	}
	// refill at ratePerMin/60 tokens per second, capped at burst
	elapsed := t.Sub(b.last).Seconds()
	b.tokens += elapsed * (float64(ratePerMin) / 60.0)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = t
	if b.tokens >= float64(cost) {
		b.tokens -= float64(cost)
		return true
	}
	return false
}

func (m *Memory) CheckQuota(key string, estimate, limit int64, window time.Duration) Decision {
	if limit <= 0 {
		return Allow
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.curWindow(key, window)
	if q.used+estimate > limit {
		return Block
	}
	return Allow
}

func (m *Memory) DebitQuota(key string, actual int64, window time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	q := m.curWindow(key, window)
	q.used += actual
}

// curWindow returns the live window for key, resetting if elapsed. Caller holds mu.
func (m *Memory) curWindow(key string, window time.Duration) *quotaWin {
	t := m.now()
	q := m.quotas[key]
	if q == nil || !t.Before(q.windowEnd) {
		q = &quotaWin{windowEnd: t.Add(window)}
		m.quotas[key] = q
	}
	return q
}

var _ LimiterStore = (*Memory)(nil)
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/limiter/ -v -race` → PASS.
```bash
git add internal/limiter/
git commit -s -m "feat(limiter): in-memory rate token-bucket + two-phase quota window"
```

---

### Task A3: budget — BudgetStore(µUSD 누적, 2단계)

**Files:** Create `internal/budget/budget.go`, `internal/budget/budget_test.go`

- [ ] **Step 1: 실패 테스트**

`internal/budget/budget_test.go`:
```go
package budget

import (
	"testing"
	"time"
)

func TestBudgetTwoPhaseMicros(t *testing.T) {
	b := NewMemory()
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }
	key := "team:month"
	limit := int64(5_000_000) // 5 USD in µUSD
	if d := b.Check(key, 4_000_000, limit, 30*24*time.Hour); d != Allow {
		t.Fatalf("under: %v", d)
	}
	b.Debit(key, 4_000_000, 30*24*time.Hour)
	if d := b.Check(key, 2_000_000, limit, 30*24*time.Hour); d != Block {
		t.Fatalf("4M+2M>5M should block: %v", d)
	}
	if d := b.Check(key, 500_000, limit, 30*24*time.Hour); d != Allow {
		t.Fatalf("4M+0.5M≤5M should allow: %v", d)
	}
}
```

- [ ] **Step 2: 실패 확인** → `undefined: NewMemory`

- [ ] **Step 3: 구현 budget.go** (limiter의 quota와 동일 패턴, µUSD)

```go
// Package budget tracks per-team spend in integer micro-USD (§5.3). Same
// two-phase optimistic-check + post-debit shape as quota, but the unit is
// money (µUSD), fed by the pricing table. In-memory now; Redis v0.2.
package budget

import (
	"sync"
	"time"
)

type Decision int

const (
	Allow Decision = iota
	Block
)

type BudgetStore interface {
	Check(key string, estimateMicros, limitMicros int64, window time.Duration) Decision
	Debit(key string, actualMicros int64, window time.Duration)
}

type win struct {
	spent     int64
	windowEnd time.Time
}

type Memory struct {
	mu  sync.Mutex
	m   map[string]*win
	now func() time.Time
}

func NewMemory() *Memory { return &Memory{m: map[string]*win{}, now: time.Now} }

func (b *Memory) Check(key string, estimateMicros, limitMicros int64, window time.Duration) Decision {
	if limitMicros <= 0 {
		return Allow
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	w := b.cur(key, window)
	if w.spent+estimateMicros > limitMicros {
		return Block
	}
	return Allow
}

func (b *Memory) Debit(key string, actualMicros int64, window time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.cur(key, window).spent += actualMicros
}

func (b *Memory) cur(key string, window time.Duration) *win {
	t := b.now()
	w := b.m[key]
	if w == nil || !t.Before(w.windowEnd) {
		w = &win{windowEnd: t.Add(window)}
		b.m[key] = w
	}
	return w
}

var _ BudgetStore = (*Memory)(nil)
```

- [ ] **Step 4: 통과 + 커밋**

```bash
git add internal/budget/
git commit -s -m "feat(budget): in-memory two-phase µUSD budget store"
```

---

### Task A4: config(teams/pricing) + audit CostRef

**Files:** Modify `internal/config/config.go`, `internal/audit/record.go` + tests

- [ ] **Step 1: config 실패 테스트** — teams + pricing 파싱

`internal/config/config_test.go`에 추가:
```go
func TestLoadTeamsAndPricing(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{
	  "teams": {"platform-eng": {"allowed_models":["claude-sonnet-4-6"],"rate_limit":{"requests_per_minute":300,"tokens_per_minute":2000000},"quota":{"tokens_per_day":50000000,"on_exceeded":"block"},"budget":{"usd_per_month":5000,"on_exceeded":"warn"}}},
	  "pricing": {"on_missing":"allow","overrides":{"anthropic-direct":{"claude-sonnet-4-6":{"input_per_mtok":3.0,"output_per_mtok":15.0}}}}
	}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	tm := cfg.Teams["platform-eng"]
	if tm.RateLimit.RequestsPerMinute != 300 || tm.Quota.TokensPerDay != 50000000 || tm.Quota.OnExceeded != "block" || tm.Budget.OnExceeded != "warn" {
		t.Fatalf("team: %+v", tm)
	}
	if cfg.Pricing.OnMissing != "allow" {
		t.Fatalf("pricing on_missing: %q", cfg.Pricing.OnMissing)
	}
}
```

- [ ] **Step 2: config.go 타입 추가**

```go
type RateLimitConfig struct {
	RequestsPerMinute int64 `json:"requests_per_minute"`
	TokensPerMinute   int64 `json:"tokens_per_minute"`
}
type QuotaConfig struct {
	TokensPerDay   int64  `json:"tokens_per_day"`
	TokensPerMonth int64  `json:"tokens_per_month"`
	OnExceeded     string `json:"on_exceeded"` // block|warn
}
type BudgetConfig struct {
	USDPerMonth float64 `json:"usd_per_month"` // converted to µUSD at use
	OnExceeded  string  `json:"on_exceeded"`
}
type TeamConfig struct {
	AllowedModels []string        `json:"allowed_models"`
	RateLimit     RateLimitConfig `json:"rate_limit"`
	Quota         QuotaConfig     `json:"quota"`
	Budget        BudgetConfig    `json:"budget"`
}
// Pricing: per-MTok rates as human USD floats in config, converted to µUSD-per-MTok int64 at load.
type RateConfig struct {
	InputPerMTok        float64 `json:"input_per_mtok"`
	OutputPerMTok       float64 `json:"output_per_mtok"`
	CacheReadPerMTok    float64 `json:"cache_read_per_mtok"`
	CacheWrite5mPerMTok float64 `json:"cache_write_5m_per_mtok"`
	CacheWrite1hPerMTok float64 `json:"cache_write_1h_per_mtok"`
}
type PricingConfig struct {
	OnMissing string                            `json:"on_missing"` // allow|block
	Overrides map[string]map[string]RateConfig  `json:"overrides"`  // provider → model → rate
}
```
`Config`에 `Teams map[string]TeamConfig json:"teams"`, `Pricing PricingConfig json:"pricing"` 추가.

- [ ] **Step 3: audit CostRef — record.go의 Cost *struct{} → 실제 타입**

`internal/audit/record.go`:
```go
type CostRef struct {
	AmountUSDMicros int64  `json:"amount_usd_micros"`
	PricingMissing  bool   `json:"pricing_missing"`
	PricingVersion  string `json:"pricing_version,omitempty"`
}
```
`Record.Cost`를 `*struct{}` → `*CostRef`로 변경. record_test.go의 기존 테스트가 깨지지 않는지 확인(Cost nil이면 omitempty로 생략 — 변경 없음).

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/config/ ./internal/audit/ -v` → PASS.
```bash
git add internal/config/config.go internal/config/config_test.go internal/audit/record.go
git commit -s -m "feat(config,audit): team governance config + pricing + audit CostRef"
```

---

### Task A5: 거버넌스 파이프라인 통합 (messages.go 사전체크+사후차감+cost)

**Files:** Modify `internal/server/anthropicapi/messages.go`, new `internal/server/governance.go` (shared by both ingresses), tests

거버넌스를 ingress가 공유하도록 `internal/server/governance.go`에 `Governor`를 둔다(anthropic + openai ingress 둘 다 사용). messages handler가 Governor를 호출: allow-list(이미 있음) → rate limit → quota check → budget check (사전, provider 호출 전) → 응답 후 quota debit + budget debit + audit cost 채움.

- [ ] **Step 1: Governor 실패 테스트 — internal/server/governance_test.go**

```go
package server

import (
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
)

func testGovernor() *Governor {
	teams := map[string]TeamPolicy{
		"t": {RatePerMin: 60, RateBurst: 2, TokensPerDay: 1000, QuotaExceeded: "block", BudgetMicrosPerMonth: 0, BudgetExceeded: "block"},
	}
	return NewGovernor(teams, limiter.NewMemory(), budget.NewMemory(),
		pricing.New(pricing.OnMissingAllow, map[pricing.Key]pricing.Rate{{"p", "m"}: {InputPerMTok: 1_000_000, OutputPerMTok: 1_000_000}}))
}

func TestGovernorQuotaBlocks(t *testing.T) {
	g := testGovernor()
	// debit team t up to its daily limit, then a pre-check must block
	g.lim.DebitQuota("t", 1000, 24*time.Hour)
	dec := g.PreCheck("t", 100)
	if dec.Allowed {
		t.Fatalf("quota exhausted must block: %+v", dec)
	}
	if dec.Status != 429 {
		t.Fatalf("quota block status = %d, want 429", dec.Status)
	}
}

func TestGovernorSettleComputesCost(t *testing.T) {
	g := testGovernor()
	cost, missing := g.Settle("t", "p", "m", pricing.Usage{Input: 1000, Output: 500})
	// 1000*1 + 500*1 = 1500 µUSD
	if missing || cost != 1500 {
		t.Fatalf("settle cost=%d missing=%v", cost, missing)
	}
}
```

- [ ] **Step 2: 실패 확인** → `undefined: Governor`

- [ ] **Step 3: governance.go 구현**

```go
package server

import (
	"time"

	"github.com/inferplane/inferplane/internal/budget"
	"github.com/inferplane/inferplane/internal/limiter"
	"github.com/inferplane/inferplane/internal/pricing"
)

type TeamPolicy struct {
	RatePerMin           int64
	RateBurst            int64
	TokensPerMinute      int64
	TokensPerDay         int64
	QuotaExceeded        string // block|warn
	BudgetMicrosPerMonth int64
	BudgetExceeded       string
}

type Governor struct {
	teams map[string]TeamPolicy
	lim   limiter.LimiterStore
	bud   budget.BudgetStore
	price *pricing.Table
}

func NewGovernor(teams map[string]TeamPolicy, lim limiter.LimiterStore, bud budget.BudgetStore, price *pricing.Table) *Governor {
	return &Governor{teams: teams, lim: lim, bud: bud, price: price}
}

type GovDecision struct {
	Allowed bool
	Status  int    // 429 (rate/quota), 402 (budget), 0 (allowed)
	Reason  string
}

// PreCheck enforces rate limit + quota + budget BEFORE the upstream call.
// estimate is the request's estimated input tokens. block policy → deny;
// warn policy → allow (still settled afterward).
func (g *Governor) PreCheck(team string, estimateTokens int64) GovDecision {
	p, ok := g.teams[team]
	if !ok {
		return GovDecision{Allowed: true}
	}
	// rate limit (RPM): 1 request unit
	if p.RatePerMin > 0 && !g.lim.AllowRate("rate:"+team, 1, p.RatePerMin, max64(p.RateBurst, 1)) {
		return GovDecision{Status: 429, Reason: "rate limit exceeded"}
	}
	// quota (daily tokens)
	if p.TokensPerDay > 0 {
		if g.lim.CheckQuota("quota:"+team, estimateTokens, p.TokensPerDay, 24*time.Hour) == limiter.Block {
			if p.QuotaExceeded != "warn" {
				return GovDecision{Status: 429, Reason: "token quota exceeded"}
			}
		}
	}
	// budget (monthly µUSD) — pre-check with estimate cost 0 is impossible
	// before the call; budget is enforced as a soft pre-gate on accumulated
	// spend only (estimate 0), real enforcement is the post-debit threshold.
	if p.BudgetMicrosPerMonth > 0 {
		if g.bud.Check("budget:"+team, 0, p.BudgetMicrosPerMonth, 30*24*time.Hour) == budget.Block {
			if p.BudgetExceeded != "warn" {
				return GovDecision{Status: 402, Reason: "budget exceeded"}
			}
		}
	}
	return GovDecision{Allowed: true}
}

// Settle records actual token usage against quota and computes+debits cost.
// Returns the cost µUSD and whether pricing was missing (for the audit record).
func (g *Governor) Settle(team, provider, model string, u pricing.Usage) (costMicros int64, pricingMissing bool) {
	p := g.teams[team]
	if p.TokensPerDay > 0 {
		g.lim.DebitQuota("quota:"+team, u.Input+u.Output, 24*time.Hour)
	}
	costMicros, pricingMissing = g.price.CostUSDMicros(provider, model, u)
	if p.BudgetMicrosPerMonth > 0 {
		g.bud.Debit("budget:"+team, costMicros, 30*24*time.Hour)
	}
	return costMicros, pricingMissing
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
```

- [ ] **Step 4: messages.go 통합**

MessagesHandler에 `*Governor`를 선택적으로 주입(nil-safe). ServeHTTP에서 allow-list 통과 후:
- 추정 input 토큰: `estimateTokens(raw)` (M2 count_tokens의 추정기 재사용 — len/4, 또는 간단히). PreCheck(team, estimate). block이면 writeErr(status, reason) + audit started(outcome status). 
- 응답 후(serveComplete/serveStream): usage에서 pricing.Usage 구성 → Settle → audit completed의 Cost = &CostRef{AmountUSDMicros, PricingMissing, PricingVersion}.
import cycle 주의: messages.go(anthropicapi)가 server.Governor를 import하면 cycle(server→anthropicapi). → Governor를 internal/server에 두지 말고 **internal/governance** 신규 패키지로. (M3의 principal과 동일 회피.) governance.go를 `package governance`로 옮기고 server/anthropicapi 둘 다 import.

**수정**: governance.go를 `internal/governance/governance.go`(package governance)로 생성. TeamPolicy/Governor/GovDecision를 거기에. messages.go는 `governance.Governor` 사용. governance_test.go도 package governance.

- [ ] **Step 5: messages 통합 테스트**

messages_test.go에 governor 주입 + quota block(429) + cost in audit 테스트 추가.

- [ ] **Step 6: 통과 + 커밋**

Run: `go test ./internal/... -v -race` → PASS, `go build ./...`.
```bash
git add internal/governance/ internal/server/anthropicapi/messages.go internal/server/anthropicapi/messages_test.go
git commit -s -m "feat(governance): Governor pre-check (rate/quota/budget) + settle (cost→audit)"
```

═══════════════════════════════════════════════════════════
# GROUP B — OpenAI ingress + 변환 + openai_compatible provider
═══════════════════════════════════════════════════════════

### Task B1: ProxyRequest.IngressProtocol + openai 변환 코어

**Files:** Modify `providers/provider.go`(IngressProtocol 필드), Create `internal/openai/convert.go`, `internal/openai/convert_test.go`

OpenAI ↔ canonical(Anthropic) 변환. 매핑:
- **roles**: OpenAI system→Anthropic top-level system; user/assistant→messages; tool→user message with tool_result block.
- **assistant tool_calls** `[{id,function:{name,arguments(JSON string)}}]` ↔ Anthropic **tool_use** blocks `{type:tool_use,id,name,input(object)}`.
- **tool message** `{role:tool,tool_call_id,content}` ↔ Anthropic **tool_result** block `{type:tool_result,tool_use_id,content}`.
- **finish_reason** stop/length/tool_calls ↔ **stop_reason** end_turn/max_tokens/tool_use.
- **usage** prompt_tokens/completion_tokens ↔ input_tokens/output_tokens.
- **max_tokens**/max_completion_tokens ↔ max_tokens. temperature/top_p 통과.

- [ ] **Step 1: provider.go에 IngressProtocol 추가**

`ProxyRequest`에 `IngressProtocol string // "anthropic" | "openai"` 필드 추가. 기존 호출처(anthropic ingress)는 "anthropic"으로 설정(messages.go 수정). 빌드 유지.

- [ ] **Step 2: 실패 테스트 — openai 요청→canonical, canonical 응답→openai**

`internal/openai/convert_test.go`:
```go
package openai

import (
	"encoding/json"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestRequestToCanonicalBasics(t *testing.T) {
	in := []byte(`{"model":"gpt-x","max_tokens":256,"temperature":0.7,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`)
	cr, err := RequestToCanonical(in)
	if err != nil {
		t.Fatal(err)
	}
	if cr.Model != "gpt-x" || cr.MaxTokens == nil || *cr.MaxTokens != 256 {
		t.Fatalf("req: %+v", cr)
	}
	// system extracted to top-level system; user message present
	if len(cr.System) == 0 {
		t.Fatal("system not mapped")
	}
	if len(cr.Messages) != 1 || cr.Messages[0].Role != "user" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
}

func TestResponseFromCanonical(t *testing.T) {
	txt := "answer"
	stop := "end_turn"
	in, out := int64(10), int64(3)
	resp := &schema.ChatResponse{ID: "msg_1", Model: "m", Role: "assistant",
		Content: []schema.ContentBlock{{Type: "text", Text: &txt}}, StopReason: &stop,
		Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}
	oai := ResponseFromCanonical(resp)
	var m map[string]any
	json.Unmarshal(oai, &m)
	if m["object"] != "chat.completion" {
		t.Fatalf("object: %v", m["object"])
	}
	choices := m["choices"].([]any)
	c0 := choices[0].(map[string]any)
	if c0["finish_reason"] != "stop" {
		t.Fatalf("finish_reason: %v", c0["finish_reason"])
	}
	msg := c0["message"].(map[string]any)
	if msg["content"] != "answer" {
		t.Fatalf("content: %v", msg["content"])
	}
	usage := m["usage"].(map[string]any)
	if usage["prompt_tokens"].(float64) != 10 || usage["completion_tokens"].(float64) != 3 {
		t.Fatalf("usage: %v", usage)
	}
}

func TestToolCallRoundTrip(t *testing.T) {
	// OpenAI assistant tool_call → canonical tool_use → back to OpenAI
	in := []byte(`{"model":"m","messages":[{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"bash","arguments":"{\"cmd\":\"ls\"}"}}]},{"role":"tool","tool_call_id":"call_1","content":"ok"}]}`)
	cr, err := RequestToCanonical(in)
	if err != nil {
		t.Fatal(err)
	}
	// assistant message → tool_use block; tool message → tool_result block
	foundToolUse, foundToolResult := false, false
	for _, msg := range cr.Messages {
		for _, b := range msg.Content {
			if b.Type == "tool_use" && b.Name == "bash" && b.ID == "call_1" {
				foundToolUse = true
			}
			if b.Type == "tool_result" && b.ToolUseID == "call_1" {
				foundToolResult = true
			}
		}
	}
	if !foundToolUse || !foundToolResult {
		t.Fatalf("tool mapping: use=%v result=%v\n%+v", foundToolUse, foundToolResult, cr.Messages)
	}
}
```

- [ ] **Step 3: 구현 convert.go**

The implementer writes the full bidirectional conversion. Provide the explicit mapping (above) + finish_reason/stop_reason tables. Key functions:
- `RequestToCanonical(openaiBody []byte) (*schema.ChatRequest, error)`: parse OpenAI request, map system messages → `cr.System` (json.RawMessage array of text blocks), user/assistant/tool messages → `cr.Messages` ([]schema.Message); assistant.tool_calls → tool_use blocks; tool messages → user message with tool_result block; max_tokens/max_completion_tokens → cr.MaxTokens; carry temperature/top_p into cr.Extra.
- `CanonicalToRequest(cr *schema.ChatRequest) []byte`: inverse (for Anthropic-ingress → openai_compatible provider). Best-effort: text + tool mapping; thinking blocks dropped (documented).
- `ResponseFromCanonical(*schema.ChatResponse) []byte`: build OpenAI chat.completion JSON (choices[0].message.content from text blocks, tool_calls from tool_use blocks, finish_reason from stop_reason, usage).
- `ChunkFromCanonical(*schema.ChatChunk, state) []byte`: build OpenAI chat.completion.chunk for streaming (delta.content from text_delta). Needs minimal state (whether role sent). Return nil for events with no OpenAI equivalent (e.g. ping).
- `ResponseToCanonical([]byte) *schema.ChatResponse` / `ChunkToCanonical([]byte) *schema.ChatChunk`: parse vLLM OpenAI responses into canonical for governance observation.
- helper maps: `finishToStop` (stop→end_turn, length→max_tokens, tool_calls→tool_use), `stopToFinish` (inverse).

stop_reason↔finish_reason mapping is fixed; provide both maps. The implementer ensures the three tests pass and adds streaming chunk tests.

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/openai/ -v` → PASS. `go build ./...`.
```bash
git add providers/provider.go internal/server/anthropicapi/messages.go internal/openai/
git commit -s -m "feat(openai): OpenAI↔canonical conversion + ProxyRequest.IngressProtocol"
```

---

### Task B2: openai_compatible provider

**Files:** Create `providers/openaicompat/openaicompat.go`, `providers/openaicompat/openaicompat_test.go`, modify `cmd/inferplane/main.go`(register), `internal/config`(없으면)

vLLM/Ollama/llm-d. **프로토콜 매칭 전달**: ingress가 openai면 RawBody verbatim 전달(무손실); ingress가 anthropic이면 canonical→OpenAI 변환(openai.CanonicalToRequest). 응답: vLLM OpenAI → ingress가 openai면 verbatim tee; anthropic이면 OpenAI→canonical→Anthropic 변환. provider는 canonical 관찰을 위해 응답을 ResponseToCanonical로 파싱.

- [ ] **Step 1: 실패 테스트** (httptest fake vLLM)

```go
package openaicompat

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCompleteForwardsOpenAIVerbatimWhenIngressOpenAI(t *testing.T) {
	var gotBody []byte
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2}}`)
	}))
	defer up.Close()
	p, _ := factory(providers.Config{Type: "openai_compatible", BaseURL: up.URL, APIKey: "k"})
	raw := []byte(`{"model":"qwen","messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "qwen", Upstream: "Qwen/Qwen2.5", RawBody: raw, IngressProtocol: "openai"})
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != string(raw) {
		t.Fatalf("openai ingress → openai provider must forward verbatim:\n got: %s\nwant: %s", gotBody, raw)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 5 {
		t.Fatalf("usage observation: %+v", resp.Parsed)
	}
}
```
(plus: anthropic ingress → openai provider converts body; streaming test.)

- [ ] **Step 2-4**: 구현 + 통과 + 커밋. provider builds upstream body: if `req.IngressProtocol == "openai"` use `req.RawBody`; else `openai.CanonicalToRequest(req.Parsed)`. Upstream model id = req.Upstream (set the model field in the OpenAI body to Upstream — for verbatim case, the body already has the client's model; openai_compatible servers often accept it, but set it to Upstream via a minimal top-level rewrite like bedrock's toInvokeBody to map model→Upstream). POST to BaseURL+"/v1/chat/completions" with Authorization Bearer APIKey. Parse response via openai.ResponseToCanonical for Parsed. Streaming: SSE data: lines → ChunkToCanonical → StreamEvent{Raw=<re-serialize per ingress>, Chunk}. NOTE: for openai ingress the StreamEvent.Raw should be the OpenAI SSE bytes (verbatim upstream); for anthropic ingress, re-serialize canonical→Anthropic SSE. **Simplest**: provider always emits StreamEvent.Raw in the INGRESS protocol — but provider doesn't render to ingress... Actually the ingress handler renders. Reconsider: keep provider returning canonical-observation chunks + the UPSTREAM raw bytes; the INGRESS handler decides final wire format. To avoid over-coupling, openai_compatible provider returns StreamEvent{Raw: <upstream OpenAI SSE bytes>, Chunk: canonical}. The OpenAI ingress tees Raw verbatim (openai→openai). The Anthropic ingress (cross-protocol) ignores Raw and re-serializes Chunk via schema.WriteAnthropicSSE. So: ingress handler chooses Raw-tee vs Chunk-reserialize based on whether provider's wire matches ingress. Document this in the StreamEvent contract: Raw is the PROVIDER's native wire; cross-protocol ingress re-serializes from Chunk.

```bash
git commit -s -m "feat(openaicompat): vLLM/Ollama provider with protocol-match forwarding"
```

---

### Task B3: OpenAI ingress (/v1/chat/completions + /v1/models)

**Files:** Create `internal/server/openaiapi/chat.go`, `models.go` + tests, modify `internal/server/server.go`(mux)

OpenAI ingress: parse OpenAI body → canonical(observation) + keep raw. Resolve model. Build ProxyRequest{Parsed:canonical, RawBody:openai-bytes, IngressProtocol:"openai"}. Governor pre-check. Provider Complete/Stream. RESPONSE: if provider native == openai (openai_compatible) → tee provider RawBody/Raw verbatim as OpenAI; if provider native == anthropic (anthropic/bedrock) → convert canonical response → OpenAI via openai.ResponseFromCanonical / streaming via openai.ChunkFromCanonical. Settle + audit.

- [ ] Implement chat.go (mirrors anthropicapi/messages.go structure but OpenAI wire + conversion on the response side), models.go (OpenAI `{object:list,data:[{id,object:model,owned_by}]}` filtered by allow-list). Tests via mockprovider + a canonical-returning fake. Wire into server.go DataMux: `mux.Handle("POST /v1/chat/completions", ...)`, `mux.Handle("GET /v1/models", ...)` — but `/v1/models` conflicts with the Anthropic one! Both ingresses expose GET /v1/models in different formats. Resolution: Anthropic clients and OpenAI clients both hit GET /v1/models but expect different shapes. M5 decision: keep GET /v1/models = Anthropic shape (Claude Code), and OpenAI clients use the same endpoint — OpenAI models list shape differs. Since OpenCode(openai) and Claude Code(anthropic) both call /v1/models, serve **OpenAI shape on a content-negotiation or a separate path**. Simplest: OpenAI ingress mounts its chat at /v1/chat/completions (unambiguous) and reuses the existing /v1/models returning Anthropic shape — OpenCode tolerates? NO. Decision: detect by path is impossible (same path). Use the fact that OpenCode hits /v1/models and expects OpenAI shape; Claude Code hits /v1/models and expects Anthropic shape. **They can't share.** M5 resolution: serve OpenAI-shaped /v1/models ONLY when the request has no anthropic-version header (heuristic), else Anthropic shape. Implement a single /v1/models handler that branches on presence of `anthropic-version` header (Anthropic clients send it) → Anthropic shape; else OpenAI shape. Document this heuristic.

- [ ] commit `feat(openai): /v1/chat/completions ingress + content-negotiated /v1/models`

---

═══════════════════════════════════════════════════════════
# GROUP C — failover / circuit breaker
═══════════════════════════════════════════════════════════

### Task C1: circuit breaker + priority fallback in router

**Files:** Create `internal/router/breaker.go`, `internal/router/breaker_test.go`, modify `internal/router/router.go`

- [ ] **Step 1: breaker 실패 테스트** — 연속 N 실패 → open, 지수백오프 → half-open → close

```go
package router

import (
	"testing"
	"time"
)

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	b := newBreaker(3, time.Second)
	now := time.Unix(1_700_000_000, 0)
	b.now = func() time.Time { return now }
	if !b.Allow("p") {
		t.Fatal("closed initially")
	}
	b.RecordFailure("p")
	b.RecordFailure("p")
	if !b.Allow("p") {
		t.Fatal("2 failures < threshold, still closed")
	}
	b.RecordFailure("p") // 3rd → open
	if b.Allow("p") {
		t.Fatal("should be open after 3 consecutive failures")
	}
	now = now.Add(2 * time.Second) // backoff elapsed → half-open
	if !b.Allow("p") {
		t.Fatal("half-open after backoff")
	}
	b.RecordSuccess("p") // close
	if !b.Allow("p") {
		t.Fatal("closed after success")
	}
}
```

- [ ] **Step 2-4**: breaker.go (per-provider state: consecutive failures, open-until time, exponential backoff), router.ResolveChain(model) → []Target (priority list), router enforces breaker.Allow per target. RecordFailure/Success/Allow. Wire into messages.go/chat.go: try targets in order, skip open breakers, fallback ONLY pre-TTFT (before first chunk), record fallback in audit + `x-inferplane-fallback` header. mid-stream failure → error event termination (no fallback). commit `feat(router): priority fallback chain + circuit breaker (pre-TTFT only)`.

> NOTE: M5 router currently returns single target (Resolve). Add ResolveChain returning all targets; keep Resolve as ResolveChain()[0] for compatibility. Fallback orchestration lives in a small helper the ingress calls; pre-TTFT means: for non-streaming, retry next target on error; for streaming, only fall back if Stream() returns an error BEFORE yielding the first event.

---

### Task C2 (carry-over): Converse 샘플링 파라미터 패스스루

**Files:** Modify `providers/bedrock/converse.go`, `converse_test.go`

M4 리뷰 발견: toConverseRequest가 temperature/top_p/stop_sequences를 Inference에 안 넣음. raw body에서 파싱해 cr.Inference에 추가(awsClient.buildInference가 이미 처리). 테스트 추가 + 구현 + commit `fix(bedrock): pass temperature/top_p/stop_sequences through Converse`.

---

### Task C3: M5 게이트 — OpenCode 실연동 (수동)

- [ ] config에 openai_compatible provider(vLLM/Ollama) + team quota/budget. OpenCode를 OpenAI base URL로 inferplane에 연결.
- [ ] 게이트 체크리스트: OpenCode 대화(openai_compatible 경유, 무손실 verbatim), quota 초과 시 429 block, budget 초과 시 402(또는 warn), 감사로그에 cost.amount_usd_micros 채워짐 + pricing_missing(self-hosted 모델은 true unless chargeback 단가 설정), rate limit 동작, failover(두 provider 중 하나 죽이면 pre-TTFT 폴백 + x-inferplane-fallback 헤더).

---

## Self-Review 결과

- **스펙 커버리지**: §5.3 rate/quota/budget 3분리 → A2/A3/A5. µUSD 단가(provider,model)·TTL cache·round-half-even·on_missing → A1. audit cost 채움 → A4/A5. §3.2 OpenAI ingress → B3. §3.3 변환 매트릭스(verbatim 일치/변환 불일치) → B1/B2/B3. openai_compatible provider → B2. §4.5 failover/circuit breaker pre-TTFT → C1. M4 carry-over → C2. ✓
- **플레이스홀더**: B1/B2/B3 변환은 매핑 테이블을 명시하되 일부 "구현자가 매핑대로" 위임(OpenAI 변환은 잘 정의됨, 테스트로 고정). /v1/models 공유 충돌은 anthropic-version 헤더 휴리스틱으로 해소(문서화). C1 fallback 오케스트레이션은 helper로. 거버넌스 코드(A)는 전부 명시. ✓
- **타입 일관성**: pricing.Table/Key/Rate/Usage/OnMissing(A1) → A5/Governor 사용. limiter.LimiterStore/Memory/Decision(A2), budget.BudgetStore/Memory(A3) → governance.Governor(A5). governance.Governor/TeamPolicy/GovDecision(A5, package governance) → messages.go/chat.go. openai.RequestToCanonical/ResponseFromCanonical/ChunkFromCanonical/CanonicalToRequest/ResponseToCanonical/ChunkToCanonical(B1) → B2/B3. ProxyRequest.IngressProtocol(B1) → B2/B3. audit.CostRef(A4) → A5/B3. router breaker(C1) → messages/chat. ✓
- **알려진 한계 (의도)**: budget 사전 체크는 누적 spend 기반(estimate 0)이라 단일 고비용 요청은 사후에야 차단(설계 §5.3 오버슈트 허용과 일치). Redis 미구현(인터페이스만, v0.2). /v1/models 공유는 anthropic-version 헤더 휴리스틱. 변환은 Claude Code/OpenCode 공통 케이스(text/tool/usage) 중심, 이색 케이스 best-effort(§3.3). rate limit·quota·circuit breaker는 인스턴스-로컬(다중 레플리카 N배, 문서화). budget 단위 µUSD는 A1 pricing과 일관.
