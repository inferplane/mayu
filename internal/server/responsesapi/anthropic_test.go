package responsesapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/responses"
)

func TestAnthropicAdapterStripsOnlyProtocolPhase(t *testing.T) {
	raw := []byte(`{"model":"m","input":[{"type":"function_call","call_id":"a","name":"f","arguments":"{}"},{"role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"Reading second file"}]},{"type":"function_call","call_id":"b","name":"f","arguments":"{\"phase\":\"application-value\"}"},{"type":"function_call_output","call_id":"a","output":"ok"},{"type":"function_call_output","call_id":"b","output":"ok"}]}`)
	parsed, err := responses.RequestToCanonical(raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := anthropicRequest(parsed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), `"phase":"commentary"`) || !strings.Contains(string(body), `"phase":"application-value"`) || !json.Valid(body) {
		t.Fatalf("incorrect phase translation: %s", body)
	}
}
