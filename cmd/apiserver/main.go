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
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"flag"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/internal/discussion"
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

	// 8.1 squad-overview read model (ISI-2760). Its backing is the controller-runtime informer
	// cache over Team/Project/Run — available only where the host has cluster access (in-cluster
	// ServiceAccount or a KUBECONFIG). When it cannot be built (e.g. a cluster-less local run) we
	// log and leave it nil: GET /api/squad/overview then keeps its documented 501, an honest
	// contract rather than a hard start failure.
	var overview apiserver.SquadOverviewReader
	if reader, stopCache, oerr := apiserver.NewCacheOverviewReader(ctx, 30*time.Second); oerr != nil {
		log.Printf("ksquad-apiserver: squad-overview read model disabled (GET /api/squad/overview → 501): %v", oerr)
	} else {
		overview = reader
		defer stopCache()
		log.Printf("ksquad-apiserver: squad-overview read model ready (informer cache synced)")
	}

	srv := apiserver.NewServer(apiserver.Options{
		Authenticator: authn,
		Discussion:    discussion.NewHandler(discussion.NewStore(db)),
		Ready:         dbReady{db},
		Overview:      overview,
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
