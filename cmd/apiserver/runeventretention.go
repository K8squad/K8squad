package main

// ISI-7484 WS-3 (parent ISI-7482/ISI-7480): run-step retention sweep. The
// outbox now doubles as the replayable Activity history (SSE Last-Event-ID
// replay + GET /api/runs/{id}/events hydration), so it needs a bounded life.
// This loop prunes RELAYED run-entity rows past the retention window through
// the governed coord.prune_run_events path (db/migrations/0044) — the single
// sanctioned exception to the outbox append-only guard. It never touches an
// unflushed relay row (at-least-once NATS delivery is untouched), never a
// non-run row, and never re-persists anything into coord.comment (ISI-7454
// guardrail).

import (
	"context"
	"log"
	"strings"
	"time"

	"github.com/K8squad/K8squad/pkg/events"
)

const (
	// defaultRunEventRetention keeps a week of run-step history: comfortably
	// beyond any SSE reconnect / reload window, bounded enough to keep the
	// journal (and this index) small.
	defaultRunEventRetention = 7 * 24 * time.Hour
	// runEventPruneInterval is the sweep cadence. Prune is a cheap indexed
	// DELETE (idx_outbox_run_retention) that is usually a no-op, so an hourly
	// tick is fine; 6h would also be reasonable.
	runEventPruneInterval = time.Hour
)

// runEventRetentionFromEnv reads KSQUAD_RUN_EVENT_RETENTION (Go duration, e.g.
// "168h" / "72h"; default 7d). "0" (or "0s") DISABLES retention — the journal
// keeps growing, the pre-ISI-7484 behavior — so an operator can turn the sweep
// off without a redeploy. A malformed or negative value warns and falls back
// to the default rather than silently disabling retention.
func runEventRetentionFromEnv(getenv func(string) string) time.Duration {
	raw := strings.TrimSpace(getenv("KSQUAD_RUN_EVENT_RETENTION"))
	if raw == "" {
		return defaultRunEventRetention
	}
	if raw == "0" || raw == "0s" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Printf("ksquad-apiserver: invalid KSQUAD_RUN_EVENT_RETENTION %q, using default %s", raw, defaultRunEventRetention)
		return defaultRunEventRetention
	}
	return d
}

// runEventPruneLoop prunes until ctx is done: once immediately at startup
// (catch up after downtime), then on every tick. Failures are logged and
// retried on the next tick — retention is best-effort housekeeping and must
// never crash or block serving.
func runEventPruneLoop(ctx context.Context, store *events.SQLStore, retention time.Duration) {
	prune := func() {
		if ctx.Err() != nil {
			return
		}
		pruned, err := store.PruneRunEventsBefore(ctx, time.Now().Add(-retention))
		if err != nil {
			log.Printf("ksquad-apiserver: run-event retention prune failed (will retry): %v", err)
			return
		}
		if pruned > 0 {
			log.Printf("ksquad-apiserver: pruned %d run-event rows past retention %s", pruned, retention)
		}
	}
	prune()
	ticker := time.NewTicker(runEventPruneInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			prune()
		}
	}
}
