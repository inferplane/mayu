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
