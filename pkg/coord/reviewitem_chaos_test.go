//go:build chaos

// reviewitem_chaos_test.go — the real-Postgres integration gate for the ISI-4776
// system PR-review dedup primitive (coord.EnsureReviewWorkItem, reviewitem.go),
// proved against the SHIPPED coord schema (db/migrations/0001 + 0020) on a live
// Postgres, inside the same required chaos gate as TestSpine.
//
// This closes the KNOWN GAP flagged by the E4-wiring author and by the Testing
// Architect gate (ISI-4782): the advisory-lock create-if-absent race cannot be
// reached by the offline unit lane (reviewitem_unit_test.go pins only the
// pre-BeginTx input guards). Here we exercise the DB-backed invariants the wiring
// depends on:
//
//   - RACE: N goroutines calling EnsureReviewWorkItem for the SAME (project,
//     dedup-label) at once produce EXACTLY ONE insert — exactly one Created=true,
//     exactly one work_item row carrying the label, exactly one 'work_item_created'
//     audit row. This is the property pg_advisory_xact_lock exists to guarantee;
//     a naive find-then-create would double-insert here.
//   - LEVEL-TRIGGERED RE-RUN: a sequential re-ensure of an existing label is an
//     idempotent no-op (Created=false) and returns the row's CURRENT State — the
//     signal the dispatch adapter reads to decide "already dispatched, skip" vs
//     "backlog orphan, self-heal", so a repeated reconcile never double-dispatches.
//   - ISOLATION: two DIFFERENT dedup labels never contend (distinct advisory-lock
//     keys) and both insert — unrelated PRs are not serialised against each other.
//   - AUTHORSHIP: the row is authored under the SYSTEM principal the caller passes
//     (never an agent), stamped into created_by and the audit principal, with
//     initiated_by_user_id NULL — the ISI-4711 custody-wall shape on the real row.
package coord_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	reviewProject   = "55555555-5555-5555-5555-555555555555"
	reviewTeam      = "66666666-6666-6666-6666-666666666666"
	reviewPrincipal = "system:review-automation" // reviewtrigger.Initiator
	reviewLabel     = "ksquad.review=deadbeefcafe"
)

// resetReviewSchema re-applies the base spine (0001) + the create-fields columns
// (0020: priority/work_mode/labels) into a clean coord schema — the teeth bite the
// real DDL EnsureReviewWorkItem writes (labels text[], the audit_log co-commit).
func resetReviewSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, reviewCreateFieldsSQL(t))
}

// reviewCreateFieldsSQL locates the shipped 0020 migration (labels/priority/
// work_mode), mirroring coordMigrationSQL's candidate paths.
func reviewCreateFieldsSQL(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "0020_work_item_create_fields.sql"))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "db", "migrations", "0020_work_item_create_fields.sql"),
		filepath.Join("db", "migrations", "0020_work_item_create_fields.sql"),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("cannot locate 0020_work_item_create_fields.sql (looked in %v)", candidates)
	return ""
}

func newReviewStore(t *testing.T, db *sql.DB) *coord.WorkItemWriteStore {
	t.Helper()
	s, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	return s
}

func reviewInput(label, title string) coord.EnsureReviewWorkItemInput {
	return coord.EnsureReviewWorkItemInput{
		ProjectID:  reviewProject,
		TeamID:     reviewTeam,
		Title:      title,
		Body:       "automated review body",
		DedupLabel: label,
		Principal:  reviewPrincipal,
	}
}

