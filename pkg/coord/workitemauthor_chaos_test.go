//go:build chaos

// workitemauthor_chaos_test.go — the real-Postgres integration gate for the
// ADR-0024 (ISI-4735) agent-authored create/edit entry (workitemauthor.go),
// proved against the SHIPPED coord schema (db/migrations/0001 + 0018) on a live
// Postgres, inside the same required chaos gate as TestSpine.
//
// These are the DB-backed properties the offline unit lane (workitemauthor_unit_test.go)
// cannot reach: custody enforcement (the agent must hold the parent's claim),
// fan-out bounding (depth cap + per-run budget), honest agent audit provenance
// (principal = agent, run_id set, initiated_by_user_id NULL), and existence-hiding
// (a parent outside custody is indistinguishable from a missing one).
package coord_test

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

const (
	authorProject = "33333333-3333-3333-3333-333333333333"
	authorTeam    = "44444444-4444-4444-4444-444444444444"
	authorAgent   = "john"       // matches coord.claim.assignee_agent
	authorPrinc   = "agent:john" // the agent's server-stamped principal
	authorRun     = "00000000-0000-0000-0000-0000000000a1"
)

// resetAuthorSchema re-applies the base spine (0001) + the assignee_agent column
// (0018) + the create-time attribute columns (0020) into a clean coord schema —
// the teeth bite the real DDL exactly as the apiserver migration runner provisions
// it. 0020 is mandatory: AgentCreateWorkItem INSERTs/RETURNs
// work_item.priority/work_mode/labels (workitemauthor.go), so a bare 0001+0018
// schema fails the create outright — this is why ISI-4913 pairs the 0020 fix with
// making these tests actually run in the chaos lane (TestSpine-named). Matches the
// S5 custody test's 0001+0018+0020 shape.
func resetAuthorSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, assigneeMigrationSQL(t))
	mustExec(t, db, migrationFile(t, "0020_work_item_create_fields.sql"))
}

// assigneeMigrationSQL locates the shipped 0018 migration, mirroring
// coordMigrationSQL's candidate paths.
func assigneeMigrationSQL(t *testing.T) string {
	t.Helper()
	candidates := []string{}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append(candidates, filepath.Join(dir, "0018_claim_assignee.sql"))
	}
	candidates = append(candidates,
		filepath.Join("..", "..", "db", "migrations", "0018_claim_assignee.sql"),
		filepath.Join("db", "migrations", "0018_claim_assignee.sql"),
	)
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("cannot locate 0018_claim_assignee.sql (looked in %v)", candidates)
	return ""
}

// authorSeedItem inserts a work_item (the trigger auto-provisions its claim row) and
// returns its id. parent "" ⇒ a root item.
func authorSeedItem(t *testing.T, db *sql.DB, parent, title string) string {
	t.Helper()
	var id string
	var parentParam any
	if parent != "" {
		parentParam = parent
	}
	if err := db.QueryRow(`
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, created_by)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, 'user:alice')
		RETURNING id::text`, authorProject, authorTeam, parentParam, title).Scan(&id); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	return id
}

// grantCustody stamps the item's claim with the agent as assignee (attribution) —
// the "parent's assigned agent" custody path of ADR-0024 §4.1.
func grantCustody(t *testing.T, db *sql.DB, itemID string) {
	t.Helper()
	mustExec(t, db, `UPDATE coord.claim SET assignee_agent = $1 WHERE work_item_id = $2::uuid`, authorAgent, itemID)
}

func newAuthorStore(t *testing.T, db *sql.DB) *coord.WorkItemWriteStore {
	t.Helper()
	s, err := coord.NewWorkItemWriteStore(db)
	if err != nil {
		t.Fatalf("NewWorkItemWriteStore: %v", err)
	}
	return s
}

func authorInput(parent string) coord.AgentCreateWorkItemInput {
	return coord.AgentCreateWorkItemInput{
		ParentID: parent, Title: "child", Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}
}

