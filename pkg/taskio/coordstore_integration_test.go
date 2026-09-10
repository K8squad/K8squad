//go:build discussion_integration

// CoordStore integration test against a REAL Postgres with the SHIPPED coord
// migration (ISI-3601 S2): proves the four task-io endpoints round-trip through
// the coord schema end-to-end — the SQL text, the claim-fence custody check, the
// agent-initiated lane transition, and the append-only comment thread — the
// pieces a fake Store cannot.
//
// Same gate as internal/apiserver: CI provisions Postgres and runs
//
//	go test -tags=discussion_integration ./pkg/taskio/ -run TestCoordStoreIntegration
//
// DATABASE_URL unset ⇒ SKIP (a developer without Postgres is not blocked).
package taskio

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx"

	"github.com/K8squad/K8squad/pkg/coord"
)

func openTaskIOTestDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL unset — skipping the task-io integration test (needs real Postgres)")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("ping db: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// applyCoordSchema resets coord and applies the SHIPPED files — not inline DDL —
// so drift between the migrations and the adapter goes RED here (0001 base +
// 0015 change_ref, which the M1.5 read/write path rides on).
func applyCoordSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	for _, name := range []string{"0001_coord_schema.sql", "0015_work_item_change_ref.sql"} {
		var mig []byte
		var err error
		for _, c := range []string{
			filepath.Join("..", "..", "db", "migrations", name),
			filepath.Join("db", "migrations", name),
		} {
			if mig, err = os.ReadFile(c); err == nil {
				break
			}
		}
		if mig == nil {
			wd, _ := os.Getwd()
			t.Fatalf("could not read shipped migration %s (cwd %s)", name, wd)
		}
		if _, err := db.ExecContext(ctx, string(mig)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
}

// seedClaimedItem inserts one work item held by (runID, principal) at the given
// fence, exactly as the dispatcher would after claiming it. Returns the item id.
func seedClaimedItem(t *testing.T, db *sql.DB, runID, principal string, fence int64, state string) string {
	t.Helper()
	projectID := uuid.NewString()
	var itemID string
	err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, title, body, state, created_by)
		VALUES ($1::uuid, $2, $3, $4, $5)
		RETURNING id::text`,
		projectID, "S2 seam", "the run-scoped task-io seam", state, principal).Scan(&itemID)
	if err != nil {
		t.Fatalf("seed work item: %v", err)
	}
	// The 0001 trigger provisions the (unclaimed) claim row; set custody to our run.
	if _, err := db.Exec(`
		UPDATE coord.claim
		   SET holder_principal = $2, run_id = $3::uuid, fence_token = $4, acquired_at = now()
		 WHERE work_item_id = $1::uuid`,
		itemID, principal, runID, fence); err != nil {
		t.Fatalf("set claim custody: %v", err)
	}
	return itemID
}

func TestCoordStoreIntegrationFullFlow(t *testing.T) {
	db := openTaskIOTestDB(t)
	applyCoordSchema(t, db)

	const principal = "agent-A"
	runID := uuid.NewString()
	itemID := seedClaimedItem(t, db, runID, principal, 5, "in_progress")

	state, err := coord.NewHumanStateStore(db)
	if err != nil {
		t.Fatalf("state store: %v", err)
	}
	store, err := NewCoordStore(db, state)
	if err != nil {
		t.Fatalf("coord store: %v", err)
	}
	minter, err := NewMinter(testKey(), time.Hour)
	if err != nil {
		t.Fatalf("minter: %v", err)
	}
	h := NewHandler(minter, store)
	tok, err := minter.Mint(runID, itemID, principal)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}

	call := func(method, path, body string) *httptest.ResponseRecorder {
		var r *http.Request
		if body != "" {
			r = httptest.NewRequest(method, path, strings.NewReader(body))
		} else {
			r = httptest.NewRequest(method, path, nil)
		}
		r.Header.Set("Authorization", "Bearer "+tok)
		w := httptest.NewRecorder()
		h.Mux().ServeHTTP(w, r)
		return w
	}

	// checkout — confirms our existing custody, returns the seeded fence.
	w := call(http.MethodPost, "/checkout", "")
	if w.Code != http.StatusOK {
		t.Fatalf("checkout: %d %s", w.Code, w.Body.String())
	}
	var co checkoutResponse
	if err := json.Unmarshal(w.Body.Bytes(), &co); err != nil {
		t.Fatalf("decode checkout: %v", err)
	}
	if co.FenceToken != 5 || co.WorkItemID != itemID || co.RunID != runID {
		t.Fatalf("checkout response = %+v, want fence 5 / item %s / run %s", co, itemID, runID)
	}

	// get-task — reflects the seeded content, empty comment thread.
	w = call(http.MethodGet, "/get-task", "")
	if w.Code != http.StatusOK {
		t.Fatalf("get-task: %d %s", w.Code, w.Body.String())
	}
	var td TaskDetail
	if err := json.Unmarshal(w.Body.Bytes(), &td); err != nil {
		t.Fatalf("decode get-task: %v", err)
	}
	if td.WorkItemID != itemID || td.State != "in_progress" || td.Title != "S2 seam" {
		t.Fatalf("get-task detail = %+v", td)
	}
	if td.Holder != principal || td.RunID != runID || td.FenceToken != 5 {
		t.Fatalf("get-task custody = holder %q run %q fence %d", td.Holder, td.RunID, td.FenceToken)
	}
	if len(td.Comments) != 0 {
		t.Fatalf("expected no comments yet, got %d", len(td.Comments))
	}

	// post-comment — appends a provenanced note attributed to the token principal.
	w = call(http.MethodPost, "/post-comment", `{"body":"progress: seam wired"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("post-comment: %d %s", w.Code, w.Body.String())
	}
	var cm Comment
	if err := json.Unmarshal(w.Body.Bytes(), &cm); err != nil {
		t.Fatalf("decode comment: %v", err)
	}
	if cm.Author != principal || cm.Body != "progress: seam wired" {
		t.Fatalf("comment = %+v", cm)
	}

	// post-change (M1.5/ISI-4131) — the run reports a commit + a PR link.
	w = call(http.MethodPost, "/post-change", `{"kind":"commit","ref":"deadbeefcafe","summary":"fix the widget"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("post-change: %d %s", w.Code, w.Body.String())
	}
	var cr ChangeRef
	if err := json.Unmarshal(w.Body.Bytes(), &cr); err != nil {
		t.Fatalf("decode change ref: %v", err)
	}
	if cr.Kind != "commit" || cr.Ref != "deadbeefcafe" || cr.Author != principal || cr.RunID != runID {
		t.Fatalf("change ref = %+v", cr)
	}
	w = call(http.MethodPost, "/post-change", `{"kind":"pull_request","ref":"https://scm.example/pr/7"}`)
	if w.Code != http.StatusCreated {
		t.Fatalf("post-change PR: %d %s", w.Code, w.Body.String())
	}

	// A bad kind / empty ref is a 400 — the tight enum is enforced at the seam.
	w = call(http.MethodPost, "/post-change", `{"kind":"sneaky","ref":"x"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("post-change bad kind: %d, want 400", w.Code)
	}
	w = call(http.MethodPost, "/post-change", `{"kind":"commit"}`)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("post-change empty ref: %d, want 400", w.Code)
	}

	// update-status — agent-initiated lane move in_progress → in_review.
	w = call(http.MethodPost, "/update-status", `{"status":"in_review"}`)
	if w.Code != http.StatusNoContent {
		t.Fatalf("update-status: %d %s", w.Code, w.Body.String())
	}

	// get-task again — the new status, the new comment AND the change refs are
	// all visible on the ticket without any human relay (the M1.5 AC).
	w = call(http.MethodGet, "/get-task", "")
	if err := json.Unmarshal(w.Body.Bytes(), &td); err != nil {
		t.Fatalf("decode get-task#2: %v", err)
	}
	if td.State != "in_review" {
		t.Fatalf("state after update = %q, want in_review", td.State)
	}
	if len(td.Comments) != 1 || td.Comments[0].Body != "progress: seam wired" || td.Comments[0].Author != principal {
		t.Fatalf("comments after post = %+v", td.Comments)
	}
	if len(td.ChangeRefs) != 2 {
		t.Fatalf("change refs after posts = %+v, want 2", td.ChangeRefs)
	}
	if td.ChangeRefs[0].Ref != "deadbeefcafe" || td.ChangeRefs[0].RunID != runID ||
		td.ChangeRefs[1].Ref != "https://scm.example/pr/7" || td.ChangeRefs[1].Summary != "" {
		t.Fatalf("change refs after posts = %+v", td.ChangeRefs)
	}

	// An invalid target lane is a 422 (invalid transition), not a silent no-op.
	w = call(http.MethodPost, "/update-status", `{"status":"nonsense"}`)
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid transition: %d, want 422", w.Code)
	}
}

// TestCoordStoreIntegrationStaleFence proves a checkout by a run that does NOT
// hold the claim is rejected 409 — custody lost, fence monotonicity preserved.
func TestCoordStoreIntegrationStaleFence(t *testing.T) {
	db := openTaskIOTestDB(t)
	applyCoordSchema(t, db)

	const holder = "agent-holder"
	realRun := uuid.NewString()
	itemID := seedClaimedItem(t, db, realRun, holder, 9, "in_progress")

	state, _ := coord.NewHumanStateStore(db)
	store, _ := NewCoordStore(db, state)
	minter, _ := NewMinter(testKey(), time.Hour)
	h := NewHandler(minter, store)

	// A token for a DIFFERENT run that never claimed the item.
	otherRun := uuid.NewString()
	tok, _ := minter.Mint(otherRun, itemID, "agent-intruder")

	r := httptest.NewRequest(http.MethodPost, "/checkout", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	h.Mux().ServeHTTP(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("stale-fence checkout: %d, want 409", w.Code)
	}
}
