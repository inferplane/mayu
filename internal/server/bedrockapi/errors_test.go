package bedrockapi

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// A Bedrock-mode client (AWS SDK / Claude Code CLAUDE_CODE_USE_BEDROCK=1)
// parses errors as {"message": "..."} plus the X-Amzn-ErrorType header — the
// Anthropic {"type":"error","error":{...}} envelope is unparseable to it.
func TestWriteErrAWSShape(t *testing.T) {
	cases := []struct {
		status   int
		wantType string
	}{
		{400, "ValidationException"},
		{401, "UnauthorizedException"},
		{403, "AccessDeniedException"},
		{404, "ResourceNotFoundException"},
		{429, "ThrottlingException"},
		{500, "InternalServerException"},
		{502, "InternalServerException"},
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeErr(rec, c.status, "something went wrong")
		if rec.Code != c.status {
			t.Fatalf("status %d: got %d", c.status, rec.Code)
		}
		if got := rec.Header().Get("X-Amzn-ErrorType"); !strings.HasPrefix(got, c.wantType) {
			t.Fatalf("status %d: X-Amzn-ErrorType = %q, want prefix %q", c.status, got, c.wantType)
		}
		var body struct {
			Message string `json:"message"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || body.Message == "" {
			t.Fatalf("status %d: body not AWS-shaped {\"message\":...}: %s", c.status, rec.Body.String())
		}
		if strings.Contains(rec.Body.String(), `"type":"error"`) {
			t.Fatalf("status %d: anthropic error envelope leaked: %s", c.status, rec.Body.String())
		}
	}
}
