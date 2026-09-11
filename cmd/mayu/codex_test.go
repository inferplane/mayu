package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/inferplane/inferplane/internal/openai"
	"github.com/inferplane/inferplane/internal/responses"
	"github.com/inferplane/inferplane/pkg/schema"
)

// Opt-in local-client acceptance: no OpenAI/model service or real credential is
// used. Empty child-only CODEX_HOME isolates the CLI's intended configuration
// directory from the developer's login, plugins, MCP servers and projects.
func TestE2ECodexCLIToolRoundTrip(t *testing.T) {
	codexCLIRoundTrip(t, "openai_responses")
}

func TestE2ECodexCLIChatBridgeToolRoundTrip(t *testing.T) {
	codexCLIRoundTrip(t, "openai_compatible")
}

func codexCLIRoundTrip(t *testing.T, providerType string) {
	t.Helper()
	if os.Getenv("INFERPLANE_TEST_CODEX") != "1" {
		t.Skip("set INFERPLANE_TEST_CODEX=1 with codex installed for local CLI acceptance")
	}
	binary, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("Codex acceptance requested but codex is not installed")
	}
	t.Setenv("CODEX_TEST_UPSTREAM_KEY", "local-test-upstream")
	var mu sync.Mutex
	var calls int
	var toolOutputSeen bool
	var problem string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		path := "/v1/responses"
		if providerType == "openai_compatible" {
			path = "/v1/chat/completions"
		}
		if r.URL.Path != path || r.Header.Get("Authorization") != "Bearer local-test-upstream" {
			problem = "wrong upstream route or gateway credential"
			http.Error(w, "test upstream refused", 400)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			problem = "request read failed"
			http.Error(w, "read failed", 400)
			return
		}
		var req struct {
			Model    string          `json:"model"`
			Input    json.RawMessage `json:"input"`
			Messages json.RawMessage `json:"messages"`
			Tools    []struct {
				Type     string `json:"type"`
				Name     string `json:"name"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if json.Unmarshal(body, &req) != nil || req.Model != "upstream-codex-test" {
			problem = "model was not rewritten to the configured upstream"
			http.Error(w, "invalid request", 400)
			return
		}
		calls++
		if providerType == "openai_compatible" {
			req.Input = req.Messages
			for i := range req.Tools {
				req.Tools[i].Name = req.Tools[i].Function.Name
			}
		}
		in, out := int64(20), int64(5)
		index := 0
		chunks := []*schema.ChatChunk{{Type: "message_start", Message: &schema.ChatResponse{
			ID: fmt.Sprintf("resp_test_%d", calls), Usage: &schema.Usage{InputTokens: &in},
		}}}
		if calls == 1 {
			tool, args := "", ""
			for _, candidate := range req.Tools {
				switch candidate.Name {
				case "exec_command":
					tool, args = candidate.Name, `{"cmd":"printf CODEX_GATEWAY_TOOL_OK","max_output_tokens":100}`
				case "shell_command":
					tool, args = candidate.Name, `{"command":"printf CODEX_GATEWAY_TOOL_OK","timeout_ms":10000}`
				case "shell":
					tool, args = candidate.Name, `{"command":["sh","-c","printf CODEX_GATEWAY_TOOL_OK"],"timeout_ms":10000}`
				}
				if tool != "" {
					break
				}
			}
			if tool == "" {
				problem = "CLI did not advertise a supported shell tool"
				http.Error(w, "missing tool", 400)
				return
			}
			delta, _ := json.Marshal(map[string]string{"type": "input_json_delta", "partial_json": args})
			chunks = append(chunks,
				&schema.ChatChunk{Type: "content_block_start", Index: &index, ContentBlock: &schema.ContentBlock{Type: "tool_use", ID: "call_test", Name: tool}},
				&schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: delta},
				&schema.ChatChunk{Type: "content_block_stop", Index: &index},
				&schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{OutputTokens: &out}, Delta: json.RawMessage(`{"stop_reason":"tool_use"}`)},
			)
		} else {
			toolOutputSeen = strings.Contains(string(req.Input), "CODEX_GATEWAY_TOOL_OK") &&
				(strings.Contains(string(req.Input), "function_call_output") || strings.Contains(string(req.Input), `"tool"`))
			text := ""
			chunks = append(chunks,
				&schema.ChatChunk{Type: "content_block_start", Index: &index, ContentBlock: &schema.ContentBlock{Type: "text", Text: &text}},
				&schema.ChatChunk{Type: "content_block_delta", Index: &index, Delta: json.RawMessage(`{"type":"text_delta","text":"CODEX_GATEWAY_FINAL_OK"}`)},
				&schema.ChatChunk{Type: "content_block_stop", Index: &index},
				&schema.ChatChunk{Type: "message_delta", Usage: &schema.Usage{OutputTokens: &out}, Delta: json.RawMessage(`{"stop_reason":"end_turn"}`)},
			)
		}
		chunks = append(chunks, &schema.ChatChunk{Type: "message_stop"})
		w.Header().Set("Content-Type", "text/event-stream")
		renderer := responses.NewStreamState(req.Model)
		chatState := openai.StreamState{IncludeUsage: true}
		for _, chunk := range chunks {
			if providerType == "openai_compatible" {
				if data := openai.ChunkFromCanonical(chunk, &chatState); data != nil {
					fmt.Fprintf(w, "data: %s\n\n", data)
				}
				w.(http.Flusher).Flush()
				continue
			}
			events, err := renderer.Convert(chunk)
			if err != nil {
				problem = "fixture could not encode a Responses event"
				return
			}
			for _, event := range events {
				if err := responses.WriteEvent(w, event); err != nil {
					problem = "fixture could not write stream"
					return
				}
			}
			w.(http.Flusher).Flush()
		}
		if providerType == "openai_compatible" {
			io.WriteString(w, "data: [DONE]\n\n")
		}
	}))
	defer up.Close()
	dataURL, adminURL, _ := bootGateway(t, func(cfg map[string]any, _ string) {
		cfg["providers"] = map[string]any{"up": map[string]any{
			"type": providerType, "base_url": up.URL, "api_key_ref": map[string]any{"env": "CODEX_TEST_UPSTREAM_KEY"},
		}}
		cfg["models"] = map[string]any{"auto": map[string]any{
			"context_window": 200000, "capabilities": []string{"tools", "reasoning"},
			"targets": []any{map[string]any{"provider": "up", "model": "upstream-codex-test"}},
		}}
		cfg["pricing"] = map[string]any{"overrides": map[string]any{
			"up": map[string]any{"upstream-codex-test": map[string]any{"free": true}},
		}}
	})
	_, key := createKey(t, adminURL, "codex-test", []string{"auto"})
	root := t.TempDir()
	childConfig, workspace := filepath.Join(root, "codex-config"), filepath.Join(root, "workspace")
	for _, dir := range []string{childConfig, workspace} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, binary, "-a", "never", "exec", "--ephemeral", "--skip-git-repo-check",
		"--sandbox", "read-only", "-C", workspace, "--json",
		"-c", `model="auto"`, "-c", `model_provider="inferplane_test"`,
		"-c", fmt.Sprintf(`model_providers.inferplane_test={name="local test",base_url=%q,env_key="INFERPLANE_CODEX_TEST_KEY",wire_api="responses"}`, dataURL+"/v1"),
		"-c", "check_for_update_on_startup=false", "-c", `web_search="disabled"`,
		"Use the shell to print CODEX_GATEWAY_TOOL_OK, then reply CODEX_GATEWAY_FINAL_OK. Do not read or change files.")
	cmd.Env = append(os.Environ(), "CODEX_HOME="+childConfig, "INFERPLANE_CODEX_TEST_KEY="+key)
	output, runErr := cmd.CombinedOutput()
	mu.Lock()
	defer mu.Unlock()
	safeOutput := strings.ReplaceAll(string(output), key, "[REDACTED_GATEWAY_KEY]")
	if runErr != nil || problem != "" || calls < 2 || !toolOutputSeen || !strings.Contains(safeOutput, "CODEX_GATEWAY_FINAL_OK") {
		t.Fatalf("Codex roundtrip: err=%v problem=%q calls=%d toolOutput=%t\n%s", runErr, problem, calls, toolOutputSeen, safeOutput)
	}
}
