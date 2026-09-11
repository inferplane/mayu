// Command inferplaned is the inferplane control plane: policy and
// routing-rule distribution, budget-lease issuance, short-lived credential
// brokering, and telemetry aggregation. Inference traffic NEVER passes
// through it — mayu (cmd/mayu), the node-local data plane, sits on the
// request path and enforces what inferplaned distributes.
//
// Distribution (ADR-034) is live: --policies points at GovernancePolicy
// files/directories (watched — edits propagate on the next heartbeat), data
// planes POST /v1alpha1/sync (policy pull + consumption report + lease
// renewal + rejection report in one round trip), and GET /v1alpha1/dataplanes
// shows the connected version distribution. Credential brokering (ADR-040)
// is live behind INFERPLANED_BROKER_ROLE_ARN: when set, POST
// /v1alpha1/credentials vends short-lived Bedrock credentials to data planes.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
	_ "time/tzdata" // embed the IANA tz database: distroless/static ships no /usr/share/zoneinfo, so a named-zone LoadLocation would fail only in production

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"

	"github.com/inferplane/inferplane/internal/adminauth"
	"github.com/inferplane/inferplane/internal/controlplane"
	"github.com/inferplane/inferplane/internal/controlplane/ui"
	"github.com/inferplane/inferplane/internal/policy"
	"github.com/inferplane/inferplane/internal/policystore"
	"github.com/inferplane/inferplane/internal/telemetry"
)

// policyStoreBootTimeout bounds the policy store's boot attach (seed + first
// load) so an unreachable database fails the boot instead of hanging it.
const policyStoreBootTimeout = 10 * time.Second

func main() {
	// Loopback-only by default: without INFERPLANED_TOKEN set there is no
	// authentication, so the control plane must not listen on all
	// interfaces. Set the token (shared with each mayu's
	// control_plane.token_ref) before widening the listen address.
	listen := flag.String("listen", "127.0.0.1:7601", "control plane listen address")
	policies := flag.String("policies", "", "GovernancePolicy file or directory to distribute (watched)")
	flag.Parse()

	oidc, err := loadOIDCEnv(os.Getenv)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}

	if err := run(*listen, *policies, os.Getenv("INFERPLANED_TOKEN"), oidc); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// isLoopback reports whether the listen address binds only a loopback
