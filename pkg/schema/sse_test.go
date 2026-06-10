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
		if !jsonSemanticEqual(orig, []byte(gotData)) {
			t.Fatalf("event %d mismatch:\n got: %s\nwant: %s", n, gotData, orig)
		}
		n++
	}
	if n == 0 {
		t.Fatal("no events read from fixture")
	}
}