// TestSpineAuthorCreate_WithCustody_CreatesChild_HonestAudit — the happy path: an agent
// holding the parent's claim creates a child in 'backlog', inheriting the parent's
// team, with an audit row stamped agent-principal + run_id + initiated_by NULL.
func TestSpineAuthorCreate_WithCustody_CreatesChild_HonestAudit(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	parent := authorSeedItem(t, db, "", "epic")
	grantCustody(t, db, parent)

	s := newAuthorStore(t, db)
	rec, err := s.AgentCreateWorkItem(context.Background(), authorInput(parent))
	if err != nil {
		t.Fatalf("create with custody: %v", err)
	}
	if rec.State != "backlog" {
		t.Fatalf("child state = %q, want backlog", rec.State)
	}
	if rec.ParentID == nil || *rec.ParentID != parent {
		t.Fatalf("child parent = %v, want %s", rec.ParentID, parent)
	}
	if rec.TeamID == nil || *rec.TeamID != authorTeam {
		t.Fatalf("child team = %v, want inherited %s", rec.TeamID, authorTeam)
	}
	if rec.CreatedBy != authorPrinc {
		t.Fatalf("created_by = %q, want the agent principal %q", rec.CreatedBy, authorPrinc)
	}

	// Honest agent provenance on the audit row.
	var principal string
	var runID sql.NullString
	var initiatedBy sql.NullString
	if err := db.QueryRow(`
		SELECT principal, run_id::text, initiated_by_user_id::text FROM coord.audit_log
		 WHERE work_item_id = $1::uuid AND event_type = 'work_item_created'`,
		rec.ID).Scan(&principal, &runID, &initiatedBy); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if principal != authorPrinc {
		t.Fatalf("audit principal = %q, want agent %q", principal, authorPrinc)
	}
	if !runID.Valid || runID.String != authorRun {
		t.Fatalf("audit run_id = %v, want %s (per-run budget provenance)", runID, authorRun)
	}
	if initiatedBy.Valid {
		t.Fatalf("audit initiated_by_user_id = %q, want NULL (agent never spoofed as human)", initiatedBy.String)
	}
}

// TestSpineAuthorCreate_NoCustody_Denied — an agent with no claim on the parent is
// refused, and nothing is written. Existence-hiding: same refusal whether the
// parent is unheld or absent.
func TestSpineAuthorCreate_NoCustody_Denied(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	parent := authorSeedItem(t, db, "", "epic") // no grantCustody
	s := newAuthorStore(t, db)

	if _, err := s.AgentCreateWorkItem(context.Background(), authorInput(parent)); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("unheld parent: got %v, want ErrAgentAuthorNotInCustody", err)
	}
	// A parent that does not exist is refused identically (no existence probe).
	missing := "99999999-9999-9999-9999-999999999999"
	if _, err := s.AgentCreateWorkItem(context.Background(), authorInput(missing)); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("missing parent: got %v, want ErrAgentAuthorNotInCustody", err)
	}
	assertChildCount(t, db, parent, 0)
}

// TestSpineAuthorCreate_DepthCapExceeded — a chain at the cap refuses a deeper child.
func TestSpineAuthorCreate_DepthCapExceeded(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	// Build epic(1)→story(2)→task(3)→subtask(4) directly, custody on the deepest.
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	task := authorSeedItem(t, db, story, "task")
	subtask := authorSeedItem(t, db, task, "subtask") // depth 4 == cap
	grantCustody(t, db, subtask)
	s := newAuthorStore(t, db)

	// A child under the depth-4 subtask would be depth 5 > cap ⇒ refused.
	if _, err := s.AgentCreateWorkItem(context.Background(), authorInput(subtask)); !errors.Is(err, coord.ErrAgentAuthorDepthExceeded) {
		t.Fatalf("over-deep child: got %v, want ErrAgentAuthorDepthExceeded", err)
	}
	assertChildCount(t, db, subtask, 0)

	// A child under the depth-3 task (→ depth 4) is allowed once custody is granted.
	grantCustody(t, db, task)
	if _, err := s.AgentCreateWorkItem(context.Background(), authorInput(task)); err != nil {
		t.Fatalf("depth-4 child should be allowed: %v", err)
	}
}

