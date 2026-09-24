// Command memory is the ksquad-memory service (Story 6.1 / ISI-2716). It is a first-class Go binary,
// distinct from ksquad-operator and ksquad-apiserver (§17.3), that stands up the §7.2 knowledge-record
// store over the shared Postgres + pgvector. On start it applies (or verifies) db/migrations, ensures
// the vector extension, and reports readiness — failing closed if pgvector is absent or the schema is
// at an unexpected version (AC1). It never silently degrades to a bespoke in-app vector store.
//
// The tool surface builds on this store: the untrusted read tools (memory_search / discussion_search)
// and the authorized write tool (memory_write, Story 6.3 — author + tenancy server-stamped from the BFF
// headers). They are exposed over a thin JSON/HTTP surface until the shared MCP transport (Story 6.2)
// lands; the ReadService/WriteService plug into the MCP registry unchanged when it does.
package main

import (
	"context"
	"database/sql"
	"encoding/base64"
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
	"github.com/K8squad/K8squad/internal/handoffmirror"
	"github.com/K8squad/K8squad/internal/memory"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mcpauthtoken"
)

// decodeSigningKey mirrors cmd/operator.decodeSigningKey / internal/apiserver's
// key decode: accept a raw or base64-encoded HS256 key so cmd/memory reads the
// SAME KSQUAD_JWT_SIGNING_KEY value the control plane mints with (a shared Secret
// mints and verifies the run capability token, ADR-0024a D2 option (a)). A
// too-short/invalid value is handed through verbatim so mcpauthtoken.NewMinter
// rejects it loudly (< 32 bytes).
func decodeSigningKey(key string) []byte {
	if raw, err := base64.RawStdEncoding.DecodeString(key); err == nil && len(raw) >= 32 {
		return raw
	}
	if raw, err := base64.StdEncoding.DecodeString(key); err == nil && len(raw) >= 32 {
		return raw
	}
	return []byte(key)
}

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

	// The §7.1 embedder seam: a live semantic embedder when Config.EmbedderEndpoint is set, else the
	// deterministic local default. Shared by the read tools (embed the query), the write tool (embed the
	// body), and the indexer/mirror (embed projected bodies) — one vector space for the whole substrate.
	embedder := memory.NewEmbedder(cfg)
	if cfg.EmbedderEndpoint != "" {
		log.Printf("ksquad-memory: semantic embedder wired (endpoint=%s, model=%s)", cfg.EmbedderEndpoint, cfg.EmbedderModel)
	} else {
		log.Printf("ksquad-memory: using deterministic local embedder (no EmbedderEndpoint configured)")
	}

	// Untrusted read tools (§7.3.2): memory_search + the scoped discussion_search(project). The
	// authorized write tool (§6.3): memory_write, author/tenancy server-stamped from the BFF headers.
	// Reachable over a thin JSON/HTTP surface until the shared MCP transport (6.2) lands.
	readSvc := memory.NewReadService(store, embedder)
	writeSvc := memory.NewWriteService(store, embedder)

	// The authored discussion write tool (discussion_post, Story 6.4 / ISI-4075): the write peer of
	// discussion_search. It calls the already-fenced discussion.Store directly (provenance server-
	// stamped from the BFF headers, Team scope enforced by the store). Fail OPEN, consistent with the
	// discussion indexer below: if the discussion DB handle can't open, log and leave the writer nil so
	// discussion_post is simply unmounted (AC5) — a memory-DB-only deployment still serves the reads.
	var discuss memory.DiscussionWriter
	if db := openDiscussionDB(cfg.DatabaseURL); db != nil {
		discuss = discussion.NewStore(db)
	}
	tools := memory.NewToolHTTP(readSvc, writeSvc, discuss)
	// Story 6.2 (ISI-3179): the MCP JSON-RPC transport over the SAME ReadService/WriteService/discuss
	// seam. It serves the streamable-HTTP /mcp endpoint (initialize + tools/list + tools/call) that
	// MCP-speaking agents and the operator's MCPServer probe talk, alongside the thin per-tool JSON/HTTP
	// routes above during the compatibility window. Tenancy/authorship are the same server-authenticated
	// headers (INV3). discussion_post rides the same fenced discuss writer as the HTTP shim, so it is
	// advertised and served only when the discussion DB opened (nil ⇒ unmounted, AC5) (ISI-4085).
	mcpTools := memory.NewToolMCP(readSvc, writeSvc, discuss)

	// ADR-0024 agent work-item authoring lane (ISI-4741): the capability-gated
	// work_item_create / _update / _assign MCP tools, backed by coord over the SAME
	// Postgres the discussion writer uses. Fail-open, matching discussion_post: if
	// the coord DB handle or store can't be built, log and leave the tools
	// unmounted — a read-only / DB-less deployment still serves the reads. The
	// capability gate is deny-by-default over the control-plane-stamped
	// X-Agent-Capabilities header (role→capability, O-1). The Team-agent resolver
	// that backs the PM→implementer ASSIGN verb is now supplied here too (ISI-4743):
	// its own shared informer cache over Team CRs (mirroring the apiserver's), so
	// work_item_assign drives a real dispatch. It is FAIL-OPEN — a cluster-less /
	// RBAC-less deployment leaves the dispatch backend nil, and assign stays
	// honestly unavailable while create + update still serve (the reads never go
	// down). It never authorizes against an empty Team world.
	var teamResolver coord.TeamAgentResolver
	if reader, stopCache, rerr := memory.NewTeamCacheReader(ctx, 30*time.Second); rerr != nil {
		log.Printf("ksquad-memory: Team-agent resolver unavailable — work_item_assign stays honestly unavailable (create/update serve): %v", rerr)
	} else {
		defer stopCache()
		teamResolver = memory.NewClientTeamAgentResolver(reader)
		log.Printf("ksquad-memory: Team-agent resolver ready (informer cache synced) — work_item_assign enabled")
	}
	if author, dispatcher := openAgentAuthor(cfg.DatabaseURL, teamResolver); author != nil {
		mcpTools.WithWorkItemAuthor(author, dispatcher, memory.NewHeaderCapabilityResolver())
		if dispatcher != nil {
			log.Printf("ksquad-memory: agent work-item authoring tools mounted (create/update/assign)")
		} else {
			log.Printf("ksquad-memory: agent work-item authoring tools mounted (create/update; assign honestly unavailable — no Team-agent resolver, ISI-4743)")
		}
		// ADR-0024a S3/D2 (ISI-4869): enable the token-auth (sandbox) path when the
		// shared HS256 signing key is distributed to this process (D2 option (a) —
		// the SAME KSQUAD_JWT_SIGNING_KEY the control plane mints with). A sandbox
		// then reaches the authoring tools with a verified run capability token, and
		// its client X-* identity headers are discarded. No key ⇒ the token path
		// stays off and only the trusted BFF header path serves (inert until the key
		// is delivered — Henrik's D2 readiness call on key distribution).
		if raw := os.Getenv("KSQUAD_JWT_SIGNING_KEY"); raw != "" {
			if minter, merr := mcpauthtoken.NewMinter(decodeSigningKey(raw), 0); merr != nil {
				log.Printf("ksquad-memory: run-capability-token auth DISABLED — KSQUAD_JWT_SIGNING_KEY invalid (sandbox authoring stays header-path only): %v", merr)
			} else {
				mcpTools.WithAuthoringTokenAuth(memory.NewMCPAuthTokenVerifier(minter))
				log.Printf("ksquad-memory: run-capability-token auth ENABLED — sandbox authoring accepts verified tokens; client X-* identity headers discarded on that path (ADR-0024a S3/D2)")
			}
		} else {
			log.Printf("ksquad-memory: run-capability-token auth off — no KSQUAD_JWT_SIGNING_KEY; authoring served on the trusted BFF header path only (ADR-0024a S3/D2 inert)")
		}
	}

	// Best-effort discussion→pgvector indexer (10.2, §7.6/§17.4). It projects committed discussion
	// messages into the memory index out of band; it NEVER blocks a room write or Run (AC5). If the
	// discussion schema is absent or the DB handle can't open, indexing is simply disabled — it must
	// never prevent the memory service (and its reads) from starting.
	startDiscussionIndexer(ctx, cfg.DatabaseURL, store, embedder)

	// Best-effort handoff→memory mirror (6.6, §8.5): projects committed 2.8 handoff artifacts into the
	// memory index out of band so the NEXT Run recalls them at the untrusted-recall tier. Same fail-open
	// posture as the discussion indexer — a coord-schema absence or DB problem disables mirroring with a
	// log line, never preventing the memory service (and its reads) from starting (AC6).
	startHandoffMirror(ctx, cfg.DatabaseURL, store, embedder)

	mux := http.NewServeMux()
	tools.Mount(mux)
	mcpTools.Mount(mux)
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

