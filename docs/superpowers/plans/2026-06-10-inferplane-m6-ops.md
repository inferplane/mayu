# inferplane M6 — 운영 준비(Operational Readiness) 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** inferplane을 운영 가능한 v0.1로 마무리한다 — Prometheus `/metrics`(OTel GenAI 네이밍), 자체 TLS 리스너, Helm chart, Grafana 대시보드, Dockerfile + quickstart README. 게이트: `docker run` 한 줄 → 키 발급 → 클라이언트 5분 연결 + `/metrics` 스크레이프 (스펙: §6.2·§2.3·§9).

**Architecture:** 메트릭은 `internal/metrics`에 단일 Prometheus registry + collector로 모은다. M3/M5의 기존 atomic 카운터는 그대로 두되 metrics 패키지가 hook으로 Prometheus 메트릭을 증가시키고, ingress/governance/router가 그 hook을 호출한다(기존 패키지가 prometheus에 직접 의존하지 않게 — 단방향). `/metrics`는 admin 평면(`:9090`) 무인증. TLS는 자체 리스너 옵션(비-K8s용); K8s는 ingress/메시 종단 권장. Helm/Docker/README는 코드 외 산출물.

**Tech Stack:** Go 1.25+, `github.com/prometheus/client_golang/prometheus` + `/promhttp`(세 번째 외부 의존성, pure-Go). Helm 3, Docker(멀티스테이지 → distroless/static). M5 위에 구축.

---

## M6 결정 기록 (승인된 설계 r4)

- **/metrics**(§6.2): OTel GenAI semantic conventions 네이밍. admin 평면 무인증(§5.5). 카디널리티 가드 — `team`·`model` 레이블은 **config 선언값만**(요청 입력값 직접 레이블 금지). `inferplane_budget_spend_usd_total`은 **관측용 근사치**(정산 진실원은 µUSD 집계, 메트릭 아님).
- **TLS**(§2.3): `server.tls{cert_ref,key_ref}` → `ListenAndServeTLS`. K8s는 메시/ingress 종단 권장(문서).
- **Helm/Docker/README**: 단일 바이너리 + 5분 데모(v0.1 성공 기준).
- **메트릭 hook 패턴**: 기존 audit/governance/router는 prometheus를 import하지 않는다. `internal/metrics`가 등록을 소유하고, 호출처가 metrics 함수를 호출. (audit의 atomic 카운터는 M3/M5 테스트용으로 유지, metrics가 별도 Prometheus 카운터를 운영 — 이중이지만 단순/안전.)

## 마일스톤 로드맵 (6개 중 6번 — 마지막)

| M | 범위 | 게이트 |
|---|---|---|
| M1✅~M5✅ | (완료) | |
| **M6 (이 계획)** | metrics/TLS/Helm/Docker/quickstart | docker run→키 발급→클라이언트 5분 + /metrics 스크레이프 |

---

## 파일 구조

```
go.mod / go.sum                   # prometheus/client_golang (third external dep)
internal/metrics/
  metrics.go                      # Registry + collectors + hook funcs (OTel GenAI naming)
  metrics_test.go
internal/server/
  metricsapi.go                   # GET /metrics handler (promhttp)
  server.go (수정)                # AdminMux에 /metrics 등록
  tls.go                          # TLS 리스너 구성 (cert/key from config refs)
internal/config/config.go (수정)  # ServerConfig.TLS{CertRef,KeyRef *SecretRef→경로}
cmd/inferplane/main.go (수정)     # ListenAndServeTLS when tls configured; metrics hooks 호출 wiring
# ── 메트릭 호출처 (수정) ──
internal/server/anthropicapi/messages.go (수정)  # requests_total, token_usage, duration, ttft, fallback
internal/server/openaiapi/chat.go (수정)         # 동일
internal/governance/governance.go (수정)         # quota_utilization, budget_spend, pricing_miss
internal/router/router.go (수정)                 # circuit_state
internal/audit/metrics.go (수정 또는 hook)       # audit_write_failures, buffer_utilization → prometheus
# ── 코드 외 산출물 ──
Dockerfile                        # 멀티스테이지 → static binary (distroless/static or scratch)
.dockerignore
charts/inferplane/
  Chart.yaml
  values.yaml
  templates/_helpers.tpl
  templates/configmap.yaml
  templates/deployment.yaml
  templates/service.yaml
  templates/serviceaccount.yaml
  templates/secret-note.txt        # (existingSecret 참조 안내; chart는 시크릿 생성 안 함)
deploy/grafana/inferplane.json     # Grafana 대시보드
README.md (수정)                   # quickstart (docker run 5분)
examples/config.json (수정)        # tls 예시(주석) + 그대로
```