// TestSpineAuthorCreate_RunBudgetExceeded — a run at its create budget refuses the next
// create. We drive the boundary with a tiny synthetic budget by pre-seeding
// AgentAuthorRunBudget prior authored-create audit rows for the run.
func TestSpineAuthorCreate_RunBudgetExceeded(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	parent := authorSeedItem(t, db, "", "epic")
	grantCustody(t, db, parent)
	s := newAuthorStore(t, db)

	// Pre-fill the run's budget with prior authored creates (audit rows on the
	// parent carrying this run_id), so the next real create is exactly over budget.
	for i := 0; i < coord.AgentAuthorRunBudget; i++ {
		mustExec(t, db, `
			INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload)
			VALUES ($1::uuid, $2::uuid, 'work_item_created', $3, '{}'::jsonb)`,
			parent, authorRun, authorPrinc)
	}
	if _, err := s.AgentCreateWorkItem(context.Background(), authorInput(parent)); !errors.Is(err, coord.ErrAgentAuthorRunBudgetExceeded) {
		t.Fatalf("over-budget create: got %v, want ErrAgentAuthorRunBudgetExceeded", err)
	}

	// A DIFFERENT run still has full budget.
	fresh := authorInput(parent)
	fresh.RunID = "00000000-0000-0000-0000-0000000000b2"
	if _, err := s.AgentCreateWorkItem(context.Background(), fresh); err != nil {
		t.Fatalf("fresh run should be under budget: %v", err)
	}
}