// openDiscussionDB opens the long-lived Postgres handle backing the discussion_post tool. Fail-open,
// matching the indexer/mirror posture: a handle that can't be opened returns nil so the caller leaves
// discussion_post unmounted (AC5) rather than taking down the memory service. The handle lives for the
// process lifetime (it backs the HTTP tool surface), so it is intentionally not closed here.
func openDiscussionDB(dsn string) *sql.DB {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("ksquad-memory: discussion_post tool disabled (open db: %v)", err)
		return nil
	}
	return db
}

// openAgentAuthor builds the coord-backed agent authoring stores over the shared
// Postgres, fail-open exactly like openDiscussionDB: any setup problem returns a nil
// author so the caller leaves the work_item_* tools unmounted rather than taking
// down the memory service. It returns the create/update backend (WorkItemWriteStore)
// and — when a TeamAgentResolver is supplied (ISI-4743) — the assign dispatch
// backend (WorkItemDispatchStore) over the SAME db handle, so work_item_assign
// drives a real PM→implementer dispatch. A nil resolver (cluster-less / RBAC-less
// deployment) yields a nil dispatcher and assign is refused honestly by the MCP
// edge; a dispatch-store construction error is likewise degraded to nil rather than
// failing create/update. The handle lives for the process lifetime (it backs the
// tool surface).
func openAgentAuthor(dsn string, resolver coord.TeamAgentResolver) (memory.WorkItemAuthor, memory.WorkItemDispatcher) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("ksquad-memory: agent authoring tools disabled (open db: %v)", err)
		return nil, nil
	}
	writes, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		log.Printf("ksquad-memory: agent authoring tools disabled (write store: %v)", err)
		return nil, nil
	}
	if resolver == nil {
		return writes, nil
	}
	dispatch, err := coord.NewWorkItemDispatchStore(db, resolver)
	if err != nil {
		// Degrade assign to honestly-unavailable rather than dropping create/update:
		// the resolver is present but the store could not be built (should not happen
		// with a non-nil db + resolver), so keep the authoring lane serving reads.
		log.Printf("ksquad-memory: work_item_assign disabled (dispatch store: %v)", err)
		return writes, nil
	}
	return writes, dispatch
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
	// Attach the durable watermark (Story J-C AC1): the indexer resumes from its last committed position
	// across a restart instead of re-scanning from zero or skipping a window. The *memory.PgVectorStore
	// backs the projection_cursor table; the projection is idempotent on the derived record id (AC2), so
	// a crash between a batch write and the cursor save re-projects each message exactly once.
	ix := discussionindex.NewIndexer(discussion.NewStore(db), store, embedder, 0).WithCursor(store)
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