// interface. An empty host (":7601") binds ALL interfaces and is not
// loopback; "localhost" is trusted as loopback without resolution.
func isLoopback(listen string) bool {
	host, _, err := net.SplitHostPort(listen)
	if err != nil || host == "" {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// validateBoot rejects the two ways INFERPLANED_TOKEN/OIDC config could leave
// inferplaned either unauthenticatable or unauthenticated. Split out from
// run() so it's testable without starting (and blocking on) a real listener.
func validateBoot(listen, token string, oidc *oidcEnv) error {
	// A JWT-shaped static token would be routed to the OIDC verifier by
	// authn's total rule (adminauth.IsOIDCBearerShape) and could never
	// authenticate — same rejection internal/config's validateOIDC applies
	// to mayu's static admin tokens.
	if adminauth.IsOIDCBearerShape(token) {
		return fmt.Errorf("INFERPLANED_TOKEN must not be JWT-shaped (three dot-separated base64url segments) — it would be routed to the OIDC verifier and could never authenticate")
	}

	// Refuse to serve unauthenticated beyond loopback (PR #50 review
	// finding): an open /v1alpha1/sync would let any network peer reset
	// reported spend or read the fleet view. An advisory log is not a
	// guard — this is. OIDC configured covers authentication on its own
	// (an SSO-only deploy with no static token is a legitimate posture),
	// so it — not just a non-empty token — satisfies the loopback waiver.
	if token == "" && oidc == nil && !isLoopback(listen) {
		return fmt.Errorf("INFERPLANED_TOKEN must be set (or console SSO configured) when --listen (%s) is not loopback; refusing to start unauthenticated on a non-loopback address", listen)
	}
	return nil
}

// validateBrokerEnv checks the ADR-040 credential-broker env pair.
// INFERPLANED_BROKER_ROLE_ARN unset ⇒ the endpoint is never mounted and this
// is a no-op, so every existing deployment is unaffected.
func validateBrokerEnv(token, brokerToken, roleARN string) error {
	if roleARN == "" {
		return nil
	}
	if brokerToken == "" {
		return fmt.Errorf("INFERPLANED_BROKER_TOKEN must be set when INFERPLANED_BROKER_ROLE_ARN is set: POST /v1alpha1/credentials mints portable AWS credentials and must never be reachable unauthenticated")
	}
	// Same rejection validateBoot applies to INFERPLANED_TOKEN: a JWT-shaped
	// secret is a console identity by shape, and the broker endpoint rejects
	// those outright, so it could never authenticate.
	if adminauth.IsOIDCBearerShape(brokerToken) {
		return fmt.Errorf("INFERPLANED_BROKER_TOKEN must not be JWT-shaped (three dot-separated base64url segments) — the credential endpoint rejects console-shaped bearers and it could never authenticate")
	}
	// The ADR-028 distinct-client-id rule (ADR-040 decision 1): the heartbeat
	// token is deployed to every node and grants policy reads. If the same
	// value also minted AWS credentials, compromising any one node's env would
	// yield Bedrock access WITHOUT mayu — the bypass this feature closes.
	if brokerToken == token {
		return fmt.Errorf("INFERPLANED_BROKER_TOKEN must differ from INFERPLANED_TOKEN — a heartbeat-token compromise must not also yield credential brokering")
	}
	return nil
}

// validatePolicyWriteEnv checks the dedicated policy-write bearer
// (INFERPLANED_POLICY_WRITE_TOKEN, review/fable5 §08 B1). Unset is allowed —
// writes then fail closed for every static bearer (authnWrite) and only a
// console OIDC identity can edit policies. When set it must be a distinct,
// non-JWT-shaped secret: the heartbeat token is on every data plane, and a
// write token equal to it would hand every node operator fleet-wide policy
// authority — the exact hole this token exists to close.
func validatePolicyWriteEnv(token, brokerToken, writeToken string) error {
	if writeToken == "" {
		return nil
	}
	if adminauth.IsOIDCBearerShape(writeToken) {
		return fmt.Errorf("INFERPLANED_POLICY_WRITE_TOKEN must not be JWT-shaped — a JWT-shaped bearer is routed to the OIDC verifier and could never match")
	}
	if writeToken == token {
		return fmt.Errorf("INFERPLANED_POLICY_WRITE_TOKEN must differ from INFERPLANED_TOKEN — the heartbeat token is deployed to every data plane and must not carry policy-write authority")
	}
	if brokerToken != "" && writeToken == brokerToken {
		return fmt.Errorf("INFERPLANED_POLICY_WRITE_TOKEN must differ from INFERPLANED_BROKER_TOKEN — one secret must not grant both credential brokering and policy writes")
	}
	return nil
}

// buildMux assembles every HTTP route inferplaned serves. Split out from
// run() so Task 8's end-to-end test can drive the real mux (real
// adminauth.Verifier, real controlplane wiring) through httptest without
// starting — and blocking on — a real listener. closePG releases the
// Postgres pool when INFERPLANED_USAGE_DSN was set; it is a no-op otherwise
// and safe to defer unconditionally.
func buildMux(policies, token string, oidc *oidcEnv) (mux *http.ServeMux, cp *controlplane.Server, closePG func(), err error) {
	var opts []controlplane.Option
	var connectSrc []string
	if oidc != nil {
		verifier := adminauth.NewVerifier(adminauth.VerifierConfig{
			Issuer: oidc.Issuer, ClientID: oidc.ClientID, GroupsClaim: oidc.GroupsClaim,
		})
		opts = append(opts, controlplane.WithOIDC(verifier, oidc.mapping()))
		connectSrc = oidc.connectSrc()
	}

	mux = http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "ok")
	})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// The supported config API versions are the first thing a data
		// plane (or operator) needs to know before propagating rules.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"apiVersions": policy.SupportedAPIVersions,
		})
	})

	// Usage telemetry (ADR-036) mounts UNCONDITIONALLY — a telemetry-only
	// inferplaned (no --policies) is a valid deployment; policy distribution
	// below stays opt-in. Memory keeps a bounded 24h of windows; setting
	// INFERPLANED_USAGE_DSN (the INFERPLANED_TOKEN precedent — a fixed env
	// var, never a flag value carrying a secret) layers the durable Postgres
	// store behind it: writes ack only after the PG commit, queries fall
	// back to memory (marked degraded) through an outage. Construction is
	// lazy — a PG outage never blocks boot.
	agg := telemetry.Aggregator(telemetry.NewMemoryAggregator(24 * time.Hour))
	closePG = func() {}
	if dsn := os.Getenv("INFERPLANED_USAGE_DSN"); dsn != "" {
		pg, err := telemetry.NewPostgresAggregator(dsn)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("usage store: %w", err)
		}
		closePG = pg.Close
		agg = telemetry.NewDurableAggregator(agg, pg)
		log.Print("inferplaned: usage telemetry persisting to postgres (INFERPLANED_USAGE_DSN set)")
	}
	controlplane.NewUsageServer(token, agg, opts...).Mount(mux)
	// Credential brokering (ADR-040) is opt-in on INFERPLANED_BROKER_ROLE_ARN
	// — the INFERPLANED_POLICY_DSN pattern. Unset ⇒ POST /v1alpha1/credentials
	// is not registered at all (404) and behavior is byte-identical. It is a
	// MACHINE channel with its own token: the console's OIDC identities are
	// rejected, never verified.
	brokerRoleARN := os.Getenv("INFERPLANED_BROKER_ROLE_ARN")
	brokerToken := os.Getenv("INFERPLANED_BROKER_TOKEN")
	if err := validateBrokerEnv(token, brokerToken, brokerRoleARN); err != nil {
		return nil, nil, closePG, err
	}
	if brokerRoleARN == "" && brokerToken != "" {
		log.Print("inferplaned: INFERPLANED_BROKER_TOKEN is set but INFERPLANED_BROKER_ROLE_ARN is not — credential brokering is OFF and POST /v1alpha1/credentials is not mounted")
	}
	if brokerRoleARN != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
		if err != nil {
			return nil, nil, closePG, fmt.Errorf("credential broker: aws config: %w", err)
		}
		bs := controlplane.NewBrokerServer(brokerToken, brokerRoleARN, sts.NewFromConfig(awsCfg))
		bs.OnError = func(err error) { log.Print("inferplaned: ", err) }
		bs.Mount(mux)
		log.Print("inferplaned: credential brokering enabled (INFERPLANED_BROKER_ROLE_ARN set) — POST /v1alpha1/credentials vends short-lived Bedrock credentials to data planes")
	}
	// The read-only usage console (data-free static shell; data via the
	// bearer-gated usage API — see internal/controlplane/ui). connectSrc is
	// nil (byte-identical CSP) unless INFERPLANED_OIDC_LOGIN_ORIGINS is set.
	mux.Handle("/ui/", http.StripPrefix("/ui", ui.Handler(connectSrc...)))
	if oidc != nil {
		// Exact pattern beats the "/ui/" prefix pattern above (Go 1.22
		// ServeMux precedence) regardless of registration order. Mounted
		// only when OIDC is configured — an always-{sso:false} route would
		// be a permanent lie about a feature that was never wired up.
		mux.HandleFunc("GET /ui/auth/config", controlplane.AuthConfigHandler(func() *controlplane.AuthConfigView {
			return &controlplane.AuthConfigView{SSO: true, Issuer: oidc.Issuer, ClientID: oidc.ClientID}
		}))
	}

	// Policy documents are DB-authoritative when INFERPLANED_POLICY_DSN is set
	// (ADR-038): the --policies file channel becomes the ONE-TIME seed source
	// and stops being watched. v1 keeps --policies required as that seed
	// source — a DSN alone has nothing to seed from.
	policyDSN := os.Getenv("INFERPLANED_POLICY_DSN")
	if policyDSN != "" && policies == "" {
		return nil, nil, closePG, errors.New("INFERPLANED_POLICY_DSN is set but --policies is empty: the policy file channel is the one-time seed source for the policy store")
	}
	if policies != "" {
		cp, err = controlplane.NewServer(token, policies, opts...)
		if err != nil {
			return nil, nil, closePG, fmt.Errorf("policies: %w", err)
		}
		// Policy writes need their own authority (review/fable5 §08 B1).
		writeToken := os.Getenv("INFERPLANED_POLICY_WRITE_TOKEN")
		if err := validatePolicyWriteEnv(token, brokerToken, writeToken); err != nil {
			return nil, nil, closePG, err
		}
		cp.SetPolicyWriteToken(writeToken)
		if policyDSN != "" && writeToken == "" && oidc == nil {
			log.Print("inferplaned: INFERPLANED_POLICY_DSN is set but neither INFERPLANED_POLICY_WRITE_TOKEN nor console SSO is configured — PUT/DELETE /v1alpha1/policies will refuse every bearer (the heartbeat token never carries write authority); set a write token to edit policies over the API")
		}
		if policyDSN != "" {
			ps, err := policystore.NewPostgres(policyDSN)
			if err != nil {
				return nil, nil, closePG, fmt.Errorf("policy store: %w", err)
			}
			closeUsage := closePG
			closePG = func() { ps.Close(); closeUsage() }
			// Unlike the usage store (ADR-036, lazy — losing telemetry beats
			// blocking boot), the policy store is AUTHORITATIVE: booting on
			// possibly-stale file content while claiming DB authority would
			// distribute the wrong rules. Attach (seed + first load) is
			// therefore a hard, bounded boot dependency.
			actx, cancel := context.WithTimeout(context.Background(), policyStoreBootTimeout)
			defer cancel()
			if err := cp.AttachPolicyStore(actx, ps); err != nil {
				return nil, nil, closePG, fmt.Errorf("policy store: %w", err)
			}
			log.Print("inferplaned: GovernancePolicy documents are postgres-authoritative (INFERPLANED_POLICY_DSN set); --policies is seed-only and no longer watched")
		}
		cp.Mount(mux)
	}
	return mux, cp, closePG, nil
}

