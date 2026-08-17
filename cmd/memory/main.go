// Command memory is the ksquad-memory service (Story 6.1 / ISI-2716). It is a first-class Go binary,
// distinct from ksquad-operator and ksquad-apiserver (§17.3), that stands up the §7.2 knowledge-record
// store over the shared Postgres + pgvector. On start it applies (or verifies) db/migrations, ensures
// the vector extension, and reports readiness — failing closed if pgvector is absent or the schema is
// at an unexpected version (AC1). It never silently degrades to a bespoke in-app vector store.
//
// The MCP tool surface (memory.write/memory.search) is Story 6.2 and builds on this store; this binary
// stands up the service + store and exposes a health/readiness endpoint.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the discussion projection source

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/internal/discussionindex"
	"github.com/K8squad/K8squad/internal/memory"
)

func main() {
	var (
		configPath  = flag.String("config", "/etc/ksquad/memory-config.json", "Path to memory service configuration file")
		databaseURL = flag.String("database-url", "", "Postgres connection URL (overrides config/env)")
		httpPort    = flag.Int("http-port", 0, "HTTP port for health/readiness (overrides config)")
	)
	flag.Parse()

	cfg, err := memory.LoadConfig(*configPath)
	if err != nil {
		log.Fatalf("ksquad-memory: load config: %v", err)
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Fail closed at start (AC1): connect, apply/verify migrations, ensure pgvector + schema version.
	startCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	store, err := memory.Open(startCtx, cfg)
	cancel()
	if err != nil {
		log.Fatalf("ksquad-memory: refusing to start: %v", err)
	}
	defer store.Close()
	log.Printf("ksquad-memory: store ready (pgvector, dim=%d)", memory.EmbeddingDim)

	// The §7.1 embedder seam: the deterministic local default (a live endpoint client is a fast-follow
	// behind Config.Embedder*). Shared by the read tools (embed the query) and the indexer (embed bodies).
	embedder := memory.NewHashingEmbedder()

	// Untrusted read tools (§7.3.2): memory_search + the scoped discussion_search(project). Until the
	// shared MCP transport (6.2) lands, they are reachable over a thin JSON/HTTP surface.
	readSvc := memory.NewReadService(store, embedder)
	tools := memory.NewToolHTTP(readSvc)

	// Best-effort discussion→pgvector indexer (10.2, §7.6/§17.4). It projects committed discussion
	// messages into the memory index out of band; it NEVER blocks a room write or Run (AC5). If the
	// discussion schema is absent or the DB handle can't open, indexing is simply disabled — it must
	// never prevent the memory service (and its reads) from starting.
	startDiscussionIndexer(ctx, cfg.DatabaseURL, store, embedder)

	mux := http.NewServeMux()
	tools.Mount(mux)
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		if err := store.Ready(r.Context()); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			_ = json.NewEncoder(w).Encode(map[string]string{"status": "not-ready", "error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
	})

	srv := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.HTTPPort),
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		log.Printf("ksquad-memory: health server on %s", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("ksquad-memory: http server: %v", err)
		}
	}()

	<-ctx.Done()
	log.Printf("ksquad-memory: shutting down")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	_ = srv.Shutdown(shutdownCtx)
}

// startDiscussionIndexer launches the best-effort discussion→memory indexer in the background. It is
// deliberately fail-open: any setup problem (can't open the DB handle) disables indexing with a log
// line rather than taking down the memory service — recall is a fast-follow property, never a gate on
// the room or the service (AC5, §7.6). The sweep itself tolerates a missing discussion schema (its
// query error is logged and retried), so the indexer can start before 10.1 is provisioned.
func startDiscussionIndexer(ctx context.Context, dsn string, store *memory.PgVectorStore, embedder memory.Embedder) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("ksquad-memory: discussion indexer disabled (open db: %v)", err)
		return
	}
	ix := discussionindex.NewIndexer(discussion.NewStore(db), store, embedder, 0)
	interval := 15 * time.Second
	if v := os.Getenv("DISCUSSION_INDEX_INTERVAL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			interval = d
		}
	}
	log.Printf("ksquad-memory: discussion indexer running (interval=%s)", interval)
	go func() {
		ix.Run(ctx, interval)
		_ = db.Close()
	}()
}
