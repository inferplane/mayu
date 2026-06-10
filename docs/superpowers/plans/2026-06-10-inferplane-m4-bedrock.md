# inferplane M4 — Bedrock Provider 구현 계획

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Claude Code가 inferplane을 통해 Amazon Bedrock의 Claude 모델로 대화·스트리밍하고(InvokeModel native Anthropic 경로), 비-Claude 모델(Kimi/GLM/Nova)은 Converse로 라우팅되며, 프롬프트 캐시가 보존되고 thinking 블록 순서가 무손실 유지된다 (스펙: §4.3).

**Architecture:** `providers/bedrock/`는 코어 밖의 독립 패키지(`registry.go` 1줄). Bedrock 호출은 `aws-sdk-go-v2`(pure-Go)를 쓰되, **SDK 특화 코드는 얇은 어댑터(`awsClient`)에 격리**하고 provider 로직은 좁은 `invoker`/`converser` 인터페이스에만 의존한다 — 테스트는 fake를 주입해 실 AWS 자격 없이 전부 단위 검증한다. Claude=InvokeModel(본문에서 `model` 제거 + `anthropic_version` 주입, cache-relevant prefix 불변), 스트리밍은 Bedrock event stream payload(이미 Anthropic SSE event JSON)를 `ChatChunk`로 파싱 후 `schema.WriteAnthropicSSE`로 재직렬화해 `StreamEvent{Raw,Chunk}` 반환(ingress tee 무변경). 비-Claude=Converse 변환.

**Tech Stack:** Go 1.25+, `aws-sdk-go-v2`(`config` + `service/bedrockruntime` + `aws/...`), 표준 `encoding/json`. 테스트는 fake invoker/converser(no network). M3 위에 구축.

---

## M4 결정 기록 (승인된 설계 §4.3 기반)