---

### Task 1: prometheus 의존성 추가

**Files:** Modify `go.mod`, `go.sum`

- [ ] **Step 1: 추가 (NETWORK)**
```bash
cd /home/atomoh/mayu
go get github.com/prometheus/client_golang/prometheus@latest
go get github.com/prometheus/client_golang/prometheus/promhttp@latest
```
실패 시 BLOCKED 보고.
- [ ] **Step 2: 빌드 확인** `go build ./... && go list -m github.com/prometheus/client_golang`. (indirect → Task 2 import 후 direct, tidy는 Task 2.)
- [ ] **Step 3: 커밋**
```bash
git add go.mod go.sum
git commit -s -m "build: add prometheus/client_golang for /metrics"
```

---

### Task 2: internal/metrics — registry + collectors + hooks

**Files:** Create `internal/metrics/metrics.go`, `internal/metrics/metrics_test.go`

OTel GenAI semantic conventions 네이밍. 모든 메트릭을 한 registry에 등록하고, 호출처가 부르는 얇은 hook 함수를 노출. 카디널리티 가드는 호출처 책임(config 선언값만 전달)이지만, metrics 패키지는 레이블을 그대로 받는다.

- [ ] **Step 1: 실패 테스트**

`internal/metrics/metrics_test.go`:
```go
package metrics

import (
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestTokenUsageCounter(t *testing.T) {
	m := New()
	m.ObserveTokenUsage("input", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 1200)
	m.ObserveTokenUsage("output", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 850)
	got := testutil.ToFloat64(m.tokenUsage.WithLabelValues("input", "claude-sonnet-4-6", "anthropic-direct", "platform-eng"))
	if got != 1200 {
		t.Fatalf("input token usage = %v, want 1200", got)
	}
}

func TestRequestsTotalAndExposition(t *testing.T) {
	m := New()
	m.ObserveRequest("anthropic", "claude-sonnet-4-6", "anthropic-direct", "platform-eng", 200, 1.5, 0.4)
	// gather and confirm the metric names are present with GenAI naming
	out := gather(t, m)
	for _, want := range []string{
		"gen_ai_client_token_usage_total",
		"gen_ai_server_request_duration_seconds",
		"gen_ai_server_time_to_first_token_seconds",
		"inferplane_requests_total",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("metric %q not exposed in:\n%s", want, out)
		}
	}
}

func TestCircuitStateGauge(t *testing.T) {
	m := New()
	m.SetCircuitState("anthropic-direct", 2) // open
	got := testutil.ToFloat64(m.circuitState.WithLabelValues("anthropic-direct"))
	if got != 2 {
		t.Fatalf("circuit state = %v, want 2", got)
	}
}

func gather(t *testing.T, m *Metrics) string {
	t.Helper()
	mfs, err := m.reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	var sb strings.Builder
	for _, mf := range mfs {
		sb.WriteString(mf.GetName())
		sb.WriteString("\n")
	}
	_ = prometheus.NewRegistry
	return sb.String()
}
```

- [ ] **Step 2: 실패 확인** `go test ./internal/metrics/ -v` → `undefined: New`

- [ ] **Step 3: 구현 metrics.go**

