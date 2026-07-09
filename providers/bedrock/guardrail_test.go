package bedrock

import (
	"context"
	"testing"

	"github.com/inferplane/inferplane/providers"
)

const mockInvokeResp = `{"id":"msg_b","type":"message","role":"assistant","model":"m","content":[],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":1}}`

func TestGuardrail_VersionOrDraft(t *testing.T) {
	if got := (Guardrail{ID: "gr1"}).versionOrDraft(); got != "DRAFT" {
		t.Fatalf("empty version = %q, want DRAFT", got)
	}
	if got := (Guardrail{ID: "gr1", Version: "3"}).versionOrDraft(); got != "3" {
		t.Fatalf("explicit version = %q, want 3", got)
	}
}

func TestBuildGuardrailConfig_ZeroValueIsNil(t *testing.T) {
	if buildGuardrailConfig(Guardrail{}) != nil {
		t.Fatal("zero-value Guardrail must produce a nil GuardrailConfiguration")
	}
	if buildGuardrailStreamConfig(Guardrail{}) != nil {
		t.Fatal("zero-value Guardrail must produce a nil GuardrailStreamConfiguration")
	}
}

func TestBuildGuardrailConfig_SetsIdentifierAndVersion(t *testing.T) {
	cfg := buildGuardrailConfig(Guardrail{ID: "gr1", Version: "2"})
	if cfg == nil || *cfg.GuardrailIdentifier != "gr1" || *cfg.GuardrailVersion != "2" {
		t.Fatalf("got %+v", cfg)
	}
	streamCfg := buildGuardrailStreamConfig(Guardrail{ID: "gr1"})
	if streamCfg == nil || *streamCfg.GuardrailIdentifier != "gr1" || *streamCfg.GuardrailVersion != "DRAFT" {
		t.Fatalf("got %+v", streamCfg)
	}
}

// TestProviderDefaultGuardrail_ReachesInvokeAndConverse proves the
// provider-level default (from Settings, factory-populated) reaches all four
// call paths: Invoke, InvokeStream, Converse, ConverseStream.
func TestProviderDefaultGuardrail_ReachesAllFourPaths(t *testing.T) {
	want := Guardrail{ID: "default-gr", Version: "1"}

	t.Run("Invoke", func(t *testing.T) {
		fi := &fakeInvoker{respBody: []byte(mockInvokeResp)}
		p := &provider{inv: fi, modelAPI: map[string]string{}, defaultGuardrail: want}
		_, err := p.Complete(context.Background(), &providers.ProxyRequest{
			Model: "m", Upstream: "anthropic.claude-x", RawBody: []byte(`{"model":"m","messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if fi.gotGuardrail != want {
			t.Fatalf("Invoke got guardrail %+v, want %+v", fi.gotGuardrail, want)
		}
	})

	t.Run("InvokeStream", func(t *testing.T) {
		fi := &fakeInvoker{streamRaw: [][]byte{[]byte(`{"type":"message_stop"}`)}}
		p := &provider{inv: fi, modelAPI: map[string]string{}, defaultGuardrail: want}
		seq, err := p.Stream(context.Background(), &providers.ProxyRequest{
			Model: "m", Upstream: "anthropic.claude-x", RawBody: []byte(`{"model":"m","messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		} // drain
		if fi.gotGuardrail != want {
			t.Fatalf("InvokeStream got guardrail %+v, want %+v", fi.gotGuardrail, want)
		}
	})

	t.Run("Converse", func(t *testing.T) {
		fc := &fakeConverser{}
		p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}, defaultGuardrail: want}
		_, err := p.Complete(context.Background(), &providers.ProxyRequest{
			Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		if fc.gotReq.Guardrail != want {
			t.Fatalf("Converse got guardrail %+v, want %+v", fc.gotReq.Guardrail, want)
		}
	})

	t.Run("ConverseStream", func(t *testing.T) {
		fc := &fakeConverser{}
		p := &provider{conv: fc, modelAPI: map[string]string{"m": "converse"}, defaultGuardrail: want}
		seq, err := p.Stream(context.Background(), &providers.ProxyRequest{
			Model: "m", Upstream: "m", RawBody: []byte(`{"messages":[]}`),
		})
		if err != nil {
			t.Fatal(err)
		}
		for range seq {
		} // drain
		if fc.gotReq.Guardrail != want {
			t.Fatalf("ConverseStream got guardrail %+v, want %+v", fc.gotReq.Guardrail, want)
		}
	})
}

// TestPerRequestGuardrail_OverridesDefault proves a per-team override
// (ProxyRequest.GuardrailID) wins over the provider's configured default.
func TestPerRequestGuardrail_OverridesDefault(t *testing.T) {
	fi := &fakeInvoker{respBody: []byte(mockInvokeResp)}
	p := &provider{inv: fi, modelAPI: map[string]string{}, defaultGuardrail: Guardrail{ID: "default-gr", Version: "1"}}
	_, err := p.Complete(context.Background(), &providers.ProxyRequest{
		Model: "m", Upstream: "anthropic.claude-x", RawBody: []byte(`{"model":"m","messages":[]}`),
		GuardrailID: "team-gr", GuardrailVersion: "7",
	})
	if err != nil {
		t.Fatal(err)
	}
	want := Guardrail{ID: "team-gr", Version: "7"}
	if fi.gotGuardrail != want {
		t.Fatalf("override guardrail = %+v, want %+v", fi.gotGuardrail, want)
	}
}

// TestNoGuardrail_SDKFieldsNil proves that with neither a default nor an
// override, the SDK guardrail fields are never set (nil), i.e. Guardrail{}
// reaches Invoke/Converse untouched — already covered by
// TestBuildGuardrailConfig_ZeroValueIsNil at the builder layer; this pins it
// at the provider layer too.
func TestNoGuardrail_SDKFieldsNil(t *testing.T) {
	fi := &fakeInvoker{respBody: []byte(mockInvokeResp)}
	p := &provider{inv: fi, modelAPI: map[string]string{}}
	_, err := p.Complete(context.Background(), &providers.ProxyRequest{
		Model: "m", Upstream: "anthropic.claude-x", RawBody: []byte(`{"model":"m","messages":[]}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fi.gotGuardrail != (Guardrail{}) {
		t.Fatalf("expected zero-value guardrail, got %+v", fi.gotGuardrail)
	}
}
