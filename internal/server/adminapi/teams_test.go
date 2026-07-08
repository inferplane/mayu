package adminapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
)

// doAsTeams mirrors keys_test.go's doAs for *TeamsHandler.
func doAsTeams(t *testing.T, h *TeamsHandler, id *principal.AdminIdentity, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if id != nil {
		req = req.WithContext(principal.WithAdmin(req.Context(), *id))
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestTeamsHandler_noIdentityDenied(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	rec := doAsTeams(t, h, nil, "GET", "/admin/teams", "")
	if rec.Code != 403 {
		t.Fatalf("no identity: got %d, want 403 (fail-closed)", rec.Code)
	}
}

func TestTeamsHandler_upsertListDeleteRoundTrip(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)

	body := `{"rpm":60,"tpm":1000,"tokens_per_day":10000,"quota_on_exceeded":"block","budget_usd_micros":5000000,"budget_on_exceeded":"warn","allowed_models":["m1","m2"]}`
	rec := doAsTeams(t, h, &adminID, "PUT", "/admin/teams/platform-eng", body)
	if rec.Code != 200 {
		t.Fatalf("upsert: %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["source"] != "record" || got["rpm"].(float64) != 60 {
		t.Fatalf("upsert response: %+v", got)
	}

	rec = doAsTeams(t, h, &adminID, "GET", "/admin/teams", "")
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 1 || list.Data[0]["name"] != "platform-eng" {
		t.Fatalf("list: %+v", list.Data)
	}

	rec = doAsTeams(t, h, &adminID, "DELETE", "/admin/teams/platform-eng", "")
	if rec.Code != 204 {
		t.Fatalf("delete: %d %s", rec.Code, rec.Body.String())
	}
	rec = doAsTeams(t, h, &adminID, "GET", "/admin/teams", "")
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 0 {
		t.Fatalf("team survived delete: %+v", list.Data)
	}
}

// TestTeamsHandler_guardrailRoundTrip proves guardrail_id/guardrail_version
// round-trip through upsert -> teamView (D6, ADR-019).
func TestTeamsHandler_guardrailRoundTrip(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	rec := doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{"guardrail_id":"gr-abc123","guardrail_version":"3"}`)
	if rec.Code != 200 {
		t.Fatalf("upsert: %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["guardrail_id"] != "gr-abc123" || got["guardrail_version"] != "3" {
		t.Fatalf("guardrail fields not round-tripped: %+v", got)
	}

	// DRAFT and the unset ("") default are both valid.
	rec = doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t2", `{"guardrail_id":"gr-x","guardrail_version":"DRAFT"}`)
	if rec.Code != 200 {
		t.Fatalf("DRAFT version: %d %s", rec.Code, rec.Body.String())
	}
	rec = doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t3", `{}`)
	if rec.Code != 200 {
		t.Fatalf("no override: %d %s", rec.Code, rec.Body.String())
	}
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["guardrail_id"] != "" || got["guardrail_version"] != "" {
		t.Fatalf("no-override team should have empty guardrail fields: %+v", got)
	}
}

func TestTeamsHandler_deleteMissingReturns404(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	rec := doAsTeams(t, h, &adminID, "DELETE", "/admin/teams/nonexistent", "")
	if rec.Code != 404 {
		t.Fatalf("delete missing: got %d, want 404", rec.Code)
	}
}

// TestTeamsHandler_deleteValidatesNameSameAsUpsert proves DELETE rejects a
// malformed name (control characters, over-length) with 400 before it ever
// reaches the store — the same validateTeamName gate PUT already used.
func TestTeamsHandler_deleteValidatesNameSameAsUpsert(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	rec := doAsTeams(t, h, &adminID, "DELETE", "/admin/teams/"+strings.Repeat("x", 257), "")
	if rec.Code != 400 {
		t.Fatalf("delete over-length name: got %d, want 400", rec.Code)
	}
}

func TestTeamsHandler_upsertIsIdempotentUpdate(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{"rpm":1}`)
	rec := doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{"rpm":2}`)
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["rpm"].(float64) != 2 {
		t.Fatalf("second PUT must overwrite: %+v", got)
	}
	rec = doAsTeams(t, h, &adminID, "GET", "/admin/teams", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)
	if len(list.Data) != 1 {
		t.Fatalf("upsert must not duplicate rows: %+v", list.Data)
	}
}

func TestTeamsHandler_validation(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	cases := []struct {
		name, path, body string
	}{
		{"empty name", "/admin/teams/", `{}`},
		{"name too long", "/admin/teams/" + strings.Repeat("x", 257), `{}`},
		{"name with slash", "/admin/teams/a%2Fb", `{}`}, // decoded path still contains '/'
		{"negative rpm", "/admin/teams/t", `{"rpm":-1}`},
		{"negative tpm", "/admin/teams/t", `{"tpm":-1}`},
		{"negative tokens_per_day", "/admin/teams/t", `{"tokens_per_day":-1}`},
		{"negative budget", "/admin/teams/t", `{"budget_usd_micros":-1}`},
		{"bad quota_on_exceeded", "/admin/teams/t", `{"quota_on_exceeded":"deny"}`},
		{"bad budget_on_exceeded", "/admin/teams/t", `{"budget_on_exceeded":"deny"}`},
		{"allowed_models too big", "/admin/teams/t", `{"allowed_models":["` + strings.Repeat("m", 5000) + `"]}`},
		{"malformed json", "/admin/teams/t", `not-json`},
		{"guardrail_version without guardrail_id", "/admin/teams/t", `{"guardrail_version":"3"}`},
		{"guardrail_version not numeric or DRAFT", "/admin/teams/t", `{"guardrail_id":"gr-abc","guardrail_version":"latest"}`},
		{"guardrail_id too long", "/admin/teams/t", `{"guardrail_id":"` + strings.Repeat("g", 2049) + `"}`},
		{"guardrail_id control char", "/admin/teams/t", `{"guardrail_id":"grabc"}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := doAsTeams(t, h, &adminID, "PUT", c.path, c.body)
			if rec.Code != 400 {
				t.Fatalf("%s: got %d %s, want 400", c.name, rec.Code, rec.Body.String())
			}
		})
	}
}

