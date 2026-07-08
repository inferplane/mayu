package adminapi

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/inferplane/inferplane/internal/audit"
	"github.com/inferplane/inferplane/internal/keystore"
	"github.com/inferplane/inferplane/internal/principal"
	"github.com/inferplane/inferplane/pkg/ulid"
)

// TeamsHandler serves /admin/teams (D3, ADR-016): teams as first-class
// keystore records. Reads are available to any AdminAuth identity (a
// projection of already-visible names + policy); writes (PUT/DELETE) are
// mounted behind requireAdmin in server.go — a team-mapped identity must not
// be able to raise its own team's budget. Deleting a record does NOT revoke
// any key; the team reverts to its config policy (if any) or ungoverned
// (ADR-016 precedence: a DB record wins over config only while it exists).
type TeamsHandler struct {
	store       keystore.TeamStore
	configTeams func() []string // names declared in config, for the "source":"config" rows list() adds
	emit        func(audit.Record)
}

// NewTeamsHandler builds the handler. configTeams may be nil (no config-only
// names to surface). emit (nil-safe) receives admin_team_upserted /
// admin_team_deleted / admin_denied audit records.
func NewTeamsHandler(store keystore.TeamStore, configTeams func() []string, emit func(audit.Record)) *TeamsHandler {
	return &TeamsHandler{store: store, configTeams: configTeams, emit: emit}
}

