package adminapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

func newTestStore(t *testing.T) *keystore.SQLiteStore {
	s, err := keystore.OpenSQLite(filepath.Join(t.TempDir(), "k.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestCreateKeyReturnsPlaintextOnce(t *testing.T) {
	h := NewKeysHandler(newTestStore(t), nil)
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"team":"platform-eng","allowed_models":["claude-sonnet-4-6"]}`))
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		KeyID     string `json:"key_id"`
		Plaintext string `json:"plaintext"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	if !strings.HasPrefix(out.Plaintext, "ik_") || out.KeyID == "" {
		t.Fatalf("expected plaintext+key_id: %+v", out)
	}
}

func TestListKeysOmitsSecrets(t *testing.T) {
	store := newTestStore(t)
	store.Create(context.Background(), "t", []string{"*"})
	h := NewKeysHandler(store, nil)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "plaintext") {
		t.Fatalf("list must not expose plaintext: %s", rec.Body.String())
	}
}

func TestRevokeKey(t *testing.T) {
	store := newTestStore(t)
	_, p, _ := store.Create(context.Background(), "t", []string{"*"})
	h := NewKeysHandler(store, nil)
	req := httptest.NewRequest("DELETE", "/admin/keys/"+p.KeyID, nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Fatalf("revoke: %d", rec.Code)
	}
}

// --- per-team authZ + admin audit (plan 2026-06-12 task 6) ---

type emittedRecords struct{ recs []audit.Record }

func (e *emittedRecords) emit(r audit.Record) { e.recs = append(e.recs, r) }

func (e *emittedRecords) events() []string {
	var out []string
	for _, r := range e.recs {
		out = append(out, r.Event)
	}
	return out
}

