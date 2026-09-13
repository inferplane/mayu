package controlplane

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/policy"
)

// BudgetAuthorityBackend is implemented by the Postgres ledger. It owns policy
// reads, clock and issuance in one transaction; process-local caches cannot
// authorize grants in this mode.
type BudgetAuthorityBackend interface {
	Sync(context.Context, string, policy.AuthorityRequest) (policy.SyncResponse, error)
}

func (s *Server) SetBudgetAuthority(backend BudgetAuthorityBackend) { s.authority = backend }

func (s *Server) BudgetAuthorityReady(ctx context.Context) error {
	if s.authority == nil {
		return nil
	}
	if ready, ok := s.authority.(interface{ Ready(context.Context) error }); ok {
		return ready.Ready(ctx)
	}
	return nil
}

func (s *Server) authoritySync(w http.ResponseWriter, r *http.Request, request policy.SyncRequest) {
	if request.Authority == nil || request.Authority.Protocol != policy.AuthorityProtocol {
		http.Error(w, `{"error":"durable budget protocol required"}`, http.StatusConflict)
		return
	}
	// Portable budget authority is a machine channel, not console OIDC.
	token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if s.token == "" || adminauth.IsOIDCBearerShape(token) ||
		!strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") ||
		subtle.ConstantTimeCompare([]byte(token), []byte(s.token)) != 1 {
		http.Error(w, `{"error":"machine authentication required"}`, http.StatusUnauthorized)
		return
	}
	if len(request.Reports) != 0 {
		http.Error(w, `{"error":"legacy reports are invalid in durable mode"}`, http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	response, err := s.authority.Sync(ctx, request.Dataplane, *request.Authority)
	if err != nil {
		http.Error(w, `{"error":"durable budget authority unavailable"}`, http.StatusServiceUnavailable)
		return
	}
	if response.Authority == nil || response.Authority.Protocol != policy.AuthorityProtocol || len(response.Leases) != 0 {
		http.Error(w, `{"error":"invalid durable authority response"}`, http.StatusServiceUnavailable)
		return
	}
	s.mu.Lock()
	s.dataplanes[request.Dataplane] = &dpInfo{
		APIVersions: request.APIVersions, Generation: response.Generation,
		LastSeen: time.Now(), Rejections: request.Rejections,
	}
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}