func TestTeamsHandler_zeroValueMeansUnlimited(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), nil, nil)
	rec := doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{}`)
	if rec.Code != 200 {
		t.Fatalf("empty body upsert: %d %s", rec.Code, rec.Body.String())
	}
	var got map[string]any
	json.Unmarshal(rec.Body.Bytes(), &got)
	if got["rpm"].(float64) != 0 || got["budget_usd_micros"].(float64) != 0 {
		t.Fatalf("empty body should mean unlimited: %+v", got)
	}
}

func TestTeamsHandler_listIncludesConfigOnlyNamesNotShadowedByRecord(t *testing.T) {
	h := NewTeamsHandler(newTestStore(t), func() []string { return []string{"config-only", "both"} }, nil)
	doAsTeams(t, h, &adminID, "PUT", "/admin/teams/both", `{"rpm":5}`)

	rec := doAsTeams(t, h, &adminID, "GET", "/admin/teams", "")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &list)

	bySource := map[string]string{}
	for _, row := range list.Data {
		bySource[row["name"].(string)] = row["source"].(string)
	}
	if bySource["config-only"] != "config" {
		t.Fatalf("config-only team should be source=config: %+v", list.Data)
	}
	if bySource["both"] != "record" {
		t.Fatalf("a DB record must shadow the config row for the same name (ADR-016 precedence): %+v", list.Data)
	}
	if len(list.Data) != 2 {
		t.Fatalf("expected exactly 2 rows (no duplicate for 'both'): %+v", list.Data)
	}
}

func TestTeamsHandler_auditEventsOnUpsertAndDelete(t *testing.T) {
	em := &emittedRecords{}
	h := NewTeamsHandler(newTestStore(t), nil, em.emit)
	doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{"rpm":1}`)
	doAsTeams(t, h, &adminID, "DELETE", "/admin/teams/t", "")
	got := em.events()
	if len(got) != 2 || got[0] != "admin_team_upserted" || got[1] != "admin_team_deleted" {
		t.Fatalf("audit events = %v, want [admin_team_upserted admin_team_deleted]", got)
	}
}

