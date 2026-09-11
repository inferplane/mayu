package server

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/providerstore"
	"github.com/inferplane/inferplane/internal/server/configapi"
)

// nopWriter is the minimal non-nil configapi.Writer: with a nil writer the
// handler 405s before requireAdmin's verdict matters, so these tests need one.
type nopWriter struct{}

func (nopWriter) WriteProvider(context.Context, providerstore.ProviderRow) error { return nil }
func (nopWriter) DeleteProvider(context.Context, string) error                   { return nil }
func (nopWriter) WriteModel(context.Context, string, providerstore.ModelRoute) error {
	return nil
}
func (nopWriter) DeleteModel(context.Context, string) error { return nil }

// TestAdminMux_ProviderModelWritesRequireFullAdmin proves the S2 fix:
// PUT/DELETE /admin/providers/ and /admin/models/ sit behind requireAdmin,
// the SAME full-admin tier as the connection probe on the same data. Before
// this gate a team-mapped (non-admin) console identity could persist an
// attacker-controlled base_url + api_key_ref pair that live traffic would
// then resolve and send out — a credential-exfiltration primitive.
func TestAdminMux_ProviderModelWritesRequireFullAdmin(t *testing.T) {
	v := &fakeVerifier{claims: adminauth.Claims{Subject: "u-alpha", Groups: []string{"team-alpha"}}}
	mapping := adminauth.MappingConfig{GroupMappings: []adminauth.GroupMapping{{Group: "team-alpha", Teams: []string{"alpha"}}}}
	mux := AdminMux(stubStore{}, []string{"admin-tok"}, v, mapping, func() configapi.View { return configapi.View{} },
		nil, nil, nil, nil, nil, nopWriter{}, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil)

	do := func(bearer, method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		return rec
	}

	providerBody := `{"type":"openai_compatible","base_url":"https://attacker.example","api_key_ref":{"env":"PATH"}}`
	modelBody := `{"targets":[{"provider":"p","model":"m"}]}`

	// team-mapped identity: every write verb on both resources is 403.
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/admin/providers/evil", providerBody},
		{"DELETE", "/admin/providers/evil", ""},
		{"PUT", "/admin/models/m1", modelBody},
		{"DELETE", "/admin/models/m1", ""},
	} {
		if rec := do(jwtShaped, c.method, c.path, c.body); rec.Code != 403 {
			t.Errorf("team-mapped %s %s = %d, want 403", c.method, c.path, rec.Code)
		}
	}

	// full admin: past requireAdmin (any non-401/403 status proves it reached
	// the write handler — this test only pins the authorization tier).
	if rec := do("admin-tok", "PUT", "/admin/providers/ok", providerBody); rec.Code == 401 || rec.Code == 403 {
		t.Errorf("full-admin PUT /admin/providers/ok = %d, must reach the handler", rec.Code)
	}
	if rec := do("admin-tok", "PUT", "/admin/models/m1", modelBody); rec.Code == 401 || rec.Code == 403 {
		t.Errorf("full-admin PUT /admin/models/m1 = %d, must reach the handler", rec.Code)
	}
}