// TestSpineAuthorUpdate_DescendantOfCustody_Allowed — update reaches the in-custody item
// AND its descendants (O-3); an item under no held ancestor is refused.
func TestSpineAuthorUpdate_DescendantOfCustody_Allowed(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	grantCustody(t, db, epic) // custody on the ancestor only
	s := newAuthorStore(t, db)

	newTitle := "renamed story"
	rec, err := s.AgentUpdateWorkItem(context.Background(), story, coord.AgentUpdateWorkItemInput{
		Title: &newTitle, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	})
	if err != nil {
		t.Fatalf("update descendant of custody: %v", err)
	}
	if rec.Title != newTitle {
		t.Fatalf("title = %q, want %q", rec.Title, newTitle)
	}

	// A sibling tree with no held ancestor is refused.
	other := authorSeedItem(t, db, "", "other-epic")
	if _, err := s.AgentUpdateWorkItem(context.Background(), other, coord.AgentUpdateWorkItemInput{
		Title: &newTitle, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("update outside custody: got %v, want ErrAgentAuthorNotInCustody", err)
	}
}

// TestSpineAuthorUpdate_DetachToRoot_Denied (F1 / I1, ISI-4746) — a reparent to parent_id:""
// would NULL the parent and promote an in-custody sub-ticket to an agent-controlled
// ROOT item. It is refused ErrAgentAuthorRootDenied and the parent link is untouched.
func TestSpineAuthorUpdate_DetachToRoot_Denied(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	grantCustody(t, db, epic) // whole subtree in custody scope
	s := newAuthorStore(t, db)

	detach := ""
	if _, err := s.AgentUpdateWorkItem(context.Background(), story, coord.AgentUpdateWorkItemInput{
		ParentID: &detach, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); !errors.Is(err, coord.ErrAgentAuthorRootDenied) {
		t.Fatalf("detach-to-root: got %v, want ErrAgentAuthorRootDenied", err)
	}
	assertParent(t, db, story, epic) // parent link untouched
}

// TestSpineAuthorUpdate_ReparentToUnheld_Denied (F1 / I2 dest, ISI-4746) — a reparent whose
// DESTINATION the agent does not hold is refused: only source custody is settled by
// the frontier walk, so the destination gate must bite. Existence-hiding refusal.
func TestSpineAuthorUpdate_ReparentToUnheld_Denied(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	grantCustody(t, db, epic)
	other := authorSeedItem(t, db, "", "other-epic") // no custody granted
	s := newAuthorStore(t, db)

	if _, err := s.AgentUpdateWorkItem(context.Background(), story, coord.AgentUpdateWorkItemInput{
		ParentID: &other, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); !errors.Is(err, coord.ErrAgentAuthorNotInCustody) {
		t.Fatalf("reparent to unheld dest: got %v, want ErrAgentAuthorNotInCustody", err)
	}
	assertParent(t, db, story, epic) // parent link untouched
}

// TestSpineAuthorUpdate_ReparentDepthCapExceeded (F1 / I3, ISI-4746) — a reparent whose
// destination would push the moved item past the depth cap is refused; the same move
// under a shallower held destination is allowed.
func TestSpineAuthorUpdate_ReparentDepthCapExceeded(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	// epic(1)→story(2)→task(3)→subtask(4), whole subtree held via the root epic.
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	task := authorSeedItem(t, db, story, "task")
	subtask := authorSeedItem(t, db, task, "subtask") // depth 4 == cap
	grantCustody(t, db, epic)
	itemX := authorSeedItem(t, db, epic, "movable") // depth 2, held (descendant of epic)
	s := newAuthorStore(t, db)

	// Reparent itemX under the depth-4 subtask ⇒ new depth 5 > cap ⇒ refused.
	if _, err := s.AgentUpdateWorkItem(context.Background(), itemX, coord.AgentUpdateWorkItemInput{
		ParentID: &subtask, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); !errors.Is(err, coord.ErrAgentAuthorDepthExceeded) {
		t.Fatalf("over-deep reparent: got %v, want ErrAgentAuthorDepthExceeded", err)
	}
	assertParent(t, db, itemX, epic) // move rejected, parent untouched

	// Reparent itemX under the depth-3 task ⇒ new depth 4 == cap ⇒ allowed.
	if _, err := s.AgentUpdateWorkItem(context.Background(), itemX, coord.AgentUpdateWorkItemInput{
		ParentID: &task, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); err != nil {
		t.Fatalf("depth-4 reparent should be allowed: %v", err)
	}
	assertParent(t, db, itemX, task)
}

// TestSpineAuthorUpdate_ReparentSubtreeDepthCapExceeded — the depth cap must bound the
// WHOLE moved subtree, not just the moved node (ADR-0024 I3). A movable(2)→mchild(3)
// pair grafted under a d3 parent would seat the node at the cap (d4) while its child
// spills to d5; the per-node check would wave it through, so this pins the
// subtree-height bound.
func TestSpineAuthorUpdate_ReparentSubtreeDepthCapExceeded(t *testing.T) {
	db := openDB(t, dsnOrFatal(t))
	resetAuthorSchema(t, db)
	epic := authorSeedItem(t, db, "", "epic")
	story := authorSeedItem(t, db, epic, "story")
	task := authorSeedItem(t, db, story, "task")
	movable := authorSeedItem(t, db, epic, "movable") // depth 2
	authorSeedItem(t, db, movable, "mchild")          // depth 3; height(movable subtree)=2
	grantCustody(t, db, epic)
	s := newAuthorStore(t, db)

	// Under task(d3): movable→d4 (fits alone) but mchild→d5 > cap ⇒ refused, untouched.
	if _, err := s.AgentUpdateWorkItem(context.Background(), movable, coord.AgentUpdateWorkItemInput{
		ParentID: &task, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); !errors.Is(err, coord.ErrAgentAuthorDepthExceeded) {
		t.Fatalf("over-deep subtree reparent: got %v, want ErrAgentAuthorDepthExceeded", err)
	}
	assertParent(t, db, movable, epic)

	// Under story(d2): movable→d3, mchild→d4 == cap ⇒ allowed.
	if _, err := s.AgentUpdateWorkItem(context.Background(), movable, coord.AgentUpdateWorkItemInput{
		ParentID: &story, Principal: authorPrinc, AgentName: authorAgent, RunID: authorRun,
	}); err != nil {
		t.Fatalf("at-cap subtree reparent should be allowed: %v", err)
	}
	assertParent(t, db, movable, story)
}

// ---- small DB helpers (chaos lane) ----

func assertParent(t *testing.T, db *sql.DB, item, wantParent string) {
	t.Helper()
	var parent sql.NullString
	if err := db.QueryRow(`SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`, item).Scan(&parent); err != nil {
		t.Fatalf("read parent: %v", err)
	}
	got := ""
	if parent.Valid {
		got = parent.String
	}
	if got != wantParent {
		t.Fatalf("parent = %q, want %q", got, wantParent)
	}
}

func assertChildCount(t *testing.T, db *sql.DB, parent string, want int) {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT count(*) FROM coord.work_item WHERE parent_id = $1::uuid`, parent).Scan(&n); err != nil {
		t.Fatalf("count children: %v", err)
	}
	if n != want {
		t.Fatalf("child count = %d, want %d", n, want)
	}
}