func (h *TeamsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	id, ok := principal.AdminFrom(r.Context())
	if !ok {
		http.Error(w, `{"error":"no admin identity"}`, http.StatusForbidden)
		return
	}
	switch r.Method {
	case http.MethodGet:
		h.list(w, r)
	case http.MethodPut:
		h.upsert(w, r, id)
	case http.MethodDelete:
		h.delete(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (h *TeamsHandler) adminEvent(event string, id principal.AdminIdentity, team string) {
	if h.emit == nil {
		return
	}
	sub, method := id.Subject, id.AuthMethod
	h.emit(audit.Record{
		SchemaVersion: 1,
		Event:         event,
		ID:            ulid.New(),
		TS:            time.Now().UTC().Format(time.RFC3339Nano),
		Principal:     audit.PrincipalRef{Team: team, User: &sub, AuthMethod: &method},
		Request:       audit.RequestRef{Ingress: "admin"},
	})
}

func teamView(t keystore.TeamRecord, source string) map[string]any {
	v := map[string]any{"name": t.Name, "source": source}
	if source != "record" {
		return v // config-only row: name + source only, values live in the file
	}
	v["allowed_models"] = t.AllowedModels
	v["rpm"] = t.RPM
	v["tpm"] = t.TPM
	v["tokens_per_day"] = t.TokensPerDay
	v["quota_on_exceeded"] = t.QuotaOnExceeded
	v["budget_usd_micros"] = t.BudgetUSDMicros
	v["budget_on_exceeded"] = t.BudgetOnExceeded
	v["created_at"] = t.CreatedAt
	v["updated_at"] = t.UpdatedAt
	return v
}

func (h *TeamsHandler) list(w http.ResponseWriter, r *http.Request) {
	records, err := h.store.ListTeams(r.Context())
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	haveRecord := make(map[string]bool, len(records))
	out := make([]map[string]any, 0, len(records))
	for _, t := range records {
		haveRecord[t.Name] = true
		out = append(out, teamView(t, "record"))
	}
	if h.configTeams != nil {
		for _, name := range h.configTeams() {
			if haveRecord[name] {
				continue // a DB record for this name replaces the config row (ADR-016 precedence)
			}
			out = append(out, teamView(keystore.TeamRecord{Name: name}, "config"))
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}

// teamWriteBody is the wire shape of a team upsert. Budget is integer
// microUSD (never float, CLAUDE.md) — the console converts from a USD input
// client-side, same as the key-creation form.
type teamWriteBody struct {
	AllowedModels    []string `json:"allowed_models,omitempty"`
	RPM              int64    `json:"rpm,omitempty"`
	TPM              int64    `json:"tpm,omitempty"`
	TokensPerDay     int64    `json:"tokens_per_day,omitempty"`
	QuotaOnExceeded  string   `json:"quota_on_exceeded,omitempty"`
	BudgetUSDMicros  int64    `json:"budget_usd_micros,omitempty"`
	BudgetOnExceeded string   `json:"budget_on_exceeded,omitempty"`
}

// maxAllowedModelsBytes bounds the serialized allowed_models list, mirroring
// keys.go's maxMetadataBytes cap — an admin-authenticated caller must not be
// able to grow every /admin/teams response without limit.
const maxAllowedModelsBytes = 4096

var validOnExceeded = map[string]bool{"": true, "block": true, "warn": true}

func validateTeamName(name string) error {
	if name == "" {
		return fmt.Errorf("team name required")
	}
	if len(name) > 256 {
		return fmt.Errorf("team name exceeds 256 bytes")
	}
	if strings.Contains(name, "/") {
		return fmt.Errorf("team name must not contain '/'")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return fmt.Errorf("team name must not contain control characters")
		}
	}
	return nil
}

func (b teamWriteBody) validate() error {
	if b.RPM < 0 || b.TPM < 0 || b.TokensPerDay < 0 || b.BudgetUSDMicros < 0 {
		return fmt.Errorf("rpm/tpm/tokens_per_day/budget_usd_micros must be non-negative")
	}
	if !validOnExceeded[b.QuotaOnExceeded] {
		return fmt.Errorf("quota_on_exceeded must be one of: block, warn")
	}
	if !validOnExceeded[b.BudgetOnExceeded] {
		return fmt.Errorf("budget_on_exceeded must be one of: block, warn")
	}
	if len(strings.Join(b.AllowedModels, ",")) > maxAllowedModelsBytes {
		return fmt.Errorf("allowed_models exceeds %d bytes joined", maxAllowedModelsBytes)
	}
	return nil
}

func (h *TeamsHandler) upsert(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/teams/")
	if err := validateTeamName(name); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	var body teamWriteBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSONError(w, http.StatusBadRequest, "invalid request body: "+err.Error())
		return
	}
	if err := body.validate(); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	rec := keystore.TeamRecord{
		Name: name, AllowedModels: body.AllowedModels, RPM: body.RPM, TPM: body.TPM,
		TokensPerDay: body.TokensPerDay, QuotaOnExceeded: body.QuotaOnExceeded,
		BudgetUSDMicros: body.BudgetUSDMicros, BudgetOnExceeded: body.BudgetOnExceeded,
	}
	if err := h.store.UpsertTeam(r.Context(), rec); err != nil {
		http.Error(w, `{"error":"upsert failed"}`, http.StatusInternalServerError)
		return
	}
	h.adminEvent("admin_team_upserted", id, name)
	got, _, err := h.store.GetTeam(r.Context(), name)
	if err != nil {
		http.Error(w, `{"error":"upsert succeeded but read-back failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(teamView(got, "record"))
}

func (h *TeamsHandler) delete(w http.ResponseWriter, r *http.Request, id principal.AdminIdentity) {
	name := strings.TrimPrefix(r.URL.Path, "/admin/teams/")
	if name == "" {
		writeJSONError(w, http.StatusBadRequest, "team name required in path")
		return
	}
	if err := h.store.DeleteTeam(r.Context(), name); err != nil {
		if err == keystore.ErrTeamNotFound {
			http.Error(w, `{"error":"team not found"}`, http.StatusNotFound)
			return
		}
		http.Error(w, `{"error":"delete failed"}`, http.StatusInternalServerError)
		return
	}
	h.adminEvent("admin_team_deleted", id, name)
	w.WriteHeader(http.StatusNoContent)
}

func writeJSONError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// UsersHandler serves GET /admin/users (D3, ADR-016): a READ-ONLY projection
// derived from key owners — there is no users table. Per-user spend is not
// available (audit/analytics events carry a team dimension, not owner) and is
// deliberately omitted rather than approximated.
type UsersHandler struct {
	store keystore.Store
}

func NewUsersHandler(store keystore.Store) *UsersHandler {
	return &UsersHandler{store: store}
}

func (h *UsersHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if _, ok := principal.AdminFrom(r.Context()); !ok {
		http.Error(w, `{"error":"no admin identity"}`, http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	keys, err := h.store.List(r.Context())
	if err != nil {
		http.Error(w, `{"error":"list failed"}`, http.StatusInternalServerError)
		return
	}
	type agg struct {
		teams map[string]bool
		count int
	}
	byOwner := map[string]*agg{}
	var order []string
	for _, k := range keys {
		owner := k.Owner
		if owner == "" {
			owner = "(unowned)"
		}
		a, ok := byOwner[owner]
		if !ok {
			a = &agg{teams: map[string]bool{}}
			byOwner[owner] = a
			order = append(order, owner)
		}
		a.teams[k.Team] = true
		a.count++
	}
	out := make([]map[string]any, 0, len(order))
	for _, owner := range order {
		a := byOwner[owner]
		teams := make([]string, 0, len(a.teams))
		for t := range a.teams {
			teams = append(teams, t)
		}
		out = append(out, map[string]any{"owner": owner, "teams": teams, "key_count": a.count})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"data": out})
}