```go
// Package metrics owns the Prometheus registry and exposes thin hook functions
// the rest of inferplane calls. Metric names follow OpenTelemetry GenAI semantic
// conventions (gen_ai.*) rendered in Prometheus form (gen_ai_*). Cardinality
// guard: callers must pass only config-declared team/model values, never raw
// request input. The budget_spend metric is an observability approximation —
// the settlement source of truth is the µUSD budget store, not this gauge.
package metrics

import "github.com/prometheus/client_golang/prometheus"

type Metrics struct {
	reg *prometheus.Registry

	tokenUsage      *prometheus.CounterVec   // gen_ai_client_token_usage_total
	requestDuration *prometheus.HistogramVec // gen_ai_server_request_duration_seconds
	ttft            *prometheus.HistogramVec // gen_ai_server_time_to_first_token_seconds
	requestsTotal   *prometheus.CounterVec   // inferplane_requests_total
	fallbackTotal   *prometheus.CounterVec   // inferplane_fallback_total
	circuitState    *prometheus.GaugeVec     // inferplane_circuit_state
	quotaUtil       *prometheus.GaugeVec     // inferplane_quota_utilization_ratio
	budgetSpend     *prometheus.CounterVec   // inferplane_budget_spend_usd_total
	pricingMiss     *prometheus.CounterVec   // inferplane_pricing_miss_total
	auditFailures   *prometheus.CounterVec   // inferplane_audit_write_failures_total
	auditBufferUtil prometheus.Gauge         // inferplane_audit_buffer_utilization_ratio
}

func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		reg: reg,
		tokenUsage: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "gen_ai_client_token_usage_total",
			Help: "Tokens used, by type (input|output|cache_read|cache_write_5m|cache_write_1h).",
		}, []string{"type", "model", "provider", "team"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gen_ai_server_request_duration_seconds",
			Help:    "End-to-end request duration.",
			Buckets: prometheus.DefBuckets,
		}, []string{"model", "provider", "ingress", "status"}),
		ttft: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "gen_ai_server_time_to_first_token_seconds",
			Help:    "Time to first streamed token.",
			Buckets: prometheus.DefBuckets,
		}, []string{"model", "provider"}),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_requests_total", Help: "Total requests.",
		}, []string{"ingress", "model", "provider", "team", "status"}),
		fallbackTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_fallback_total", Help: "Provider fallbacks.",
		}, []string{"model", "from_provider", "to_provider", "reason"}),
		circuitState: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "inferplane_circuit_state", Help: "Circuit breaker state (0=closed,1=half,2=open).",
		}, []string{"provider"}),
		quotaUtil: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "inferplane_quota_utilization_ratio", Help: "Quota utilization 0..1.",
		}, []string{"team", "window"}),
		budgetSpend: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_budget_spend_usd_total", Help: "Approximate spend in USD (observability only; settlement truth is the µUSD store).",
		}, []string{"team", "model", "cost_type"}),
		pricingMiss: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_pricing_miss_total", Help: "Requests with no pricing rate for (provider,model).",
		}, []string{"provider", "model"}),
		auditFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "inferplane_audit_write_failures_total", Help: "Audit sink write failures.",
		}, []string{"sink"}),
		auditBufferUtil: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "inferplane_audit_buffer_utilization_ratio", Help: "Audit WAL buffer utilization 0..1.",
		}),
	}
	reg.MustRegister(m.tokenUsage, m.requestDuration, m.ttft, m.requestsTotal,
		m.fallbackTotal, m.circuitState, m.quotaUtil, m.budgetSpend, m.pricingMiss,
		m.auditFailures, m.auditBufferUtil)
	return m
}

// Registry exposes the registry for the /metrics handler.
func (m *Metrics) Registry() *prometheus.Registry { return m.reg }

func (m *Metrics) ObserveTokenUsage(typ, model, provider, team string, tokens int64) {
	if tokens <= 0 {
		return
	}
	m.tokenUsage.WithLabelValues(typ, model, provider, team).Add(float64(tokens))
}

// ObserveRequest records one completed request: counter + duration, and TTFT if >0.
func (m *Metrics) ObserveRequest(ingress, model, provider, team string, status int, durationSec, ttftSec float64) {
	st := statusClass(status)
	m.requestsTotal.WithLabelValues(ingress, model, provider, team, st).Inc()
	m.requestDuration.WithLabelValues(model, provider, ingress, st).Observe(durationSec)
	if ttftSec > 0 {
		m.ttft.WithLabelValues(model, provider).Observe(ttftSec)
	}
}

func (m *Metrics) ObserveFallback(model, from, to, reason string) {
	m.fallbackTotal.WithLabelValues(model, from, to, reason).Inc()
}
func (m *Metrics) SetCircuitState(provider string, state int) {
	m.circuitState.WithLabelValues(provider).Set(float64(state))
}
func (m *Metrics) SetQuotaUtilization(team, window string, ratio float64) {
	m.quotaUtil.WithLabelValues(team, window).Set(ratio)
}
func (m *Metrics) AddBudgetSpend(team, model, costType string, usd float64) {
	m.budgetSpend.WithLabelValues(team, model, costType).Add(usd)
}
func (m *Metrics) IncPricingMiss(provider, model string) {
	m.pricingMiss.WithLabelValues(provider, model).Inc()
}
func (m *Metrics) IncAuditFailure(sink string) { m.auditFailures.WithLabelValues(sink).Inc() }
func (m *Metrics) SetAuditBufferUtilization(r float64) { m.auditBufferUtil.Set(r) }

func statusClass(status int) string {
	switch {
	case status >= 200 && status < 300:
		return "2xx"
	case status >= 400 && status < 500:
		return "4xx"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
```

