//go:build chaos

// blocked_metrics_chaos_test.go — the live-Postgres teeth for the Story 13.3
// blocked-by gauge read half (ISI-2916). The sampler/loop control flow is proven
// without Postgres in blocked_metrics_test.go; here we prove CountBlocked's SQL
// counts exactly the rows with blocked_reason IS NOT NULL against a real Postgres,
// the same DATABASE_URL wiring the rest of the coord chaos gate uses (dsnOrFatal /
// openDB in spine_chaos_test.go). Self-contained schema so it depends on no other
// fixture: a minimal work_item carrying the shipped blocked_reason column (§8.6,
// 0001_coord_schema.sql).
package coord_test

import (
	"context"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

func TestCountBlockedCountsOnlyBlockedRows(t *testing.T) {
	ctx := context.Background()
	db := openDB(t, dsnOrFatal(t))
	defer db.Close()

	const tbl = "blocked_gauge_wi"
	for _, s := range []string{
		`DROP TABLE IF EXISTS ` + tbl,
		`CREATE TABLE ` + tbl + `(id int PRIMARY KEY, state text NOT NULL, blocked_reason text)`,
		// two blocked (reason non-NULL), one blocked by empty-string (still non-NULL
		// → counted: the condition is IS NOT NULL, not non-empty), two not blocked.
		`INSERT INTO ` + tbl + ` VALUES
			(1,'open','needs_approval'),
			(2,'open','waiting_on_dep'),
			(3,'open',''),
			(4,'open',NULL),
			(5,'done',NULL)`,
	} {
		if _, err := db.ExecContext(ctx, s); err != nil {
			t.Fatalf("schema/seed %q: %v", s, err)
		}
	}

	cfg := coord.DefaultSweepConfig()
	cfg.WorkItem = tbl
	cfg.Claim = tbl // unused by CountBlocked; satisfies validate()
	cfg.OpenState = "open"
	cfg.DoneState = "done"
	cfg.Interval = time.Second
	store, err := coord.NewSweepStore(db, cfg)
	if err != nil {
		t.Fatalf("NewSweepStore: %v", err)
	}

	got, err := store.CountBlocked(ctx)
	if err != nil {
		t.Fatalf("CountBlocked: %v", err)
	}
	if got != 3 {
		t.Fatalf("CountBlocked = %d, want 3 (rows 1,2,3 have blocked_reason IS NOT NULL; 4,5 are NULL)", got)
	}
}
