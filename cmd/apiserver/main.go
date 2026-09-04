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
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/internal/artifactbrowser"
	"github.com/K8squad/K8squad/internal/buildbrowser"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/runsource"
	"github.com/K8squad/K8squad/pkg/auth"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/events"
	"github.com/K8squad/K8squad/pkg/issuesync"
	"github.com/K8squad/K8squad/pkg/orgops"
	"github.com/K8squad/K8squad/pkg/search"
	"github.com/K8squad/K8squad/pkg/taskio"
	"github.com/K8squad/K8squad/pkg/telemetry"
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

	// Install the OpenTelemetry spine (ISI-3668, prerequisite for the onboarding/compose
	// observability in ISI-3666/ISI-3669). This mirrors cmd/operator/main.go:110: it registers a
	// W3C trace-context propagator, a TracerProvider so every inbound request opens a server span,
	// and the otelslog bridge so structured logs carry trace_id/span_id. Without it, telemetry.Tracer()
	// in this process is a no-op and traces/logs from the new endpoints silently drop. Exports to
	// stdout for now; the MeterProvider is a separate track (ISI-3593).
	_, otelShutdown, err := telemetry.Setup(ctx, telemetry.Options{ServiceName: "ksquad-apiserver"})
	if err != nil {
		log.Fatalf("ksquad-apiserver: initialize OpenTelemetry spine: %v", err)
	}
	defer func() {
		// Flush on a fresh bounded context: the signal ctx is already cancelled by the time we
		// return, and Shutdown must still drain buffered spans/logs.
		flushCtx, flushCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer flushCancel()
		if serr := otelShutdown(flushCtx); serr != nil {
			log.Printf("ksquad-apiserver: OpenTelemetry shutdown: %v", serr)
		}
	}()

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
	// 8.10/8.11 Agents org read model (ISI-3548): the same informer cache backs the
	// Team→Agent→Role org diagram, its live per-agent status SSE, and agent detail/runs.
	var org apiserver.OrgReader
	// Story A / 13.8 OTelConfig read model (ISI-2917): a projection over the SAME
	// cache — the cluster-scoped OTelConfig CR the Settings page reads.
	var otelConfig apiserver.OTelConfigSource
	// 8.8a dashboard: the same informer cache that backs overview/credentials
	// also feeds the dashboard's live-Runs tile, so the cache block below is
	// the ONE place all three read models get their reader.
	var dashboardReader client.Reader
	if cacheReader, stopCache, cerr := apiserver.NewCacheReader(ctx, 30*time.Second); cerr != nil {
		log.Printf("ksquad-apiserver: informer cache unavailable — squad-overview + credential read models disabled (GET /api/squad/overview, GET /api/credentials → 501): %v", cerr)
	} else {
		defer stopCache()
		overview = apiserver.NewClientOverviewReader(cacheReader)
		credentials = apiserver.NewClientCredentialReader(cacheReader)
		org = apiserver.NewClientOrgReader(cacheReader)
		otelConfig = apiserver.NewClientOTelConfigSource(cacheReader)
		dashboardReader = cacheReader
		log.Printf("ksquad-apiserver: squad-overview + credential + agents-org read models ready (informer cache synced)")
	}

	// 11.2 issue⇄work-item linkage API (ISI-2738): GET/POST/DELETE
	// /api/projects/{projectId}/issue-links over issuesync.SQLStore
	// (scm.issue_link, migration 0013). Needs BOTH the DB (link store) and
	// the informer cache (Team→namespace + Project tenancy resolution); a
	// dev run without either keeps the documented 501. The sync loop itself
	// lives in the operator's repo-sync reconciler, not here.
	var issueLinks *apiserver.IssueLinkService
	if db != nil && dashboardReader != nil {
		if linkStore, lerr := issuesync.NewSQLStore(db); lerr != nil {
			log.Printf("ksquad-apiserver: issue-link store unavailable (issue-link API → 501, story 11.2): %v", lerr)
		} else {
			issueLinks = apiserver.NewIssueLinkService(linkStore, dashboardReader)
			log.Printf("ksquad-apiserver: issue-link API ready (11.2 GitHub issues ⇄ work items)")
		}
	}

	// 8.7a/8.7d build-browser read-model (ISI-2759). Production wires a Postgres-backed RunSource
	// (Run→Team/owner/workspace from the coord store); until then a dev runs file lets the real
	// git read-model serve a local repo. Nil ⇒ the routes keep the documented 501 (fail visible).
	// The SAME RunSource is shared by the 8.3 artifact browser (ISI-2900) so the 8.7d per-principal
	// + Team-scope gate resolves identical Run facts on both read models — tenancy inputs cannot
	// drift between sibling console surfaces.
	var builds *buildbrowser.Service
	var artifacts *artifactbrowser.Service
	artStore, aerr := artifactbrowser.NewProdStore(db)
	if aerr != nil {
		log.Fatalf("ksquad-apiserver: artifact store: %v", aerr)
	}
	if runsPath := os.Getenv("KSQUAD_DEV_RUNS"); runsPath != "" {
		// Dev override (ISI-2759): a static runs file drives the LIVE git read-model against a local
		// repo. NOT for production — a real deployment leaves KSQUAD_DEV_RUNS unset and takes the
		// Postgres branch below.
		runs, rerr := buildbrowser.LoadStaticRuns(runsPath)
		if rerr != nil {
			log.Fatalf("ksquad-apiserver: load dev runs: %v", rerr)
		}
		builds = buildbrowser.NewService(runs, buildbrowser.NewGitReader())
		artifacts = artifactbrowser.NewService(runs, artStore)
		// #nosec G706 -- operator-supplied env path in a startup warning, not request-tainted input.
		log.Printf("ksquad-apiserver: WARNING — using static dev runs from %s; NOT for production", runsPath)
	} else {
		// Production (ISI-3207): the Postgres-backed RunSource resolves a Run's Team/owner from the
		// coord custody row (claim.run_id → holder_principal ⋈ work_item.team_id) and its git coords
		// from the 8.7c build-snapshot meta. The SAME source backs both read models so their 8.7d
		// gate resolves identical Run facts. The build reader serves a COMPLETED Run from the captured
		// snapshot: Meta from the persisted summary today; tree/diff/file byte reads degrade to 404
		// (existence-hiding) until the ISI-2900 blob store binds a BundleResolver here.
		runs, rerr := runsource.NewPostgresRunSource(db)
		if rerr != nil {
			log.Fatalf("ksquad-apiserver: build/artifact run source: %v", rerr)
		}
		snapStore, serr := runsource.NewPostgresSnapshotStore(db)
		if serr != nil {
			log.Fatalf("ksquad-apiserver: build snapshot store: %v", serr)
		}
		builds = buildbrowser.NewService(runs, runsource.NewSnapshotStoreReader(snapStore, nil))
		artifacts = artifactbrowser.NewService(runs, artStore)
		log.Printf("ksquad-apiserver: build-browser + artifact-browser wired to the Postgres Run source (8.7e backend, ISI-3207)")
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

	// 8.18 global search read path (ISI-2912): the FTS searcher over coord.work_item
	// (migration 0012). The DB is a hard start dependency here, so the searcher is
	// always bound (the documented-501 fallback exists only for a searcher-less host
	// shape). RBAC scope (admin fleet-wide vs Team-fenced) is applied in-query per ADR-039.
	searcher, err := search.NewPostgresSearcher(db)
	if err != nil {
		log.Fatalf("ksquad-apiserver: search store: %v", err)
	}

	// 15.3 per-Project membership store (auth.project_membership, db/migrations/0010): ONE
	// instance backs both the 15.4 enforcement gate (Options.ProjectRoles, read-only resolver)
	// and the 8.15 admin Users & Roles surface (Auth.Memberships, grant/revoke/list), so a grant
	// made in the console is immediately effective at the enforcement wall (ISI-2911).
	memberships := auth.NewPostgresMembershipStore(db)

	// 8.5 CRD-apply write surface (ISI-3198): the DIRECT controller-runtime client the
	// compose endpoints apply through. Built where the host has cluster access (in-cluster
	// SA or KUBECONFIG); when it cannot be built (cluster-less dev run) we log and leave it
	// nil so POST/PUT /api/{teams,projects,agents,roles,skills} keep the documented 501 —
	// an honest contract, not a hard start failure. The SAME memberships store backs its
	// write-tier gate, and the SAME coord.audit_log writer records its provenance rows.
	var composeCRD *apiserver.ComposeService
	// The direct (uncached) write client is shared by the console compose surface
	// (8.5) and the run-scoped org-ops seam (ISI-3626) — one write path into the
	// cluster, never two.
	var crdApplier apiserver.CRDApplier
	if applier, aerr := apiserver.NewCRDApplier(); aerr != nil {
		log.Printf("ksquad-apiserver: CRD-apply write surface disabled (POST/PUT /api/{teams,projects,agents,roles,skills} → 501): %v", aerr)
	} else {
		crdApplier = applier
		composeCRD = apiserver.NewComposeService(applier, memberships, coordAuditWriter(db))
		log.Printf("ksquad-apiserver: CRD-apply write surface ready (8.5 compose endpoints)")
	}

	// Audit log read model (ISI-2881). The DB connection is already available.
	var auditLog apiserver.AuditLogReader
	if db != nil {
		auditLog = apiserver.NewDBAuditLogReader(db)
		log.Printf("ksquad-apiserver: audit log read model ready")
	} else {
		log.Printf("ksquad-apiserver: no database connection — audit log route will answer 501 until the database is available")
	}

	// ISI-3601 S2 run-scoped task-io seam: the coord-backed Store + the run-token
	// verifier over the SAME HS256 signing key the operator mints with (so the
	// operator can mint and the apiserver can verify with one configured secret).
	// Absent a >=32-byte key the seam is disabled (nil handler): a token the
	// operator minted could not be verified here anyway, so fail visible rather
	// than mount an endpoint that rejects every real token.
	var taskIOHandler http.Handler
	// OrgOps (ISI-3626) shares the SAME run-token verifier as task-io — one
	// KSQUAD_COORD_TOKEN drives both surfaces — so the minter is built once here
	// and reused below.
	var runTokenMinter *taskio.Minter
	if key := apiserver.DecodeJWTKey(cfg.JWTSigningKey); len(key) >= 32 {
		minter, merr := taskio.NewMinter(key, time.Duration(cfg.JWTTTLSeconds)*time.Second)
		store, serr := taskio.NewCoordStore(db, workItemState)
		switch {
		case merr != nil:
			log.Printf("ksquad-apiserver: task-io seam disabled (minter): %v", merr)
		case serr != nil:
			log.Printf("ksquad-apiserver: task-io seam disabled (store): %v", serr)
		default:
			runTokenMinter = minter
			taskIOHandler = taskio.NewHandler(minter, store).Mux()
			log.Printf("ksquad-apiserver: task-io seam ready (/api/task-io: get-task/post-comment/update-status/checkout)")
		}
	} else {
		log.Printf("ksquad-apiserver: task-io seam disabled — no >=32B JWT signing key (set auth.signingKeySecretRef / KSQUAD_JWT_SIGNING_KEY)")
	}

	// ISI-3626 run-scoped board-ops seam (/api/org-ops). It needs BOTH the shared
	// run-token verifier (above) and the direct write client (crdApplier); absent
	// either it stays unmounted. Enforcement is server-side per verb: create-agent
	// / create-skill require org:write, create-project / archive-project require
	// project:write — scopes the operator derives from the run's Role at mint time.
	var orgOpsHandler http.Handler
	if runTokenMinter != nil && crdApplier != nil {
		store := orgops.NewCRDStore(crdApplier, coordAuditWriter(db))
		orgOpsHandler = orgops.NewHandler(runTokenMinter, store).Mux()
		log.Printf("ksquad-apiserver: org-ops seam ready (/api/org-ops: create-agent/create-skill/create-project/archive-project; org:write/project:write scoped)")
	} else {
		log.Printf("ksquad-apiserver: org-ops seam disabled — needs both a >=32B JWT signing key and a CRD write client")
	}

	srv := apiserver.NewServer(apiserver.Options{
		Authenticator: authn,
		Discussion:    discussion.NewHandler(discussion.NewStore(db)),
		Ready:         dbReady{db},
		Overview:      overview,
		Credentials:   credentials,
		Org:           org,
		OTelConfig:    otelConfig,
		Builds:        builds,
		Artifacts:     artifacts,
		AuditTrail:    apiserver.NewPostgresAuditTrailReader(db),
		WorkItemState: workItemState,
		Search:        searcher,
		// 15.4 per-Project RBAC (ISI-2921): the membership store over auth.project_membership
		// (db/migrations/0010) gates project-scoped routes. Wired unconditionally against the
		// same *sql.DB the auth stores use; a cluster/db-less dev run never reaches NewServer.
		ProjectRoles: memberships,
		ComposeCRD:   composeCRD,
		IssueLinks:   issueLinks,
		Killer:       apiserver.NewProdRunKiller(db),
		AuditLog:     auditLog,
		// Epic D tool-usage panel read model (ISI-3288, D3): aggregates the
		// operator's ksquad_* tool metrics. Unset takes the in-cluster
		// operator metrics default; a scrape that cannot reach it answers
		// 503 with the reason (the panel renders a degraded state).
		ToolUsage: apiserver.NewOperatorMetricsToolUsage(os.Getenv("KSQUAD_OPERATOR_METRICS_URL")),
		Hub:       hub,
		TaskIO:    taskIOHandler,
		OrgOps:    orgOpsHandler,
		Auth: apiserver.AuthRoutesOptions{
			Service:        authSvc,
			Authenticator:  authn,
			CookieName:     cfg.SessionCookie,
			SecureCookies:  cfg.SecureCookies,
			TrustedProxies: trustedProxies,
			AllowedOrigins: allowedOrigins,
			Audit:          coordAuditWriter(db),
			Memberships:    memberships,
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