func run(listen, policies, token string, oidc *oidcEnv) error {
	if err := validateBoot(listen, token, oidc); err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	mux, cp, closePG, err := buildMux(policies, token, oidc)
	if err != nil {
		return err
	}
	defer closePG()
	if cp != nil && !cp.PolicyStoreAttached() {
		go cp.Watch(ctx, func(err error) { log.Print("inferplaned: ", err) })
	}

	srv := &http.Server{
		Addr:    listen,
		Handler: mux,
		// Full read/write deadlines, not just headers: a slow-dripped sync
		// body would otherwise hold a goroutine forever — MaxBytesReader
		// bounds the size but not the rate (PR #50 review finding).
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		mode := "scaffold: health endpoints only"
		if cp != nil {
			mode = "distributing policies from " + policies
			if token == "" && oidc == nil {
				mode += " (UNAUTHENTICATED — set INFERPLANED_TOKEN before leaving loopback)"
			}
		}
		if oidc != nil {
			mode += " (console SSO enabled)"
		}
		log.Printf("inferplaned control plane listening on %s (%s)", listen, mode)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("listen on %s: %w", listen, err)
	case <-ctx.Done():
		log.Print("shutting down")
		sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(sctx); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			return fmt.Errorf("shutdown: %w", err)
		}
		return nil
	}
}
