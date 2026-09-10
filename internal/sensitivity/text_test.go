package sensitivity

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
)

func textRequest(text string) []byte {
	encoded, _ := json.Marshal(text)
	return []byte(`{"messages":[{"role":"user","content":` + string(encoded) + `}]}`)
}

func TestDetectors(t *testing.T) {
	tests := []struct {
		name, text string
		categories []string
	}{
		{"email", "send to Alice.Example+tag@example.test", []string{"email"}},
		{"us phone", "call +1 (212) 555-0123", []string{"phone"}},
		{"korean phone", "call 010-1234-5678", []string{"phone"}},
		{"international phone", "call +44 20 7946 0958", []string{"phone"}},
		{"card", "4111 1111 1111 1111", []string{"credit_card"}},
		{"card hyphens", "5555-5555-5555-4444", []string{"credit_card"}},
		{"card compact", "378282246310005", []string{"credit_card"}},
		{"adjacent spaced cards", "4111 1111 1111 1111 5555 5555 5555 4444", []string{"credit_card"}},
		{"numeric prefix card", "1 4111 1111 1111 1111", []string{"credit_card"}},
		{"rejected card before valid card", "4111 1111 1111 1112 5555 5555 5555 4444", []string{"credit_card"}},
		{"newline separated cards", "4111 1111 1111 1111\n5555 5555 5555 4444", []string{"credit_card"}},
		{"overlong digit prefix", "12344111111111111111", nil},
		{"overlong digit suffix", "41111111111111111234", nil},
		{"adjacent compact digits", "41111111111111115555555555554444", nil},
		{"card across detector chunk", strings.Repeat("x", 8190) + "1 4111 1111 1111 1111", []string{"credit_card"}},
		{"invalid luhn", "4111 1111 1111 1112", nil},
		{"all zero card", "0000000000000000", nil},
		{"overlong card", "941111111111111119", nil},
		{"ssn", "123-45-6789", []string{"ssn"}},
		{"invalid ssn", "000-00-0000", nil},
		{"ipv4", "host 192.168.1.10", []string{"ipv4"}},
		{"invalid ipv4", "host 999.168.1.10", nil},
		{"korean rrn", "900101-1234567", []string{"korean_rrn"}},
		{"invalid rrn date shape", "991399-1234567", nil},
		{"ordinary numbers", "version 1234, count 123456, build 20260909", nil},
		{"deduplicated sorted", "bob@example.test 192.168.1.10 alice@example.test 4111 1111 1111 1111", []string{"credit_card", "email", "ipv4"}},
		{"late string", strings.Repeat("a ", 8190) + "alice@example.test", []string{"email"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := NewInspector().Inspect(context.Background(), "openai", textRequest(tt.text))
			if err != nil || !got.Complete || !slices.Equal(got.Categories, tt.categories) {
				t.Fatalf("inspection = %+v, %v; want categories %v", got, err, tt.categories)
			}
		})
	}
}

func TestSpeculativeEmbeddedJSON(t *testing.T) {
	tests := []struct {
		name, text string
		keyword    string
		match      bool
		categories []string
	}{
		{"malformed escaped keyword", `{"topic":"secu\u0072ity"`, "security", false, nil},
		{"malformed escaped key", `{"secu\u0072ity":"safe"`, "security", false, nil},
		{"trailing malformed text", `{"topic":"secu\u0072ity"} trailing`, "security", false, nil},
		{"valid escaped keyword", `{"topic":"secu\u0072ity"}`, "security", true, nil},
		{"ordinary malformed prose", `{"topic":"security"`, "security", true, nil},
		{"malformed escaped email", `{"email":"alice\u0040example.test"`, "alice@example.test", false, nil},
		{"valid escaped email", `{"email":"alice\u0040example.test"}`, "alice@example.test", true, []string{"email"}},
		{"ordinary malformed email", `{"email":"alice@example.test"`, "alice@example.test", true, []string{"email"}},
		{"nested malformed keyword", `{"inner":"{\"topic\":\"secu\\u0072ity\"}"`, "security", false, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := textRequest(tt.text)
			before := bytes.Clone(raw)
			got, err := NewInspector().Inspect(context.Background(), "openai", raw)
			if err != nil || !got.Complete || !slices.Equal(got.Categories, tt.categories) {
				t.Errorf("inspection = %+v, %v; want categories %v", got, err, tt.categories)
			}
			if match := MatchesKeywords(raw, []string{tt.keyword}); match != tt.match {
				t.Errorf("MatchesKeywords = %v; want %v", match, tt.match)
			}
			if !bytes.Equal(raw, before) {
				t.Fatal("speculative decoding mutated request")
			}
		})
	}
}

func TestMatchesKeywords(t *testing.T) {
	tests := []struct {
		name, raw string
		keywords  []string
		want      bool
	}{
		{"folded content", `{"messages":[{"role":"user","content":"Security review"}]}`, []string{"security"}, true},
		{"escaped unicode", `{"messages":[{"role":"user","content":"auth\u0065ntication"}]}`, []string{"AUTHENTICATION"}, true},
		{"escaped arguments", `{"messages":[{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"f","arguments":"{\"topic\":\"secu\\u0072ity\"}"}}]}]}`, []string{"security"}, true},
		{"keys", `{"metadata":{"SECURITY":"yes"}}`, []string{"security"}, true},
		{"unicode simple fold", `{"text":"Σ"} `, []string{"ς"}, true},
		{"no match", `{"text":"routine change"}`, []string{"security"}, false},
		{"empty keywords", `{"text":"security"}`, nil, false},
		{"empty keyword", `{"text":"safe"}`, []string{""}, false},
		{"no cross string match", `{"a":"secu","b":"rity"}`, []string{"security"}, false},
		{"malformed", `{"text":"security"`, []string{"security"}, false},
		{"trailing data", `{"text":"security"} {}`, []string{"security"}, false},
		{"not json", `security`, []string{"security"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			before := bytes.Clone(raw)
			keywords := slices.Clone(tt.keywords)
			if got := MatchesKeywords(raw, keywords); got != tt.want {
				t.Fatalf("MatchesKeywords = %v; want %v", got, tt.want)
			}
			if !bytes.Equal(raw, before) || !slices.Equal(keywords, tt.keywords) {
				t.Fatal("keyword matching mutated borrowed input")
			}
		})
	}
}

func TestInspectionBounded(t *testing.T) {
	deep := strings.Repeat(`{"nested":`, 256) + `"alice@example.test"` + strings.Repeat(`}`, 256)
	embedded := `"alice\u0040example.test"`
	for range 12 {
		data, _ := json.Marshal(embedded)
		embedded = string(data)
	}
	for _, raw := range [][]byte{textRequest(deep), textRequest(embedded)} {
		got, err := NewInspector().Inspect(context.Background(), "openai", raw)
		if err == nil && got.Complete {
			t.Fatalf("traversal bound reported complete: %+v", got)
		}
		if MatchesKeywords(raw, []string{"nested", "alice"}) {
			t.Fatal("bounded traversal must not report a partial keyword match")
		}
	}
}
