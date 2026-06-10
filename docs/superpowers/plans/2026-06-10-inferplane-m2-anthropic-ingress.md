# inferplane M2 — Anthropic ingress ↔ anthropic provider 직통 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code가 `ANTHROPIC_BASE_URL`로 inferplane에 붙어 Anthropic API Direct로 대화·툴콜·스트리밍하고, 프롬프트 캐시 hit율이 직결과 동일하게 유지되는 최소 게이트웨이를 만든다 (스펙: `docs/specs/2026-06-10-inferplane-gateway-design.md` r4, §3.1·§4.1·§2.3·§2.4).

**Architecture:** ingress(`/v1/messages`)는 요청을 canonical로 파싱(라우팅·관찰용)하되 **원본 바이트를 그대로 upstream에 전달**해 캐시 prefix를 불변으로 보존한다(§4.4). 응답 스트리밍은 **tee** — upstream SSE 원본 바이트를 클라이언트로 흘려보내면서 동시에 각 이벤트를 `ChatChunk`로 파싱해 usage를 관찰한다(M3 감사·M5 거버넌스가 끼어들 자리). Provider는 `iter.Seq2` 이터레이터를 반환하고, 새 provider는 `providers/<name>/` 한 패키지 + `registry.go` 한 줄로 끝난다(§8).

**Tech Stack:** Go 1.23+ 표준 `net/http` + `ServeMux`(프레임워크 없음), 표준 `encoding/json`, `iter.Seq2`. 외부 의존성 0. 테스트는 `net/http/httptest` 가짜 upstream + mockprovider(실 API 키 불필요). M1 `pkg/schema` 위에 구축.

---

## M2 인터페이스 결정 (승인됨 + 1건 정련)

승인된 것: Provider 인터페이스(Complete/Stream/TokenCounter), `ProxyRequest`(원본 바이트 전달로 캐시 불변식 보장), 스트리밍 tee, 임시 단일 키 인증.

**정련 1건 — `Stream`의 이터레이터 원소:** 설계 스케치는 `iter.Seq2[*schema.ChatChunk, error]`였으나, tee("원본 바이트를 클라이언트에 전달하며 ChatChunk로 관찰")를 단일 `iter.Seq2`로 구현하려면 각 원소가 **원본 SSE 바이트 + 파싱된 청크**를 함께 운반해야 한다. 따라서 원소 타입을 `*providers.StreamEvent{Raw []byte, Chunk *schema.ChatChunk}`로 정련한다. `iter.Seq2` 기반이라는 승인 조건은 유지된다. (canonical 스키마 `pkg/schema`는 오염시키지 않음 — `StreamEvent`는 `providers` 패키지의 전송 래퍼.)

---

## 파일 구조

```
pkg/schema/
  model_info.go                # ModelInfo — /v1/models 응답 단위 (공개 API)
  model_info_test.go
providers/
  provider.go                  # Provider, TokenCounter 인터페이스 + ProxyRequest/ProxyResponse/StreamEvent
  registry.go                  # Register(name, factory) / New(name, cfg)  ★ 새 provider PR이 건드리는 1줄
  registry_test.go
  anthropic/
    anthropic.go               # Provider 구현: Complete/Stream/CountTokens/Models
    anthropic_test.go          # httptest 가짜 upstream 통합 테스트
    sseread.go                 # upstream SSE 응답 → StreamEvent 이터레이터 (byte-exact)
    sseread_test.go
  testing/mockprovider/
    mockprovider.go            # 결정적 mock (ingress E2E 테스트용)
internal/
  config/
    config.go                  # M2 부분집합 로드: server, providers, models
    config_test.go
  router/
    router.go                  # 모델명 → provider 해석 (M2: 단일 타깃, 폴백 M5)
    router_test.go
  server/
    server.go                  # ServeMux 조립, :8080 data + :9090 admin 리스너
    auth.go                    # 임시 단일 키 미들웨어 (constant-time)
    auth_test.go
    anthropicapi/
      messages.go              # POST /v1/messages (비스트리밍 + SSE tee)
      messages_test.go
      count_tokens.go          # POST /v1/messages/count_tokens (절대 5xx 금지)
      count_tokens_test.go
      models.go                # GET /v1/models (config 모델 목록)
      models_test.go
      sse_write.go             # ChatChunk → Anthropic SSE 직렬화 (M5용 + 골든 검증)
      sse_write_test.go
cmd/inferplane/
  main.go                      # serve 서브커맨드 (keys/audit는 M3)
testdata/
  m2-config.yaml               # 통합 테스트용 샘플 config
```

원칙: `providers/anthropic/`는 코어 밖. 새 provider PR = `providers/<name>/` 추가 + `registry.go`에 blank import 1줄(M4 bedrock에서 실증). `pkg/schema`만 공개 API 약속.

---

### Task 1: pkg/schema.ModelInfo

**Files:**
- Create: `pkg/schema/model_info.go`
- Test: `pkg/schema/model_info_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/model_info_test.go`:
```go
package schema

import (
	"encoding/json"
	"testing"
)

func TestModelInfoRoundTrip(t *testing.T) {
	// Anthropic /v1/models 의 data 원소 형태.
	in := `{"type":"model","id":"claude-sonnet-4-6","display_name":"Claude Sonnet 4.6","created_at":"2026-02-19T00:00:00Z"}`
	var m ModelInfo
	if err := json.Unmarshal([]byte(in), &m); err != nil {
		t.Fatal(err)
	}
	if m.ID != "claude-sonnet-4-6" || m.DisplayName != "Claude Sonnet 4.6" || m.Type != "model" {
		t.Fatalf("typed fields: %+v", m)
	}
	out, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestModelInfo -v`
Expected: FAIL — `undefined: ModelInfo`

- [ ] **Step 3: 구현**

`pkg/schema/model_info.go`:
```go
package schema

// ModelInfo is one entry in the Anthropic GET /v1/models response.
// Type is always "model" on the wire; CreatedAt is RFC3339.
type ModelInfo struct {
	Type        string `json:"type"`
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at,omitempty"`
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestModelInfo -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/model_info.go pkg/schema/model_info_test.go
git commit -s -m "feat(schema): ModelInfo for /v1/models entries"
```

---

### Task 2: providers 코어 타입 + 레지스트리

**Files:**
- Create: `providers/provider.go`, `providers/registry.go`
- Test: `providers/registry_test.go`

- [ ] **Step 1: 인터페이스/타입 정의 (provider.go)**

`providers/provider.go`:
```go
// Package providers defines the Provider interface and the transport types
// the gateway uses to proxy a request to an upstream LLM API. The canonical
// schema (pkg/schema) is kept pure; ProxyRequest/ProxyResponse/StreamEvent
// are transport wrappers that also carry the ORIGINAL upstream bytes, so the
// gateway can forward them verbatim and preserve the prompt-cache prefix
// (design doc §4.4) while still observing parsed content for governance.
package providers

import (
	"context"
	"iter"
	"net/http"

	"github.com/inferplane/inferplane/pkg/schema"
)

// ProxyRequest is one inbound request resolved to a target. RawBody is what
// gets sent upstream UNMODIFIED — the gateway parses Parsed only to route and
// observe, never to re-serialize the request (cache invariant, §4.4).
type ProxyRequest struct {
	Model    string              // resolved model name (routing/observation)
	Parsed   *schema.ChatRequest // parsed for inspection; do NOT re-serialize for upstream
	RawBody  []byte              // original request bytes → forwarded verbatim
	Headers  http.Header         // anthropic-version / anthropic-beta passthrough
	Stream   bool                // req.stream
	Upstream string              // target model id at the upstream (may differ from Model)
}

// ProxyResponse is a non-streaming upstream response. RawBody is teed to the
// client verbatim; Parsed is for observation (usage → audit/quota in M3/M5).
type ProxyResponse struct {
	StatusCode int
	Headers    http.Header
	RawBody    []byte
	Parsed     *schema.ChatResponse // nil if status != 2xx or body not parseable
}

