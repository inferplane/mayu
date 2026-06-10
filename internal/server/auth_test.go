package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

type stubStore struct {
	key string
	p   keystore.Principal
}

func (s stubStore) Create(context.Context, string, []string) (string, keystore.Principal, error) {
	return "", keystore.Principal{}, nil
}
func (s stubStore) Resolve(_ context.Context, plaintext string) (keystore.Principal, error) {
	if plaintext == s.key {
		return s.p, nil
	}
	return keystore.Principal{}, keystore.ErrKeyNotFound
}
func (s stubStore) Revoke(context.Context, string) error               { return nil }
func (s stubStore) List(context.Context) ([]keystore.Principal, error) { return nil, nil }
func (s stubStore) Close() error                                       { return nil }

func TestKeyAuthResolvesPrincipal(t *testing.T) {
	store := stubStore{key: "ik_good", p: keystore.Principal{KeyID: "ik_abc", Team: "platform-eng", AllowedModels: []string{"*"}}}
	var gotTeam string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p, ok := principal.From(r.Context())
		if !ok {
			t.Fatal("principal not injected")
		}
		gotTeam = p.Team
		w.WriteHeader(200)
	})
	h := KeyAuth(store, next)

	cases := []struct {
		name, header, value string
		want                int
	}{
		{"valid x-api-key", "x-api-key", "ik_good", 200},
		{"valid bearer", "Authorization", "Bearer ik_good", 200},
		{"wrong key", "x-api-key", "ik_bad", 401},
		{"missing", "", "", 401},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest("POST", "/v1/messages", nil)
			if c.header != "" {
				req.Header.Set(c.header, c.value)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Fatalf("got %d want %d", rec.Code, c.want)
			}
		})
	}
	if gotTeam != "platform-eng" {
		t.Fatalf("principal team not propagated: %q", gotTeam)
	}
}
