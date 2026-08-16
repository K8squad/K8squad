// Command event-relay runs the Story 12.1 / ISI-2260 domain-event relay worker:
// it tails coord.outbox (LISTEN/NOTIFY + poll) and publishes each unflushed row
// to NATS JetStream (subject ksquad.{entity}.{project}.{squad}.{event_type}),
// stamping published_at at-least-once. It is DECOUPLED from the write path — a
// standalone process that only reads the outbox and publishes — so NATS being
// down (or this worker being down) never blocks a Run/claim/memory/scm write
// (§17.4). In-cluster the same worker runs inside the apiserver; this binary is
// the standalone deployment (Dockerfile.event-relay) driven by the Story 9.4
// event-relay ConfigMap.
//
// Configuration (env; keys mirror the event-relay.yaml ConfigMap):
//
//	DATABASE_URL           (required) Postgres DSN for the outbox
//	RELAY_NATS_URL         (required) NATS URL            (ConfigMap relay.natsUrl)
//	RELAY_SUBJECT_PREFIX   subject root, default "ksquad" (ConfigMap relay.subjectPrefix)
//	RELAY_POLL_INTERVAL    poll fallback cadence, default "2s"
//	RELAY_METRICS_ADDR     Prometheus /metrics listen addr, default ":9090"
package main

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/K8squad/K8squad/pkg/events"
	"github.com/K8squad/K8squad/pkg/events/jetstream"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))

	dsn := os.Getenv("DATABASE_URL")
	natsURL := os.Getenv("RELAY_NATS_URL")
	if dsn == "" || natsURL == "" {
		log.Error("event-relay: DATABASE_URL and RELAY_NATS_URL are both required")
		os.Exit(2)
	}
	prefix := envOr("RELAY_SUBJECT_PREFIX", events.DefaultPrefix)
	poll := durationOr(log, "RELAY_POLL_INTERVAL", 2*time.Second)
	metricsAddr := envOr("RELAY_METRICS_ADDR", ":9090")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	db, err := sql.Open("pgx", dsn)
	if err != nil {
		log.Error("event-relay: open db", "err", err)
		os.Exit(1)
	}
	defer func() { _ = db.Close() }()

	// NATS/JetStream publisher. Connect retries a down bus rather than failing
	// (§17.4): the relay must start and buffer even if NATS is unavailable.
	pub, err := jetstream.Connect(ctx, jetstream.Config{URL: natsURL, Prefix: prefix})
	if err != nil {
		log.Error("event-relay: connect nats", "err", err, "url", natsURL)
		os.Exit(1)
	}
	defer func() { _ = pub.Close() }()
	if serr := pub.StreamEnsureErr(); serr != nil {
		log.Warn("event-relay: stream not yet ensured (chart may own it; will buffer)", "err", serr)
	}

	// NOTIFY wake on coord_outbox (latency only; the poll is the durable path).
	waker, err := events.NewPgWaker(dsn)
	if err != nil {
		log.Warn("event-relay: LISTEN unavailable, falling back to poll-only", "err", err)
	}
	if waker != nil {
		defer func() { _ = waker.Close() }()
	}

	relay, err := events.NewRelay(events.RelayConfig{
		Store:        events.NewSQLStore(db),
		Publisher:    pub,
		Prefix:       prefix,
		Metrics:      events.NewPrometheusMetrics(),
		Waker:        waker, // nil is fine — poll-only
		PollInterval: poll,
		Logger:       log,
	})
	if err != nil {
		log.Error("event-relay: build relay", "err", err)
		os.Exit(1)
	}

	// §17.2 metrics endpoint — scrape target, NOT a readiness gate (the relay
	// never gates apiserver health; NATS-down only grows the backlog gauges).
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.Handler())
	srv := &http.Server{Addr: metricsAddr, Handler: mux}
	go func() {
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("event-relay: metrics server", "err", err)
		}
	}()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("event-relay: started", "prefix", prefix, "poll", poll.String(), "metrics", metricsAddr,
		"listen", waker != nil)
	if err := relay.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("event-relay: relay stopped", "err", err)
		os.Exit(1)
	}
	log.Info("event-relay: shut down cleanly")
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func durationOr(log *slog.Logger, key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		log.Warn("event-relay: bad duration, using default", "key", key, "value", v, "default", def.String())
		return def
	}
	return d
}
