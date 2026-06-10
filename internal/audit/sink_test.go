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