- [ ] **Step 4: 통과 + tidy + 커밋**

Run: `go test ./internal/metrics/ -v` → PASS. `go mod tidy`(prometheus direct화), full `go test ./...`, `go vet ./...`, `gofmt -l .` clean.
```bash
git add internal/metrics/ go.mod go.sum
git commit -s -m "feat(metrics): Prometheus registry + GenAI-convention collectors and hooks"
```

---

### Task 3: GET /metrics 핸들러 + AdminMux 등록

**Files:** Create `internal/server/metricsapi.go`, modify `internal/server/server.go`, `internal/server/server_test.go`

- [ ] **Step 1: 실패 테스트 — /metrics 무인증 스크레이프**

`internal/server/server_test.go`에 추가:
```go
import "github.com/inferplane/inferplane/internal/metrics"

func TestAdminMuxMetricsUnauthed(t *testing.T) {
	m := metrics.New()
	m.ObserveRequest("anthropic", "claude-sonnet-4-6", "anthropic-direct", "t", 200, 1.0, 0)
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, m)
	req := httptest.NewRequest("GET", "/metrics", nil) // NO auth
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/metrics should be unauthenticated 200, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "inferplane_requests_total") {
		t.Fatalf("/metrics missing exposition: %s", rec.Body.String())
	}
}
```
(파일 상단 import에 strings 추가 필요 시.)

- [ ] **Step 2: 실패 확인** → AdminMux 시그니처 불일치(인자 3개)

- [ ] **Step 3: metricsapi.go + AdminMux 수정**

`internal/server/metricsapi.go`:
```go
package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/metrics"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsHandler serves Prometheus exposition for the given registry.
func metricsHandler(m *metrics.Metrics) http.Handler {
	return promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{})
}
```
`server.go` AdminMux 시그니처에 `m *metrics.Metrics` 추가, `/metrics` 무인증 등록:
```go
func AdminMux(store keystore.Store, adminTokens []string, m *metrics.Metrics) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	if m != nil {
		mux.Handle("GET /metrics", metricsHandler(m)) // unauthenticated (§5.5)
	}
	keys := adminapi.NewKeysHandler(store)
	mux.Handle("/admin/keys", AdminTokenAuth(adminTokens, keys))
	mux.Handle("/admin/keys/", AdminTokenAuth(adminTokens, keys))
	return mux
}
```
server_test.go의 기존 AdminMux 호출처에 `, nil`(또는 metrics.New()) 추가.

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/server/ -v -race`, full `go test ./...` → PASS.
```bash
git add internal/server/metricsapi.go internal/server/server.go internal/server/server_test.go
git commit -s -m "feat(server): unauthenticated GET /metrics on admin plane"
```

---

### Task 4: 메트릭 호출처 wiring (ingress/governance/router)

**Files:** Modify `internal/server/anthropicapi/messages.go`, `internal/server/openaiapi/chat.go`, `internal/governance/governance.go`, `internal/router/router.go`, `internal/server/server.go`(DataMux가 metrics 전달), `cmd/inferplane/main.go`

메트릭을 실제로 채운다. 각 핸들러/거버너/라우터에 `*metrics.Metrics`(nil-safe)를 주입.

- [ ] **Step 1: 실패 테스트 — ingress가 요청을 메트릭에 기록**

messages_test.go에 추가(metrics 주입 → 요청 후 카운터 증가):
```go
import "github.com/inferplane/inferplane/internal/metrics"