// doAs sends a request with the given AdminIdentity injected (as the
// AdminAuth middleware would) — or none when id is nil (fail-closed path).
func doAs(t *testing.T, h *KeysHandler, id *principal.AdminIdentity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	var rd *strings.Reader
	if body != "" {
		rd = strings.NewReader(body)
	} else {
		rd = strings.NewReader("")
	}
	req := httptest.NewRequest(method, path, rd)
	if id != nil {
		req = req.WithContext(principal.WithAdmin(req.Context(), *id))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

var (
	adminID  = principal.AdminIdentity{Subject: "root", IsAdmin: true, AuthMethod: "break_glass"}
	memberID = principal.AdminIdentity{Subject: "u-alpha", Teams: []string{"alpha"}, AuthMethod: "oidc"}
)

func TestCreateKeyTeamEntitlement(t *testing.T) {
	em := &emittedRecords{}
	h := NewKeysHandler(newTestStore(t), em.emit)

	cases := []struct {
		name string
		id   *principal.AdminIdentity
		team string
		want int
	}{
		{"member own team", &memberID, "alpha", 200},
		{"member other team", &memberID, "beta", 403},
		{"admin any team", &adminID, "gamma", 200},
		{"no identity fail-closed", nil, "alpha", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doAs(t, h, tc.id, "POST", "/admin/keys", `{"team":"`+tc.team+`","allowed_models":["*"]}`)
			if rec.Code != tc.want {
				t.Fatalf("= %d, want %d: %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestRevokeKeyTeamEntitlement(t *testing.T) {
	em := &emittedRecords{}
	store := newTestStore(t)
	h := NewKeysHandler(store, em.emit)

	_, alphaKey, err := store.Create(context.Background(), "alpha", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}
	_, betaKey, err := store.Create(context.Background(), "beta", []string{"*"})
	if err != nil {
		t.Fatal(err)
	}

	if rec := doAs(t, h, &memberID, "DELETE", "/admin/keys/"+betaKey.KeyID, ""); rec.Code != 403 {
		t.Fatalf("member revoking other team = %d, want 403", rec.Code)
	}
	if rec := doAs(t, h, &memberID, "DELETE", "/admin/keys/"+alphaKey.KeyID, ""); rec.Code != 204 {
		t.Fatalf("member revoking own team = %d, want 204", rec.Code)
	}
	if rec := doAs(t, h, &adminID, "DELETE", "/admin/keys/"+betaKey.KeyID, ""); rec.Code != 204 {
		t.Fatalf("admin revoke = %d, want 204", rec.Code)
	}
}

func TestAdminActionsEmitAuditRecords(t *testing.T) {
	em := &emittedRecords{}
	store := newTestStore(t)
	h := NewKeysHandler(store, em.emit)

	// success create (oidc member)
	rec := doAs(t, h, &memberID, "POST", "/admin/keys", `{"team":"alpha","allowed_models":["*"]}`)
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	// denied create (cross-team)
	if rec := doAs(t, h, &memberID, "POST", "/admin/keys", `{"team":"beta","allowed_models":["*"]}`); rec.Code != 403 {
		t.Fatal("want 403")
	}
	// revoke the created key
	var out struct {
		KeyID string `json:"key_id"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	var created struct {
		KeyID string `json:"key_id"`
	}
	_ = created
	ps, _ := store.List(context.Background())
	if rec := doAs(t, h, &memberID, "DELETE", "/admin/keys/"+ps[0].KeyID, ""); rec.Code != 204 {
		t.Fatal("revoke failed")
	}

	want := []string{"admin_key_created", "admin_denied", "admin_key_revoked"}
	if got := em.events(); !reflect.DeepEqual(got, want) {
		t.Fatalf("audit events = %v, want %v", got, want)
	}

	// PII stance: User carries the opaque sub, never email; auth_method set.
	first := em.recs[0]
	if first.Principal.User == nil || *first.Principal.User != "u-alpha" {
		t.Fatalf("audit User = %v, want sub u-alpha", first.Principal.User)
	}
	if first.Principal.AuthMethod == nil || *first.Principal.AuthMethod != "oidc" {
		t.Fatalf("audit AuthMethod = %v, want oidc", first.Principal.AuthMethod)
	}
	if first.Principal.Team != "alpha" || first.ID == "" || first.TS == "" {
		t.Fatalf("audit record incomplete: %+v", first)
	}
	// denied record carries the denied team and no key id
	denied := em.recs[1]
	if denied.Principal.Team != "beta" || denied.Principal.KeyID != "" {
		t.Fatalf("denied record: %+v", denied.Principal)
	}
}

// TestListRequiresNoTeamButRequiresIdentity: list is read-only and team-wide;
// any authenticated admin-plane identity may call it, but no identity (a
// handler reached without the middleware) is fail-closed.
func TestListFailClosedWithoutIdentity(t *testing.T) {
	em := &emittedRecords{}
	h := NewKeysHandler(newTestStore(t), em.emit)
	if rec := doAs(t, h, nil, "GET", "/admin/keys", ""); rec.Code != 403 {
		t.Fatalf("no identity = %d, want 403 (fail-closed)", rec.Code)
	}
	if rec := doAs(t, h, &memberID, "GET", "/admin/keys", ""); rec.Code != 200 {
		t.Fatalf("member list = %d, want 200", rec.Code)
	}
}

// erroringStore wraps a Store and fails List — the P4-gate probe for the
// revoke fail-closed path.
type erroringStore struct{ keystore.Store }

func (erroringStore) List(context.Context) ([]keystore.Principal, error) {
	return nil, context.DeadlineExceeded
}

// TestRevokeFailsClosedOnLookupError (P4 gate): if the team lookup errors,
// revoke must 500 — proceeding would skip the entitlement check (fail-open).
func TestRevokeFailsClosedOnLookupError(t *testing.T) {
	store := newTestStore(t)
	_, p, _ := store.Create(context.Background(), "alpha", []string{"*"})
	h := NewKeysHandler(erroringStore{store}, nil)
	if rec := doAs(t, h, &memberID, "DELETE", "/admin/keys/"+p.KeyID, ""); rec.Code != 500 {
		t.Fatalf("revoke with failing lookup = %d, want 500 (fail-closed)", rec.Code)
	}
	// The key must still exist (revoke must not have run).
	ps, err := store.List(context.Background())
	if err != nil || len(ps) != 1 {
		t.Fatalf("key was revoked despite failed entitlement lookup: %v %v", ps, err)
	}
}

func TestCreateKeyWithGovernanceOptions_roundTrip(t *testing.T) {
	h := NewKeysHandler(newTestStore(t), nil)
	body := `{"team":"platform-eng","allowed_models":["*"],"budget_usd_micros":5000000,"tpm":1000,"rpm":60,"owner":"alice","expires_at":"2099-01-01T00:00:00Z","metadata":{"purpose":"ci"}}`
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(body))
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["budget_usd_micros"] != float64(5000000) || out["owner"] != "alice" {
		t.Fatalf("governance fields not returned: %+v", out)
	}
	if out["expires_at"] != "2099-01-01T00:00:00Z" {
		t.Fatalf("expires_at not returned: %+v", out)
	}
}

func TestCreateKeyWithGovernanceOptions_badExpiryIs400(t *testing.T) {
	h := NewKeysHandler(newTestStore(t), nil)
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(`{"team":"t","expires_at":"not-a-date"}`))
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 400 {
		t.Fatalf("bad expires_at: got %d, want 400", rec.Code)
	}
}

func TestListKeys_omitsZeroGovernanceFields(t *testing.T) {
	store := newTestStore(t)
	store.Create(context.Background(), "t", []string{"*"})
	h := NewKeysHandler(store, nil)
	req := httptest.NewRequest("GET", "/admin/keys", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if strings.Contains(rec.Body.String(), "budget_usd_micros") || strings.Contains(rec.Body.String(), "owner") {
		t.Fatalf("zero-value governance fields should be omitted: %s", rec.Body.String())
	}
}

func TestCreateKeyWithGovernanceOptions_rejectsNegativeValues(t *testing.T) {
	for _, body := range []string{
		`{"team":"t","budget_usd_micros":-1}`,
		`{"team":"t","tpm":-1}`,
		`{"team":"t","rpm":-1}`,
	} {
		h := NewKeysHandler(newTestStore(t), nil)
		req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(body))
		req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Fatalf("negative value %s: got %d, want 400", body, rec.Code)
		}
	}
}

func TestCreateKeyWithGovernanceOptions_expirySubSecondPrecisionPreserved(t *testing.T) {
	h := NewKeysHandler(newTestStore(t), nil)
	body := `{"team":"t","expires_at":"2099-01-01T00:00:00.123456789Z"}`
	req := httptest.NewRequest("POST", "/admin/keys", strings.NewReader(body))
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	var out map[string]any
	json.Unmarshal(rec.Body.Bytes(), &out)
	if out["expires_at"] != "2099-01-01T00:00:00.123456789Z" {
		t.Fatalf("sub-second expiry precision lost: got %v", out["expires_at"])
	}
}