// vanishingTeamStore simulates a concurrent delete winning the race between
// UpsertTeam succeeding and the handler's own read-back — GetTeam always
// reports "not found" regardless of what was just upserted.
type vanishingTeamStore struct{ keystore.TeamStore }

func (vanishingTeamStore) UpsertTeam(context.Context, keystore.TeamRecord) error { return nil }
func (vanishingTeamStore) GetTeam(context.Context, string) (keystore.TeamRecord, bool, error) {
	return keystore.TeamRecord{}, false, nil
}

// TestTeamsHandler_upsertReadBackNotFoundIs500NotEmpty200 pins the fix for a
// real bug: the read-back after UpsertTeam must check its ok return value —
// discarding it would encode and 200 a zero-value TeamRecord as if the
// upsert had produced an empty team, and would emit admin_team_upserted
// despite the response being an error.
func TestTeamsHandler_upsertReadBackNotFoundIs500NotEmpty200(t *testing.T) {
	em := &emittedRecords{}
	h := NewTeamsHandler(vanishingTeamStore{}, nil, em.emit)
	rec := doAsTeams(t, h, &adminID, "PUT", "/admin/teams/t", `{"rpm":1}`)
	if rec.Code != 500 {
		t.Fatalf("read-back not-found: got %d %s, want 500 (not a fabricated 200)", rec.Code, rec.Body.String())
	}
	if got := em.events(); len(got) != 0 {
		t.Fatalf("audit events = %v, want none — a failed read-back must not record admin_team_upserted", got)
	}
}

// --- UsersHandler ---

func TestUsersHandler_noIdentityDenied(t *testing.T) {
	h := NewUsersHandler(newTestStore(t))
	req := httptest.NewRequest("GET", "/admin/users", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("no identity: got %d, want 403", rec.Code)
	}
}

func TestUsersHandler_rejectsPOST(t *testing.T) {
	h := NewUsersHandler(newTestStore(t))
	req := httptest.NewRequest("POST", "/admin/users", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 405 {
		t.Fatalf("POST: got %d, want 405", rec.Code)
	}
}

func TestUsersHandler_derivesFromKeyOwnersGroupedAcrossTeams(t *testing.T) {
	store := newTestStore(t)
	ctx := context.Background()
	store.CreateWithOptions(ctx, "alpha", []string{"*"}, keystore.KeyOptions{Owner: "alice"})
	store.CreateWithOptions(ctx, "beta", []string{"*"}, keystore.KeyOptions{Owner: "alice"})
	store.CreateWithOptions(ctx, "alpha", []string{"*"}, keystore.KeyOptions{Owner: "bob"})
	store.CreateWithOptions(ctx, "alpha", []string{"*"}, keystore.KeyOptions{}) // no owner

	h := NewUsersHandler(store)
	req := httptest.NewRequest("GET", "/admin/users", nil)
	req = req.WithContext(principal.WithAdmin(req.Context(), adminID))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []struct {
			Owner    string   `json:"owner"`
			Teams    []string `json:"teams"`
			KeyCount int      `json:"key_count"`
		} `json:"data"`
	}
	json.Unmarshal(rec.Body.Bytes(), &out)
	byOwner := map[string]int{}
	teamsOf := map[string][]string{}
	for _, u := range out.Data {
		byOwner[u.Owner] = u.KeyCount
		teamsOf[u.Owner] = u.Teams
	}
	if byOwner["alice"] != 2 || len(teamsOf["alice"]) != 2 {
		t.Fatalf("alice should have 2 keys across 2 teams: %+v", out.Data)
	}
	if byOwner["bob"] != 1 {
		t.Fatalf("bob should have 1 key: %+v", out.Data)
	}
	if byOwner["(unowned)"] != 1 {
		t.Fatalf("ownerless key must collapse under (unowned): %+v", out.Data)
	}
}
