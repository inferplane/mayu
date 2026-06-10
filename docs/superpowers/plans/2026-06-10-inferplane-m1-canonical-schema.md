# inferplane M1 — Canonical 스키마 + 테스트 인프라 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Anthropic 왕복 무손실 불변식(§2.2-1)을 골든 파일 테스트로 보증하는 `pkg/schema` canonical 타입과 테스트 하네스를 구축한다 (스펙: `docs/specs/2026-06-10-inferplane-gateway-design.md` r4).

**Architecture:** canonical 스키마는 Anthropic Messages 형태를 기반으로 한 프로토콜 중립 superset. 핵심 전략은 **"최소 타입화, 최대 보존"** — 거버넌스 파이프라인이 지금 필요로 하는 필드(model, messages, 블록 타입·순서, cache_control, usage)만 타입으로 승격하고, 나머지(system, tools, delta, tool_result.content)는 `json.RawMessage`로 byte 보존한다. 모든 구조체는 `Extra map[string]json.RawMessage`로 미지(未知) 필드를 보존해 round-trip 무손실을 달성한다. 타입 승격은 그 필드를 실제로 해석해야 하는 마일스톤(M5 교차 프로토콜 변환)에서 수행한다.

**Tech Stack:** Go 1.23+ (`iter.Seq2`는 M2부터), 표준 `encoding/json`, 외부 의존성 0. 테스트는 표준 `testing` 패키지만.

**마일스톤 로드맵 (전체 6개 중 1번):**

| M | 범위 | 게이트 |
|---|---|---|
| **M1 (이 계획)** | pkg/schema + 골든 테스트 + 거버넌스 파일 | 골든 테스트 green + 스키마 리뷰 승인 |
| M2 | Anthropic ingress ↔ provider 직통 (/v1/messages, count_tokens, /v1/models, SSE) | 실제 Claude Code 연동 + 캐시 hit율 유지 |
| M3 | virtual key + 감사로그 (SQLite, 해시 체인, WAL, verify CLI) | 키 발급→인증→체인 검증 통과 |
| M4 | bedrock provider (InvokeModel 우선, Converse, IRSA) | Claude Code→Bedrock 실연동 + thinking 순서 골든 통과 |
| M5 | rate limit/quota/budget + OpenAI ingress + openai_compatible | OpenCode 실연동 + quota block + 비용 필드 검증 |
| M6 | failover/메트릭/Helm/TLS/quickstart | docker run→키 발급→Claude Code 5분 시연 |

각 마일스톤 시작 전 인터페이스 승인 필수. 게이트 통과 전 다음 마일스톤 시작 금지.

---

## 파일 구조

```
go.mod                          # module github.com/inferplane/inferplane, go 1.23
LICENSE                         # Apache 2.0
GOVERNANCE.md MAINTAINERS.md SECURITY.md CODE_OF_CONDUCT.md CONTRIBUTING.md  # 거버넌스 (DCO 포함)
.gitignore
pkg/schema/
  extra.go                      # 미지 필드 보존 헬퍼 (unmarshalWithExtra/marshalWithExtra)
  blocks.go                     # ContentBlock, CacheControl
  message.go                    # Message (string|array content 양형 보존)
  request.go                    # ChatRequest
  response.go                   # ChatResponse, Usage, CacheCreation
  chunk.go                      # ChatChunk (스트리밍 이벤트)
  *_test.go                     # 각 파일 단위 테스트
  roundtrip_test.go             # 골든 파일 하네스
  testdata/roundtrip/
    request/*.json              # 요청 왕복 픽스처
    response/*.json             # 응답 왕복 픽스처
    stream/*.sse                # SSE 이벤트 스트림 픽스처
```

각 파일 책임: `extra.go`는 보존 메커니즘 단 하나만 소유. 블록/메시지/요청/응답/청크는 파일당 타입 1군. 하네스는 픽스처를 추가하면 자동으로 검증 대상이 되는 구조 (M2 게이트에서 실트래픽 캡처를 픽스처로 추가).

---

### Task 1: 저장소 스캐폴드 + 거버넌스 파일

**Files:**
- Create: `go.mod`, `.gitignore`, `LICENSE`, `GOVERNANCE.md`, `MAINTAINERS.md`, `SECURITY.md`, `CODE_OF_CONDUCT.md`, `CONTRIBUTING.md`, `README.md`

- [ ] **Step 1: go.mod와 .gitignore 생성**

```bash
cd /home/atomoh/mayu && go mod init github.com/inferplane/inferplane
```

`.gitignore`:
```
/bin/
*.db
*.db-wal
*.db-shm
.claude/*.local.json
coverage.out
```

- [ ] **Step 2: LICENSE 다운로드**

```bash
curl -fsSL https://www.apache.org/licenses/LICENSE-2.0.txt -o LICENSE
head -3 LICENSE   # "Apache License" 확인
```

- [ ] **Step 3: 거버넌스 파일 작성**