// StreamEvent is one upstream SSE event. Raw is the exact event bytes
// (incl. "event:"/"data:" lines + blank-line terminator) teed to the client;
// Chunk is the parsed observation (nil for events with no JSON data payload,
// e.g. comment-only keepalives). This wrapper is why Stream yields
// *StreamEvent rather than *schema.ChatChunk: a single iter.Seq2 must carry
// BOTH the bytes to forward and the parsed view to observe.
type StreamEvent struct {
	Raw   []byte
	Chunk *schema.ChatChunk
}

// Provider proxies canonical requests to one upstream. New providers implement
// this in their own package; adding one touches providers/<name>/ + one line
// in registry.go and nothing in the core (design doc §8).
type Provider interface {
	Name() string
	Models() []schema.ModelInfo
	Complete(ctx context.Context, req *ProxyRequest) (*ProxyResponse, error)
	Stream(ctx context.Context, req *ProxyRequest) (iter.Seq2[*StreamEvent, error], error)
}

// TokenCounter is an optional capability. Providers that can count tokens
// upstream implement it; count_tokens falls back to an estimator otherwise
// (design doc §3.1, §10 #1).
type TokenCounter interface {
	CountTokens(ctx context.Context, req *ProxyRequest) (int64, error)
}

// Config is the per-provider settings slice the registry hands to a factory.
// Kept minimal for M2; providers read what they need.
type Config struct {
	Type     string            // "anthropic" | (M4) "bedrock" | (M5) "openai_compatible"
	BaseURL  string            // upstream base, e.g. https://api.anthropic.com
	APIKey   string            // resolved secret (never logged)
	Models   []schema.ModelInfo
	Settings map[string]string // provider-specific extras
}
```

- [ ] **Step 2: 레지스트리 실패 테스트 작성**

`providers/registry_test.go`:
```go
package providers

import "testing"

func TestRegistryRegisterAndNew(t *testing.T) {
	Register("fake-m2", func(cfg Config) (Provider, error) {
		return nil, nil // factory presence is what we assert here
	})
	if _, ok := factories["fake-m2"]; !ok {
		t.Fatal("factory not registered")
	}
	if _, err := New(Config{Type: "missing-xyz"}); err == nil {
		t.Fatal("expected error for unknown provider type")
	}
}
```

- [ ] **Step 3: 실패 확인**

Run: `go test ./providers/ -run TestRegistry -v`
Expected: FAIL — `undefined: Register`

- [ ] **Step 4: 레지스트리 구현 (registry.go)**

`providers/registry.go`:
```go
package providers

import "fmt"

// Factory builds a Provider from its config slice.
type Factory func(Config) (Provider, error)

// factories maps provider type → constructor. Providers register here via
// init(); the core never imports a concrete provider package directly except
// through blank imports collected in this file's package over time.
var factories = map[string]Factory{}

// Register adds a provider factory. Called from a provider package's init().
func Register(typ string, f Factory) {
	if _, dup := factories[typ]; dup {
		panic("providers: duplicate registration for type " + typ)
	}
	factories[typ] = f
}

// New constructs a provider for cfg.Type.
func New(cfg Config) (Provider, error) {
	f, ok := factories[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("providers: unknown provider type %q", cfg.Type)
	}
	return f(cfg)
}
```

- [ ] **Step 5: 통과 확인 + 커밋**

Run: `go test ./providers/ -v` → PASS, `gofmt -l .` empty, `go vet ./...` clean.
```bash
git add providers/provider.go providers/registry.go providers/registry_test.go
git commit -s -m "feat(providers): Provider interface, transport types, and registry"
```

---

### Task 3: mockprovider (결정적 mock)

**Files:**
- Create: `providers/testing/mockprovider/mockprovider.go`
- Test: `providers/testing/mockprovider/mockprovider_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`providers/testing/mockprovider/mockprovider_test.go`:
```go
package mockprovider

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestMockComplete(t *testing.T) {
	m := New("claude-sonnet-4-6")
	resp, err := m.Complete(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6"})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Parsed == nil || resp.Parsed.Model != "claude-sonnet-4-6" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

func TestMockStreamEmitsUsage(t *testing.T) {
	m := New("claude-sonnet-4-6")
	seq, err := m.Stream(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6", Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sawUsage bool
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Chunk != nil && ev.Chunk.Usage != nil {
			sawUsage = true
		}
	}
	if !sawUsage {
		t.Fatal("expected a chunk carrying usage")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./providers/testing/mockprovider/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: 구현**

`providers/testing/mockprovider/mockprovider.go`:
```go
// Package mockprovider is a deterministic in-memory Provider for tests.
// It needs no network and emits a fixed Anthropic-shaped message + stream.
package mockprovider