// startHandoffMirror launches the best-effort handoff→memory mirror (6.6, §8.5) in the background.
// Deliberately fail-open, exactly like the discussion indexer: any setup problem disables mirroring
// with a log line rather than taking down the memory service — the mirror is best-effort over the
// committed 2.8 coord artifact, and a memory outage must never gate the artifact (AC6). The sweep
// tolerates a missing coord schema (its query error is logged and retried), so it can start before
// the coord spine is provisioned. The superseder is the same *memory.PgVectorStore: a republished
// handoff soft-retracts its earlier mirrors so recall surfaces only the newest publication.
func startHandoffMirror(ctx context.Context, dsn string, store *memory.PgVectorStore, embedder memory.Embedder) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Printf("ksquad-memory: handoff mirror disabled (open db: %v)", err)
		return
	}
	m := handoffmirror.NewMirror(handoffmirror.NewSQLSource(db), store, embedder, store, 0)
	interval := 15 * time.Second
	if v := os.Getenv("HANDOFF_MIRROR_INTERVAL"); v != "" {
		if d, perr := time.ParseDuration(v); perr == nil {
			interval = d
		}
	}
	log.Printf("ksquad-memory: handoff mirror running (interval=%s)", interval)
	go func() {
		m.Run(ctx, interval)
		_ = db.Close()
	}()
}
