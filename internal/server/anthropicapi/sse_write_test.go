package anthropicapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
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
	if !strings.HasPrefix(out, "event: content_block_start\n") {
		t.Fatalf("missing event line: %q", out)
	}
	if !strings.Contains(out, `"type":"content_block_start"`) || !strings.HasSuffix(out, "\n\n") {
		t.Fatalf("bad framing: %q", out)
	}
}

func ptr(s string) *string { return &s }

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
		var gotData string
		for _, l := range strings.Split(b.String(), "\n") {
			if strings.HasPrefix(l, "data: ") {
				gotData = strings.TrimPrefix(l, "data: ")
			}
		}
		if !jsonEqual(orig, []byte(gotData)) {
			t.Fatalf("event %d data mismatch:\n got: %s\nwant: %s", n, gotData, orig)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events read from fixture")
	}
}

func jsonEqual(a, b []byte) bool {
	var va, vb any
	if json.Unmarshal(a, &va) != nil || json.Unmarshal(b, &vb) != nil {
		return false
	}
	x, _ := json.Marshal(va)
	y, _ := json.Marshal(vb)
	return string(x) == string(y)
}
