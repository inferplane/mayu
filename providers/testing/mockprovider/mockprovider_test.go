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