import (
	"context"
	"encoding/json"
	"iter"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type mock struct{ model string }

// New returns a mock provider serving exactly one model id.
func New(model string) providers.Provider { return &mock{model: model} }

func (m *mock) Name() string { return "mock" }

func (m *mock) Models() []schema.ModelInfo {
	return []schema.ModelInfo{{Type: "model", ID: m.model, DisplayName: m.model}}
}

func ptrStr(s string) *string { return &s }
func ptrI64(i int64) *int64    { return &i }

func (m *mock) Complete(_ context.Context, _ *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	in, out := int64(10), int64(5)
	resp := &schema.ChatResponse{
		ID: "msg_mock", Type: "message", Role: "assistant", Model: m.model,
		Content:    []schema.ContentBlock{{Type: "text", Text: ptrStr("ok")}},
		StopReason: ptrStr("end_turn"),
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	raw, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: raw, Parsed: resp}, nil
}

func (m *mock) Stream(_ context.Context, _ *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	in, out := int64(10), int64(5)
	events := []*providers.StreamEvent{
		{Raw: []byte("event: message_start\ndata: {\"type\":\"message_start\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_start"}},
		{Raw: []byte("event: message_delta\ndata: {\"type\":\"message_delta\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}},
		{Raw: []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			Chunk: &schema.ChatChunk{Type: "message_stop"}},
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/testing/mockprovider/ -v` → PASS.
```bash
git add providers/testing/mockprovider/
git commit -s -m "test(providers): deterministic mockprovider for ingress tests"
```

---

### Task 4: config 로더 (M2 부분집합)

**Files:**
- Create: `internal/config/config.go`, `testdata/m2-config.yaml`
- Test: `internal/config/config_test.go`

설계 §7 config은 YAML이지만 M2는 외부 의존성 0 원칙을 지키기 위해 **표준 라이브러리만으로 파싱 가능한 최소 부분집합**을 다룬다. YAML 파서를 도입하지 않고, M2 config는 표준 `encoding/json`으로 읽는 **JSON 파일**로 시작한다(설계의 YAML 표면은 M6 Helm에서 yaml.v3 도입 시 통일). secret은 `ref` 방식만 — 평문 금지(§7).

- [ ] **Step 1: 실패 테스트 + 픽스처**

`testdata/m2-config.yaml` (실제로는 JSON 내용 — M6에서 YAML 전환):
```json
{
  "server": { "listen": ":8080", "admin_listen": ":9090", "drain_grace": "10s" },
  "providers": {
    "anthropic-direct": {
      "type": "anthropic",
      "base_url": "https://api.anthropic.com",
      "api_key_ref": { "env": "ANTHROPIC_API_KEY" }
    }
  },
  "models": {
    "claude-sonnet-4-6": { "targets": [ { "provider": "anthropic-direct", "model": "claude-sonnet-4-6" } ] }
  }
}
```

`internal/config/config_test.go`:
```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadResolvesSecretRef(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-test-xyz")
	cfg, err := Load(filepath.Join("..", "..", "testdata", "m2-config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	p, ok := cfg.Providers["anthropic-direct"]
	if !ok {
		t.Fatal("provider missing")
	}
	if p.APIKey != "sk-test-xyz" {
		t.Fatalf("secret ref not resolved: %q", p.APIKey)
	}
	if cfg.Models["claude-sonnet-4-6"].Targets[0].Provider != "anthropic-direct" {
		t.Fatalf("model mapping wrong: %+v", cfg.Models)
	}
}

func TestLoadRejectsInlineSecret(t *testing.T) {
	dir := t.TempDir()
	f := filepath.Join(dir, "bad.json")
	os.WriteFile(f, []byte(`{"providers":{"x":{"type":"anthropic","api_key":"sk-plaintext"}}}`), 0o600)
	if _, err := Load(f); err == nil {
		t.Fatal("expected rejection of inline api_key")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/config/ -v`
Expected: FAIL — `undefined: Load`

- [ ] **Step 3: 구현**

`internal/config/config.go`:
```go
// Package config loads inferplane's M2 configuration subset. Secrets are only
// referenced (env/file/secret), never inline — an inline api_key is rejected
// at load (design doc §7).
package config

import (
	"encoding/json"
	"fmt"
	"os"
)

type SecretRef struct {
	Env  string `json:"env,omitempty"`
	File string `json:"file,omitempty"`
}

type ProviderConfig struct {
	Type      string     `json:"type"`
	BaseURL   string     `json:"base_url"`
	APIKeyRef *SecretRef `json:"api_key_ref,omitempty"`
	// APIKey is the RESOLVED secret, filled at load. Tagged "-" so a config
	// file can never set it inline (defense-in-depth alongside the scan below).
	APIKey string `json:"-"`
}

type Target struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type ModelConfig struct {
	Targets []Target `json:"targets"`
}

type ServerConfig struct {
	Listen      string `json:"listen"`
	AdminListen string `json:"admin_listen"`
	DrainGrace  string `json:"drain_grace"`
}

type Config struct {
	Server    ServerConfig              `json:"server"`
	Providers map[string]ProviderConfig `json:"providers"`
	Models    map[string]ModelConfig    `json:"models"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Reject inline secrets before structured parse: any provider object with
	// a literal "api_key" key is a config error (§7).
	var probe struct {
		Providers map[string]map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for name, p := range probe.Providers {
		if _, bad := p["api_key"]; bad {
			return nil, fmt.Errorf("config: provider %q has inline api_key; use api_key_ref (§7)", name)
		}
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	for name, p := range cfg.Providers {
		secret, err := resolveSecret(p.APIKeyRef)
		if err != nil {
			return nil, fmt.Errorf("config: provider %q secret: %w", name, err)
		}
		p.APIKey = secret
		cfg.Providers[name] = p
	}
	return &cfg, nil
}

func resolveSecret(ref *SecretRef) (string, error) {
	if ref == nil {
		return "", nil
	}
	switch {
	case ref.Env != "":
		v := os.Getenv(ref.Env)
		if v == "" {
			return "", fmt.Errorf("env %s is empty", ref.Env)
		}
		return v, nil
	case ref.File != "":
		b, err := os.ReadFile(ref.File)
		if err != nil {
			return "", err
		}
		return string(b), nil
	default:
		return "", fmt.Errorf("empty secret ref")
	}
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/config/ -v` → PASS.
```bash
git add internal/config/ testdata/m2-config.yaml
git commit -s -m "feat(config): M2 config loader with secret-ref resolution (no inline secrets)"
```

---

### Task 5: router (모델명 → provider)

**Files:**
- Create: `internal/router/router.go`
- Test: `internal/router/router_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/router/router_test.go`:
```go
package router

import (
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
	"github.com/inferplane/inferplane/providers"
)

func TestResolveModel(t *testing.T) {
	provs := map[string]providers.Provider{"anthropic-direct": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "anthropic-direct", Model: "claude-sonnet-4-6"}}},
	}
	r := New(provs, models)
	p, upstream, err := r.Resolve("claude-sonnet-4-6")
	if err != nil {
		t.Fatal(err)
	}
	if p.Name() != "mock" || upstream != "claude-sonnet-4-6" {
		t.Fatalf("resolve wrong: %s %s", p.Name(), upstream)
	}
	if _, _, err := r.Resolve("unknown-model"); err == nil {
		t.Fatal("expected error for unknown model")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/router/ -v`
Expected: FAIL — `undefined: New`

- [ ] **Step 3: 구현**

`internal/router/router.go`:
```go
// Package router resolves a requested model name to a provider + upstream
// model id. M2 uses the first configured target only; priority fallback and
// circuit breaking arrive in M5 (design doc §4.5).
package router

import (
	"fmt"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/providers"
)

type Router struct {
	provs  map[string]providers.Provider
	models map[string]config.ModelConfig
}

func New(provs map[string]providers.Provider, models map[string]config.ModelConfig) *Router {
	return &Router{provs: provs, models: models}
}

// Resolve returns the provider and upstream model id for a requested model.
func (r *Router) Resolve(model string) (providers.Provider, string, error) {
	mc, ok := r.models[model]
	if !ok || len(mc.Targets) == 0 {
		return nil, "", fmt.Errorf("router: no route for model %q", model)
	}
	t := mc.Targets[0] // M2: first target only
	p, ok := r.provs[t.Provider]
	if !ok {
		return nil, "", fmt.Errorf("router: model %q points at unknown provider %q", model, t.Provider)
	}
	return p, t.Model, nil
}

// AllModels returns every configured model name (for /v1/models in M2; M3
// filters by the virtual key's allow-list).
func (r *Router) AllModels() []string {
	out := make([]string, 0, len(r.models))
	for name := range r.models {
		out = append(out, name)
	}
	return out
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/router/ -v` → PASS.
```bash
git add internal/router/
git commit -s -m "feat(router): model-to-provider resolution (single target, M2)"
```

---

### Task 6: 임시 단일 키 인증 미들웨어

**Files:**
- Create: `internal/server/auth.go`
- Test: `internal/server/auth_test.go`

- [ ] **Step 1: 실패 테스트 작성**

`internal/server/auth_test.go`:
```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestDevKeyAuth(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) })
	h := DevKeyAuth("secret-key", next)

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"valid x-api-key", "x-api-key", "secret-key", 200},
		{"valid bearer", "Authorization", "Bearer secret-key", 200},
		{"wrong key", "x-api-key", "nope", 401},
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
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/server/ -run TestDevKeyAuth -v`
Expected: FAIL — `undefined: DevKeyAuth`

- [ ] **Step 3: 구현**

`internal/server/auth.go`:
```go
package server

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// DevKeyAuth is the TEMPORARY single-key gate for M2. It compares the client's
// x-api-key or Authorization: Bearer against one configured key in constant
// time. Replaced by virtual-key auth + key store in M3. The upstream provider
// key is never exposed to the client (design doc §5.2 — established from M2).
func DevKeyAuth(devKey string, next http.Handler) http.Handler {
	want := []byte(devKey)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := r.Header.Get("x-api-key")
		if got == "" {
			got = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		}
		if subtle.ConstantTimeCompare([]byte(got), want) != 1 {
			writeAnthropicError(w, http.StatusUnauthorized, "authentication_error", "invalid API key")
			return
		}
		next.ServeHTTP(w, r)
	})
}
```

> 참고: `writeAnthropicError`는 Task 10에서 정의된다. Task 6 단독 테스트는 `auth_test.go`만 컴파일하므로 이 단계에서 `server` 패키지에 임시 stub이 필요하면 Task 10 전까지는 다음 helper를 `auth.go`에 함께 둔다(Task 10에서 `errors.go`로 이동):

`internal/server/auth.go`에 helper 추가 (같은 커밋):
```go
import "encoding/json"

// writeAnthropicError emits an Anthropic-shaped error body. Anthropic clients
// (Claude Code) expect {"type":"error","error":{"type","message"}}.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/server/ -run TestDevKeyAuth -v` → PASS.
```bash
git add internal/server/auth.go internal/server/auth_test.go
git commit -s -m "feat(server): temporary single-key auth middleware (constant-time)"
```

---

### Task 7: anthropic provider — Complete (비스트리밍)

**Files:**
- Create: `providers/anthropic/anthropic.go`
- Test: `providers/anthropic/anthropic_test.go`

- [ ] **Step 1: 실패 테스트 작성 (httptest 가짜 upstream)**

`providers/anthropic/anthropic_test.go`:
```go
package anthropic

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCompleteForwardsRawBodyAndParsesUsage(t *testing.T) {
	var gotBody []byte
	var gotKey, gotVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":12,"output_tokens":3}}`)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"messages":[{"role":"user","content":"hi"}]}`)
	hdr := http.Header{"Anthropic-Version": {"2023-06-01"}}
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{
		Model: "claude-sonnet-4-6", Upstream: "claude-sonnet-4-6", RawBody: raw, Headers: hdr,
	})
	if err != nil {
		t.Fatal(err)
	}
	// Cache invariant: upstream receives the EXACT request bytes.
	if string(gotBody) != string(raw) {
		t.Fatalf("upstream body mutated:\n got: %s\nwant: %s", gotBody, raw)
	}
	// Gateway's own key, not the client's.
	if gotKey != "sk-up" {
		t.Fatalf("upstream key = %q, want gateway key", gotKey)
	}
	if gotVersion != "2023-06-01" {
		t.Fatalf("anthropic-version not forwarded: %q", gotVersion)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 12 {
		t.Fatalf("usage not parsed: %+v", resp.Parsed)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./providers/anthropic/ -run TestComplete -v`
Expected: FAIL — `undefined: factory`

- [ ] **Step 3: 구현 (anthropic.go — Complete + factory + 등록)**

`providers/anthropic/anthropic.go`:
```go
// Package anthropic proxies to the Anthropic Messages API (api.anthropic.com).
// It forwards the request body verbatim (cache invariant §4.4), injects the
// gateway's own credential (§5.2), and parses responses into canonical types
// for observation. Registered as provider type "anthropic".
package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"time"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func init() { providers.Register("anthropic", factory) }

type provider struct {
	baseURL string
	apiKey  string
	client  *http.Client
}

func factory(cfg providers.Config) (providers.Provider, error) {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &provider{
		baseURL: base,
		apiKey:  cfg.APIKey,
		client:  &http.Client{Timeout: 0}, // streaming: no whole-request timeout; ctx governs
	}, nil
}

func (p *provider) Name() string { return "anthropic" }

func (p *provider) Models() []schema.ModelInfo { return nil } // M2: models come from config

// buildUpstream constructs the upstream request, forwarding RawBody verbatim
// and copying passthrough headers, then injecting the gateway credential.
func (p *provider) buildUpstream(ctx context.Context, path string, req *providers.ProxyRequest) (*http.Request, error) {
	u, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+path, bytes.NewReader(req.RawBody))
	if err != nil {
		return nil, err
	}
	// Passthrough anthropic-version / anthropic-beta (design doc §3.1).
	for _, h := range []string{"Anthropic-Version", "Anthropic-Beta", "Content-Type"} {
		if v := req.Headers.Get(h); v != "" {
			u.Header.Set(h, v)
		}
	}
	if u.Header.Get("Content-Type") == "" {
		u.Header.Set("Content-Type", "application/json")
	}
	u.Header.Set("x-api-key", p.apiKey) // gateway's credential, never the client's
	return u, nil
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	u, err := p.buildUpstream(ctx, "/v1/messages", req)
	if err != nil {
		return nil, err
	}
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("anthropic: read upstream: %w", err)
	}
	out := &providers.ProxyResponse{StatusCode: resp.StatusCode, Headers: resp.Header, RawBody: body}
	if resp.StatusCode/100 == 2 {
		var parsed schema.ChatResponse
		if json.Unmarshal(body, &parsed) == nil {
			out.Parsed = &parsed
		}
	}
	return out, nil
}

// Stream is implemented in Task 8; CountTokens in Task 9.
var _ = time.Second // placeholder removed when Stream lands
```

> `Stream`/`CountTokens`는 다음 태스크에서 추가하므로, Task 7 단계에서는 `provider`가 아직 `providers.Provider`를 완전히 만족하지 않는다. 테스트는 `Complete`만 호출하므로 컴파일을 위해 `factory`가 `providers.Provider`가 아닌 `*provider`를 반환하도록 **Task 7 한정** 시그니처를 `func factory(cfg providers.Config) (*provider, error)`로 두고, Task 8에서 `Stream` 추가 후 `(providers.Provider, error)`로 되돌린다. (테스트도 `p, _ := factory(...)`로 구상 타입을 받는다.)

수정: Task 7의 `factory` 시그니처와 테스트를 위 노트대로 `*provider` 반환으로 작성한다. Step 1 테스트의 `factory(...)`는 이미 구상 타입을 받으므로 그대로 통과.

- [ ] **Step 4: 통과 확인**

Run: `go test ./providers/anthropic/ -run TestComplete -v`
Expected: PASS (upstream이 원본 바이트 수신, usage 파싱 확인)

- [ ] **Step 5: 커밋**

```bash
git add providers/anthropic/anthropic.go providers/anthropic/anthropic_test.go
git commit -s -m "feat(anthropic): Complete proxies verbatim body with gateway credential"
```

---

### Task 8: anthropic provider — SSE upstream reader + Stream

**Files:**
- Create: `providers/anthropic/sseread.go`
- Test: `providers/anthropic/sseread_test.go`
- Modify: `providers/anthropic/anthropic.go` (Stream 추가, factory 반환 타입 복원)

- [ ] **Step 1: SSE reader 실패 테스트 작성**

`providers/anthropic/sseread_test.go`:
```go
package anthropic

import (
	"strings"
	"testing"
)

func TestReadSSEEventsByteExact(t *testing.T) {
	body := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

	var rawConcat strings.Builder
	var types []string
	var sawUsage bool
	for ev, err := range readSSE(strings.NewReader(body)) {
		if err != nil {
			t.Fatal(err)
		}
		rawConcat.Write(ev.Raw) // tee: raw bytes must reassemble to the original stream
		if ev.Chunk != nil {
			types = append(types, ev.Chunk.Type)
			if ev.Chunk.Usage != nil {
				sawUsage = true
			}
		}
	}
	if rawConcat.String() != body {
		t.Fatalf("raw passthrough not byte-exact:\n got: %q\nwant: %q", rawConcat.String(), body)
	}
	want := []string{"message_start", "ping", "message_delta", "message_stop"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("event types: %v", types)
	}
	if !sawUsage {
		t.Fatal("expected usage on message_delta")
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./providers/anthropic/ -run TestReadSSE -v`
Expected: FAIL — `undefined: readSSE`

- [ ] **Step 3: SSE reader 구현 (sseread.go)**

`providers/anthropic/sseread.go`:
```go
package anthropic

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

// readSSE parses an Anthropic SSE response into a sequence of StreamEvents.
// Raw is the exact bytes of each event block (all lines up to and including
// the blank-line terminator) so the ingress can tee them to the client
// verbatim; Chunk is the parsed "data:" JSON (nil if the block has no data
// line, e.g. a comment keepalive). Byte-exactness is the tee guarantee.
func readSSE(r io.Reader) iter.Seq2[*providers.StreamEvent, error] {
	return func(yield func(*providers.StreamEvent, error) bool) {
		br := bufio.NewReader(r)
		var block bytes.Buffer
		var dataLine []byte
		flush := func() bool {
			if block.Len() == 0 {
				return true
			}
			ev := &providers.StreamEvent{Raw: append([]byte(nil), block.Bytes()...)}
			if dataLine != nil {
				var c schema.ChatChunk
				if json.Unmarshal(dataLine, &c) == nil {
					ev.Chunk = &c
				}
			}
			block.Reset()
			dataLine = nil
			return yield(ev, nil)
		}
		for {
			line, err := br.ReadBytes('\n')
			if len(line) > 0 {
				block.Write(line)
				trimmed := strings.TrimRight(string(line), "\r\n")
				if trimmed == "" { // blank line ends an event
					if !flush() {
						return
					}
				} else if strings.HasPrefix(trimmed, "data:") {
					dataLine = []byte(strings.TrimSpace(strings.TrimPrefix(trimmed, "data:")))
				}
			}
			if err == io.EOF {
				flush() // emit any trailing event without a final blank line
				return
			}
			if err != nil {
				yield(nil, err)
				return
			}
		}
	}
}
```

- [ ] **Step 4: SSE reader 통과 확인**

Run: `go test ./providers/anthropic/ -run TestReadSSE -v`
Expected: PASS (raw 재조립 byte-exact + 이벤트 타입 + usage)

- [ ] **Step 5: Stream 추가 + factory 반환 타입 복원 (anthropic.go 수정)**

`providers/anthropic/anthropic.go` — `var _ = time.Second` placeholder 줄을 삭제하고 `time` import 제거, 다음 메서드를 추가:
```go
func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	u, err := p.buildUpstream(ctx, "/v1/messages", req)
	if err != nil {
		return nil, err
	}
	u.Header.Set("Accept", "text/event-stream")
	resp, err := p.client.Do(u)
	if err != nil {
		return nil, fmt.Errorf("anthropic: upstream stream: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		return nil, fmt.Errorf("anthropic: upstream status %d: %s", resp.StatusCode, body)
	}
	// Wrap readSSE so the body is closed when iteration ends.
	inner := readSSE(resp.Body)
	return func(yield func(*providers.StreamEvent, error) bool) {
		defer resp.Body.Close()
		for ev, err := range inner {
			if !yield(ev, err) {
				return
			}
		}
	}, nil
}
```

그리고 `factory` 반환 타입을 `(providers.Provider, error)`로 복원:
```go
func factory(cfg providers.Config) (providers.Provider, error) {
	base := cfg.BaseURL
	if base == "" {
		base = "https://api.anthropic.com"
	}
	return &provider{baseURL: base, apiKey: cfg.APIKey, client: &http.Client{}}, nil
}
```

Task 7 테스트의 `p, _ := factory(...)`는 이제 `providers.Provider` 인터페이스를 받는다 — `p.Complete(...)`는 인터페이스 메서드이므로 그대로 컴파일. (`Stream`이 추가되어 인터페이스를 만족.)

- [ ] **Step 6: Stream 통합 테스트 추가 (anthropic_test.go)**

`providers/anthropic/anthropic_test.go`에 추가:
```go
func TestStreamTeesRawAndObservesUsage(t *testing.T) {
	sse := "event: message_start\ndata: {\"type\":\"message_start\"}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"input_tokens\":4,\"output_tokens\":9}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, sse)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{
		Model: "claude-sonnet-4-6", RawBody: []byte(`{"stream":true}`), Headers: http.Header{}, Stream: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	var raw strings.Builder
	var lastOut int64
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		raw.Write(ev.Raw)
		if ev.Chunk != nil && ev.Chunk.Usage != nil && ev.Chunk.Usage.OutputTokens != nil {
			lastOut = *ev.Chunk.Usage.OutputTokens
		}
	}
	if raw.String() != sse {
		t.Fatalf("tee not byte-exact")
	}
	if lastOut != 9 {
		t.Fatalf("usage observation wrong: %d", lastOut)
	}
}
```
(파일 상단 import에 `"strings"` 추가.)

- [ ] **Step 7: 전체 통과 + 커밋**

Run: `go test ./providers/anthropic/ -v` → PASS, `go vet ./...` clean, `gofmt -l .` empty.
```bash
git add providers/anthropic/
git commit -s -m "feat(anthropic): streaming via byte-exact SSE tee with usage observation"
```

---

### Task 9: anthropic provider — CountTokens (TokenCounter)

**Files:**
- Modify: `providers/anthropic/anthropic.go` (CountTokens 추가)
- Test: `providers/anthropic/anthropic_test.go` (추가)

- [ ] **Step 1: 실패 테스트 작성**

`providers/anthropic/anthropic_test.go`에 추가:
```go
func TestCountTokensProxiesUpstream(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":1234}`)
	}))
	defer upstream.Close()

	p, _ := factory(providers.Config{Type: "anthropic", BaseURL: upstream.URL, APIKey: "sk-up"})
	tc, ok := p.(providers.TokenCounter)
	if !ok {
		t.Fatal("anthropic provider should implement TokenCounter")
	}
	n, err := tc.CountTokens(context.Background(), &providers.ProxyRequest{
		RawBody: []byte(`{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`),
		Headers: http.Header{"Anthropic-Version": {"2023-06-01"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 1234 {
		t.Fatalf("count = %d, want 1234", n)
	}
	if gotPath != "/v1/messages/count_tokens" {
		t.Fatalf("path = %q", gotPath)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./providers/anthropic/ -run TestCountTokens -v`
Expected: FAIL — `p.(providers.TokenCounter)` 단언 실패 (`CountTokens` 미구현)

- [ ] **Step 3: 구현 (anthropic.go에 CountTokens 추가)**

```go
func (p *provider) CountTokens(ctx context.Context, req *providers.ProxyRequest) (int64, error) {
	u, err := p.buildUpstream(ctx, "/v1/messages/count_tokens", req)
	if err != nil {
		return 0, err
	}
	resp, err := p.client.Do(u)
	if err != nil {
		return 0, fmt.Errorf("anthropic: count_tokens call: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return 0, err
	}
	if resp.StatusCode/100 != 2 {
		return 0, fmt.Errorf("anthropic: count_tokens status %d: %s", resp.StatusCode, body)
	}
	var out struct {
		InputTokens int64 `json:"input_tokens"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return 0, fmt.Errorf("anthropic: count_tokens parse: %w", err)
	}
	return out.InputTokens, nil
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/anthropic/ -v` → PASS (compile-time `var _ providers.TokenCounter = (*provider)(nil)` 추가로 인터페이스 만족 고정).

`anthropic.go` 하단에 추가:
```go
var (
	_ providers.Provider     = (*provider)(nil)
	_ providers.TokenCounter = (*provider)(nil)
)
```

```bash
git add providers/anthropic/
git commit -s -m "feat(anthropic): CountTokens proxies /v1/messages/count_tokens"
```

---

### Task 10: /v1/messages 핸들러 (비스트리밍 + SSE tee)

**Files:**
- Create: `internal/server/anthropicapi/messages.go`, `internal/server/errors.go`
- Test: `internal/server/anthropicapi/messages_test.go`
- Modify: `internal/server/auth.go` (writeAnthropicError를 errors.go로 이동)

- [ ] **Step 1: errors.go로 helper 이동**

`internal/server/auth.go`에서 `writeAnthropicError`와 그 `encoding/json` import를 제거하고, `internal/server/errors.go` 신규 생성:
```go
package server

import (
	"encoding/json"
	"net/http"
)

// writeAnthropicError emits {"type":"error","error":{"type","message"}} —
// the shape Anthropic clients (Claude Code) expect.
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
```
Run: `go test ./internal/server/ -run TestDevKeyAuth -v` → still PASS (재배치 확인).

- [ ] **Step 2: 핸들러 실패 테스트 작성 (mockprovider 경유 E2E)**

`internal/server/anthropicapi/messages_test.go`:
```go
package anthropicapi

import (
	"context"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func testRouter() *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	return router.New(provs, models)
}

func TestMessagesNonStreaming(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"msg_mock"`) {
		t.Fatalf("body missing mock response: %s", rec.Body.String())
	}
}

func TestMessagesStreamingTee(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"claude-sonnet-4-6","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "event: message_start") || !strings.Contains(body, "event: message_stop") {
		t.Fatalf("stream not teed verbatim: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type = %q", ct)
	}
}

func TestMessagesUnknownModel(t *testing.T) {
	h := NewMessagesHandler(testRouter())
	req := httptest.NewRequest("POST", "/v1/messages",
		strings.NewReader(`{"model":"no-such-model","messages":[]}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 404 && rec.Code != 400 {
		t.Fatalf("expected 4xx for unknown model, got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"error"`) {
		t.Fatalf("expected anthropic error body: %s", rec.Body.String())
	}
	_ = context.Background
	_ = io.Discard
}
```

- [ ] **Step 3: 실패 확인**

Run: `go test ./internal/server/anthropicapi/ -run TestMessages -v`
Expected: FAIL — `undefined: NewMessagesHandler`

- [ ] **Step 4: 구현 (messages.go)**

`internal/server/anthropicapi/messages.go`:
```go
// Package anthropicapi implements the Anthropic-shaped ingress endpoints.
package anthropicapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type MessagesHandler struct{ r *router.Router }

func NewMessagesHandler(r *router.Router) *MessagesHandler { return &MessagesHandler{r: r} }

func (h *MessagesHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, err := io.ReadAll(req.Body)
	if err != nil {
		writeErr(w, 400, "invalid_request_error", "could not read request body")
		return
	}
	// Parse for routing/observation ONLY. RawBody is forwarded verbatim.
	var parsed schema.ChatRequest
	if err := json.Unmarshal(raw, &parsed); err != nil {
		writeErr(w, 400, "invalid_request_error", "malformed JSON")
		return
	}
	prov, upstream, err := h.r.Resolve(parsed.Model)
	if err != nil {
		writeErr(w, 404, "not_found_error", "unknown model: "+parsed.Model)
		return
	}
	pr := &providers.ProxyRequest{
		Model: parsed.Model, Upstream: upstream, Parsed: &parsed,
		RawBody: raw, Headers: req.Header, Stream: parsed.Stream != nil && *parsed.Stream,
	}
	if pr.Stream {
		h.serveStream(w, req, prov, pr)
		return
	}
	h.serveComplete(w, req, prov, pr)
}

func (h *MessagesHandler) serveComplete(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest) {
	resp, err := prov.Complete(req.Context(), pr)
	if err != nil {
		writeErr(w, 502, "api_error", "upstream error")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	w.Write(resp.RawBody) // tee verbatim
	// resp.Parsed.Usage is the observation hook for M3 audit / M5 quota.
}

func (h *MessagesHandler) serveStream(w http.ResponseWriter, req *http.Request, prov providers.Provider, pr *providers.ProxyRequest) {
	seq, err := prov.Stream(req.Context(), pr)
	if err != nil {
		writeErr(w, 502, "api_error", "upstream stream error")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "api_error", "streaming unsupported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	for ev, err := range seq {
		if err != nil {
			return // upstream broke mid-stream; client sees a truncated stream (M5: error event)
		}
		w.Write(ev.Raw) // tee original bytes verbatim
		flusher.Flush()
		// ev.Chunk.Usage on message_delta is the settlement observation point.
	}
}

func writeErr(w http.ResponseWriter, status int, errType, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"type":  "error",
		"error": map[string]string{"type": errType, "message": msg},
	})
}
```

- [ ] **Step 5: 통과 확인**

Run: `go test ./internal/server/anthropicapi/ -run TestMessages -v`
Expected: PASS (비스트리밍 mock 응답, 스트리밍 tee 원문, unknown model 4xx)

- [ ] **Step 6: 커밋**

```bash
git add internal/server/anthropicapi/messages.go internal/server/anthropicapi/messages_test.go internal/server/errors.go internal/server/auth.go
git commit -s -m "feat(ingress): /v1/messages non-streaming + SSE tee handler"
```

---

### Task 11: /v1/messages/count_tokens 핸들러 (절대 5xx 금지)

**Files:**
- Create: `internal/server/anthropicapi/count_tokens.go`
- Test: `internal/server/anthropicapi/count_tokens_test.go`

설계 §3.1: count_tokens는 **어떤 경우에도 유효한 JSON을 반환**한다 (501/5xx 시 Claude Code 크래시 이력). M2 전략: 타깃 provider가 `TokenCounter`면 위임(정확), 아니면 보수적 추정(`max(1, len(body)/4)`). §10 #1의 토크나이저 동봉 spike는 비-Anthropic provider가 등장하는 M4/M5로 연기 — M2엔 anthropic provider뿐이라 항상 정확 경로.

- [ ] **Step 1: 실패 테스트 작성**

`internal/server/anthropicapi/count_tokens_test.go`:
```go
package anthropicapi

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

// estimatorRouter uses mockprovider which does NOT implement TokenCounter,
// forcing the estimator fallback path.
func estimatorRouter() *router.Router {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	return router.New(provs, models)
}

func TestCountTokensAlwaysValidJSON(t *testing.T) {
	h := NewCountTokensHandler(estimatorRouter())
	// unknown model: must STILL return 200 + valid {"input_tokens":N}, never 4xx/5xx.
	body := `{"model":"no-such-model","messages":[{"role":"user","content":"hello world"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("count_tokens must never return non-200; got %d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"input_tokens"`) {
		t.Fatalf("missing input_tokens: %s", rec.Body.String())
	}
}

func TestCountTokensEstimatorFallback(t *testing.T) {
	h := NewCountTokensHandler(estimatorRouter())
	body := `{"model":"claude-sonnet-4-6","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest("POST", "/v1/messages/count_tokens", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"input_tokens"`) {
		t.Fatalf("estimator fallback failed: %d %s", rec.Code, rec.Body.String())
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/server/anthropicapi/ -run TestCountTokens -v`
Expected: FAIL — `undefined: NewCountTokensHandler`

- [ ] **Step 3: 구현**

`internal/server/anthropicapi/count_tokens.go`:
```go
package anthropicapi

import (
	"encoding/json"
	"io"
	"net/http"

	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type CountTokensHandler struct{ r *router.Router }

func NewCountTokensHandler(r *router.Router) *CountTokensHandler { return &CountTokensHandler{r: r} }

// ServeHTTP NEVER returns a non-200 / non-JSON response. A 501/4xx/5xx here
// crashes Claude Code (truncated-JSON crash, design doc §3.1). On any failure
// it falls back to a conservative estimate and still returns
// {"input_tokens": N} with HTTP 200.
func (h *CountTokensHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	raw, _ := io.ReadAll(req.Body)
	n := h.count(req, raw)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(200)
	json.NewEncoder(w).Encode(map[string]int64{"input_tokens": n})
}

func (h *CountTokensHandler) count(req *http.Request, raw []byte) int64 {
	var parsed schema.ChatRequest
	_ = json.Unmarshal(raw, &parsed) // best-effort; estimator works on raw bytes too
	prov, upstream, err := h.r.Resolve(parsed.Model)
	if err == nil {
		if tc, ok := prov.(providers.TokenCounter); ok {
			pr := &providers.ProxyRequest{Model: parsed.Model, Upstream: upstream, RawBody: raw, Headers: req.Header}
			if got, cerr := tc.CountTokens(req.Context(), pr); cerr == nil {
				return got
			}
		}
	}
	return estimateTokens(raw)
}

// estimateTokens is the conservative fallback for providers without a
// TokenCounter (M2: none; M4/M5 may bundle a tokenizer per §10 #1). ~4 bytes
// per token is a coarse upper-ish bound; valid output matters more than
// precision here.
func estimateTokens(raw []byte) int64 {
	n := int64(len(raw) / 4)
	if n < 1 {
		n = 1
	}
	return n
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/server/anthropicapi/ -run TestCountTokens -v` → PASS.
```bash
git add internal/server/anthropicapi/count_tokens.go internal/server/anthropicapi/count_tokens_test.go
git commit -s -m "feat(ingress): count_tokens handler that never returns non-200"
```

---

### Task 12: /v1/models 핸들러

**Files:**
- Create: `internal/server/anthropicapi/models.go`
- Test: `internal/server/anthropicapi/models_test.go`

설계 §3.1: Claude Code v2.1.129+ 게이트웨이 모델 디스커버리가 호출. M2는 config의 모든 모델 반환(allow-list 필터는 M3 키 도입 후). Anthropic models-list 형태.

- [ ] **Step 1: 실패 테스트 작성**

`internal/server/anthropicapi/models_test.go`:
```go
package anthropicapi

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
)

func TestModelsListShape(t *testing.T) {
	h := NewModelsHandler(testRouter()) // testRouter from messages_test.go (same package)
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("status %d", rec.Code)
	}
	var out struct {
		Data    []map[string]any `json:"data"`
		HasMore bool             `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(out.Data) != 1 || out.Data[0]["id"] != "claude-sonnet-4-6" || out.Data[0]["type"] != "model" {
		t.Fatalf("unexpected data: %+v", out.Data)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/server/anthropicapi/ -run TestModelsList -v`
Expected: FAIL — `undefined: NewModelsHandler`

- [ ] **Step 3: 구현**

`internal/server/anthropicapi/models.go`:
```go
package anthropicapi

import (
	"encoding/json"
	"net/http"
	"sort"

	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/pkg/schema"
)

type ModelsHandler struct{ r *router.Router }

func NewModelsHandler(r *router.Router) *ModelsHandler { return &ModelsHandler{r: r} }

// ServeHTTP returns the configured models in Anthropic's GET /v1/models shape.
// M2 returns all configured models; M3 filters by the virtual key's allow-list
// (design doc §3.1).
func (h *ModelsHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	names := h.r.AllModels()
	sort.Strings(names) // deterministic order
	data := make([]schema.ModelInfo, 0, len(names))
	for _, n := range names {
		data = append(data, schema.ModelInfo{Type: "model", ID: n, DisplayName: n})
	}
	resp := map[string]any{"data": data, "has_more": false}
	if len(data) > 0 {
		resp["first_id"] = data[0].ID
		resp["last_id"] = data[len(data)-1].ID
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./internal/server/anthropicapi/ -run TestModelsList -v` → PASS.
```bash
git add internal/server/anthropicapi/models.go internal/server/anthropicapi/models_test.go
git commit -s -m "feat(ingress): GET /v1/models for Claude Code model discovery"
```

---

### Task 13: SSE 직렬화기 (ChatChunk → Anthropic SSE) + 골든 검증

**Files:**
- Create: `internal/server/anthropicapi/sse_write.go`
- Test: `internal/server/anthropicapi/sse_write_test.go`

설계: M2 동일 프로토콜 경로는 tee(원본 전달)라 이 직렬화기를 **출력 경로에 쓰지 않는다**. 하지만 M5 교차 프로토콜(OpenAI ingress→anthropic provider)에서 canonical → Anthropic SSE 재직렬화가 필요하므로 **지금 만들어 골든 검증**한다(M1 SSE 픽스처로 왕복: 파싱된 ChatChunk → 직렬화 → 의미 동등).

- [ ] **Step 1: 실패 테스트 작성**

`internal/server/anthropicapi/sse_write_test.go`:
```go
package anthropicapi

import (
	"strings"
	"testing"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestWriteSSEEvent(t *testing.T) {
	idx := 0
	chunk := &schema.ChatChunk{Type: "content_block_start", Index: &idx,
		ContentBlock: &schema.ContentBlock{Type: "text", Text: ptr("")}}
	var b strings.Builder
	if err := WriteSSEEvent(&b, chunk); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	// Anthropic SSE framing: "event: <type>\ndata: <json>\n\n"
	if !strings.HasPrefix(out, "event: content_block_start\n") {
		t.Fatalf("missing event line: %q", out)
	}
	if !strings.Contains(out, `"type":"content_block_start"`) || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("bad framing: %q", out)
	}
}

func ptr(s string) *string { return &s }
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/server/anthropicapi/ -run TestWriteSSE -v`
Expected: FAIL — `undefined: WriteSSEEvent`

- [ ] **Step 3: 구현**

`internal/server/anthropicapi/sse_write.go`:
```go
package anthropicapi

import (
	"encoding/json"
	"io"

	"github.com/inferplane/inferplane/pkg/schema"
)

// WriteSSEEvent serializes a canonical ChatChunk into one Anthropic SSE event
// ("event: <type>\ndata: <json>\n\n"). NOT used on the M2 same-protocol path
// (which tees original upstream bytes); it exists for M5 cross-protocol
// re-serialization (OpenAI ingress → Anthropic provider). Golden-validated now
// so M5 builds on a verified serializer.
func WriteSSEEvent(w io.Writer, chunk *schema.ChatChunk) error {
	data, err := json.Marshal(chunk)
	if err != nil {
		return err
	}
	if _, err := io.WriteString(w, "event: "+chunk.Type+"\n"); err != nil {
		return err
	}
	if _, err := w.Write(append([]byte("data: "), data...)); err != nil {
		return err
	}
	_, err = io.WriteString(w, "\n\n")
	return err
}
```

- [ ] **Step 4: 골든 왕복 검증 테스트 추가**

`internal/server/anthropicapi/sse_write_test.go`에 추가 — M1의 스트리밍 픽스처 파일을 읽어 각 이벤트를 ChatChunk로 파싱 → WriteSSEEvent로 재직렬화 → data 부분이 의미 동등한지 확인:
```go
import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/inferplane/inferplane/providers/anthropic"
)

func TestWriteSSEMatchesGoldenData(t *testing.T) {
	// Reuse M1's streaming golden fixture. The serializer's data: payload must
	// be semantically equal to the original event's data: payload.
	path := filepath.Join("..", "..", "..", "pkg", "schema", "testdata", "roundtrip", "stream", "streaming-tool-use.sse")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("golden fixture not found: %v", err)
	}
	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	var n int
	for sc.Scan() {
		line := sc.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		orig := []byte(strings.TrimPrefix(line, "data: "))
		var c schema.ChatChunk
		if err := json.Unmarshal(orig, &c); err != nil {
			t.Fatalf("event %d parse: %v", n, err)
		}
		var b strings.Builder
		if err := WriteSSEEvent(&b, &c); err != nil {
			t.Fatal(err)
		}
		// extract data: line from our output
		out := b.String()
		var gotData string
		for _, l := range strings.Split(out, "\n") {
			if strings.HasPrefix(l, "data: ") {
				gotData = strings.TrimPrefix(l, "data: ")
			}
		}
		if !jsonEqual(t, orig, []byte(gotData)) {
			t.Fatalf("event %d data mismatch:\n got: %s\nwant: %s", n, gotData, orig)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events read from fixture")
	}
	_ = anthropic.Name // ensure import used if needed; remove if unused
}

func jsonEqual(t *testing.T, a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	x, _ := json.Marshal(va)
	y, _ := json.Marshal(vb)
	return string(x) == string(y)
}
```
> `anthropic.Name`가 존재하지 않으면 그 줄과 import를 삭제한다 — 골든 파일 경로 접근에 anthropic 패키지는 불필요하다. (작성자 판단으로 미사용 import 정리.)

- [ ] **Step 5: 통과 + 커밋**

Run: `go test ./internal/server/anthropicapi/ -v` → 전부 PASS, `gofmt -l .` empty.
```bash
git add internal/server/anthropicapi/sse_write.go internal/server/anthropicapi/sse_write_test.go
git commit -s -m "feat(ingress): Anthropic SSE serializer with golden validation (M5 prep)"
```

---

### Task 14: server 조립 + main.go serve

**Files:**
- Create: `internal/server/server.go`, `cmd/inferplane/main.go`
- Test: `internal/server/server_test.go`

- [ ] **Step 1: 서버 조립 실패 테스트 작성**

`internal/server/server_test.go`:
```go
package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/providers"
	"github.com/inferplane/inferplane/providers/testing/mockprovider"
)

func TestDataMuxRoutesAndAuths(t *testing.T) {
	provs := map[string]providers.Provider{"p": mockprovider.New("claude-sonnet-4-6")}
	models := map[string]config.ModelConfig{
		"claude-sonnet-4-6": {Targets: []config.Target{{Provider: "p", Model: "claude-sonnet-4-6"}}},
	}
	r := router.New(provs, models)
	mux := DataMux(r, "dev-key")

	// missing auth → 401
	req := httptest.NewRequest("GET", "/v1/models", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 401 {
		t.Fatalf("unauth /v1/models = %d, want 401", rec.Code)
	}

	// with auth → 200
	req2 := httptest.NewRequest("GET", "/v1/models", nil)
	req2.Header.Set("x-api-key", "dev-key")
	rec2 := httptest.NewRecorder()
	mux.ServeHTTP(rec2, req2)
	if rec2.Code != 200 {
		t.Fatalf("auth /v1/models = %d, want 200", rec2.Code)
	}
}

func TestAdminMuxHealthz(t *testing.T) {
	mux := AdminMux()
	req := httptest.NewRequest("GET", "/healthz", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("/healthz = %d", rec.Code)
	}
	_ = http.StatusOK
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./internal/server/ -run 'TestDataMux|TestAdminMux' -v`
Expected: FAIL — `undefined: DataMux`

- [ ] **Step 3: 구현 (server.go)**

`internal/server/server.go`:
```go
package server

import (
	"net/http"

	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server/anthropicapi"
)

// DataMux builds the data-plane (:8080) handler: Anthropic ingress endpoints
// behind the temporary dev-key auth (M2). All endpoints require the key.
func DataMux(r *router.Router, devKey string) http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST /v1/messages", anthropicapi.NewMessagesHandler(r))
	mux.Handle("POST /v1/messages/count_tokens", anthropicapi.NewCountTokensHandler(r))
	mux.Handle("GET /v1/models", anthropicapi.NewModelsHandler(r))
	return DevKeyAuth(devKey, mux)
}

// AdminMux builds the admin-plane (:9090) handler: health + (M3) /metrics,
// admin API. /healthz and /readyz are unauthenticated (design doc §5.5 splits
// metrics/health auth from admin auth).
func AdminMux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) })
	return mux
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./internal/server/ -v` → PASS.

- [ ] **Step 5: main.go serve 서브커맨드**

`cmd/inferplane/main.go`:
```go
// Command inferplane is the gateway binary. M2 implements the `serve`
// subcommand; `keys` and `audit` arrive in M3.
package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/inferplane/inferplane/internal/config"
	"github.com/inferplane/inferplane/internal/router"
	"github.com/inferplane/inferplane/internal/server"
	"github.com/inferplane/inferplane/providers"

	_ "github.com/inferplane/inferplane/providers/anthropic" // register "anthropic"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "serve" {
		fmt.Fprintln(os.Stderr, "usage: inferplane serve --config <path>")
		os.Exit(2)
	}
	cfgPath := "config.json"
	for i := 2; i < len(os.Args)-1; i++ {
		if os.Args[i] == "--config" {
			cfgPath = os.Args[i+1]
		}
	}
	if err := run(cfgPath); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(cfgPath string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	devKey := os.Getenv("INFERPLANE_DEV_KEY")
	if devKey == "" {
		return errors.New("INFERPLANE_DEV_KEY must be set (M2 temporary auth)")
	}

	// Build providers from config.
	provs := map[string]providers.Provider{}
	for name, pc := range cfg.Providers {
		p, err := providers.New(providers.Config{Type: pc.Type, BaseURL: pc.BaseURL, APIKey: pc.APIKey})
		if err != nil {
			return fmt.Errorf("provider %q: %w", name, err)
		}
		provs[name] = p
	}
	r := router.New(provs, cfg.Models)

	dataSrv := &http.Server{Addr: cfg.Server.Listen, Handler: server.DataMux(r, devKey)}
	adminSrv := &http.Server{Addr: cfg.Server.AdminListen, Handler: server.AdminMux()}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errc := make(chan error, 2)
	go func() { errc <- dataSrv.ListenAndServe() }()
	go func() { errc <- adminSrv.ListenAndServe() }()
	fmt.Printf("inferplane serving data=%s admin=%s\n", cfg.Server.Listen, cfg.Server.AdminListen)

	select {
	case <-ctx.Done():
		_ = dataSrv.Close()
		_ = adminSrv.Close()
		return nil
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
```

- [ ] **Step 6: 빌드 + 전체 테스트 + 커밋**

Run:
```bash
go build ./... && go test ./... && go vet ./... && gofmt -l .
```
Expected: 빌드 성공, 전체 PASS, vet 클린, gofmt 출력 없음.
```bash
git add internal/server/server.go internal/server/server_test.go cmd/inferplane/main.go
git commit -s -m "feat(server): assemble data/admin muxes and serve subcommand"
```

---

### Task 15: M2 게이트 — 실제 Claude Code 연동 (수동)

이 태스크는 자동 테스트가 아니라 **게이트 검증**이다. 사용자/운영자가 실제 Claude Code로 확인한다.

- [ ] **Step 1: 바이너리 빌드**

```bash
go build -o bin/inferplane ./cmd/inferplane
```

- [ ] **Step 2: config + 환경변수 준비**

`config.json` (Task 4 픽스처와 동형, 실제 모델 매핑 추가):
```json
{
  "server": { "listen": ":8080", "admin_listen": ":9090" },
  "providers": {
    "anthropic-direct": { "type": "anthropic", "base_url": "https://api.anthropic.com", "api_key_ref": { "env": "ANTHROPIC_API_KEY" } }
  },
  "models": {
    "claude-sonnet-4-6": { "targets": [ { "provider": "anthropic-direct", "model": "claude-sonnet-4-6" } ] },
    "claude-opus-4-8": { "targets": [ { "provider": "anthropic-direct", "model": "claude-opus-4-8" } ] }
  }
}
```
```bash
export ANTHROPIC_API_KEY=sk-ant-...        # 실제 게이트웨이 소유 키
export INFERPLANE_DEV_KEY=local-dev-key    # 클라이언트가 쓸 임시 키
./bin/inferplane serve --config config.json
```

- [ ] **Step 3: Claude Code 연결 (별도 셸)**

```bash
export ANTHROPIC_BASE_URL=http://localhost:8080
export ANTHROPIC_API_KEY=local-dev-key     # inferplane dev key (실 키 아님)
export CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY=1
claude
```

- [ ] **Step 4: 게이트 체크리스트 (모두 통과해야 M2 완료)**

- [ ] 대화: 간단한 프롬프트에 응답이 스트리밍으로 표시된다.
- [ ] 툴콜: 파일 읽기/Bash 등 도구 호출이 정상 동작한다 (tool_use/tool_result 보존).
- [ ] 모델 디스커버리: `/v1/models`가 200 + config 모델 목록을 반환 (Claude Code가 크래시 없이 모델 인식).
- [ ] count_tokens: 컨텍스트 관리가 정상 (count_tokens 200 응답, 크래시 없음).
- [ ] **캐시 hit율 유지**: 같은 세션 두 번째 요청부터 upstream usage의 `cache_read_input_tokens > 0`. inferplane 경유 hit율이 직결과 동일함을 확인 — 직결 대비 저하 없으면 §4.4 캐시 불변식 통과. (Anthropic 콘솔 또는 응답 usage로 확인.)

- [ ] **Step 5: 게이트 통과 기록**

체크리스트 전부 통과 시 M2 완료. 미통과 항목은 디버깅 후 해당 태스크로 회귀.

---

## Self-Review 결과

- **스펙 커버리지**: §3.1 이중 ingress 중 Anthropic 측(messages SSE/count_tokens/models) → Task 10/11/12. §4.1 Provider 인터페이스(iter.Seq2, TokenCounter) → Task 2/7/8/9. §4.4 캐시 불변식(원본 바이트 전달) → Task 7 `RawBody` 검증 + Task 10 tee. §2.3 net/http+ServeMux, 리스너 분리 → Task 14. §2.4 미들웨어(타입드 체인은 M3+, M2는 auth만) → Task 6. §5.2 클라이언트/upstream 인증 분리 → Task 7/Task 6. §5.5 /metrics·health 무인증 → Task 14 AdminMux. §8 providers 코어 밖 + registry 1줄 → Task 2/7. 테스트 3층(골든은 M1, httptest 가짜 upstream → Task 7/8/9, mockprovider E2E → Task 10/11/12). ✓
- **플레이스홀더**: 없음 — 모든 스텝에 실제 Go 코드/명령/기대출력. count_tokens 추정 전략은 §10 #1을 M4/M5로 명시적 연기하고 M2 폴백을 구현으로 확정. ✓
- **타입 일관성**: `ProxyRequest`/`ProxyResponse`/`StreamEvent`/`Config`(Task 2) 시그니처가 Task 7/8/9/10/11에서 일관 사용. `schema.ChatChunk.Usage`/`.Type`/`.Index`/`.ContentBlock`, `schema.ContentBlock.Text *string`, `schema.Usage.InputTokens/OutputTokens *int64`, `schema.ChatRequest.Model/.Stream *bool`, `schema.ChatResponse.Usage` — 전부 M1 확정 타입과 일치(스트림 픽스처의 `*string`/`*int64` 포인터 필드 반영). `router.Resolve` 3-반환(provider, upstream, error)이 Task 5 정의·Task 10/11 사용처 일치. `DevKeyAuth`/`DataMux`/`AdminMux`(Task 6/14) 일관. ✓
- **알려진 한계 (의도)**: M2 count_tokens 추정기는 char/4 휴리스틱 — anthropic provider는 항상 정확(upstream 위임) 경로라 M2에선 추정기가 실트래픽에 안 쓰임. 비-Anthropic provider 토크나이저는 M4/M5 spike. 스트림 중단 시 클라이언트는 잘린 스트림을 봄(M5에서 표준 에러 이벤트 + grace-period 드레인). 폴백/circuit breaker는 M5. 단일 dev 키는 M3에서 virtual key로 교체.
