// Command apiserver is the ksquad-apiserver HTTP host (§17.3) — the binary the console BFF
// (ISI-2180) proxies every request to at http://ksquad-apiserver:8080. It stands up the one
// gorilla/mux root router that mounts the §7.5 discussion surface behind the §13 BFF authz
// choke point, hosts the §4.4 SSE run-progress hub, and exposes health/readiness for the
// Deployment probes.
//
// It mirrors the cmd/memory pattern: load config (file < env < flags), open the shared Postgres
// DSN, and fail closed at start if the store is unreachable — a Run/discussion write must never
// hit a half-initialized host.
//
// Identity (§13/ADR-033): the BFF forwards ONLY the HttpOnly ksquad_session cookie; this host
// resolves it into the server-derived AuthorContext (the "internal identity mint"). The Postgres
// session store is owned by the auth/console track and not yet built, so absent a resolver the
// gated surface fails closed (401). Set KSQUAD_DEV_SESSIONS to a JSON sessions file for a local
// end-to-end run only.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"flag"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/internal/artifactbrowser"
	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/events"
)

func main() {
	var (
		configPath  = flag.String("config", "/etc/ksquad/apiserver-config.json", "Path to apiserver configuration file")
		databaseURL = flag.String("database-url", "", "Postgres connection URL (overrides config/env)")
		httpPort    = flag.Int("http-port", 0, "HTTP port (overrides config)")
	)
	flag.Parse()

	cfg, err := apiserver.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("ksquad-apiserver: load config: %v", err)
	}
	// Flags and env override the file. DATABASE_URL is the conventional deployment knob.
	if *databaseURL != "" {
		cfg.DatabaseURL = *databaseURL
	} else if env := os.Getenv("DATABASE_URL"); env != "" {
		cfg.DatabaseURL = env
	}
	if *httpPort != 0 {
		cfg.HTTPPort = *httpPort
	} else if env := os.Getenv("HTTP_PORT"); env != "" {
		if p, perr := strconv.Atoi(env); perr == nil {
			cfg.HTTPPort = p
		}
	}
	if env := os.Getenv("KSQUAD_SESSION_COOKIE"); env != "" {
		cfg.SessionCookie = env
	}
	cfg.ApplyEnvOverrides()
	if err := cfg.Validate(); err != nil {
		log.Fatalf("ksquad-apiserver: invalid config: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail closed at start: connect + ping the store of record before serving.
	db, err := sql.Open("pgx", cfg.DatabaseURL)
	if err != nil {
		log.Fatalf("ksquad-apiserver: open database: %v", err)
	}
	defer db.Close()
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	if err := db.PingContext(startCtx); err != nil {
		cancel()
		log.Fatalf("ksquad-apiserver: refusing to start, database unreachable: %v", err)
	}
	cancel()
	log.Printf("ksquad-apiserver: store ready")

	// §13 identity resolver. Production resolves the forwarded ksquad_session cookie through the
	// Postgres auth.session store (ISI-2758, db/migrations/0006_auth_schema.sql); a dev sessions file
	// overrides it for a local end-to-end run only. Both are fail-closed — an unresolvable cookie is 401.
	var resolver apiserver.SessionResolver
	if devPath := os.Getenv("KSQUAD_DEV_SESSIONS"); devPath != "" {
		r, derr := apiserver.LoadStaticSessions(devPath)
		if derr != nil {
			log.Fatalf("ksquad-apiserver: load dev sessions: %v", derr)
		}
		resolver = r
		// devPath is the operator-supplied KSQUAD_DEV_SESSIONS env path, echoed once in a startup
		// warning — not attacker-controlled request input.
		// #nosec G706 -- operator-supplied env path in a startup warning, not request-tainted input.
		log.Printf("ksquad-apiserver: WARNING — using static dev sessions from %s; NOT for production", devPath)
	} else {
		resolver = apiserver.NewPostgresSessionResolver(db)
		log.Printf("ksquad-apiserver: resolving ksquad_session via the Postgres auth.session store (fail-closed)")
	}

	authn := apiserver.NewCookieAuthenticator(resolver)
	authn.CookieName = cfg.SessionCookie

	// ── Epic 15 identity seam (ISI-2920): the pkg/auth core inside the apiserver ──
	authSvc := buildAuthService(ctx, db, cfg)
	if authSvc != nil {
		bootstrapAdmin(ctx, db, cfg)
	}
	var trustedProxies []*net.IPNet
	if cfg.TrustedProxies != "" {
		trustedProxies = auth.ParseCIDRs(cfg.TrustedProxies)
		if len(trustedProxies) == 0 {
			log.Fatalf("ksquad-apiserver: trustedProxies %q parsed to an empty set (must be IPs/CIDRs)", cfg.TrustedProxies)
		}
	}
	var allowedOrigins []string
	for _, o := range strings.Split(cfg.AllowedOrigins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowedOrigins = append(allowedOrigins, o)
		}
	}

	// The ONE shared informer-cache backing for both cache-backed read models — 8.1
	// squad-overview (ISI-2760, Team/Project/Run) and 8.6 credentials (ISI-2902,
	// Team/Agent/Run). Available only where the host has cluster access (in-cluster
	// ServiceAccount or a KUBECONFIG); one cache, not one-per-reader, so watches and in-memory
	// copies are never duplicated. When it cannot be built (e.g. a cluster-less local run) we
	// log and leave the readers nil: their routes then keep their documented 501s — an honest
	// contract rather than a hard start failure.
	//
	// Known cost, accepted for now (PR #87 review): both projections list Runs unfiltered per
	// request, so per-page-load work grows with cluster history. A status.phase field index on
	// this cache is the follow-up once the reconciler writes real Paused conditions (ISI-2898).
	var overview apiserver.SquadOverviewReader
	var credentials apiserver.CredentialOverviewReader
	if cacheReader, stopCache, cerr := apiserver.NewCacheReader(ctx, 30*time.Second); cerr != nil {
		log.Printf("ksquad-apiserver: informer cache unavailable — squad-overview + credential read models disabled (GET /api/squad/overview, GET /api/credentials → 501): %v", cerr)
	} else {
		defer stopCache()
		overview = apiserver.NewClientOverviewReader(cacheReader)
		credentials = apiserver.NewClientCredentialReader(cacheReader)
		log.Printf("ksquad-apiserver: squad-overview + credential read models ready (informer cache synced)")
	}

	// 8.7a/8.7d build-browser read-model (ISI-2759). Production wires a Postgres-backed RunSource
	// (Run→Team/owner/workspace from the coord store); until then a dev runs file lets the real
	// git read-model serve a local repo. Nil ⇒ the routes keep the documented 501 (fail visible).
	// The SAME RunSource is shared by the 8.3 artifact browser (ISI-2900) so the 8.7d per-principal
	// + Team-scope gate resolves identical Run facts on both read models — tenancy inputs cannot
	// drift between sibling console surfaces.
	var builds *buildbrowser.Service
	var artifacts *artifactbrowser.Service
	if runsPath := os.Getenv("KSQUAD_DEV_RUNS"); runsPath != "" {
		runs, rerr := buildbrowser.LoadStaticRuns(runsPath)
		if rerr != nil {
			log.Fatalf("ksquad-apiserver: load dev runs: %v", rerr)
		}
		builds = buildbrowser.NewService(runs, buildbrowser.NewGitReader())
		artStore, aerr := artifactbrowser.NewProdStore(db)
		if aerr != nil {
			log.Fatalf("ksquad-apiserver: artifact store: %v", aerr)
		}
		artifacts = artifactbrowser.NewService(runs, artStore)
		// #nosec G706 -- operator-supplied env path in a startup warning, not request-tainted input.
		log.Printf("ksquad-apiserver: WARNING — using static dev runs from %s; NOT for production", runsPath)
	} else {
		log.Printf("ksquad-apiserver: no Run source configured — build-browser and artifact-browser routes answer 501 until the read-model backing lands (ISI-2759/ISI-2900)")
	}

	// §4.4 SSE run-progress publish source (ISI-2756). The run-entity rows on coord.outbox — the
	// same durable journal the NATS relay flushes — are read DIRECTLY as both the live fan-out
	// feed and the Last-Event-ID replay tail, so the console transport carries no NATS client and
	// the outbox stays the single source of truth for stream + resume. This is a read-only
	// downstream projection (§17.4): it never writes the outbox and is never on a write path or the
	// readiness probe, so a lagging projection delays console progress but never blocks a Run.
	runEvents := events.NewSQLStore(db)
	hub := apiserver.NewHub()
	hub.SetReplayer(apiserver.NewRunReplayer(runEvents))
	projector := apiserver.NewRunEventSource(runEvents, hub)
	go func() {
		if err := projector.Run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("ksquad-apiserver: run-event projector stopped: %v", err)
		}
	}()

	// 8.14a human board-lane transition write path (ISI-2909): the DB is a hard start
	// dependency here, so the store is always bound (the documented-501 fallback exists
	// only for a store-less host shape, e.g. a DB-less dev run).
	workItemState, err := coord.NewHumanStateStore(db)
	if err != nil {
		log.Fatalf("ksquad-apiserver: work-item state store: %v", err)
	}

	srv := apiserver.NewServer(apiserver.Options{
		Authenticator: authn,
		Discussion:    discussion.NewHandler(discussion.NewStore(db)),
		Ready:         dbReady{db},
		Overview:      overview,
		Credentials:   credentials,
		Builds:        builds,
		Artifacts:     artifacts,
		AuditTrail:    apiserver.NewPostgresAuditTrailReader(db),
		WorkItemState: workItemState,
		Hub:           hub,
		Auth: apiserver.AuthRoutesOptions{
			Service:        authSvc,
			Authenticator:  authn,
			CookieName:     cfg.SessionCookie,
			SecureCookies:  cfg.SecureCookies,
			TrustedProxies: trustedProxies,
			AllowedOrigins: allowedOrigins,
			Audit:          coordAuditWriter(db),
		},
	})

	httpSrv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 5 * time.Second,
		// No WriteTimeout: the SSE hub holds long-lived streaming responses (§4.4).
	}
	go func() {
		log.Printf("ksquad-apiserver: listening on %s", httpSrv.Addr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ksquad-apiserver: http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("ksquad-apiserver: shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = httpSrv.Shutdown(shutdownCtx)
}

// dbReady adapts *sql.DB to apiserver.ReadinessChecker: /readyz is green iff a ping succeeds.
type dbReady struct{ db *sql.DB }

func (d dbReady) Ready(ctx context.Context) error { return d.db.PingContext(ctx) }

// buildAuthService assembles the pkg/auth core (15.1): stores over the shared DSN,
// the HS256 JWT issuer, the per-IP login brake, and the 15.9 groupMapping seam.
func buildAuthService(ctx context.Context, db *sql.DB, cfg apiserver.Config) *auth.Service {
	key := apiserver.DecodeJWTKey(cfg.JWTSigningKey)
	if len(key) < 32 {
		// In-process auto-generation (ADR-033): acceptable for dev; sessions die on
		// restart. Helm 9.5 mounts the durable signing-key Secret in production.
		gen := auth.GenerateSigningKey()
		key = apiserver.DecodeJWTKey(gen)
		log.Printf("ksquad-apiserver: WARNING — no jwtSigningKey configured; auto-generated (sessions will not survive restarts; set auth.signingKeySecretRef / KSQUAD_JWT_SIGNING_KEY for production)")
	}
	issuer, err := auth.NewJWTIssuer(key, time.Duration(cfg.JWTTTLSeconds)*time.Second)
	if err != nil {
		log.Fatalf("ksquad-apiserver: jwt issuer: %v", err)
	}

	// Bound concurrent argon2id derivations (PR #90 review finding 2): each holds
	// 64 MiB; unbounded parallel logins would OOM the pod at the chart's memory limit.
	hashConcurrency := cfg.MaxHashConcurrency
	if hashConcurrency == 0 {
		hashConcurrency = 2
	}
	auth.SetHashConcurrency(hashConcurrency)

	// 15.9 seam: validate the groupMapping at startup so a bad chart value fails
	// fast. CONSUMPTION is deliberately absent here — groups arrive as IdP-asserted
	// claims inside the OIDC leg (15.9), never from a request body (PR #90 review
	// finding 3). ParseGroupMapping + groupmapping.go pin the contract meanwhile.
	if cfg.OidcGroupMapping != "" {
		if _, err := auth.ParseGroupMapping(cfg.OidcGroupMapping); err != nil {
			log.Fatalf("ksquad-apiserver: invalid oidcGroupMapping: %v", err)
		}
		log.Printf("ksquad-apiserver: oidc groupMapping config validated (consumed by the OIDC login leg, 15.9)")
	}

	limiter := auth.NewRateLimiter(cfg.LoginRateLimit, time.Duration(cfg.LoginRateWindowSeconds)*time.Second)
	users := auth.NewPostgresUserStore(db)
	sessions := auth.NewPostgresSessionStore(db)

	// Fail visible, not fatal: if the auth schema is not applied yet the routes
	// answer 500/501-shape errors while the rest of the host keeps serving.
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if _, err := users.Count(pingCtx); err != nil {
		log.Printf("ksquad-apiserver: WARNING — auth schema not queryable (%v); /auth routes will fail until db/migrations through 0008 are applied", err)
	}

	return auth.NewService(users, sessions, issuer, limiter, auth.ServiceConfig{
		SessionTTL: time.Duration(cfg.SessionTTLSeconds) * time.Second,
	})
}

// bootstrapAdmin provisions the initial admin (15.2): ONLY on a fresh install
// (auth.user empty), from chart values / env. Idempotent by construction — a
// non-empty user table skips entirely, so re-running is a no-op.
func bootstrapAdmin(ctx context.Context, db *sql.DB, cfg apiserver.Config) {
	if cfg.BootstrapAdminUsername == "" || cfg.BootstrapAdminPassword == "" {
		return
	}
	users := auth.NewPostgresUserStore(db)
	count, err := users.Count(ctx)
	if err != nil {
		log.Printf("ksquad-apiserver: bootstrap admin skipped — cannot probe user table: %v", err)
		return
	}
	if count > 0 {
		// #nosec G706 -- count is a SQL COUNT(*) int rendered with %d; no tainted
		// string and no control characters can reach the log line.
		log.Printf("ksquad-apiserver: bootstrap admin skipped — %d users exist (idempotent no-op)", count)
		return
	}

	teamID, err := uuid.Parse(cfg.BootstrapAdminTeamID)
	if cfg.BootstrapAdminTeamID != "" && err != nil {
		// An explicitly-configured-but-malformed team id is an operator error:
		// fail loudly instead of silently inventing a random tenancy root
		// (PR #90 review).
		log.Fatalf("ksquad-apiserver: bootstrap admin teamId %q is not a uuid", cfg.BootstrapAdminTeamID)
	}
	if cfg.BootstrapAdminTeamID == "" {
		// No team configured: mint a fresh tenancy root for the install's first
		// admin (team_id has no FK; scope selection lands with 15.3 memberships).
		teamID = uuid.New()
		log.Printf("ksquad-apiserver: bootstrap admin team auto-generated %s (set KSQUAD_BOOTSTRAP_ADMIN_TEAM_ID to pin one)", teamID)
	}
	hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
	if err != nil {
		log.Printf("ksquad-apiserver: bootstrap admin skipped — hash: %v", err)
		return
	}
	u := &auth.User{
		Username: cfg.BootstrapAdminUsername, PasswordHash: hash,
		TeamID: teamID, GlobalRole: auth.RoleAdmin, // created_by stays NULL: the install-time seed
	}
	if err := users.Create(ctx, u); err != nil {
		log.Printf("ksquad-apiserver: bootstrap admin FAILED: %v", err)
		return
	}
	log.Printf("ksquad-apiserver: bootstrap admin %q created (principal %s) — clear the bootstrap password value now", u.Username, u.Principal)
}

// coordAuditWriter adapts the shared DB into the admin-mutation audit sink (§6.5,
// ADR-040: the append-only coord.audit_log). work_item_id stays NULL — these are
// platform user-admin events, not coord events; the log's append-only triggers
// cover them the same way. A failed append is logged loudly, never silently lost.
func coordAuditWriter(db *sql.DB) func(ctx context.Context, eventType, principal string, payload map[string]any) {
	return func(ctx context.Context, eventType, principal string, payload map[string]any) {
		data, err := json.Marshal(payload)
		if err != nil {
			log.Printf("ksquad-apiserver: audit %s: marshal payload: %v", eventType, err)
			return
		}
		if _, err := db.ExecContext(ctx, `
			INSERT INTO coord.audit_log (event_type, principal, payload)
			VALUES ($1, $2, $3)`, eventType, principal, data); err != nil {
			log.Printf("ksquad-apiserver: audit append FAILED for %s by %s: %v", eventType, principal, err)
		}
	}
}