- **의존성**: `aws-sdk-go-v2`. pure-Go·cgo 없음 → 단일 바이너리 유지.
- **SDK 격리**: `invoker`/`converser` 인터페이스 + 실 `awsClient` 어댑터 + 테스트 fake. provider 로직(본문 변형/스트림 재직렬화/Converse 변환)은 fake로 100% 단위 테스트; SDK 어댑터는 얇아서 게이트(실 AWS)에서만 통합 검증.
- **Claude = InvokeModel**: 본문 top-level에서 `model` 제거(modelId는 URL) + `anthropic_version:"bedrock-2023-05-31"` 주입. messages/system/tools는 `json.RawMessage`로 **바이트 불변** → 캐시 prefix 보존. `anthropic_beta`는 본문 필드로 보존.
- **스트리밍 = provider 내부 재직렬화**: event stream payload(Anthropic SSE event JSON) → `ChatChunk` → `schema.WriteAnthropicSSE` → `StreamEvent.Raw`(Anthropic SSE). ingress 무변경.
- **비-Claude = Converse**: canonical(Anthropic-shape) ↔ Converse 변환 + `additionalModelRequestFields`. M4는 text+system+기본 파라미터의 chat 경로(Kimi/GLM/Nova). Converse tool-calling 변환은 M5 하드닝(게이트 비포함).
- **Mantle**: config `api:mantle` 인식하되 M4는 invoke로 폴백 + 노트(§10 #2 spike, 게이트 외).
- **인증**: aws default credential chain. config `auth.mode: irsa|pod_identity|profile|static|default`.

## 마일스톤 로드맵 (전체 6개 중 4번)

| M | 범위 | 게이트 |
|---|---|---|
| M1✅ M2✅ M3✅ | (완료) | |
| **M4 (이 계획)** | bedrock provider (invoke/converse) | Claude Code→Bedrock Claude 실연동 + thinking 순서 골든 |
| M5 | rate limit/quota/budget + OpenAI ingress | OpenCode 실연동 + quota block + 비용 필드 |
| M6 | failover/메트릭/Helm/TLS/quickstart | docker run→5분 |

---

## 파일 구조

```
go.mod / go.sum                  # aws-sdk-go-v2/config, /service/bedrockruntime (+transitive)
pkg/schema/
  sse.go                         # WriteAnthropicSSE (ChatChunk→Anthropic SSE) — M2 sse_write.go 이전
  sse_test.go                    # (M2 골든 테스트 이전)
internal/server/anthropicapi/
  sse_write.go (삭제)            # pkg/schema로 이전
  sse_write_test.go (삭제/이전)
  messages.go (수정)             # WriteSSEEvent → schema.WriteAnthropicSSE 호출처 갱신 (있으면)
providers/bedrock/
  client.go                      # invoker/converser 인터페이스 + 실 awsClient 어댑터 + newAWSClient
  bedrock.go                     # provider: factory, registration, Name/Models, 라우팅, CountTokens
  invoke.go                      # InvokeModel 경로: 본문 변형 + Complete + Stream(재직렬화)
  invoke_test.go
  converse.go                    # Converse 경로: canonical↔Converse 변환 + Complete + Stream
  converse_test.go
  bedrock_test.go                # provider 라우팅/팩토리 (fake client)
  fake_test.go                   # fake invoker/converser
providers/register.go            # blank import bedrock (없으면 생성; main.go에 이미 있으면 거기에)
internal/config/config.go (수정) # Target에 API 필드(invoke_model|converse|mantle), ProviderConfig에 region/auth
cmd/inferplane/main.go (수정)    # bedrock blank import, provider 생성 시 region/auth 전달
examples/config.json (수정)      # bedrock provider 예시
```

> 레이어링: `providers/bedrock`는 `internal/server/...`를 import하지 않는다(코어 독립). SSE 직렬화기를 `pkg/schema`로 옮겨 양쪽이 공유한다 — 이것이 Task 1.

---

### Task 1: Anthropic SSE 직렬화기를 pkg/schema로 이전

**Files:**
- Create: `pkg/schema/sse.go`, `pkg/schema/sse_test.go`
- Delete: `internal/server/anthropicapi/sse_write.go`, `internal/server/anthropicapi/sse_write_test.go`
- Modify: any caller of `anthropicapi.WriteSSEEvent` (grep first)

bedrock provider가 SSE 직렬화기를 재사용하려면 코어 밖에서 import 가능한 위치에 있어야 한다. canonical 스키마(Anthropic-shape)의 wire 직렬화이므로 `pkg/schema`가 자연스러운 집.

- [ ] **Step 1: grep으로 현재 사용처 확인**

Run: `grep -rn "WriteSSEEvent" --include=*.go .`
Expected: `internal/server/anthropicapi/sse_write.go`(정의) + `sse_write_test.go`(테스트). messages.go가 호출하지 않으면(M2는 tee라 출력 경로 미사용) 호출처 갱신 불필요. 확인.

- [ ] **Step 2: pkg/schema/sse.go 생성 (함수명 WriteAnthropicSSE)**

`pkg/schema/sse.go`:
```go
package schema

import (
	"encoding/json"
	"io"
)

// WriteAnthropicSSE serializes a canonical ChatChunk into one Anthropic SSE
// event ("event: <type>\ndata: <json>\n\n"). Used by providers that receive a
// non-SSE wire format (e.g. Bedrock's event stream) and must re-emit canonical
// Anthropic SSE for the ingress tee, and (M5) by cross-protocol conversion.
func WriteAnthropicSSE(w io.Writer, chunk *ChatChunk) error {
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

- [ ] **Step 3: pkg/schema/sse_test.go — M2 골든 테스트 이전 (TestWriteSSEEvent + TestWriteSSEMatchesGoldenData)**

M2의 `internal/server/anthropicapi/sse_write_test.go` 내용을 `package schema`로 옮긴다. 변경점: 함수명 `WriteSSEEvent`→`WriteAnthropicSSE`; `schema.ChatChunk`→`ChatChunk`(같은 패키지); 골든 픽스처 경로 `filepath.Join("testdata","roundtrip","stream","streaming-tool-use.sse")`(같은 패키지라 상대 경로 단축); `ptr` 헬퍼가 같은 패키지의 다른 테스트와 충돌하면(grep `func ptr(` in pkg/schema) 이름을 `ptrStr`로. jsonEqual 헬퍼 포함.
```go
package schema

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteAnthropicSSEFraming(t *testing.T) {
	idx := 0
	s := ""
	chunk := &ChatChunk{Type: "content_block_start", Index: &idx,
		ContentBlock: &ContentBlock{Type: "text", Text: &s}}
	var b strings.Builder
	if err := WriteAnthropicSSE(&b, chunk); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.HasPrefix(out, "event: content_block_start\n") || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("bad framing: %q", out)
	}
	if !strings.Contains(out, `"type":"content_block_start"`) {
		t.Fatalf("missing type: %q", out)
	}
}

func TestWriteAnthropicSSEMatchesGolden(t *testing.T) {
	path := filepath.Join("testdata", "roundtrip", "stream", "streaming-tool-use.sse")
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
		var c ChatChunk
		if err := json.Unmarshal(orig, &c); err != nil {
			t.Fatalf("event %d parse: %v", n, err)
		}
		var b strings.Builder
		if err := WriteAnthropicSSE(&b, &c); err != nil {
			t.Fatal(err)
		}
		var gotData string
		for _, l := range strings.Split(b.String(), "\n") {
			if strings.HasPrefix(l, "data: ") {
				gotData = strings.TrimPrefix(l, "data: ")
			}
		}
		if !sseJSONEqual(orig, []byte(gotData)) {
			t.Fatalf("event %d mismatch:\n got: %s\nwant: %s", n, gotData, orig)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events read from fixture")
	}
}

func sseJSONEqual(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	x, _ := json.Marshal(va)
	y, _ := json.Marshal(vb)
	return string(x) == string(y)
}
```
주의: `pkg/schema/testdata/roundtrip/stream/streaming-tool-use.sse`는 M1에서 생성됨 — 경로 확인.

- [ ] **Step 4: anthropicapi sse_write.go + test 삭제, 호출처 갱신**

Step 1에서 messages.go가 WriteSSEEvent를 호출하지 않음을 확인했으면 삭제만:
```bash
git rm internal/server/anthropicapi/sse_write.go internal/server/anthropicapi/sse_write_test.go
```
호출처가 있으면 `schema.WriteAnthropicSSE`로 교체.

- [ ] **Step 5: 전체 통과 + 커밋**

Run: `go test ./... -v -run 'SSE|Golden'`, full `go test ./...`, `go vet ./...`, `gofmt -l .` → clean.
```bash
git add pkg/schema/sse.go pkg/schema/sse_test.go
git rm internal/server/anthropicapi/sse_write.go internal/server/anthropicapi/sse_write_test.go
git commit -s -m "refactor(schema): move Anthropic SSE serializer to pkg/schema (provider-shared)"
```

---

### Task 2: aws-sdk-go-v2 의존성 추가

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: 의존성 추가 (NETWORK REQUIRED)**

```bash
cd /home/atomoh/mayu
go get github.com/aws/aws-sdk-go-v2/config@latest
go get github.com/aws/aws-sdk-go-v2/service/bedrockruntime@latest
```
실패 시(네트워크) STOP → BLOCKED 보고.

- [ ] **Step 2: 빌드 확인 + 버전 기록**

```bash
go build ./... && echo ok
go list -m github.com/aws/aws-sdk-go-v2/service/bedrockruntime
```
indirect로 들어가지만 Task 3에서 import하면 direct화. `go mod tidy`는 Task 3 후에.

- [ ] **Step 3: 커밋**

```bash
git add go.mod go.sum
git commit -s -m "build: add aws-sdk-go-v2 (config + bedrockruntime) for Bedrock provider"
```

---

### Task 3: bedrock client 인터페이스 + 실 어댑터 + fake

**Files:**
- Create: `providers/bedrock/client.go`, `providers/bedrock/fake_test.go`

SDK 특화 코드를 격리하는 좁은 인터페이스. provider 로직은 이 인터페이스에만 의존 → fake로 단위 테스트.

- [ ] **Step 1: client.go — 인터페이스 + 실 어댑터**

`providers/bedrock/client.go`:
```go
// Package bedrock proxies to Amazon Bedrock. SDK-specific code is confined to
// awsClient (a thin adapter over aws-sdk-go-v2/service/bedrockruntime); the
// provider logic depends only on the narrow invoker/converser interfaces, so
// tests inject fakes and need no AWS credentials. Registered as type "bedrock".
package bedrock

import (
	"context"
	"iter"
)

// invoker is the InvokeModel surface (Claude native Anthropic path).
type invoker interface {
	// Invoke sends a body (Anthropic Messages JSON, model-less, with
	// anthropic_version) to modelID and returns the raw response body.
	Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error)
	// InvokeStream returns an iterator of raw event-stream payload chunks; each
	// chunk's bytes are an Anthropic SSE event JSON object.
	InvokeStream(ctx context.Context, modelID string, body []byte) (iter.Seq2[[]byte, error], error)
}

