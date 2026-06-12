package adminauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
)

// skew is the clock-skew leeway applied to exp/iat/nbf. go-oidc exposes no
// leeway knob, so its built-in expiry check is disabled and re-done here
// (P2 gate r1: CLI-minted tokens on skewed laptops).
const skew = 60 * time.Second

// VerifierConfig configures the admin-plane ID-token verifier. Issuer is
// taken as given — config.Load enforces https; tests construct the Verifier
// directly with an httptest issuer.
type VerifierConfig struct {
	Issuer      string
	ClientID    string
	GroupsClaim string // default "groups"; top-level claim only, no traversal
}

// Claims is the PII-minimal verified output: the opaque subject and the raw
// group names (consumed by Resolve and dropped — they never enter the
// request context).
type Claims struct {
	Subject string
	Groups  []string
}

// Verifier validates externally-acquired ID tokens against the issuer's
// JWKS (resource-server-only, ADR-004). Discovery is lazy — an unreachable
// IdP at boot must not block startup (break-glass still works) — and
// failures are negative-cached with a backoff window so an outage flood
// neither hammers the IdP nor adds per-request dial latency.
type Verifier struct {
	cfg     VerifierConfig
	backoff time.Duration // discovery negative-cache + JWKS miss window (test-tunable)

	discoveryAttempts atomic.Int64 // observability for tests

	mu      sync.Mutex
	ver     *oidc.IDTokenVerifier
	lastTry time.Time
	lastErr error
}

// NewVerifier constructs a lazy Verifier; it performs no network I/O.
func NewVerifier(cfg VerifierConfig) *Verifier {
	if cfg.GroupsClaim == "" {
		cfg.GroupsClaim = "groups"
	}
	return &Verifier{cfg: cfg, backoff: 10 * time.Second}
}

// getVerifier returns the cached IDTokenVerifier, running discovery at most
// once per backoff window while the IdP is unreachable.
func (v *Verifier) getVerifier() (*oidc.IDTokenVerifier, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.ver != nil {
		return v.ver, nil
	}
	if v.lastErr != nil && time.Since(v.lastTry) < v.backoff {
		return nil, fmt.Errorf("oidc discovery (negative-cached): %w", v.lastErr)
	}
	v.lastTry = time.Now()
	v.discoveryAttempts.Add(1)
	// Background context, NOT the request context: go-oidc's remote key set
	// keeps this context for every future JWKS fetch — a canceled request
	// context would poison later refreshes. The client timeout bounds each
	// individual fetch instead.
	bg := oidc.ClientContext(context.Background(), &http.Client{Timeout: 5 * time.Second})
	provider, err := oidc.NewProvider(bg, v.cfg.Issuer)
	if err != nil {
		v.lastErr = err
		return nil, err
	}
	var meta struct {
		JWKSURL string `json:"jwks_uri"`
	}
	if err := provider.Claims(&meta); err != nil || meta.JWKSURL == "" {
		v.lastErr = fmt.Errorf("oidc discovery: no jwks_uri: %v", err)
		return nil, v.lastErr
	}
	keys := &missLimitedKeySet{
		inner:  oidc.NewRemoteKeySet(bg, meta.JWKSURL),
		window: v.backoff,
	}
	v.lastErr = nil
	v.ver = oidc.NewVerifier(v.cfg.Issuer, keys, &oidc.Config{
		ClientID:             v.cfg.ClientID,
		SupportedSigningAlgs: []string{oidc.RS256, oidc.ES256}, // alg-confusion guard: none/HS256 rejected
		SkipExpiryCheck:      true,                             // replaced by skew-aware checks below
	})
	return v.ver, nil
}

// Verify validates raw and returns its PII-minimal claims.
func (v *Verifier) Verify(ctx context.Context, raw string) (Claims, error) {
	ver, err := v.getVerifier()
	if err != nil {
		return Claims{}, err
	}
	idt, err := ver.Verify(ctx, raw) // signature, iss, aud∋client_id, alg allow-list
	if err != nil {
		return Claims{}, fmt.Errorf("oidc verify: %w", err)
	}

	now := time.Now()
	if !idt.Expiry.IsZero() && now.After(idt.Expiry.Add(skew)) {
		return Claims{}, errors.New("oidc verify: token expired")
	}
	if !idt.IssuedAt.IsZero() && idt.IssuedAt.After(now.Add(skew)) {
		return Claims{}, errors.New("oidc verify: token issued in the future")
	}

	var extra struct {
		Azp string  `json:"azp"`
		Nbf float64 `json:"nbf"`
	}
	if err := idt.Claims(&extra); err != nil {
		return Claims{}, fmt.Errorf("oidc verify: claims: %w", err)
	}
	if extra.Nbf != 0 && time.Unix(int64(extra.Nbf), 0).After(now.Add(skew)) {
		return Claims{}, errors.New("oidc verify: token not yet valid (nbf)")
	}
	// OIDC Core §3.1.3.7: with multiple audiences the authorized party must
	// be us — otherwise a token minted for another app verifies here.
	if len(idt.Audience) > 1 && extra.Azp != v.cfg.ClientID {
		return Claims{}, errors.New("oidc verify: multi-audience token requires azp == client_id")
	}

	groups, err := extractGroups(idt, v.cfg.GroupsClaim)
	if err != nil {
		return Claims{}, err
	}
	return Claims{Subject: idt.Subject, Groups: groups}, nil
}

// extractGroups reads the configured TOP-LEVEL claim: a string array, or a
// single string (one-element list). A missing claim is empty (the caller
// 403s on no mapping); any other shape is an error — nested traversal and
// type coercion are authz surprises (P2 gate r3).
func extractGroups(idt *oidc.IDToken, claim string) ([]string, error) {
	var all map[string]any
	if err := idt.Claims(&all); err != nil {
		return nil, fmt.Errorf("oidc verify: claims: %w", err)
	}
	raw, ok := all[claim]
	if !ok || raw == nil {
		return nil, nil
	}
	switch g := raw.(type) {
	case string:
		return []string{g}, nil
	case []any:
		out := make([]string, 0, len(g))
		for _, e := range g {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("oidc verify: groups claim %q has non-string element", claim)
			}
			out = append(out, s)
		}
		return out, nil
	default:
		return nil, fmt.Errorf("oidc verify: groups claim %q is not a string array", claim)
	}
}

// missLimitedKeySet rate-limits signature-verification failures: after a
// failed VerifySignature (which is when go-oidc refetches the JWKS for an
// unknown kid), further attempts inside the window fail fast WITHOUT touching
// the network — a forged-kid flood cannot hammer the IdP (P2 gate r1/r3).
// Successful verifications never arm the limiter, so legitimate key rotation
// (miss → refetch → success) is unaffected.
type missLimitedKeySet struct {
	inner  oidc.KeySet
	window time.Duration

	mu       sync.Mutex
	lastMiss time.Time
}

func (l *missLimitedKeySet) VerifySignature(ctx context.Context, jwt string) ([]byte, error) {
	l.mu.Lock()
	if !l.lastMiss.IsZero() && time.Since(l.lastMiss) < l.window {
		l.mu.Unlock()
		return nil, errors.New("jwks: verification rate-limited after recent failure")
	}
	l.mu.Unlock()

	payload, err := l.inner.VerifySignature(ctx, jwt)
	if err != nil {
		l.mu.Lock()
		l.lastMiss = time.Now()
		l.mu.Unlock()
		return nil, err
	}
	return payload, nil
}