func TestMessagesRecordsRequestMetric(t *testing.T) {
	m := metrics.New()
	h := NewMessagesHandlerFull(testRouter(), nil, nil)
	h.metrics = m // or a constructor variant; see impl note
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`))
	ctx := principal.With(req.Context(), keystore.Principal{KeyID: "ik", Team: "t", AllowedModels: []string{"*"}})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req.WithContext(ctx))
	out := metricsText(t, m)
	if !strings.Contains(out, `inferplane_requests_total`) {
		t.Fatalf("request metric not recorded:\n%s", out)
	}
}
```
(metricsText helper: gather m.Registry() and render via expfmt, or reuse testutil. The impl note: add a `metrics *metrics.Metrics` field to the handler with a setter or a `NewMessagesHandlerWithMetrics` — keep nil-safe.)

- [ ] **Step 2-4**: 구현 + wiring
- messages.go / chat.go: handler에 `metrics *metrics.Metrics` 필드(nil-safe). ServeHTTP에서 `start := time.Now()`; 완료 시 `m.ObserveRequest(ingress, model, providerName, team, status, time.Since(start).Seconds(), ttft)` + token usage(`m.ObserveTokenUsage`). 스트리밍은 첫 청크 시각으로 ttft 계산. fallback 발생 시 `m.ObserveFallback(model, from, to, reason)`.
- governance.go: Settle 시 `m.AddBudgetSpend(team, model, "total", float64(costMicros)/1e6)`, pricing miss 시 `m.IncPricingMiss(provider, model)`, quota util은 PreCheck/Settle에서 `m.SetQuotaUtilization`. Governor에 `*metrics.Metrics` 주입(nil-safe).
- router.go: breaker state 변경 시 `m.SetCircuitState(provider, state)`. router에 metrics 주입 OR breaker가 콜백. 간단히 RecordResult/ResolveChain에서 상태 반영.
- audit: writer가 sink 실패/buffer util 시 `m.IncAuditFailure`/`SetAuditBufferUtilization`. (audit가 metrics import하면 단방향 OK — audit는 server/anthropicapi에 의존 안 함.) nil-safe.
- main.go: `m := metrics.New()` 생성 → DataMux/AdminMux/Governor/router에 전달. DataMux 시그니처에 metrics 추가.
- DataMux: `DataMux(r, store, aud, gov, m)` → 핸들러에 metrics 주입.

import cycle 주의: governance/router/audit가 internal/metrics를 import — metrics는 그 누구도 import 안 하므로(prometheus만) cycle 없음. 확인.

- [ ] **Step 5: 통과 + 커밋**

Run: `go test ./... -race`, `go vet`, `gofmt -l .`, `go build ./...` → clean.
```bash
git add internal/server/ internal/governance/ internal/router/ internal/audit/ cmd/inferplane/main.go
git commit -s -m "feat(metrics): wire request/token/fallback/circuit/budget metrics into pipeline"
```

---

### Task 5: 자체 TLS 리스너

**Files:** Create `internal/server/tls.go`, modify `internal/config/config.go`, `cmd/inferplane/main.go`, `internal/config/config_test.go`

- [ ] **Step 1: config TLS 실패 테스트**

config_test.go에 추가:
```go
func TestLoadServerTLS(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "c.json")
	os.WriteFile(f, []byte(`{"server":{"listen":":8080","admin_listen":":9090","tls":{"cert_file":"/etc/tls/cert.pem","key_file":"/etc/tls/key.pem"}}}`), 0o600)
	cfg, err := Load(f)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.TLS.CertFile != "/etc/tls/cert.pem" || cfg.Server.TLS.KeyFile != "/etc/tls/key.pem" {
		t.Fatalf("tls: %+v", cfg.Server.TLS)
	}
}
```

- [ ] **Step 2-4**: config.go에 `TLSConfig{CertFile,KeyFile string}` + `ServerConfig.TLS TLSConfig`. tls.go에 헬퍼 `func (s *http.Server) serve(tls TLSConfig)`는 과하니, main.go에서 분기: tls.CertFile!="" → `dataSrv.ListenAndServeTLS(cert,key)` else `ListenAndServe()`. (admin 평면은 평문 유지 — 메트릭/헬스는 보통 클러스터 내부.) tls.go에 작은 검증 헬퍼만:
```go
package server

import "errors"

// ValidateTLS checks the TLS file pair is fully specified (both or neither).
func ValidateTLS(certFile, keyFile string) error {
	if (certFile == "") != (keyFile == "") {
		return errors.New("server.tls: cert_file and key_file must both be set or both empty")
	}
	return nil
}
```
main.go: ValidateTLS 호출 후 data 리스너를 TLS로. config_test 통과.
- [ ] **Step 5: 커밋**
```bash
git add internal/config/config.go internal/config/config_test.go internal/server/tls.go cmd/inferplane/main.go
git commit -s -m "feat(server): optional self-TLS listener for the data plane"
```

---

### Task 6: Dockerfile + .dockerignore

**Files:** Create `Dockerfile`, `.dockerignore`

- [ ] **Step 1: Dockerfile (멀티스테이지 → static)**
```dockerfile
# Build a static single binary, run on a minimal distroless base.
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# CGO disabled → pure-Go static binary (modernc sqlite, aws-sdk, prometheus all pure-Go)
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/inferplane ./cmd/inferplane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/inferplane /usr/local/bin/inferplane
USER nonroot:nonroot
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/inferplane"]
CMD ["serve", "--config", "/etc/inferplane/config.json"]
```
`.dockerignore`:
```
.git
docs
*.md
charts
deploy
*_test.go
```
- [ ] **Step 2: 빌드 검증 (docker 가용 시; 아니면 로컬 static build로 대체)**
```bash
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /tmp/ip-static ./cmd/inferplane && file /tmp/ip-static && /tmp/ip-static 2>&1 | head -1; rm -f /tmp/ip-static
```
Expected: 빌드 성공, usage 출력. (docker가 있으면 `docker build -t inferplane:dev .`도 시도; 없으면 static build로 ENTRYPOINT 검증을 대체하고 노트.)
- [ ] **Step 3: 커밋**
```bash
git add Dockerfile .dockerignore
git commit -s -m "build: multi-stage Dockerfile (CGO-disabled static binary, distroless)"
```

---

### Task 7: Helm chart

**Files:** Create `charts/inferplane/` (Chart.yaml, values.yaml, templates/*)

- [ ] **Step 1: Chart.yaml + values.yaml**

`charts/inferplane/Chart.yaml`:
```yaml
apiVersion: v2
name: inferplane
description: LLM consumption governance gateway
type: application
version: 0.1.0
appVersion: "0.1.0"
```
`charts/inferplane/values.yaml`:
```yaml
image:
  repository: inferplane
  tag: "0.1.0"
  pullPolicy: IfNotPresent
replicaCount: 1   # single replica: SQLite key store + instance-local governance (multi-replica HA = Postgres, v0.2)
service:
  dataPort: 8080
  adminPort: 9090
serviceAccount:
  create: true
  name: ""
  annotations: {}   # e.g. eks.amazonaws.com/role-arn for Bedrock IRSA
# Secrets are referenced, never created by this chart (§7). Provide via existingSecret.
secrets:
  existingSecret: inferplane-secrets   # must contain ANTHROPIC_API_KEY, INFERPLANE_ADMIN_TOKEN, ...
# config.json rendered into a ConfigMap. Secrets use api_key_ref env: from existingSecret.
config:
  server: { listen: ":8080", admin_listen: ":9090" }
  providers:
    anthropic-direct:
      type: anthropic
      base_url: https://api.anthropic.com
      api_key_ref: { env: ANTHROPIC_API_KEY }
  models:
    claude-sonnet-4-6:
      targets: [ { provider: anthropic-direct, model: claude-sonnet-4-6 } ]
resources: {}
```

- [ ] **Step 2: templates**

`templates/_helpers.tpl` (name helpers), `templates/configmap.yaml` (config.json from `.Values.config | toJson`), `templates/serviceaccount.yaml` (create + annotations for IRSA), `templates/service.yaml` (two ports), `templates/deployment.yaml` (mounts ConfigMap at /etc/inferplane, envFrom existingSecret, probes on :9090 /healthz /readyz, ports 8080/9090, serviceAccountName). Render config via:
```yaml
# configmap.yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: {{ include "inferplane.fullname" . }}-config
data:
  config.json: |
{{ .Values.config | toJson | indent 4 }}
```
deployment.yaml key parts:
```yaml
spec:
  replicas: {{ .Values.replicaCount }}
  template:
    spec:
      serviceAccountName: {{ include "inferplane.serviceAccountName" . }}
      containers:
        - name: inferplane
          image: "{{ .Values.image.repository }}:{{ .Values.image.tag }}"
          args: ["serve","--config","/etc/inferplane/config.json"]
          envFrom:
            - secretRef: { name: {{ .Values.secrets.existingSecret }} }
          ports:
            - { name: data, containerPort: {{ .Values.service.dataPort }} }
            - { name: admin, containerPort: {{ .Values.service.adminPort }} }
          readinessProbe: { httpGet: { path: /readyz, port: admin } }
          livenessProbe:  { httpGet: { path: /healthz, port: admin } }
          volumeMounts:
            - { name: config, mountPath: /etc/inferplane }
      volumes:
        - name: config
          configMap: { name: {{ include "inferplane.fullname" . }}-config }
```

- [ ] **Step 3: 검증 (helm 가용 시)**
```bash
helm lint charts/inferplane && helm template charts/inferplane | head -40
```
helm 미설치 시: YAML 문법만 `go run` 없이 육안/`python -c "import yaml,glob; [yaml.safe_load_all(open(f)) for f in glob.glob('charts/inferplane/**/*.yaml',recursive=True)]"`로 파싱 검증(템플릿 `{{}}`는 순수 YAML 파서로 안 되니, helm 없으면 helm template 생략하고 구조만 확인 + 노트).
- [ ] **Step 4: 커밋**
```bash
git add charts/inferplane/
git commit -s -m "feat(helm): chart (ConfigMap config, IRSA ServiceAccount, existingSecret refs)"
```

---

### Task 8: Grafana 대시보드 + README quickstart

**Files:** Create `deploy/grafana/inferplane.json`, modify `README.md`

- [ ] **Step 1: Grafana 대시보드 JSON**

`deploy/grafana/inferplane.json` — 유효한 Grafana 대시보드 JSON(schemaVersion, panels). 패널: requests rate(`rate(inferplane_requests_total[5m])` by status), token usage(`rate(gen_ai_client_token_usage_total[5m])` by type/model), request duration p95(`histogram_quantile(0.95, rate(gen_ai_server_request_duration_seconds_bucket[5m]))`), TTFT p95, fallback rate, circuit state(`inferplane_circuit_state`), budget spend(`inferplane_budget_spend_usd_total`), audit failures, pricing miss. JSON은 `python -c "import json;json.load(open('deploy/grafana/inferplane.json'))"`로 유효성 검증.

- [ ] **Step 2: README quickstart**

`README.md`를 quickstart 중심으로 갱신: 5분 데모 —
```markdown
## Quickstart (5 minutes)

1. Build / run:
   docker run -d --name inferplane \
     -e ANTHROPIC_API_KEY=sk-ant-... -e INFERPLANE_ADMIN_TOKEN=admin-secret \
     -v $PWD/config.json:/etc/inferplane/config.json \
     -v inferplane-data:/var/lib/inferplane \
     -p 8080:8080 -p 9090:9090 inferplane:0.1.0
2. Issue a virtual key (local bootstrap):
   docker exec inferplane inferplane keys create --team demo --models '*' --store /var/lib/inferplane/keys.db
   # → ik_...
3. Point Claude Code at it:
   export ANTHROPIC_BASE_URL=http://localhost:8080
   export ANTHROPIC_API_KEY=ik_...
   claude
   # or OpenCode (OpenAI): set the base URL + the ik_ key
4. Verify audit + metrics:
   docker exec inferplane inferplane audit verify --file /var/lib/inferplane/audit.jsonl
   curl -s localhost:9090/metrics | grep inferplane_requests_total
```
+ config.json 예시, governance(team quota/budget) 예시, provider 3종 설명, 프로젝트 상태(v0.1 pre-release), 설계 문서 링크.

- [ ] **Step 3: 검증 + 커밋**

Run: `python3 -c "import json;json.load(open('deploy/grafana/inferplane.json'));print('grafana json ok')"`.
```bash
git add deploy/grafana/inferplane.json README.md
git commit -s -m "docs: Grafana dashboard + 5-minute quickstart README"
```

---

### Task 9: M6 게이트 — docker run 5분 시연 + /metrics (수동)

- [ ] **Step 1**: `docker build -t inferplane:0.1.0 .` (또는 static binary).
- [ ] **Step 2**: README quickstart 그대로 실행 — config.json + ANTHROPIC_API_KEY + INFERPLANE_ADMIN_TOKEN → `docker run` → `keys create` → Claude Code 연결.
- [ ] **Step 3: 게이트 체크리스트** (v0.1 성공 기준):
  - [ ] docker run 한 줄로 기동, /healthz·/readyz 200.
  - [ ] keys create → ik_ 키 발급, Claude Code가 ANTHROPIC_BASE_URL로 5분 내 대화.
  - [ ] (OpenAI) OpenCode가 openai_compatible 경유 연결.
  - [ ] `curl :9090/metrics` → inferplane_requests_total, gen_ai_client_token_usage_total 등 노출, 요청 후 증가.
  - [ ] audit verify → chain OK. quota 초과 시 429.
  - [ ] (선택) helm install → 단일 레플리카 기동, IRSA로 Bedrock.
- [ ] **Step 4: 통과 기록** — 전부 통과 시 M6 완료 = **v0.1 코드 완성**.

---

## Self-Review 결과

- **스펙 커버리지**: §6.2 메트릭(GenAI 네이밍, 무인증 /metrics, 카디널리티 가드, budget 근사치) → Task 2/3/4. §2.3 TLS → Task 5. §9 운영/배포 → Task 6(Docker)/7(Helm)/8(Grafana+README). 게이트(5분 데모+/metrics) → Task 9. ✓
- **플레이스홀더**: Task 4 wiring은 다파일 통합이라 일부 "nil-safe 주입/setter" 위임(메트릭 호출 지점은 명시). Task 7 Helm templates는 핵심만 코드로, _helpers.tpl 표준 보일러플레이트는 구현자 작성(Helm 관용). Task 8 Grafana JSON은 PromQL 쿼리 명시 + 구현자가 유효 JSON 조립. 메트릭 코드(Task 2)는 전부 명시. ✓
- **타입 일관성**: metrics.New()/Metrics/ObserveTokenUsage/ObserveRequest/ObserveFallback/SetCircuitState/SetQuotaUtilization/AddBudgetSpend/IncPricingMiss/IncAuditFailure/SetAuditBufferUtilization/Registry()(Task 2) → Task 3/4 사용처 일치. AdminMux(store,tokens,m)/DataMux(...,m)(Task 3/4) → main.go. config TLSConfig{CertFile,KeyFile}(Task 5) → main.go. ✓
- **알려진 한계 (의도)**: budget_spend·quota_util 메트릭은 관측 근사치(정산 진실원은 µUSD 집계). 메트릭은 인스턴스-로컬(다중 레플리카는 Prometheus가 인스턴스별 수집). audit atomic 카운터(M3/M5)와 Prometheus 카운터 이중(단순/안전). admin 평면 평문(메트릭/헬스 클러스터 내부 전제); 데이터 평면만 자체 TLS. Helm은 단일 레플리카 기본(SQLite). OTel trace는 v0.2(메트릭만 GenAI 네이밍).
