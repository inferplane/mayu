package bedrock

import (
	"testing"

	"github.com/inferplane/inferplane/providers"
)

func TestRegisteredAsBedrock(t *testing.T) {
	// init() registers "bedrock". New constructs (real AWS config load may fail
	// offline — that's fine); it must NOT be "unknown provider type".
	_, err := providers.New(providers.Config{Type: "bedrock", Settings: map[string]string{"region": "us-west-2"}})
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