// converser is the Converse surface (non-Claude models).
type converser interface {
	Converse(ctx context.Context, modelID string, req ConverseRequest) (ConverseResponse, error)
	ConverseStream(ctx context.Context, modelID string, req ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error)
}

// ConverseRequest/Response/StreamEvent are our minimal, SDK-independent shapes
// (the adapter maps them to/from the SDK types). Kept small for M4.
type ConverseRequest struct {
	System   string                   // single system text (M4)
	Messages []ConverseMessage        // role + text
	Inference map[string]any          // maxTokens/temperature... (additionalModelRequestFields merged in adapter)
	ModelFields map[string]any        // additionalModelRequestFields
}
type ConverseMessage struct {
	Role string // "user" | "assistant"
	Text string
}
type ConverseResponse struct {
	Text         string
	StopReason   string
	InputTokens  int64
	OutputTokens int64
}
type ConverseStreamEvent struct {
	TextDelta  string
	Done       bool
	StopReason string
	InputTokens  int64
	OutputTokens int64
}
```

real adapter (same file): `awsClient` wraps `*bedrockruntime.Client`. The implementer MUST verify exact SDK signatures via `go doc github.com/aws/aws-sdk-go-v2/service/bedrockruntime` and `go doc .../bedrockruntime/types`. Reference shape (verify before coding):
```go
import (
	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"
	brtypes "github.com/aws/aws-sdk-go-v2/service/bedrockruntime/types"
)

type awsClient struct{ c *bedrockruntime.Client }

func newAWSClient(ctx context.Context, region, authMode string) (*awsClient, error) {
	opts := []func(*awsconfig.LoadOptions) error{}
	if region != "" {
		opts = append(opts, awsconfig.WithRegion(region))
	}
	if authMode == "profile" {
		// optional: WithSharedConfigProfile — verify env var or config field for profile name
	}
	cfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, err
	}
	return &awsClient{c: bedrockruntime.NewFromConfig(cfg)}, nil
}