`GOVERNANCE.md`:
```markdown
# Governance

inferplane is an independent open source project, vendor-neutral by design.

- **Maintainers** (MAINTAINERS.md) make decisions by lazy consensus on issues/PRs.
- Disagreements are resolved by maintainer majority vote.
- Becoming a maintainer: sustained quality contributions over ~3 months,
  nominated by an existing maintainer, approved by maintainer majority.
- All contributions require DCO sign-off (see CONTRIBUTING.md). License: Apache-2.0.
- This document evolves toward CNCF governance norms as the community grows.
```

`MAINTAINERS.md`:
```markdown
# Maintainers

| Name | GitHub | Scope |
|------|--------|-------|
| atomoh | @atomoh | project lead, all areas |
```

`SECURITY.md`:
```markdown
# Security Policy

Report vulnerabilities privately to ojs0106@gmail.com.
Do not open public issues for security reports.
We aim to acknowledge within 72 hours and provide a fix timeline within 7 days.
Supported versions: latest minor release.
```

`CODE_OF_CONDUCT.md`:
```markdown
# Code of Conduct

This project follows the [Contributor Covenant v2.1](https://www.contributor-covenant.org/version/2/1/code_of_conduct/).
Report violations to ojs0106@gmail.com.
```

`CONTRIBUTING.md`:
```markdown
# Contributing

- All commits MUST be signed off (`git commit -s`) — [DCO](https://developercertificate.org/).
  CI rejects unsigned commits.
- Provider PRs touch only `providers/<name>/`, `providers/register.go`,
  and `docs/providers/` — zero core diff (design doc §8).
- Run `go test ./...` before submitting.
- Design doc: `docs/specs/2026-06-10-inferplane-gateway-design.md`.
```

`README.md`:
```markdown
# inferplane

LLM consumption governance gateway — virtual keys, team RBAC, quotas,
budgets, and tamper-evident audit logging for Claude Code / OpenCode
traffic to Anthropic, Amazon Bedrock, and self-hosted vLLM/Ollama.
Single binary, Kubernetes-native, Apache-2.0.

> Status: pre-release (v0.1 in development). Not yet announced.

Design: [docs/specs/2026-06-10-inferplane-gateway-design.md](docs/specs/2026-06-10-inferplane-gateway-design.md)
```

- [ ] **Step 4: 커밋 (DCO 서명)**

```bash
git add go.mod .gitignore LICENSE GOVERNANCE.md MAINTAINERS.md SECURITY.md CODE_OF_CONDUCT.md CONTRIBUTING.md README.md
git commit -s -m "chore: scaffold repo with Apache-2.0 license and governance files"
```

---

### Task 2: Extra 보존 헬퍼

**Files:**
- Create: `pkg/schema/extra.go`
- Test: `pkg/schema/extra_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/extra_test.go`:
```go
package schema

import (
	"encoding/json"
	"testing"
)

type sample struct {
	Known string `json:"known"`
	Extra map[string]json.RawMessage `json:"-"`
}

func TestExtraPreservesUnknownFields(t *testing.T) {
	in := []byte(`{"known":"a","future_field":{"x":1},"another":"y"}`)
	var s sample
	extra, err := unmarshalWithExtra(in, &s, "known")
	if err != nil {
		t.Fatal(err)
	}
	s.Extra = extra
	if s.Known != "a" {
		t.Fatalf("known = %q", s.Known)
	}
	out, err := marshalWithExtra(s, s.Extra)
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, in, out)
}

// assertJSONSemanticEqual: 키 순서 무시, 숫자 정밀도 보존 비교.
func assertJSONSemanticEqual(t *testing.T, want, got []byte) {
	t.Helper()
	if !jsonSemanticEqual(want, got) {
		t.Fatalf("JSON mismatch\nwant: %s\ngot:  %s", want, got)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestExtra -v`
Expected: FAIL — `undefined: unmarshalWithExtra`

- [ ] **Step 3: 구현**

`pkg/schema/extra.go`:
```go
// Package schema defines inferplane's canonical types — an
// Anthropic-Messages-shaped, protocol-neutral superset (design doc §2.2).
// Invariant: same-protocol round trip is lossless. Unknown fields are
// preserved via Extra maps; fields the pipeline does not yet interpret
// stay json.RawMessage ("minimal typing, maximal preservation").
package schema

import (
	"bytes"
	"encoding/json"
	"reflect"
)

// unmarshalWithExtra decodes data into v (standard json tags), then returns
// every top-level key NOT in known as raw bytes for lossless re-emission.
func unmarshalWithExtra(data []byte, v any, known ...string) (map[string]json.RawMessage, error) {
	if err := json.Unmarshal(data, v); err != nil {
		return nil, err
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return nil, err
	}
	for _, k := range known {
		delete(all, k)
	}
	if len(all) == 0 {
		return nil, nil
	}
	return all, nil
}

// marshalWithExtra marshals v (its json tags decide known fields), then
// overlays extra keys. Known keys always win over stale extra entries.
func marshalWithExtra(v any, extra map[string]json.RawMessage) ([]byte, error) {
	base, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if len(extra) == 0 {
		return base, nil
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(base, &m); err != nil {
		return nil, err
	}
	for k, raw := range extra {
		if _, exists := m[k]; !exists {
			m[k] = raw
		}
	}
	return json.Marshal(m)
}

// jsonSemanticEqual compares two JSON documents ignoring key order,
// using json.Number to avoid float64 precision loss on token counts.
func jsonSemanticEqual(a, b []byte) bool {
	var va, vb any
	da := json.NewDecoder(bytes.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(bytes.NewReader(b))
	db.UseNumber()
	if da.Decode(&va) != nil || db.Decode(&vb) != nil {
		return false
	}
	return reflect.DeepEqual(va, vb)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestExtra -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/extra.go pkg/schema/extra_test.go
git commit -s -m "feat(schema): unknown-field preservation helpers"
```

