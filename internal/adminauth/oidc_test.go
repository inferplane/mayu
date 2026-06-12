package adminauth

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// ---- in-test IdP: discovery + JWKS + key minting ----

type fakeIdP struct {
	srv    *httptest.Server
	rsaKey *rsa.PrivateKey
	ecKey  *ecdsa.PrivateKey
	rsaKID string
	ecKID  string
	hits   struct{ discovery, jwks atomic.Int64 }
	// rotated holds a second RSA key served only after rotate() is called.
	rotated atomic.Bool
	rsaKey2 *rsa.PrivateKey
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	idp := &fakeIdP{rsaKID: "r1", ecKID: "e1"}
	var err error
	if idp.rsaKey, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if idp.rsaKey2, err = rsa.GenerateKey(rand.Reader, 2048); err != nil {
		t.Fatal(err)
	}
	if idp.ecKey, err = ecdsa.GenerateKey(elliptic.P256(), rand.Reader); err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		idp.hits.discovery.Add(1)
		json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.srv.URL,
			"jwks_uri":                              idp.srv.URL + "/keys",
			"authorization_endpoint":                idp.srv.URL + "/auth",
			"token_endpoint":                        idp.srv.URL + "/token",
			"id_token_signing_alg_values_supported": []string{"RS256", "ES256"},
		})
	})
	mux.HandleFunc("/keys", func(w http.ResponseWriter, r *http.Request) {
		idp.hits.jwks.Add(1)
		keys := []map[string]any{
			rsaJWK(idp.rsaKID, &idp.rsaKey.PublicKey),
			ecJWK(idp.ecKID, &idp.ecKey.PublicKey),
		}
		if idp.rotated.Load() {
			keys = append(keys, rsaJWK("r2", &idp.rsaKey2.PublicKey))
		}
		json.NewEncoder(w).Encode(map[string]any{"keys": keys})
	})
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)
	return idp
}

func rsaJWK(kid string, pub *rsa.PublicKey) map[string]any {
	return map[string]any{
		"kty": "RSA", "kid": kid, "use": "sig", "alg": "RS256",
		"n": b64(pub.N.Bytes()),
		"e": b64(big.NewInt(int64(pub.E)).Bytes()),
	}
}

func ecJWK(kid string, pub *ecdsa.PublicKey) map[string]any {
	byteLen := (pub.Curve.Params().BitSize + 7) / 8
	x := make([]byte, byteLen)
	y := make([]byte, byteLen)
	pub.X.FillBytes(x)
	pub.Y.FillBytes(y)
	return map[string]any{
		"kty": "EC", "kid": kid, "use": "sig", "alg": "ES256", "crv": "P-256",
		"x": b64(x), "y": b64(y),
	}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

// mint builds a signed JWT. alg ∈ {RS256, ES256, HS256, none}; extra merges
// into the default claims (set a value to nil to delete the default).
func (idp *fakeIdP) mint(t *testing.T, alg, kid, clientID string, extra map[string]any) string {
	t.Helper()
	now := time.Now()
	claims := map[string]any{
		"iss":    idp.srv.URL,
		"sub":    "user-1",
		"aud":    clientID,
		"exp":    now.Add(time.Hour).Unix(),
		"iat":    now.Add(-time.Minute).Unix(),
		"groups": []string{"team-alpha"},
	}
	for k, v := range extra {
		if v == nil {
			delete(claims, k)
		} else {
			claims[k] = v
		}
	}
	header := map[string]any{"alg": alg, "typ": "JWT"}
	if kid != "" {
		header["kid"] = kid
	}
	hb, _ := json.Marshal(header)
	cb, _ := json.Marshal(claims)
	signing := b64(hb) + "." + b64(cb)
	digest := sha256.Sum256([]byte(signing))
	var sig []byte
	switch alg {
	case "RS256":
		key := idp.rsaKey
		if kid == "r2" {
			key = idp.rsaKey2
		}
		s, err := rsa.SignPKCS1v15(rand.Reader, key, 0x5, digest[:]) // crypto.SHA256 == 5
		if err != nil {
			t.Fatal(err)
		}
		sig = s
	case "ES256":
		r, s, err := ecdsa.Sign(rand.Reader, idp.ecKey, digest[:])
		if err != nil {
			t.Fatal(err)
		}
		sig = make([]byte, 64)
		r.FillBytes(sig[:32])
		s.FillBytes(sig[32:])
	case "HS256":
		// alg-confusion: HMAC keyed with the RSA PUBLIC key DER — a verifier
		// that trusts the header alg would validate this with the same key.
		der, _ := x509.MarshalPKIXPublicKey(&idp.rsaKey.PublicKey)
		mac := hmac.New(sha256.New, der)
		mac.Write([]byte(signing))
		sig = mac.Sum(nil)
	case "none":
		sig = nil
	default:
		t.Fatalf("unknown alg %s", alg)
	}
	return signing + "." + b64(sig)
}

func newTestVerifier(idp *fakeIdP, clientID string) *Verifier {
	v := NewVerifier(VerifierConfig{Issuer: idp.srv.URL, ClientID: clientID, GroupsClaim: "groups"})
	v.backoff = 200 * time.Millisecond // fast windows for tests
	return v
}

// ---- adversarial suite ----

const cid = "inferplane-admin"

func TestVerifyValidTokens(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	for _, tc := range []struct{ name, alg, kid string }{
		{"RS256", "RS256", "r1"},
		{"ES256", "ES256", "e1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claims, err := v.Verify(context.Background(), idp.mint(t, tc.alg, tc.kid, cid, nil))
			if err != nil {
				t.Fatalf("valid %s rejected: %v", tc.name, err)
			}
			if claims.Subject != "user-1" || len(claims.Groups) != 1 || claims.Groups[0] != "team-alpha" {
				t.Fatalf("claims = %+v", claims)
			}
		})
	}
}

