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
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

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

	mux := http.NewServeMux()
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