func (a *awsClient) Invoke(ctx context.Context, modelID string, body []byte) ([]byte, error) {
	out, err := a.c.InvokeModel(ctx, &bedrockruntime.InvokeModelInput{
		ModelId:     aws.String(modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, err
	}
	return out.Body, nil
}

func (a *awsClient) InvokeStream(ctx context.Context, modelID string, body []byte) (iter.Seq2[[]byte, error], error) {
	out, err := a.c.InvokeModelWithResponseStream(ctx, &bedrockruntime.InvokeModelWithResponseStreamInput{
		ModelId:     aws.String(modelID),
		Body:        body,
		ContentType: aws.String("application/json"),
		Accept:      aws.String("application/json"),
	})
	if err != nil {
		return nil, err
	}
	return func(yield func([]byte, error) bool) {
		stream := out.GetStream()
		defer stream.Close()
		for ev := range stream.Events() {
			switch v := ev.(type) {
			case *brtypes.ResponseStreamMemberChunk:
				if !yield(v.Value.Bytes, nil) {
					return
				}
			}
		}
		if err := stream.Err(); err != nil {
			yield(nil, err)
		}
	}, nil
}
```
Converse adapter methods map ConverseRequest→`bedrockruntime.ConverseInput` (System []SystemContentBlock, Messages []Message with ContentBlockMemberText, InferenceConfig, AdditionalModelRequestFields as document.Interface) and ConverseOutput→ConverseResponse. ConverseStream similarly via ConverseStream + GetStream().Events() (types.ConverseStreamOutputMemberContentBlockDelta etc.). The implementer verifies these type names with `go doc`. NOTE: AdditionalModelRequestFields uses `document.NewLazyDocument(modelFields)` from `github.com/aws/smithy-go/document` — verify.

- [ ] **Step 2: fake_test.go — fake invoker/converser**

`providers/bedrock/fake_test.go`:
```go
package bedrock

import (
	"context"
	"iter"
)

// fakeInvoker records the body it received and returns canned responses —
// no AWS, no network.
type fakeInvoker struct {
	gotModelID string
	gotBody    []byte
	respBody   []byte
	streamRaw  [][]byte // each = an Anthropic SSE event JSON payload
	err        error
}

func (f *fakeInvoker) Invoke(_ context.Context, modelID string, body []byte) ([]byte, error) {
	f.gotModelID = modelID
	f.gotBody = append([]byte(nil), body...)
	return f.respBody, f.err
}
func (f *fakeInvoker) InvokeStream(_ context.Context, modelID string, body []byte) (iter.Seq2[[]byte, error], error) {
	f.gotModelID = modelID
	f.gotBody = append([]byte(nil), body...)
	if f.err != nil {
		return nil, f.err
	}
	return func(yield func([]byte, error) bool) {
		for _, b := range f.streamRaw {
			if !yield(b, nil) {
				return
			}
		}
	}, nil
}

type fakeConverser struct {
	resp       ConverseResponse
	streamEv   []ConverseStreamEvent
	gotReq     ConverseRequest
	gotModelID string
}

func (f *fakeConverser) Converse(_ context.Context, modelID string, req ConverseRequest) (ConverseResponse, error) {
	f.gotModelID = modelID
	f.gotReq = req
	return f.resp, nil
}
func (f *fakeConverser) ConverseStream(_ context.Context, modelID string, req ConverseRequest) (iter.Seq2[ConverseStreamEvent, error], error) {
	f.gotModelID = modelID
	f.gotReq = req
	return func(yield func(ConverseStreamEvent, error) bool) {
		for _, e := range f.streamEv {
			if !yield(e, nil) {
				return
			}
		}
	}, nil
}
```

- [ ] **Step 3: build + commit**

Run: `go build ./...` (client.go compiles against the real SDK — verifies the signatures you used exist), `go mod tidy` (bedrockruntime/config become direct), `go test ./providers/bedrock/` (no tests yet, builds), `gofmt -l .`, `go vet ./...`.
```bash
git add providers/bedrock/client.go providers/bedrock/fake_test.go go.mod go.sum
git commit -s -m "feat(bedrock): SDK-isolating invoker/converser interfaces + AWS adapter + fakes"
```

---

### Task 4: InvokeModel 본문 변형 (cache-safe)

**Files:**
- Create: `providers/bedrock/invoke.go` (transform 함수 먼저), `providers/bedrock/invoke_test.go`

- [ ] **Step 1: 실패 테스트 — 본문 변형**

`providers/bedrock/invoke_test.go`:
```go
package bedrock

import (
	"encoding/json"
	"testing"
)

func TestToInvokeBodyStripsModelAddsVersionPreservesCachePrefix(t *testing.T) {
	in := []byte(`{"model":"claude-sonnet-4-6","max_tokens":1024,"system":[{"type":"text","text":"sys","cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":[{"type":"text","text":"hi","cache_control":{"type":"ephemeral"}}]}]}`)
	out, err := toInvokeBody(in)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if _, has := m["model"]; has {
		t.Fatal("model must be stripped (it's in the URL for InvokeModel)")
	}
	if string(m["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatalf("anthropic_version not injected: %s", m["anthropic_version"])
	}
	// cache-relevant prefix (system/messages) bytes must be IDENTICAL to input
	var inMap map[string]json.RawMessage
	json.Unmarshal(in, &inMap)
	if string(m["system"]) != string(inMap["system"]) {
		t.Fatalf("system bytes mutated:\n got: %s\nwant: %s", m["system"], inMap["system"])
	}
	if string(m["messages"]) != string(inMap["messages"]) {
		t.Fatalf("messages bytes mutated:\n got: %s\nwant: %s", m["messages"], inMap["messages"])
	}
}

func TestToInvokeBodyKeepsExistingAnthropicVersion(t *testing.T) {
	// if a client already set anthropic_version, don't clobber a beta the user chose
	in := []byte(`{"model":"m","anthropic_version":"bedrock-2023-05-31","messages":[]}`)
	out, _ := toInvokeBody(in)
	var m map[string]json.RawMessage
	json.Unmarshal(out, &m)
	if string(m["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatalf("version: %s", m["anthropic_version"])
	}
}
```

- [ ] **Step 2: 실패 확인** Run: `go test ./providers/bedrock/ -run TestToInvokeBody -v` → FAIL `undefined: toInvokeBody`

- [ ] **Step 3: 구현 (invoke.go 일부)**

`providers/bedrock/invoke.go` (transform 부분만 먼저):
```go
package bedrock

import "encoding/json"

const bedrockAnthropicVersion = `"bedrock-2023-05-31"`

// toInvokeBody rewrites an Anthropic Messages request body for Bedrock
// InvokeModel: drop top-level "model" (it goes in the URL) and inject
// "anthropic_version". Parsing only the TOP LEVEL into json.RawMessage keeps
// every system/messages/tools VALUE byte-identical, so the prompt-cache prefix
// is preserved (§4.4). Top-level key order is irrelevant to the cache.
func toInvokeBody(raw []byte) ([]byte, error) {
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		return nil, err
	}
	delete(top, "model")
	if _, has := top["anthropic_version"]; !has {
		top["anthropic_version"] = json.RawMessage(bedrockAnthropicVersion)
	}
	return json.Marshal(top)
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/bedrock/ -run TestToInvokeBody -v` → PASS.
```bash
git add providers/bedrock/invoke.go providers/bedrock/invoke_test.go
git commit -s -m "feat(bedrock): cache-safe InvokeModel body transform (strip model, inject version)"
```

---

### Task 5: bedrock provider — Complete (InvokeModel, Claude)

**Files:**
- Create: `providers/bedrock/bedrock.go`
- Modify: `providers/bedrock/invoke.go` (completeInvoke), `providers/bedrock/invoke_test.go`

- [ ] **Step 1: 실패 테스트 — Complete via invoke**

`providers/bedrock/invoke_test.go`에 추가:
```go
import (
	"context"

	"github.com/inferplane/inferplane/providers"
)

func TestProviderCompleteInvoke(t *testing.T) {
	fi := &fakeInvoker{respBody: []byte(`{"id":"msg_b","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":7,"output_tokens":2}}`)}
	p := &provider{inv: fi, modelAPI: map[string]string{}}
	raw := []byte(`{"model":"claude-sonnet-4-6","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6", Upstream: "anthropic.claude-sonnet-4-6-v1:0", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.InputTokens != 7 {
		t.Fatalf("resp: %+v", resp.Parsed)
	}
	// the invoker must have received the URL modelId and a model-less, versioned body
	if fi.gotModelID != "anthropic.claude-sonnet-4-6-v1:0" {
		t.Fatalf("modelID: %q", fi.gotModelID)
	}
	var sent map[string]json.RawMessage
	json.Unmarshal(fi.gotBody, &sent)
	if _, has := sent["model"]; has {
		t.Fatal("sent body still has model")
	}
	if string(sent["anthropic_version"]) != `"bedrock-2023-05-31"` {
		t.Fatal("sent body missing anthropic_version")
	}
}
```

- [ ] **Step 2: 실패 확인** → `undefined: provider`

- [ ] **Step 3: bedrock.go provider 골격 + invoke.go completeInvoke**

`providers/bedrock/bedrock.go`:
```go
package bedrock

import (
	"context"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

type provider struct {
	inv      invoker
	conv     converser
	modelAPI map[string]string // upstream modelId → "invoke_model"|"converse"|"mantle"
}

func (p *provider) Name() string                  { return "bedrock" }
func (p *provider) Models() []schema.ModelInfo     { return nil }

// apiFor decides invoke vs converse. Default: Claude models → invoke_model,
// others → converse. Explicit per-model config overrides. "mantle" falls back
// to invoke_model in M4 (§10 #2 spike deferred).
func (p *provider) apiFor(upstream string) string {
	if a, ok := p.modelAPI[upstream]; ok && a != "" {
		if a == "mantle" {
			return "invoke_model" // M4: fallback
		}
		return a
	}
	if strings.Contains(upstream, "anthropic.") || strings.Contains(upstream, "claude") {
		return "invoke_model"
	}
	return "converse"
}

func (p *provider) Complete(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	if p.apiFor(req.Upstream) == "converse" {
		return p.completeConverse(ctx, req)
	}
	return p.completeInvoke(ctx, req)
}

func (p *provider) Stream(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	if p.apiFor(req.Upstream) == "converse" {
		return p.streamConverse(ctx, req)
	}
	return p.streamInvoke(ctx, req)
}

var _ providers.Provider = (*provider)(nil)
```

`providers/bedrock/invoke.go`에 completeInvoke 추가:
```go
import (
	"context"
	"fmt"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func (p *provider) completeInvoke(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	body, err := toInvokeBody(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	respBody, err := p.inv.Invoke(ctx, req.Upstream, body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke: %w", err)
	}
	out := &providers.ProxyResponse{StatusCode: 200, RawBody: respBody}
	var parsed schema.ChatResponse
	if json.Unmarshal(respBody, &parsed) == nil {
		out.Parsed = &parsed
	}
	return out, nil
}
```
(invoke.go의 import에 encoding/json은 이미 있음 — context/fmt/schema/providers 추가.)

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/bedrock/ -run 'TestProviderCompleteInvoke|TestToInvokeBody' -v` → PASS.
```bash
git add providers/bedrock/bedrock.go providers/bedrock/invoke.go providers/bedrock/invoke_test.go
git commit -s -m "feat(bedrock): provider Complete via InvokeModel (Claude native path)"
```

---

### Task 6: bedrock provider — Stream (InvokeModel, event-stream → Anthropic SSE)

**Files:**
- Modify: `providers/bedrock/invoke.go` (streamInvoke), `providers/bedrock/invoke_test.go`

- [ ] **Step 1: 실패 테스트 — 스트림 재직렬화 + thinking 순서 보존**

`providers/bedrock/invoke_test.go`에 추가:
```go
import (
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
)

func TestStreamInvokeReserializesToAnthropicSSE(t *testing.T) {
	// Bedrock event-stream payloads are Anthropic SSE event JSON. Provider must
	// re-emit them as Anthropic SSE bytes (Raw) + parsed Chunk, preserving the
	// thinking→text block ORDER.
	payloads := [][]byte{
		[]byte(`{"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"x","content":[],"stop_reason":null,"stop_sequence":null,"usage":{"input_tokens":3,"output_tokens":1}}}`),
		[]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","signature":""}}`),
		[]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"reason"}}`),
		[]byte(`{"type":"content_block_stop","index":0}`),
		[]byte(`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`),
		[]byte(`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"answer"}}`),
		[]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn","stop_sequence":null},"usage":{"input_tokens":3,"output_tokens":9}}`),
		[]byte(`{"type":"message_stop"}`),
	}
	fi := &fakeInvoker{streamRaw: payloads}
	p := &provider{inv: fi, modelAPI: map[string]string{}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "claude-sonnet-4-6", Upstream: "anthropic.claude-sonnet-4-6-v1:0", RawBody: []byte(`{"model":"m","stream":true,"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	var types []string
	var lastOut int64
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
		if ev.Chunk != nil {
			types = append(types, ev.Chunk.Type)
			if ev.Chunk.Usage != nil && ev.Chunk.Usage.OutputTokens != nil {
				lastOut = *ev.Chunk.Usage.OutputTokens
			}
		}
	}
	// Raw must be valid Anthropic SSE
	if !strings.Contains(sse.String(), "event: message_start\n") || !strings.Contains(sse.String(), "event: message_stop\n") {
		t.Fatalf("not Anthropic SSE: %s", sse.String())
	}
	// thinking block (index 0) must precede text block (index 1)
	joined := strings.Join(types, ",")
	wantOrder := "message_start,content_block_start,content_block_delta,content_block_stop,content_block_start,content_block_delta,message_delta,message_stop"
	if joined != wantOrder {
		t.Fatalf("block order broken:\n got: %s\nwant: %s", joined, wantOrder)
	}
	if lastOut != 9 {
		t.Fatalf("usage: %d", lastOut)
	}
	// verify the thinking delta is before the text delta in the raw SSE
	ti := strings.Index(sse.String(), "thinking_delta")
	xi := strings.Index(sse.String(), "text_delta")
	if ti < 0 || xi < 0 || ti > xi {
		t.Fatalf("thinking must precede text in SSE output")
	}
}
```

- [ ] **Step 2: 실패 확인** → `p.Stream` exists but `streamInvoke` undefined.

- [ ] **Step 3: 구현 (invoke.go streamInvoke)**

```go
func (p *provider) streamInvoke(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	body, err := toInvokeBody(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke body: %w", err)
	}
	payloads, err := p.inv.InvokeStream(ctx, req.Upstream, body)
	if err != nil {
		return nil, fmt.Errorf("bedrock: invoke stream: %w", err)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		for payload, perr := range payloads {
			if perr != nil {
				yield(nil, perr)
				return
			}
			ev := &providers.StreamEvent{}
			var c schema.ChatChunk
			if json.Unmarshal(payload, &c) == nil {
				ev.Chunk = &c
				// re-serialize the parsed chunk as canonical Anthropic SSE
				var b strings.Builder
				if werr := schema.WriteAnthropicSSE(&b, &c); werr == nil {
					ev.Raw = []byte(b.String())
				}
			}
			if ev.Raw == nil {
				// unparseable payload: emit verbatim as a data line (defensive)
				ev.Raw = append(append([]byte("event: unknown\ndata: "), payload...), '\n', '\n')
			}
			if !yield(ev, nil) {
				return
			}
		}
	}, nil
}
```
(invoke.go import에 `iter`, `strings` 추가.)

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/bedrock/ -v` → PASS (thinking 순서 보존 확인).
```bash
git add providers/bedrock/invoke.go providers/bedrock/invoke_test.go
git commit -s -m "feat(bedrock): streaming InvokeModel — event-stream payloads to Anthropic SSE tee"
```

---

### Task 7: Converse 경로 (비-Claude) — 변환 + Complete + Stream

**Files:**
- Create: `providers/bedrock/converse.go`, `providers/bedrock/converse_test.go`

M4 범위: text 메시지 + system + 기본 파라미터(max_tokens). Converse tool-calling 변환은 M5 하드닝(노트). 게이트 비포함.

- [ ] **Step 1: 실패 테스트 — canonical→ConverseRequest 변환 + Complete + Stream**

`providers/bedrock/converse_test.go`:
```go
package bedrock

import (
	"context"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestCanonicalToConverseExtractsTextAndSystem(t *testing.T) {
	raw := []byte(`{"model":"moonshot.kimi-k2","max_tokens":256,"system":[{"type":"text","text":"be brief"}],"messages":[{"role":"user","content":[{"type":"text","text":"hello"}]},{"role":"assistant","content":"hi"}],"model_fields":{"top_k":40}}`)
	cr, err := toConverseRequest(raw)
	if err != nil {
		t.Fatal(err)
	}
	if cr.System != "be brief" {
		t.Fatalf("system: %q", cr.System)
	}
	if len(cr.Messages) != 2 || cr.Messages[0].Role != "user" || cr.Messages[0].Text != "hello" || cr.Messages[1].Text != "hi" {
		t.Fatalf("messages: %+v", cr.Messages)
	}
	if cr.ModelFields["top_k"].(float64) != 40 {
		t.Fatalf("model_fields not carried: %+v", cr.ModelFields)
	}
}

func TestProviderCompleteConverse(t *testing.T) {
	fc := &fakeConverser{resp: ConverseResponse{Text: "brief answer", StopReason: "end_turn", InputTokens: 5, OutputTokens: 3}}
	p := &provider{conv: fc, modelAPI: map[string]string{"moonshot.kimi-k2": "converse"}}
	raw := []byte(`{"model":"kimi-k2","messages":[{"role":"user","content":"q"}]}`)
	resp, err := p.Complete(context.Background(), &providers.ProxyRequest{Model: "kimi-k2", Upstream: "moonshot.kimi-k2", RawBody: raw})
	if err != nil {
		t.Fatal(err)
	}
	// the converse response must be rendered back into an Anthropic-shaped body
	if resp.StatusCode != 200 || !strings.Contains(string(resp.RawBody), "brief answer") {
		t.Fatalf("resp body: %s", resp.RawBody)
	}
	if resp.Parsed == nil || resp.Parsed.Usage == nil || *resp.Parsed.Usage.OutputTokens != 3 {
		t.Fatalf("usage: %+v", resp.Parsed)
	}
}

func TestProviderStreamConverse(t *testing.T) {
	fc := &fakeConverser{streamEv: []ConverseStreamEvent{
		{TextDelta: "par"}, {TextDelta: "tial"}, {Done: true, StopReason: "end_turn", InputTokens: 5, OutputTokens: 4},
	}}
	p := &provider{conv: fc, modelAPI: map[string]string{"glm.glm-4": "converse"}}
	seq, err := p.Stream(context.Background(), &providers.ProxyRequest{Model: "glm-4", Upstream: "glm.glm-4", RawBody: []byte(`{"messages":[]}`), Stream: true})
	if err != nil {
		t.Fatal(err)
	}
	var sse strings.Builder
	for ev, err := range seq {
		if err != nil {
			t.Fatal(err)
		}
		sse.WriteString(string(ev.Raw))
	}
	s := sse.String()
	// must produce a well-formed Anthropic SSE sequence carrying the deltas
	if !strings.Contains(s, "event: message_start") || !strings.Contains(s, "event: message_stop") ||
		!strings.Contains(s, "partial") {
		t.Fatalf("converse stream not rendered as Anthropic SSE: %s", s)
	}
}
```

- [ ] **Step 2: 실패 확인** → `undefined: toConverseRequest`

- [ ] **Step 3: 구현 (converse.go)**

`providers/bedrock/converse.go` — canonical(Anthropic) → ConverseRequest, ConverseResponse → Anthropic ChatResponse, ConverseStream → Anthropic SSE chunks. Uses schema types. The implementer writes:
- `toConverseRequest(raw []byte) (ConverseRequest, error)`: parse Anthropic body; system = concat of system text blocks (or string); messages = flatten each message's text content to ConverseMessage{Role,Text} (M4: text only; if a message has non-text blocks, concatenate text parts, ignore others with a note); carry `max_tokens`→Inference, `model_fields`→ModelFields.
- `completeConverse`: toConverseRequest → conv.Converse → build a schema.ChatResponse{Role:"assistant", Content:[text block], StopReason, Usage} → marshal to RawBody + set Parsed.
- `streamConverse`: emit a synthetic Anthropic SSE sequence — message_start, content_block_start(text,index0), content_block_delta(text_delta) per TextDelta event, content_block_stop, message_delta(stop_reason+usage on Done), message_stop. Each via schema.WriteAnthropicSSE into StreamEvent.Raw + the ChatChunk in .Chunk.

Provide the full converse.go (the implementer copies; types from schema: ChatResponse, ContentBlock{Type,Text *string}, Usage{InputTokens,OutputTokens *int64}, ChatChunk{Type,Index *int,Message,ContentBlock,Delta json.RawMessage,Usage}). Build the synthetic chunks with the canonical schema types and WriteAnthropicSSE.

Skeleton (implementer completes to pass tests):
```go
package bedrock

import (
	"context"
	"encoding/json"
	"fmt"
	"iter"
	"strings"

	"github.com/inferplane/inferplane/pkg/schema"
	"github.com/inferplane/inferplane/providers"
)

func toConverseRequest(raw []byte) (ConverseRequest, error) {
	var body struct {
		MaxTokens   *int64          `json:"max_tokens"`
		System      json.RawMessage `json:"system"`
		Messages    []schema.Message `json:"messages"`
		ModelFields map[string]any  `json:"model_fields"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return ConverseRequest{}, err
	}
	cr := ConverseRequest{Inference: map[string]any{}, ModelFields: body.ModelFields}
	if body.MaxTokens != nil {
		cr.Inference["maxTokens"] = *body.MaxTokens
	}
	cr.System = systemText(body.System)
	for _, m := range body.Messages {
		cr.Messages = append(cr.Messages, ConverseMessage{Role: m.Role, Text: messageText(m)})
	}
	return cr, nil
}

// systemText extracts plain text from an Anthropic system field (string or
// array of text blocks). M4: text only.
func systemText(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	if raw[0] == '"' {
		var s string
		json.Unmarshal(raw, &s)
		return s
	}
	var blocks []schema.ContentBlock
	json.Unmarshal(raw, &blocks)
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func messageText(m schema.Message) string {
	var parts []string
	for _, b := range m.Content {
		if b.Type == "text" && b.Text != nil {
			parts = append(parts, *b.Text)
		}
	}
	return strings.Join(parts, "")
}

func (p *provider) completeConverse(ctx context.Context, req *providers.ProxyRequest) (*providers.ProxyResponse, error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	cresp, err := p.conv.Converse(ctx, req.Upstream, cr)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse: %w", err)
	}
	txt := cresp.Text
	stop := cresp.StopReason
	in, out := cresp.InputTokens, cresp.OutputTokens
	resp := &schema.ChatResponse{
		Type: "message", Role: "assistant", Model: req.Model,
		Content:    []schema.ContentBlock{{Type: "text", Text: &txt}},
		StopReason: &stop,
		Usage:      &schema.Usage{InputTokens: &in, OutputTokens: &out},
	}
	rawBody, _ := json.Marshal(resp)
	return &providers.ProxyResponse{StatusCode: 200, RawBody: rawBody, Parsed: resp}, nil
}

func (p *provider) streamConverse(ctx context.Context, req *providers.ProxyRequest) (iter.Seq2[*providers.StreamEvent, error], error) {
	cr, err := toConverseRequest(req.RawBody)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse req: %w", err)
	}
	evs, err := p.conv.ConverseStream(ctx, req.Upstream, cr)
	if err != nil {
		return nil, fmt.Errorf("bedrock: converse stream: %w", err)
	}
	return func(yield func(*providers.StreamEvent, error) bool) {
		idx := 0
		emit := func(c *schema.ChatChunk) bool {
			var b strings.Builder
			schema.WriteAnthropicSSE(&b, c)
			return yield(&providers.StreamEvent{Raw: []byte(b.String()), Chunk: c}, nil)
		}
		empty := ""
		if !emit(&schema.ChatChunk{Type: "message_start", Message: &schema.ChatResponse{Type: "message", Role: "assistant", Model: req.Model}}) {
			return
		}
		if !emit(&schema.ChatChunk{Type: "content_block_start", Index: &idx, ContentBlock: &schema.ContentBlock{Type: "text", Text: &empty}}) {
			return
		}
		for e, eerr := range evs {
			if eerr != nil {
				yield(nil, eerr)
				return
			}
			if e.Done {
				if !emit(&schema.ChatChunk{Type: "content_block_stop", Index: &idx}) {
					return
				}
				in, out := e.InputTokens, e.OutputTokens
				stop := e.StopReason
				delta, _ := json.Marshal(map[string]any{"stop_reason": stop, "stop_sequence": nil})
				if !emit(&schema.ChatChunk{Type: "message_delta", Delta: delta, Usage: &schema.Usage{InputTokens: &in, OutputTokens: &out}}) {
					return
				}
				emit(&schema.ChatChunk{Type: "message_stop"})
				return
			}
			td := e.TextDelta
			delta, _ := json.Marshal(map[string]any{"type": "text_delta", "text": td})
			if !emit(&schema.ChatChunk{Type: "content_block_delta", Index: &idx, Delta: delta}) {
				return
			}
		}
	}, nil
}
```

- [ ] **Step 4: 통과 + 커밋**

Run: `go test ./providers/bedrock/ -v` → PASS (converse 변환 + complete + stream).
```bash
git add providers/bedrock/converse.go providers/bedrock/converse_test.go
git commit -s -m "feat(bedrock): Converse path for non-Claude models (text chat; tools M5)"
```

---

### Task 8: factory + registration + CountTokens + config + main 와이어

**Files:**
- Modify: `providers/bedrock/bedrock.go` (factory, registration, CountTokens), `internal/config/config.go` (Target.API, ProviderConfig.Region/Auth), `cmd/inferplane/main.go` (blank import + region/auth), `examples/config.json`
- Test: `providers/bedrock/bedrock_test.go`

- [ ] **Step 1: 실패 테스트 — factory routing + registration**

`providers/bedrock/bedrock_test.go`:
```go
package bedrock

import (
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestRegisteredAsBedrock(t *testing.T) {
	// init() should have registered "bedrock"; New must construct (real AWS
	// client construction may fail without creds — we only assert the type is
	// known, by checking factory presence via a config with no network call).
	// Use a recover-safe construction: newAWSClient may error offline, which is fine.
	_, err := providers.New(providers.Config{Type: "bedrock", Settings: map[string]string{"region": "us-west-2"}})
	// either constructs or errors on AWS config — but must NOT be "unknown provider type"
	if err != nil && err.Error() == `providers: unknown provider type "bedrock"` {
		t.Fatal("bedrock not registered")
	}
}

func TestApiForRouting(t *testing.T) {
	p := &provider{modelAPI: map[string]string{"glm.glm-4": "converse", "x.mantle-model": "mantle"}}
	if p.apiFor("anthropic.claude-sonnet-4-6-v1:0") != "invoke_model" {
		t.Fatal("claude → invoke_model")
	}
	if p.apiFor("glm.glm-4") != "converse" {
		t.Fatal("explicit converse override")
	}
	if p.apiFor("x.mantle-model") != "invoke_model" {
		t.Fatal("mantle → invoke fallback (M4)")
	}
	if p.apiFor("moonshot.kimi-k2") != "converse" {
		t.Fatal("non-claude default → converse")
	}
}
```

- [ ] **Step 2: 실패 확인** → registration/factory undefined.

- [ ] **Step 3: bedrock.go에 factory + init + CountTokens**

```go
func init() { providers.Register("bedrock", factory) }

func factory(cfg providers.Config) (providers.Provider, error) {
	region := cfg.Settings["region"]
	authMode := cfg.Settings["auth_mode"]
	ac, err := newAWSClient(context.Background(), region, authMode)
	if err != nil {
		return nil, fmt.Errorf("bedrock: aws config: %w", err)
	}
	// modelAPI is populated from per-target config via Settings JSON or left
	// empty (default routing). M4: read a "model_api" json blob from Settings.
	modelAPI := map[string]string{}
	if raw := cfg.Settings["model_api"]; raw != "" {
		_ = json.Unmarshal([]byte(raw), &modelAPI)
	}
	return &provider{inv: ac, conv: ac, modelAPI: modelAPI}, nil
}
```
(bedrock.go import에 context, fmt, encoding/json 추가. `awsClient`가 invoker+converser 둘 다 구현하므로 inv=conv=ac.)
CountTokens: Bedrock의 CountTokens는 가용성이 모델/리전에 따라 다르므로 M4는 **TokenCounter 미구현**(ingress의 추정기 폴백 사용) — 단순화. (게이트의 count_tokens는 anthropic-direct provider가 처리; bedrock 경유 count_tokens는 추정기로. M5/§10 #2에서 Bedrock CountTokens spike.) 따라서 CountTokens 메서드를 추가하지 않는다.

- [ ] **Step 4: config.go — Target.API + ProviderConfig.Region/Auth**

`internal/config/config.go`:
- `Target`에 `API string json:"api,omitempty"` 추가 (invoke_model|converse|mantle).
- `ProviderConfig`에 `Region string json:"region,omitempty"`, `Auth struct{Mode string json:"mode"} json:"auth,omitempty"` 추가.
- provider 생성 시(main.go) Settings에 region/auth_mode/model_api를 채워 전달.

- [ ] **Step 5: main.go — bedrock blank import + Settings 채우기**

`cmd/inferplane/main.go`:
- `_ "github.com/inferplane/inferplane/providers/bedrock"` blank import 추가.
- provider 생성 루프에서 bedrock이면 `providers.Config{Type, Settings: map[string]string{"region": pc.Region, "auth_mode": pc.Auth.Mode, "model_api": <marshal of model→api from cfg.Models targets>}}`. model_api는 cfg.Models를 순회해 이 provider를 가리키는 target의 {upstream-model: api}를 모은 JSON.

- [ ] **Step 6: examples/config.json — bedrock 예시 추가**

providers에 `"bedrock-us": {"type":"bedrock","region":"us-west-2","auth":{"mode":"irsa"}}`, models에 `"claude-sonnet-4-6-bedrock": {"targets":[{"provider":"bedrock-us","model":"anthropic.claude-sonnet-4-6-v1:0","api":"invoke_model"}]}` 추가(예시).

- [ ] **Step 7: 빌드 + 전체 통과 + 커밋**

Run: `go build ./... && go test ./... -race && go vet ./... && gofmt -l .` → clean. `go build -o /tmp/ip-m4 ./cmd/inferplane && /tmp/ip-m4 2>&1 | head -1` (usage). rm.
```bash
git add providers/bedrock/bedrock.go providers/bedrock/bedrock_test.go internal/config/config.go cmd/inferplane/main.go examples/config.json
git commit -s -m "feat(bedrock): factory/registration, config (region/auth/api), main wiring"
```

---

### Task 9: M4 게이트 — 실제 Bedrock 연동 (수동)

- [ ] **Step 1: config — bedrock provider + Claude 모델**

`config.json` providers에 bedrock-us(region, auth irsa/profile), models에 Claude를 anthropic.claude-* invoke_model로 매핑.

- [ ] **Step 2: 실행 + Claude Code 연결**

AWS 자격(IRSA/profile/env) 준비 → `inferplane serve`. Claude Code를 ANTHROPIC_BASE_URL로 붙이고 bedrock Claude 모델 요청.

- [ ] **Step 3: 게이트 체크리스트**

- [ ] Claude Code → inferplane → Bedrock Claude 대화 동작 (invoke_model 경로).
- [ ] 스트리밍 동작 (event stream → Anthropic SSE 재직렬화).
- [ ] **thinking 블록 순서 보존** (골든 테스트 + 실연동에서 thinking→text 순서, Converse 미경유 확인).
- [ ] cache_control 보존 (invoke 본문 변형이 prefix 불변 → cache_read_input_tokens > 0).
- [ ] anthropic_beta 헤더/필드 보존.
- [ ] (선택) 비-Claude 모델(Kimi/GLM) → converse 경로 동작.

- [ ] **Step 4: 통과 기록** — 전부 통과 시 M4 완료.

---

## Self-Review 결과

- **스펙 커버리지**: §4.3 Claude=invoke_model(본문 변형, cache 보존) → Task 4/5. 스트리밍 event-stream→Anthropic SSE 재직렬화(thinking 순서) → Task 6. 비-Claude=Converse → Task 7. Mantle=invoke 폴백 → Task 8 apiFor. 인증=aws chain → Task 3/8. provider 코어 독립 + registry 1줄 → Task 8. SSE 직렬화기 공유 → Task 1. ✓
- **플레이스홀더**: Task 3(SDK 어댑터)/Task 7(converse.go)은 일부 "구현자가 go doc로 SDK 시그니처 확인" 위임 — aws-sdk-go-v2 타입명은 버전별로 달라 verbatim 고정이 위험하므로 의도적. 단위 테스트는 fake 인터페이스로 100% 명시. provider 로직 코드는 전부 명시. ✓
- **타입 일관성**: `invoker`/`converser`/`ConverseRequest`/`Response`/`StreamEvent`(Task 3) → Task 5/6/7 사용처 일치. `provider{inv,conv,modelAPI}`(Task 5) → Task 7/8 일치. `toInvokeBody`(Task 4) → Task 5/6. `schema.WriteAnthropicSSE`(Task 1) → Task 6/7. `providers.ProxyRequest/Response/StreamEvent`(M2) 일치. `schema.ChatResponse/ChatChunk/Usage/ContentBlock` 포인터 필드(M1) 일치. ✓
- **알려진 한계 (의도)**: M4 Converse는 text+system+max_tokens만(tool-calling 변환은 M5). Bedrock CountTokens 미구현(추정기 폴백, §10 #2 spike). Mantle invoke 폴백. SDK 어댑터는 게이트(실 AWS)에서만 통합 검증; 단위는 fake. 비스트리밍 Converse는 단일 text 블록 응답으로 렌더(멀티 블록 비-Claude 응답은 M5).