func TestVerifyRejectsBadAlgorithms(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	for _, tc := range []struct{ name, token string }{
		{"alg none", idp.mint(t, "none", "", cid, nil)},
		{"HS256 public-key confusion", idp.mint(t, "HS256", "r1", cid, nil)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), tc.token); err == nil {
				t.Fatal("must reject")
			}
		})
	}
}

func TestVerifyRejectsWrongIssuerAudience(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	for name, extra := range map[string]map[string]any{
		"wrong iss":   {"iss": "https://evil.example.com"},
		"wrong aud":   {"aud": "other-app"},
		"missing aud": {"aud": nil},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r1", cid, extra)); err == nil {
				t.Fatal("must reject")
			}
		})
	}
}

func TestVerifyMultiAudienceAzpRule(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	multi := []string{cid, "other-app"}
	cases := []struct {
		name  string
		extra map[string]any
		ok    bool
	}{
		{"multi-aud without azp", map[string]any{"aud": multi}, false},
		{"multi-aud azp matches", map[string]any{"aud": multi, "azp": cid}, true},
		{"multi-aud azp mismatch", map[string]any{"aud": multi, "azp": "other-app"}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r1", cid, tc.extra))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v, want ok=%v (OIDC Core 3.1.3.7)", err, tc.ok)
			}
		})
	}
}

func TestVerifyClockSkew(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	now := time.Now()
	cases := []struct {
		name  string
		extra map[string]any
		ok    bool
	}{
		{"expired beyond skew", map[string]any{"exp": now.Add(-5 * time.Minute).Unix()}, false},
		{"expired within skew", map[string]any{"exp": now.Add(-30 * time.Second).Unix()}, true},
		{"iat far future", map[string]any{"iat": now.Add(5 * time.Minute).Unix()}, false},
		{"iat near future within skew", map[string]any{"iat": now.Add(30 * time.Second).Unix()}, true},
		{"nbf near future within skew", map[string]any{"nbf": now.Add(30 * time.Second).Unix()}, true},
		{"nbf far future", map[string]any{"nbf": now.Add(5 * time.Minute).Unix()}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r1", cid, tc.extra))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v, want ok=%v", err, tc.ok)
			}
		})
	}
}

func TestVerifyKeyRotation(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	// Prime the JWKS cache with the original keys.
	if _, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r1", cid, nil)); err != nil {
		t.Fatal(err)
	}
	// Rotate: serve r2; a token signed by r2 must verify after one refetch.
	idp.rotated.Store(true)
	if _, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r2", cid, nil)); err != nil {
		t.Fatalf("rotated kid not picked up: %v", err)
	}
}

func TestVerifyGroupsClaimShapes(t *testing.T) {
	idp := newFakeIdP(t)
	v := newTestVerifier(idp, cid)
	cases := []struct {
		name   string
		groups any
		want   []string
		ok     bool
	}{
		{"string array", []string{"a", "b"}, []string{"a", "b"}, true},
		{"single string becomes one-element", "solo", []string{"solo"}, true},
		{"missing claim is empty", nil, nil, true},
		{"scalar number rejected", 42, nil, false},
		{"mixed array rejected", []any{"a", 1}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			extra := map[string]any{"groups": tc.groups}
			claims, err := v.Verify(context.Background(), idp.mint(t, "RS256", "r1", cid, extra))
			if (err == nil) != tc.ok {
				t.Fatalf("err=%v, want ok=%v", err, tc.ok)
			}
			if tc.ok && fmt.Sprint(claims.Groups) != fmt.Sprint(tc.want) {
				t.Fatalf("groups=%v want %v", claims.Groups, tc.want)
			}
		})
	}
}

// TestVerifyDiscoveryOutageNegativeCache pins the r1/r3 ops finding: with the
// IdP unreachable, repeated verifies must hit the negative cache — at most one
// outbound attempt per backoff window — and recover after the window.
func TestVerifyDiscoveryOutageNegativeCache(t *testing.T) {
	idp := newFakeIdP(t)
	tok := idp.mint(t, "RS256", "r1", cid, nil)
	idp.srv.Close() // IdP down before first use

	v := NewVerifier(VerifierConfig{Issuer: idp.srv.URL, ClientID: cid, GroupsClaim: "groups"})
	v.backoff = 300 * time.Millisecond

	start := time.Now()
	for i := 0; i < 10; i++ {
		if _, err := v.Verify(context.Background(), tok); err == nil {
			t.Fatal("verify must fail with IdP down")
		}
	}
	// 10 failing verifies inside one window must be fast (no 10× dial timeouts).
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("negative cache not effective: 10 verifies took %v", elapsed)
	}
	if got := v.discoveryAttempts.Load(); got > 2 {
		t.Fatalf("discovery attempted %d times within the window, want ≤2", got)
	}
}

func TestVerifierIsLazyAtConstruction(t *testing.T) {
	// Constructing a Verifier must not dial anything (IdP down at boot is fine).
	v := NewVerifier(VerifierConfig{Issuer: "https://127.0.0.1:1", ClientID: cid})
	if v == nil {
		t.Fatal("nil verifier")
	}
	if v.discoveryAttempts.Load() != 0 {
		t.Fatal("constructor must not attempt discovery")
	}
}