---

### Task 3: ContentBlock + CacheControl

**Files:**
- Create: `pkg/schema/blocks.go`
- Test: `pkg/schema/blocks_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/blocks_test.go`:
```go
package schema

import "testing"

func TestContentBlockRoundTrip(t *testing.T) {
	cases := map[string]string{
		"text_with_cache": `{"type":"text","text":"hello","cache_control":{"type":"ephemeral","ttl":"1h"}}`,
		"tool_use":        `{"type":"tool_use","id":"toolu_01","name":"bash","input":{"command":"ls -la"}}`,
		"tool_result":     `{"type":"tool_result","tool_use_id":"toolu_01","content":[{"type":"text","text":"ok"}],"is_error":false}`,
		"thinking":        `{"type":"thinking","thinking":"step 1...","signature":"EuYBCkQYAiJA"}`,
		"redacted":        `{"type":"redacted_thinking","data":"EmwKAhgB"}`,
		"unknown_type":    `{"type":"future_block","payload":{"deep":[1,2]}}`,
		"unknown_field":   `{"type":"text","text":"x","novel_attr":true}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var b ContentBlock
			if err := b.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatal(err)
			}
			out, err := b.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}

func TestContentBlockTypedFields(t *testing.T) {
	var b ContentBlock
	_ = b.UnmarshalJSON([]byte(`{"type":"tool_use","id":"toolu_01","name":"bash","input":{}}`))
	if b.Type != "tool_use" || b.ID != "toolu_01" || b.Name != "bash" {
		t.Fatalf("typed fields not populated: %+v", b)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestContentBlock -v`
Expected: FAIL — `undefined: ContentBlock`

- [ ] **Step 3: 구현**

`pkg/schema/blocks.go`:
```go
package schema

import "encoding/json"

// CacheControl marks a prompt-cache breakpoint. The gateway never adds,
// moves, or strips these (§4.4 — cache pass-through is a hard constraint).
type CacheControl struct {
	Type  string `json:"type"`          // "ephemeral"
	TTL   string `json:"ttl,omitempty"` // "5m" | "1h" | "" (기본 5m)
	Extra map[string]json.RawMessage `json:"-"`
}

func (c *CacheControl) UnmarshalJSON(data []byte) error {
	type plain CacheControl
	extra, err := unmarshalWithExtra(data, (*plain)(c), "type", "ttl")
	c.Extra = extra
	return err
}

func (c CacheControl) MarshalJSON() ([]byte, error) {
	type plain CacheControl
	return marshalWithExtra(plain(c), c.Extra)
}

// ContentBlock is the canonical content unit — a single-struct tagged union
// over the Anthropic block vocabulary. Unknown block types round-trip via
// Extra; tool_result.content stays raw until a milestone needs to interpret it.
type ContentBlock struct {
	// Type has no omitempty on purpose: never silently drop the union discriminator.
	Type string `json:"type"`

	// text — *string: content_block_start 프레임의 "text":"" 가 정당한 값이라
	// 빈 문자열의 존재/부재를 구분해야 함 (리뷰 수정 48d412d)
	Text *string `json:"text,omitempty"`

	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`

	// tool_result — content is string OR block array; raw preserves both forms
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   json.RawMessage `json:"content,omitempty"`
	IsError   *bool           `json:"is_error,omitempty"`

	// thinking / redacted_thinking — *string: 위와 동일한 이유
	Thinking  *string `json:"thinking,omitempty"`
	Signature *string `json:"signature,omitempty"`
	Data      *string `json:"data,omitempty"`

	CacheControl *CacheControl `json:"cache_control,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var contentBlockKnown = []string{
	"type", "text", "id", "name", "input", "tool_use_id", "content",
	"is_error", "thinking", "signature", "data", "cache_control",
}

func (b *ContentBlock) UnmarshalJSON(data []byte) error {
	type plain ContentBlock
	extra, err := unmarshalWithExtra(data, (*plain)(b), contentBlockKnown...)
	b.Extra = extra
	return err
}

func (b ContentBlock) MarshalJSON() ([]byte, error) {
	type plain ContentBlock
	return marshalWithExtra(plain(b), b.Extra)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestContentBlock -v`
Expected: PASS (7개 서브테스트 + typed fields)

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/blocks.go pkg/schema/blocks_test.go
git commit -s -m "feat(schema): ContentBlock and CacheControl with lossless round trip"
```

---

### Task 4: Message (string|array content 양형 보존)

**Files:**
- Create: `pkg/schema/message.go`
- Test: `pkg/schema/message_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/message_test.go`:
```go
package schema

import "testing"

func TestMessageRoundTrip(t *testing.T) {
	cases := map[string]string{
		// Anthropic은 content에 string과 블록 배열 양형을 허용 — 원형 보존 필수
		"string_content": `{"role":"user","content":"plain text"}`,
		"block_content":  `{"role":"assistant","content":[{"type":"text","text":"hi"},{"type":"tool_use","id":"t1","name":"grep","input":{"q":"x"}}]}`,
		"block_order":    `{"role":"assistant","content":[{"type":"thinking","thinking":"...","signature":"sig"},{"type":"text","text":"answer"}]}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var m Message
			if err := m.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatal(err)
			}
			out, err := m.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}

func TestMessageBlockOrderPreserved(t *testing.T) {
	in := `{"role":"assistant","content":[{"type":"thinking","thinking":"a","signature":"s"},{"type":"text","text":"b"}]}`
	var m Message
	_ = m.UnmarshalJSON([]byte(in))
	if len(m.Content) != 2 || m.Content[0].Type != "thinking" || m.Content[1].Type != "text" {
		t.Fatalf("block order broken: %+v", m.Content)
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestMessage -v`
Expected: FAIL — `undefined: Message`

- [ ] **Step 3: 구현**

`pkg/schema/message.go`:
```go
package schema

import "encoding/json"

// Message is one turn. Anthropic accepts content as a bare string or a
// block array; contentIsString remembers the original form so re-emission
// is shape-identical (a normalized form would still be semantically equal,
// but byte-shape fidelity keeps diffs and caching analysis trivial).
type Message struct {
	Role    string         `json:"role"`
	Content []ContentBlock `json:"-"`

	contentIsString bool
	Extra           map[string]json.RawMessage `json:"-"`
}

func (m *Message) UnmarshalJSON(data []byte) error {
	var head struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &head); err != nil {
		return err
	}
	m.Role = head.Role
	if len(head.Content) > 0 && head.Content[0] == '"' {
		var s string
		if err := json.Unmarshal(head.Content, &s); err != nil {
			return err
		}
		m.Content = []ContentBlock{{Type: "text", Text: &s}}
		m.contentIsString = true
	} else if len(head.Content) > 0 {
		if err := json.Unmarshal(head.Content, &m.Content); err != nil {
			return err
		}
	}
	var all map[string]json.RawMessage
	if err := json.Unmarshal(data, &all); err != nil {
		return err
	}
	delete(all, "role")
	delete(all, "content")
	if len(all) > 0 {
		m.Extra = all
	}
	return nil
}

func (m Message) MarshalJSON() ([]byte, error) {
	out := map[string]json.RawMessage{}
	roleRaw, _ := json.Marshal(m.Role)
	out["role"] = roleRaw
	var contentRaw []byte
	var err error
	if m.contentIsString && len(m.Content) == 1 && m.Content[0].Type == "text" && m.Content[0].Text != nil {
		contentRaw, err = json.Marshal(*m.Content[0].Text)
	} else {
		contentRaw, err = json.Marshal(m.Content)
	}
	if err != nil {
		return nil, err
	}
	out["content"] = contentRaw
	for k, raw := range m.Extra {
		if _, exists := out[k]; !exists {
			out[k] = raw
		}
	}
	return json.Marshal(out)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestMessage -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/message.go pkg/schema/message_test.go
git commit -s -m "feat(schema): Message with string/array content shape fidelity"
```

---

### Task 5: ChatRequest

**Files:**
- Create: `pkg/schema/request.go`
- Test: `pkg/schema/request_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/request_test.go`:
```go
package schema

import "testing"

func TestChatRequestRoundTrip(t *testing.T) {
	// cache_control 멀티 breakpoint: system 블록 + 마지막 user 메시지.
	// tools/system/thinking은 M1에서 raw 보존 (M5에서 타입 승격).
	in := `{
	  "model": "claude-sonnet-4-6",
	  "max_tokens": 8192,
	  "stream": true,
	  "system": [
	    {"type":"text","text":"You are Claude Code.","cache_control":{"type":"ephemeral"}},
	    {"type":"text","text":"Project context...","cache_control":{"type":"ephemeral","ttl":"1h"}}
	  ],
	  "tools": [{"name":"bash","description":"run","input_schema":{"type":"object"}}],
	  "thinking": {"type":"enabled","budget_tokens":4096},
	  "messages": [
	    {"role":"user","content":[{"type":"text","text":"refactor this","cache_control":{"type":"ephemeral"}}]}
	  ],
	  "metadata": {"user_id":"team-platform"}
	}`
	var r ChatRequest
	if err := r.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if r.Model != "claude-sonnet-4-6" || r.Stream == nil || !*r.Stream || len(r.Messages) != 1 {
		t.Fatalf("typed fields: %+v", r)
	}
	out, err := r.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestChatRequest -v`
Expected: FAIL — `undefined: ChatRequest`

- [ ] **Step 3: 구현**

`pkg/schema/request.go`:
```go
package schema

import "encoding/json"

// ChatRequest — canonical 요청. 파이프라인이 해석하는 필드만 타입화:
// Model(라우팅·단가), Messages(블록 순서·cache 불변식), Stream, MaxTokens
// (TPM 추정). system/tools/tool_choice/thinking/metadata는 raw 보존 —
// 교차 프로토콜 변환(M5)에서 타입 승격한다.
type ChatRequest struct {
	Model     string    `json:"model"`
	Messages  []Message `json:"messages"`
	MaxTokens *int64    `json:"max_tokens,omitempty"`
	// *bool: 명시적 "stream":false 보존 (omitempty 결함 계열, 리뷰 수정 3d5e050)
	Stream *bool `json:"stream,omitempty"`

	System     json.RawMessage `json:"system,omitempty"`
	Tools      json.RawMessage `json:"tools,omitempty"`
	ToolChoice json.RawMessage `json:"tool_choice,omitempty"`
	Thinking   json.RawMessage `json:"thinking,omitempty"`
	Metadata   json.RawMessage `json:"metadata,omitempty"`

	Extra map[string]json.RawMessage `json:"-"`
}

var chatRequestKnown = []string{
	"model", "messages", "max_tokens", "stream",
	"system", "tools", "tool_choice", "thinking", "metadata",
}

func (r *ChatRequest) UnmarshalJSON(data []byte) error {
	type plain ChatRequest
	extra, err := unmarshalWithExtra(data, (*plain)(r), chatRequestKnown...)
	r.Extra = extra
	return err
}

func (r ChatRequest) MarshalJSON() ([]byte, error) {
	type plain ChatRequest
	return marshalWithExtra(plain(r), r.Extra)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestChatRequest -v`
Expected: PASS

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/request.go pkg/schema/request_test.go
git commit -s -m "feat(schema): ChatRequest with raw preservation for uninterpreted fields"
```

---

### Task 6: ChatResponse + Usage

**Files:**
- Create: `pkg/schema/response.go`
- Test: `pkg/schema/response_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/response_test.go`:
```go
package schema

import "testing"

func TestChatResponseRoundTrip(t *testing.T) {
	in := `{
	  "id": "msg_01ABC",
	  "type": "message",
	  "role": "assistant",
	  "model": "claude-sonnet-4-6",
	  "content": [
	    {"type":"thinking","thinking":"reasoning...","signature":"EuYB"},
	    {"type":"text","text":"done"}
	  ],
	  "stop_reason": "end_turn",
	  "stop_sequence": null,
	  "usage": {
	    "input_tokens": 1200,
	    "output_tokens": 850,
	    "cache_read_input_tokens": 45000,
	    "cache_creation_input_tokens": 1024,
	    "cache_creation": {"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":0}
	  }
	}`
	var r ChatResponse
	if err := r.UnmarshalJSON([]byte(in)); err != nil {
		t.Fatal(err)
	}
	if r.Usage == nil || r.Usage.CacheReadInputTokens == nil || *r.Usage.CacheReadInputTokens != 45000 {
		t.Fatalf("usage not typed: %+v", r.Usage)
	}
	if r.Usage.CacheCreation == nil || r.Usage.CacheCreation.Ephemeral5mInputTokens == nil || *r.Usage.CacheCreation.Ephemeral5mInputTokens != 1024 {
		t.Fatalf("cache_creation TTL detail not typed: %+v", r.Usage.CacheCreation)
	}
	out, err := r.MarshalJSON()
	if err != nil {
		t.Fatal(err)
	}
	assertJSONSemanticEqual(t, []byte(in), out)
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestChatResponse -v`
Expected: FAIL — `undefined: ChatResponse`

- [ ] **Step 3: 구현**

`pkg/schema/response.go`:
```go
package schema

import "encoding/json"

// Usage — budget 정산의 입력 (§5.3). cache 토큰은 TTL별 단가가 다르므로
// (5m=1.25x, 1h=2x) 반드시 구분 보존한다.
// 모든 수치는 *int64: upstream이 보낸 키만 재방출한다. message_delta
// usage는 output_tokens만 싣는 경우가 있고(no-omitempty면 키 추가 발생),
// 명시적 0("cache_creation_input_tokens":0)은 보존해야 한다(omitempty
// 값 타입이면 드랍) — 48d412d/3d5e050과 동일한 결함 계열의 선제 차단.
type Usage struct {
	InputTokens              *int64         `json:"input_tokens,omitempty"`
	OutputTokens             *int64         `json:"output_tokens,omitempty"`
	CacheReadInputTokens     *int64         `json:"cache_read_input_tokens,omitempty"`
	CacheCreationInputTokens *int64         `json:"cache_creation_input_tokens,omitempty"`
	CacheCreation            *CacheCreation `json:"cache_creation,omitempty"`
	Extra map[string]json.RawMessage `json:"-"`
}

type CacheCreation struct {
	Ephemeral5mInputTokens *int64 `json:"ephemeral_5m_input_tokens,omitempty"`
	Ephemeral1hInputTokens *int64 `json:"ephemeral_1h_input_tokens,omitempty"`
	Extra map[string]json.RawMessage `json:"-"`
}

func (u *Usage) UnmarshalJSON(data []byte) error {
	type plain Usage
	extra, err := unmarshalWithExtra(data, (*plain)(u),
		"input_tokens", "output_tokens", "cache_read_input_tokens",
		"cache_creation_input_tokens", "cache_creation")
	u.Extra = extra
	return err
}

func (u Usage) MarshalJSON() ([]byte, error) {
	type plain Usage
	return marshalWithExtra(plain(u), u.Extra)
}

func (c *CacheCreation) UnmarshalJSON(data []byte) error {
	type plain CacheCreation
	extra, err := unmarshalWithExtra(data, (*plain)(c),
		"ephemeral_5m_input_tokens", "ephemeral_1h_input_tokens")
	c.Extra = extra
	return err
}

func (c CacheCreation) MarshalJSON() ([]byte, error) {
	type plain CacheCreation
	return marshalWithExtra(plain(c), c.Extra)
}

// ChatResponse — canonical 비스트리밍 응답 (스트리밍 message_start의
// 골격이기도 하다). stop_reason/stop_sequence는 null 유의미 → 포인터.
type ChatResponse struct {
	ID           string          `json:"id"`
	Type         string          `json:"type"`
	Role         string          `json:"role"`
	Model        string          `json:"model"`
	Content      []ContentBlock  `json:"content"`
	StopReason   *string         `json:"stop_reason"`
	StopSequence *string         `json:"stop_sequence"`
	Usage        *Usage          `json:"usage,omitempty"`
	Extra map[string]json.RawMessage `json:"-"`
}

var chatResponseKnown = []string{
	"id", "type", "role", "model", "content",
	"stop_reason", "stop_sequence", "usage",
}

func (r *ChatResponse) UnmarshalJSON(data []byte) error {
	type plain ChatResponse
	extra, err := unmarshalWithExtra(data, (*plain)(r), chatResponseKnown...)
	r.Extra = extra
	return err
}

func (r ChatResponse) MarshalJSON() ([]byte, error) {
	type plain ChatResponse
	return marshalWithExtra(plain(r), r.Extra)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestChatResponse -v`
Expected: PASS

주의: `stop_reason: "end_turn"`, `stop_sequence: null` 왕복에서 null이
보존되는지 확인 — 포인터 + omitempty 없는 태그라 null이 그대로 나온다.

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/response.go pkg/schema/response_test.go
git commit -s -m "feat(schema): ChatResponse and Usage with TTL-split cache accounting"
```

---

### Task 7: ChatChunk (스트리밍 이벤트)

**Files:**
- Create: `pkg/schema/chunk.go`
- Test: `pkg/schema/chunk_test.go`

- [ ] **Step 1: 실패하는 테스트 작성**

`pkg/schema/chunk_test.go`:
```go
package schema

import "testing"

func TestChatChunkRoundTrip(t *testing.T) {
	cases := map[string]string{
		"message_start":  `{"type":"message_start","message":{"id":"msg_01","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":1200,"output_tokens":1}}}`,
		"block_start":    `{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`,
		"thinking_delta": `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"step"}}`,
		"text_delta":     `{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
		"input_json":     `{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`,
		"block_stop":     `{"type":"content_block_stop","index":0}`,
		"message_delta":  `{"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":1200,"output_tokens":850}}`,
		"message_stop":   `{"type":"message_stop"}`,
		"ping":           `{"type":"ping"}`,
	}
	for name, in := range cases {
		t.Run(name, func(t *testing.T) {
			var c ChatChunk
			if err := c.UnmarshalJSON([]byte(in)); err != nil {
				t.Fatal(err)
			}
			out, err := c.MarshalJSON()
			if err != nil {
				t.Fatal(err)
			}
			assertJSONSemanticEqual(t, []byte(in), out)
		})
	}
}
```

- [ ] **Step 2: 실패 확인**

Run: `go test ./pkg/schema/ -run TestChatChunk -v`
Expected: FAIL — `undefined: ChatChunk`

- [ ] **Step 3: 구현**

`pkg/schema/chunk.go`:
```go
package schema

import "encoding/json"

// ChatChunk — canonical 스트리밍 이벤트. Anthropic 이벤트 어휘를 그대로
// 채택한다 (message_start/content_block_start/content_block_delta/
// content_block_stop/message_delta/message_stop/ping/error).
// delta는 M1에서 raw 보존 — SSE 직렬화기(M2)는 재방출만 하고,
// OpenAI 변환(M5)에서 타입 승격한다. usage가 실린 message_delta가
// 정산의 진실원이다 (§5.3 드레인 정산).
type ChatChunk struct {
	Type         string          `json:"type"`
	Index        *int            `json:"index,omitempty"`
	Message      *ChatResponse   `json:"message,omitempty"`
	ContentBlock *ContentBlock   `json:"content_block,omitempty"`
	Delta        json.RawMessage `json:"delta,omitempty"`
	Usage        *Usage          `json:"usage,omitempty"`
	Extra map[string]json.RawMessage `json:"-"`
}

var chatChunkKnown = []string{
	"type", "index", "message", "content_block", "delta", "usage",
}

func (c *ChatChunk) UnmarshalJSON(data []byte) error {
	type plain ChatChunk
	extra, err := unmarshalWithExtra(data, (*plain)(c), chatChunkKnown...)
	c.Extra = extra
	return err
}

func (c ChatChunk) MarshalJSON() ([]byte, error) {
	type plain ChatChunk
	return marshalWithExtra(plain(c), c.Extra)
}
```

- [ ] **Step 4: 통과 확인**

Run: `go test ./pkg/schema/ -run TestChatChunk -v`
Expected: PASS (9개 서브테스트)

- [ ] **Step 5: 커밋**

```bash
git add pkg/schema/chunk.go pkg/schema/chunk_test.go
git commit -s -m "feat(schema): ChatChunk streaming events with raw delta preservation"
```

---

### Task 8: 골든 파일 하네스 + Claude Code 트래픽 픽스처

**Files:**
- Create: `pkg/schema/roundtrip_test.go`
- Create: `pkg/schema/testdata/roundtrip/request/claude-code-cache-multibreakpoint.json`
- Create: `pkg/schema/testdata/roundtrip/response/thinking-tool-use.json`
- Create: `pkg/schema/testdata/roundtrip/stream/streaming-tool-use.sse`

- [ ] **Step 1: 하네스 작성 (픽스처가 없으면 skip이 아닌 FAIL)**

`pkg/schema/roundtrip_test.go`:
```go
package schema

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGoldenRoundTrip — §2.2-1 무손실 불변식의 집행 지점.
// testdata/roundtrip/{request,response}/*.json 각각을 unmarshal→marshal
// 후 의미 동등성 비교. 픽스처를 추가하면 자동으로 검증 대상이 된다
// (M2 게이트에서 실제 Claude Code 캡처 트래픽을 여기에 추가).
func TestGoldenRoundTrip(t *testing.T) {
	kinds := map[string]func([]byte) ([]byte, error){
		"request": func(in []byte) ([]byte, error) {
			var v ChatRequest
			if err := v.UnmarshalJSON(in); err != nil {
				return nil, err
			}
			return v.MarshalJSON()
		},
		"response": func(in []byte) ([]byte, error) {
			var v ChatResponse
			if err := v.UnmarshalJSON(in); err != nil {
				return nil, err
			}
			return v.MarshalJSON()
		},
	}
	for kind, roundTrip := range kinds {
		files, err := filepath.Glob(filepath.Join("testdata", "roundtrip", kind, "*.json"))
		if err != nil || len(files) == 0 {
			t.Fatalf("%s: no golden fixtures found (err=%v)", kind, err)
		}
		for _, f := range files {
			t.Run(kind+"/"+filepath.Base(f), func(t *testing.T) {
				in, err := os.ReadFile(f)
				if err != nil {
					t.Fatal(err)
				}
				out, err := roundTrip(in)
				if err != nil {
					t.Fatal(err)
				}
				assertJSONSemanticEqual(t, in, out)
			})
		}
	}
}

// TestGoldenStreamRoundTrip — .sse 픽스처의 각 data: 라인을 ChatChunk로
// 왕복. 이벤트 순서는 파일 순서가 보증한다.
func TestGoldenStreamRoundTrip(t *testing.T) {
	files, err := filepath.Glob(filepath.Join("testdata", "roundtrip", "stream", "*.sse"))
	if err != nil || len(files) == 0 {
		t.Fatalf("stream: no golden fixtures found (err=%v)", err)
	}
	for _, f := range files {
		t.Run(filepath.Base(f), func(t *testing.T) {
			raw, err := os.ReadFile(f)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			sc := bufio.NewScanner(bytes.NewReader(raw))
			sc.Buffer(make([]byte, 1024*1024), 1024*1024)
			for sc.Scan() {
				line := sc.Text()
				if !strings.HasPrefix(line, "data: ") {
					continue
				}
				payload := []byte(strings.TrimPrefix(line, "data: "))
				var c ChatChunk
				if err := c.UnmarshalJSON(payload); err != nil {
					t.Fatalf("event %d: %v", n, err)
				}
				out, err := c.MarshalJSON()
				if err != nil {
					t.Fatalf("event %d: %v", n, err)
				}
				assertJSONSemanticEqual(t, payload, out)
				n++
			}
			if n == 0 {
				t.Fatal("no data: events in fixture")
			}
		})
	}
}
```

- [ ] **Step 2: 픽스처 작성 — cache 멀티 breakpoint 요청**

`pkg/schema/testdata/roundtrip/request/claude-code-cache-multibreakpoint.json`
(Claude Code 패턴: system 2블록 + tools + 마지막 user 메시지에 breakpoint,
1h TTL 혼용 — 한 줄 JSON으로 저장):
```json
{"model":"claude-sonnet-4-6","max_tokens":32000,"stream":true,"system":[{"type":"text","text":"You are Claude Code, Anthropic's official CLI for Claude.","cache_control":{"type":"ephemeral","ttl":"1h"}},{"type":"text","text":"# Project context\nrepo: inferplane...","cache_control":{"type":"ephemeral"}}],"tools":[{"name":"Bash","description":"Executes bash commands","input_schema":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]},"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"run the tests"}]},{"role":"assistant","content":[{"type":"thinking","thinking":"I should run go test.","signature":"EuYBCkQYAiJA"},{"type":"tool_use","id":"toolu_01A","name":"Bash","input":{"command":"go test ./..."}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01A","content":[{"type":"text","text":"ok  \tgithub.com/inferplane/inferplane/pkg/schema\t0.01s"}]},{"type":"text","text":"now commit","cache_control":{"type":"ephemeral"}}]}],"metadata":{"user_id":"session-abc123"}}
```

- [ ] **Step 3: 픽스처 작성 — thinking + tool_use 응답**

`pkg/schema/testdata/roundtrip/response/thinking-tool-use.json`:
```json
{"id":"msg_01XYZ","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"thinking","thinking":"The tests passed, now I need to commit.","signature":"EuYBCkQYAiJAkPq"},{"type":"text","text":"Tests pass. Committing now."},{"type":"tool_use","id":"toolu_02B","name":"Bash","input":{"command":"git commit -s -m \"feat: schema\""}}],"stop_reason":"tool_use","stop_sequence":null,"usage":{"input_tokens":5,"output_tokens":312,"cache_read_input_tokens":48230,"cache_creation_input_tokens":1024,"cache_creation":{"ephemeral_5m_input_tokens":1024,"ephemeral_1h_input_tokens":0}}}
```

- [ ] **Step 4: 픽스처 작성 — 스트리밍 tool_use SSE**

`pkg/schema/testdata/roundtrip/stream/streaming-tool-use.sse`
(Anthropic 이벤트 구조 그대로 — thinking → text → tool_use 순서):
```
event: message_start
data: {"type":"message_start","message":{"id":"msg_01S","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":4805,"output_tokens":2,"cache_read_input_tokens":44000,"cache_creation_input_tokens":0}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"User wants the file listed."}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"EuYBCkQYAiJA"}}

event: content_block_stop
data: {"type":"content_block_stop","index":0}

event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"Listing files."}}

event: content_block_stop
data: {"type":"content_block_stop","index":1}

event: content_block_start
data: {"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_03C","name":"Bash","input":{}}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"command\": \"ls"}}

event: content_block_delta
data: {"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":" -la\"}"}}

event: content_block_stop
data: {"type":"content_block_stop","index":2}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use","stop_sequence":null},"usage":{"input_tokens":4805,"output_tokens":89}}

event: message_stop
data: {"type":"message_stop"}
```

- [ ] **Step 5: 전체 테스트 실행**

Run: `go test ./pkg/schema/ -v`
Expected: PASS — 전체 (Extra/blocks/message/request/response/chunk/golden 2종)

- [ ] **Step 6: vet + 커밋**

```bash
go vet ./... && gofmt -l pkg/   # 출력 없어야 함
git add pkg/schema/roundtrip_test.go pkg/schema/testdata/
git commit -s -m "test(schema): golden round-trip harness with Claude Code traffic fixtures"
```

---

### Task 9: M1 게이트 — 스키마 리뷰 요청

- [ ] **Step 1: 전체 검증**

Run: `go test ./... && go vet ./...`
Expected: 전부 PASS, vet 클린

- [ ] **Step 2: 스키마 리뷰 요청**

사용자에게 다음 결정 사항을 명시해 리뷰 요청 (M1 게이트 — 이 승인이
M2 시작 조건):
1. canonical JSON 형태 = Anthropic Messages 기반 superset (M2 Anthropic 경로가 얇아짐)
2. "최소 타입화, 최대 보존" — system/tools/delta/tool_result.content는 raw, M5에서 승격
3. ContentBlock = 단일 구조체 태그드 유니언 (블록 타입별 인터페이스 대신)
4. Message의 string|array content 양형 보존 (shape fidelity)

---

## Self-Review 결과

- **스펙 커버리지**: §2.2 canonical 결정·불변식 1/2/3 → Task 2~8. §4.4 cache_control 보존 → Task 3/5/8 픽스처. §5.3 TTL별 cache 토큰 → Task 6. 거버넌스 파일 M1 첫 커밋 → Task 1. ✓
- **플레이스홀더**: 없음 — 모든 스텝에 실제 코드/픽스처/커맨드 포함. ✓
- **타입 일관성**: `unmarshalWithExtra(data, v, known...)` 시그니처가 Task 2 정의·Task 3~7 사용처 일치, `assertJSONSemanticEqual` Task 2 정의·전역 사용. ✓
- **알려진 한계 (의도)**: `jsonSemanticEqual`은 키 순서 무시 — Anthropic API는 키 순서 비의존이므로 안전. 블록 **배열 순서**는 슬라이스로 보존되며 이것이 §2.2-1의 본질.