// countRowsWithLabel returns how many coord.work_item rows in reviewProject carry
// the dedup label — the authoritative "did we double-create?" check.
func countRowsWithLabel(t *testing.T, db *sql.DB, label string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.work_item WHERE project_id = $1::uuid AND labels @> ARRAY[$2]::text[]`,
		reviewProject, label).Scan(&n); err != nil {
		t.Fatalf("count rows with label: %v", err)
	}
	return n
}

// countCreateAudit returns the number of 'work_item_created' audit rows for the
// single item carrying the label — proves the audit co-commits exactly once.
func countCreateAudit(t *testing.T, db *sql.DB, itemID string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(
		`SELECT count(*) FROM coord.audit_log WHERE work_item_id = $1::uuid AND event_type = 'work_item_created'`,
		itemID).Scan(&n); err != nil {
		t.Fatalf("count create audit: %v", err)
	}
	return n
}

// TestEnsureReviewWorkItem_Race_ExactlyOneInsert is the property the advisory lock
// exists for: N goroutines racing EnsureReviewWorkItem for the SAME (project,label)
// at once must yield EXACTLY ONE insert. Everyone else observes the existing row.
func TestEnsureReviewWorkItem_Race_ExactlyOneInsert(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	// The advisory lock serialises the racers; give the pool a connection per
	// racer so a waiter blocks on the LOCK (the property under test), not on a
	// starved pool. openDB caps the pool low for the port-forward; raise it here.
	const racers = 16
	db.SetMaxOpenConns(racers)
	db.SetMaxIdleConns(racers)
	resetReviewSchema(t, db)
	s := newReviewStore(t, db)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		created  int
		itemIDs  = map[string]struct{}{}
		firstErr error
	)
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start // release everyone together to maximise contention
			res, err := s.EnsureReviewWorkItem(context.Background(),
				reviewInput(reviewLabel, fmt.Sprintf("Review acme/widget#42 (racer %d)", i)))
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				if firstErr == nil {
					firstErr = err
				}
				return
			}
			if res.Created {
				created++
			}
			itemIDs[res.Item.ID] = struct{}{}
		}(i)
	}
	close(start)
	wg.Wait()

	if firstErr != nil {
		t.Fatalf("no racer may error, got: %v", firstErr)
	}
	if created != 1 {
		t.Errorf("Created=true count = %d, want EXACTLY 1 (advisory lock must admit one insert)", created)
	}
	if len(itemIDs) != 1 {
		t.Errorf("distinct item IDs returned = %d, want 1 (every racer must observe the same row)", len(itemIDs))
	}
	if n := countRowsWithLabel(t, db, reviewLabel); n != 1 {
		t.Errorf("rows carrying dedup label = %d, want EXACTLY 1 (double-create = the bug)", n)
	}
	for id := range itemIDs {
		if n := countCreateAudit(t, db, id); n != 1 {
			t.Errorf("work_item_created audit rows for %s = %d, want 1 (audit co-commits with the single insert)", id, n)
		}
	}
}

// TestEnsureReviewWorkItem_LevelTriggeredReRun: a sequential re-ensure of an
// existing label is an idempotent no-op returning the row's CURRENT state — the
// signal the dispatch adapter reads to skip an already-advanced item. A repeated
// reconcile must never produce a second row.
func TestEnsureReviewWorkItem_LevelTriggeredReRun(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetReviewSchema(t, db)
	s := newReviewStore(t, db)
	ctx := context.Background()

	first, err := s.EnsureReviewWorkItem(ctx, reviewInput(reviewLabel, "Review widget#42"))
	if err != nil {
		t.Fatalf("first ensure: %v", err)
	}
	if !first.Created {
		t.Fatal("first ensure must Create")
	}
	if first.Item.State != "backlog" {
		t.Fatalf("fresh review item state = %q, want backlog", first.Item.State)
	}

	// Simulate the item having been dispatched (advanced past the entry lane) by a
	// prior pass, then re-run the reconcile: the re-ensure must find it, report
	// Created=false, and surface the CURRENT (advanced) state so the adapter skips.
	mustExec(t, db, `UPDATE coord.work_item SET state = 'in_progress' WHERE id = $1::uuid`, first.Item.ID)

	second, err := s.EnsureReviewWorkItem(ctx, reviewInput(reviewLabel, "Review widget#42 (re-run)"))
	if err != nil {
		t.Fatalf("re-ensure: %v", err)
	}
	if second.Created {
		t.Error("re-ensure of an existing label must be Created=false (idempotent)")
	}
	if second.Item.ID != first.Item.ID {
		t.Errorf("re-ensure returned a different row %q, want the original %q", second.Item.ID, first.Item.ID)
	}
	if second.Item.State != "in_progress" {
		t.Errorf("re-ensure state = %q, want the current 'in_progress' (drives the skip-dispatch decision)", second.Item.State)
	}
	if n := countRowsWithLabel(t, db, reviewLabel); n != 1 {
		t.Errorf("rows after re-run = %d, want 1 (level-triggered re-run must not double-create)", n)
	}
}

// TestEnsureReviewWorkItem_DistinctLabelsDoNotContend: two different dedup labels
// hash to different advisory-lock keys, so both insert — unrelated PRs are never
// serialised against each other, and each gets its own row.
func TestEnsureReviewWorkItem_DistinctLabelsDoNotContend(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetReviewSchema(t, db)
	s := newReviewStore(t, db)
	ctx := context.Background()

	a, err := s.EnsureReviewWorkItem(ctx, reviewInput("ksquad.review=aaa111", "Review PR #1"))
	if err != nil {
		t.Fatalf("ensure A: %v", err)
	}
	b, err := s.EnsureReviewWorkItem(ctx, reviewInput("ksquad.review=bbb222", "Review PR #2"))
	if err != nil {
		t.Fatalf("ensure B: %v", err)
	}
	if !a.Created || !b.Created {
		t.Fatalf("both distinct-label ensures must Create: a=%v b=%v", a.Created, b.Created)
	}
	if a.Item.ID == b.Item.ID {
		t.Fatal("distinct labels must produce distinct rows")
	}
	if n := countRowsWithLabel(t, db, "ksquad.review=aaa111"); n != 1 {
		t.Errorf("rows for label A = %d, want 1", n)
	}
	if n := countRowsWithLabel(t, db, "ksquad.review=bbb222"); n != 1 {
		t.Errorf("rows for label B = %d, want 1", n)
	}
}

// TestEnsureReviewWorkItem_AuthoredUnderSystemPrincipal: the persisted row and its
// audit row carry the SYSTEM principal (never an agent), with initiated_by_user_id
// NULL — the ISI-4711 custody-wall shape verified on the real row, not a fake.
func TestEnsureReviewWorkItem_AuthoredUnderSystemPrincipal(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetReviewSchema(t, db)
	s := newReviewStore(t, db)

	res, err := s.EnsureReviewWorkItem(context.Background(), reviewInput(reviewLabel, "Review widget#42"))
	if err != nil {
		t.Fatalf("ensure: %v", err)
	}

	var createdBy string
	if err := db.QueryRow(`SELECT created_by FROM coord.work_item WHERE id = $1::uuid`, res.Item.ID).Scan(&createdBy); err != nil {
		t.Fatalf("read created_by: %v", err)
	}
	if createdBy != reviewPrincipal {
		t.Errorf("work_item.created_by = %q, want SYSTEM %q", createdBy, reviewPrincipal)
	}

	var auditPrincipal string
	var initiatedBy sql.NullString
	if err := db.QueryRow(
		`SELECT principal, initiated_by_user_id FROM coord.audit_log WHERE work_item_id = $1::uuid AND event_type = 'work_item_created'`,
		res.Item.ID).Scan(&auditPrincipal, &initiatedBy); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if auditPrincipal != reviewPrincipal {
		t.Errorf("audit principal = %q, want SYSTEM %q", auditPrincipal, reviewPrincipal)
	}
	if initiatedBy.Valid {
		t.Errorf("initiated_by_user_id = %q, want NULL (no human actor on a system-authored review item)", initiatedBy.String)
	}
}
