package configapi

import (
	"strings"
	"testing"
)

func TestParseProviderWriteRejectsInlineSecret(t *testing.T) {
	body := []byte(`{"type":"anthropic","api_key":"sk-plaintext-leak"}`)
	_, err := ParseProviderWrite("p", body)
	if err == nil {
		t.Fatal("inline api_key must be rejected")
	}
	if strings.Contains(err.Error(), "sk-plaintext-leak") {
		t.Fatalf("error must not echo the inline secret: %v", err)
	}
}

func TestParseProviderWriteRejectsSecretShapedRef(t *testing.T) {
	// An operator pasting the secret into the env ref field: it is not a valid
	// env var name, so it is rejected — AND the value must never appear in the
	// error returned to the client.
	secret := "sk-ant-api03-DEADBEEF-not-a-name"
	body := []byte(`{"type":"anthropic","api_key_ref":{"env":"` + secret + `"}}`)
	_, err := ParseProviderWrite("p", body)
	if err == nil {
		t.Fatal("secret-shaped env ref must be rejected (not a valid env var name)")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("sanitized error must not echo the ref value: %v", err)
	}
}

func TestParseProviderWriteRejectsRelativeFileRef(t *testing.T) {
	body := []byte(`{"type":"anthropic","api_key_ref":{"file":"relative/path"}}`)
	if _, err := ParseProviderWrite("p", body); err == nil {
		t.Fatal("file ref must be an absolute path")
	}
}

func TestParseProviderWriteValidEnvRef(t *testing.T) {
	body := []byte(`{"type":"anthropic","base_url":"https://api","api_key_ref":{"env":"ANTHROPIC_KEY"}}`)
	row, err := ParseProviderWrite("anthropic-prod", body)
	if err != nil {
		t.Fatalf("valid write rejected: %v", err)
	}
	if row.Name != "anthropic-prod" || row.Type != "anthropic" || row.APIKeyRefEnv != "ANTHROPIC_KEY" {
		t.Fatalf("parsed row wrong: %+v", row)
	}
	if row.APIKeyRefFile != "" {
		t.Fatalf("env ref must not set file: %+v", row)
	}
}

func TestParseProviderWriteValidBedrock(t *testing.T) {
	body := []byte(`{"type":"bedrock","region":"us-west-2","auth":{"mode":"profile","profile":"dev"}}`)
	row, err := ParseProviderWrite("bedrock-us", body)
	if err != nil {
		t.Fatal(err)
	}
	if row.Region != "us-west-2" || row.AuthMode != "profile" || row.AuthProfile != "dev" {
		t.Fatalf("bedrock fields wrong: %+v", row)
	}
}

func TestParseProviderWriteRejectsEmptyType(t *testing.T) {
	if _, err := ParseProviderWrite("p", []byte(`{"base_url":"https://x"}`)); err == nil {
		t.Fatal("empty type must be rejected")
	}
}

func TestParseProviderWriteRejectsBothRefs(t *testing.T) {
	body := []byte(`{"type":"anthropic","api_key_ref":{"env":"K","file":"/secret"}}`)
	if _, err := ParseProviderWrite("p", body); err == nil {
		t.Fatal("setting both env and file refs must be rejected")
	}
}

func TestParseModelWriteOrdered(t *testing.T) {
	body := []byte(`{"targets":[{"provider":"a","model":"1"},{"provider":"b","model":"2","api":"converse"}]}`)
	ts, err := ParseModelWrite(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 || ts[0].Provider != "a" || ts[1].API != "converse" {
		t.Fatalf("parsed targets wrong: %+v", ts)
	}
}

func TestParseModelWriteRejectsEmpty(t *testing.T) {
	if _, err := ParseModelWrite([]byte(`{"targets":[]}`)); err == nil {
		t.Fatal("a model route with no targets must be rejected")
	}
	if _, err := ParseModelWrite([]byte(`{"targets":[{"model":"x"}]}`)); err == nil {
		t.Fatal("a target with no provider must be rejected")
	}
}
