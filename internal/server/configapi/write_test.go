package configapi

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/providerstore"
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

// --- WriteHandler (T5) ---

type stubWriter struct {
	err          error
	lastProvider providerstore.ProviderRow
	lastModel    string
	lastTargets  []providerstore.Target
	deletedProv  string
	deletedModel string
}

func (s *stubWriter) WriteProvider(_ context.Context, row providerstore.ProviderRow) error {
	s.lastProvider = row
	return s.err
}
func (s *stubWriter) DeleteProvider(_ context.Context, name string) error {
	s.deletedProv = name
	return s.err
}
func (s *stubWriter) WriteModel(_ context.Context, name string, targets []providerstore.Target) error {
	s.lastModel, s.lastTargets = name, targets
	return s.err
}
func (s *stubWriter) DeleteModel(_ context.Context, name string) error {
	s.deletedModel = name
	return s.err
}

func doReq(h http.Handler, method, path, body string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, path, strings.NewReader(body)))
	return rec
}

func TestWriteHandlerStoreAbsent405(t *testing.T) {
	h := WriteHandler("providers", nil) // nil writer = store not enabled
	rec := doReq(h, "PUT", "/admin/providers/p", `{"type":"anthropic"}`)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("store-absent PUT = %d, want 405", rec.Code)
	}
}

func TestWriteHandlerPutProvider(t *testing.T) {
	w := &stubWriter{}
	h := WriteHandler("providers", w)
	rec := doReq(h, "PUT", "/admin/providers/anthropic-prod", `{"type":"anthropic","api_key_ref":{"env":"K"}}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("PUT = %d, want 204: %s", rec.Code, rec.Body)
	}
	if w.lastProvider.Name != "anthropic-prod" || w.lastProvider.APIKeyRefEnv != "K" {
		t.Fatalf("writer not called with parsed row: %+v", w.lastProvider)
	}
}

func TestWriteHandlerInlineSecretNotForwarded(t *testing.T) {
	w := &stubWriter{}
	h := WriteHandler("providers", w)
	rec := doReq(h, "PUT", "/admin/providers/p", `{"type":"anthropic","api_key":"sk-leak"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("inline api_key = %d, want 400", rec.Code)
	}
	if w.lastProvider.Name != "" {
		t.Fatal("writer must NOT be called when the body carries an inline secret")
	}
	if strings.Contains(rec.Body.String(), "sk-leak") {
		t.Fatalf("response echoed the inline secret: %s", rec.Body)
	}
}

func TestWriteHandlerInvalidTopology400(t *testing.T) {
	w := &stubWriter{err: ErrInvalidTopology}
	h := WriteHandler("providers", w)
	rec := doReq(h, "PUT", "/admin/providers/p", `{"type":"anthropic"}`)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid topology = %d, want 400", rec.Code)
	}
}

func TestWriteHandlerDeleteNotFound404(t *testing.T) {
	w := &stubWriter{err: providerstore.ErrNotFound}
	h := WriteHandler("providers", w)
	rec := doReq(h, "DELETE", "/admin/providers/gone", "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("delete missing = %d, want 404", rec.Code)
	}
}

func TestWriteHandlerModelPutDelete(t *testing.T) {
	w := &stubWriter{}
	h := WriteHandler("models", w)
	rec := doReq(h, "PUT", "/admin/models/claude", `{"targets":[{"provider":"p","model":"m"}]}`)
	if rec.Code != http.StatusNoContent || w.lastModel != "claude" || len(w.lastTargets) != 1 {
		t.Fatalf("model PUT wrong: code=%d model=%q targets=%+v", rec.Code, w.lastModel, w.lastTargets)
	}
	rec = doReq(h, "DELETE", "/admin/models/claude", "")
	if rec.Code != http.StatusNoContent || w.deletedModel != "claude" {
		t.Fatalf("model DELETE wrong: code=%d deleted=%q", rec.Code, w.deletedModel)
	}
}

func TestWriteHandlerBadNameAndMethod(t *testing.T) {
	w := &stubWriter{}
	h := WriteHandler("providers", w)
	if rec := doReq(h, "PUT", "/admin/providers/", `{"type":"anthropic"}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("empty name = %d, want 400", rec.Code)
	}
	if rec := doReq(h, "GET", "/admin/providers/p", ""); rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET = %d, want 405", rec.Code)
	}
}
