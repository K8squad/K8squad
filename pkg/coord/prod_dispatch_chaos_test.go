//go:build chaos

// ISI-2526 — TestSpineProdDispatch (D1..D4): the Story 2.9 FR-B3
// delegation-with-feedback coordinator dispatch (proddispatch.go) proved
// against the SHIPPED coord schema (db/migrations/0001 + 0002) on a real
// Postgres, inside the same required gate as TestSpine / TestSpineProdClaim
// (the workflow's -run 'TestSpine' matches this name too).
//
// The pinned §2.9 loop — read-of-record → coordinator DECIDES+PRIORITIZES →
// new FENCED dispatch — with three properties, each proven DIFFERENTIALLY
// (the naive "helpful" design must break the property first, or the pass
// means nothing):
//
//	D1  no B→A channel (P1): the coordinator learns the completing run's
//	    findings/recommendation ONLY by reading the record. Teeth: the naive
//	    side-channel design (B's findings passed straight into the dispatch,
//	    bypassing the record) detectably leaks worker content into the
//	    created item; the record-driven dispatch cannot — with no handoff
//	    rows in the record, the dispatched item provably carries none of B's
//	    content.
//	D2  coordinator defines + may override (P2): the dispatched item is
//	    authored by the coordinator (created_by = squad-lead principal) and
//	    B's recommended_next is advisory — the coordinator's override wins
//	    and is audited. Teeth: the naive auto-execute arm (the recommendation
//	    becomes the item under B's authorship) is caught by the created_by
//	    assertion.
//	D3  no custody transfer (P3): the dispatched item starts with a FRESH
//	    unheld F3-provisioned claim at fence 0; the next worker acquires it
//	    via the §6.2 claim with its own fence bump. The completing run's
//	    claim row is never rewritten, and a zombie re-acquire by B over C's
//	    live lease is rejected. Teeth: the naive custody handoff (copy B's
//	    claim onto the dispatched item so C "inherits" it) is caught by the
//	    fresh-claim scan.
//	D4  idempotent dispatch (§6.4): M concurrent coordinators dispatching the
//	    same (source, completing run) converge on ONE created item via the
//	    coord.dispatch marker. Teeth: the naive no-marker arm fans out M
//	    duplicate items.
package coord_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/K8squad/K8squad/pkg/coord"
)

// dispatchCoordinator / dispatchWorkerB / dispatchWorkerC are the three
// principals of the §2.9 loop: the squad-lead coordinator (Role CRD admitted,
// pre-authorized at admission — the spine records, not checks, the role) and
// the two runs whose custody must NEVER connect (B completed the source; C
// works the dispatched item).
const (
	dispatchCoordinator = "principal:squad-lead"
	dispatchWorkerB     = "principal:worker-b"
	dispatchWorkerC     = "principal:worker-c"
	dispatchProject     = "22222222-2222-2222-2222-222222222222"
	dispatchRunB        = "00000000-0000-0000-0000-0000000000bb"
	dispatchRunC        = "00000000-0000-0000-0000-0000000000cc"
)

// resetProdDispatchSchema re-applies BOTH shipped migrations (0001 spine +
// 0002 dispatch marker) into a clean coord schema — the teeth bite the real
// DDL exactly as the apiserver migration runner provisions it.
func resetProdDispatchSchema(t *testing.T, db *sql.DB) {
	t.Helper()
	mustExec(t, db, `DROP SCHEMA IF EXISTS coord CASCADE`)
	mustExec(t, db, coordMigrationSQL(t))
	mustExec(t, db, dispatchMigrationSQL(t))
}

// dispatchMigrationSQL locates the shipped 0002 migration, mirroring
// coordMigrationSQL's candidate paths.
func dispatchMigrationSQL(t *testing.T) string {
	t.Helper()
	candidates := []string{
		"../../db/migrations/0002_coord_dispatch.sql",
		"db/migrations/0002_coord_dispatch.sql",
	}
	if dir := os.Getenv("COORD_MIGRATIONS_DIR"); dir != "" {
		candidates = append([]string{filepath.Join(dir, "0002_coord_dispatch.sql")}, candidates...)
	}
	for _, p := range candidates {
		if b, err := os.ReadFile(p); err == nil {
			return string(b)
		}
	}
	t.Fatalf("ISI-2526: cannot locate 0002_coord_dispatch.sql (looked in %v). "+
		"Refusing to pass without the shipped dispatch-marker schema.", candidates)
	return ""
}

// seedCompletedSource creates ONE todo item, has worker B claim it via the
// production §6.2 claim (AcquireSpecific — what 2.9 rides on), then completes
// it: the item advances to done with B still the recorded holder of its claim
// row at the fence B acquired. Optionally seeds the structured 2.8 handoff
// artifact (kind "handoff", content served by the injected resolver) and a
// record comment.
//
// NB: the lane advance to done uses the guarded test double of the §6.3
// complete — the production Complete binding is story 2.4's follow-up; for
// THIS story the completed source is a precondition, not the SUT.
func seedCompletedSource(t *testing.T, db *sql.DB, handoffJSON, comment string) (sourceID string, fenceB int64) {
	t.Helper()
	ctx := context.Background()
	mustExec(t, db, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		VALUES ('`+dispatchProject+`', 'source dependency', 'todo', 'principal:board')`)
	mustQueryRow(t, db,
		`SELECT id::text FROM coord.work_item WHERE title = 'source dependency'`).Scan(&sourceID)

	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	gotID, fence, ok, err := pc.AcquireSpecific(ctx, dispatchWorkerB, dispatchRunB, sourceID, "")
	if err != nil || !ok || gotID != sourceID {
		t.Fatalf("worker B claim: id=%s ok=%v err=%v — the §2.9 loop rides the §6.2 claim", gotID, ok, err)
	}
	fenceB = fence

	if comment != "" {
		mustExec(t, db, `
			INSERT INTO coord.comment (work_item_id, author_principal, body)
			VALUES ($1::uuid, $2, $3)`, sourceID, dispatchWorkerB, comment)
	}
	if handoffJSON != "" {
		mustExec(t, db, `
			INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256)
			VALUES ($1::uuid, $2::uuid, 'handoff', 'test://handoff/b', 'deadbeef')`,
			sourceID, dispatchRunB)
	}
	mustExec(t, db,
		`UPDATE coord.work_item SET state = 'done' WHERE id = $1::uuid AND state = 'in_progress'`, sourceID)
	return sourceID, fenceB
}

func TestSpineProdDispatch(t *testing.T) {
	dsn := dsnOrFatal(t)
	t.Run("D1_no_peer_channel_record_is_the_only_carrier", func(t *testing.T) { dispatchD1NoPeerChannel(t, dsn) })
	t.Run("D2_coordinator_defines_and_overrides_advisory", func(t *testing.T) { dispatchD2CoordinatorDefines(t, dsn) })
	t.Run("D3_no_custody_transfer_fresh_fenced_claim", func(t *testing.T) { dispatchD3NoCustodyTransfer(t, dsn) })
	t.Run("D4_idempotent_dispatch_converges", func(t *testing.T) { dispatchD4Idempotent(t, dsn) })
}

// --------------------------- D1 ---------------------------------------------
// P1: the coordinator's knowledge of B's outcome is the record, nothing else.
// With a handoff artifact IN the record, ReadHandoff surfaces it; with NONE,
// the dispatched item provably carries zero B-authored content. Teeth: the
// naive side-channel arm (B's findings passed straight into the dispatch)
// DOES leak B's marker string into the created item — the assertion that
// catches it is the one the production arm passes.
func dispatchD1NoPeerChannel(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()
	const bFindingsMarker = "B-SECRET-FINDING-must-not-teleport"

	// -- legitimate half: the record DOES carry the handoff, ReadHandoff
	//    surfaces it, and the coordinator may quote it (its choice, P2).
	resetProdDispatchSchema(t, db)
	handoff := `{"findings":"` + bFindingsMarker + `","recommended_next":[{"title":"B wants X"}],"artifacts_for_downstream":[]}`
	sourceID, _ := seedCompletedSource(t, db, handoff, "B finished the dependency")
	disp, err := coord.NewProdDispatcher(db, func(ctx context.Context, uri string) ([]byte, error) {
		return []byte(handoff), nil
	})
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	view, err := disp.ReadHandoff(ctx, sourceID)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if view.Handoff == nil || view.Handoff.Findings != bFindingsMarker || len(view.Handoff.RecommendedNext) != 1 {
		t.Fatalf("ReadHandoff did not surface the record's structured handoff: %+v", view.Handoff)
	}
	if len(view.Comments) != 1 || view.Comments[0].Author != dispatchWorkerB {
		t.Fatalf("ReadHandoff did not surface the record's comments: %+v", view.Comments)
	}

	// -- P1 half: NO handoff rows in the record at all. B's findings exist
	//    only in the naive side channel (a Go value B "sends" to A). The
	//    production dispatch API has no parameter that can carry them.
	resetProdDispatchSchema(t, db)
	sourceID, _ = seedCompletedSource(t, db, "", "") // record carries nothing of B
	type sideChannel struct{ findings string }
	naiveLeak := sideChannel{findings: bFindingsMarker} // what the naive design would consume

	disp, err = coord.NewProdDispatcher(db, nil)
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	view, err = disp.ReadHandoff(ctx, sourceID)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}
	if view.Handoff != nil || len(view.Comments) != 0 {
		t.Fatalf("empty record must yield an empty view — got %+v", view)
	}

	// Teeth: the naive arm — dispatch built FROM the side channel — leaks the
	// marker into the created item.
	naiveTitle := "next: " + naiveLeak.findings
	var leaked string
	mustQueryRow(t, db, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		VALUES ('`+dispatchProject+`', $1, 'todo', $2) RETURNING title`,
		naiveTitle, dispatchCoordinator).Scan(&leaked)
	if !strings.Contains(leaked, bFindingsMarker) {
		t.Fatal("FALSIFICATION LOST ITS TEETH: the side-channel arm did not leak B's findings " +
			"into the dispatched item, so the no-leak assertion below proves nothing")
	}

	// The production arm: the coordinator authors its own item from an empty
	// view; B's marker is structurally absent.
	res, err := disp.DispatchNextOfRecord(ctx, coord.DispatchDecision{
		CoordinatorPrincipal: dispatchCoordinator,
		SourceWorkItemID:     sourceID,
		SourceRunID:          dispatchRunB,
		Title:                "next: integrate the completed dependency",
		Body:                 "coordinator-authored from the (empty) record view",
	})
	if err != nil {
		t.Fatalf("DispatchNextOfRecord: %v", err)
	}
	var body string
	mustQueryRow(t, db, `SELECT body FROM coord.work_item WHERE id = $1::uuid`,
		res.CreatedWorkItemID).Scan(&body)
	if strings.Contains(body, bFindingsMarker) {
		t.Fatalf("P1 VIOLATED: dispatched item carries B's out-of-record content (%q)", body)
	}
}

// --------------------------- D2 ---------------------------------------------
// P2: the dispatched item is the coordinator's definition — created_by is the
// squad-lead principal, and B's recommended_next is advisory the coordinator
// may override. Teeth: the naive auto-execute arm — the recommendation IS the
// item, dispatched under B's authorship — must be caught by the created_by
// assertion below (it is the exact shape that assertion rejects).
func dispatchD2CoordinatorDefines(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()

	resetProdDispatchSchema(t, db)
	handoff := `{"findings":"done","recommended_next":[{"title":"B recommends X"}],"artifacts_for_downstream":[]}`
	sourceID, _ := seedCompletedSource(t, db, handoff, "handoff comment")
	disp, err := coord.NewProdDispatcher(db, func(ctx context.Context, uri string) ([]byte, error) {
		return []byte(handoff), nil
	})
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	view, err := disp.ReadHandoff(ctx, sourceID)
	if err != nil {
		t.Fatalf("ReadHandoff: %v", err)
	}

	// Teeth: naive auto-execute — B's recommendation dispatched under B's
	// authorship. The property assertions on the production arm must reject
	// exactly this shape.
	drafts := coord.AdoptRecommendation(view)
	if len(drafts) == 0 {
		t.Fatal("cannot build teeth: advisory recommendation missing from the view")
	}
	var naiveCreator, naiveTitle string
	mustQueryRow(t, db, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		VALUES ($1, $2, 'todo', $3) RETURNING created_by, title`,
		dispatchProject, drafts[0].Title, dispatchWorkerB).Scan(&naiveCreator, &naiveTitle)
	if naiveCreator == dispatchCoordinator || naiveTitle != "B recommends X" {
		t.Fatal("FALSIFICATION LOST ITS TEETH: the auto-execute arm did not produce a " +
			"worker-authored recommendation item, so the created_by assertion proves nothing")
	}

	// The production arm: the coordinator OVERRIDES B's "X" with its own
	// prioritized "Y", and the override is audited.
	res, err := disp.DispatchNextOfRecord(ctx, coord.DispatchDecision{
		CoordinatorPrincipal: dispatchCoordinator,
		SourceWorkItemID:     sourceID,
		SourceRunID:          dispatchRunB,
		Title:                "coordinator prioritizes Y over B's X",
		Body:                 "adopted=none, overridden",
		AdvisoryFollowed:     false,
	})
	if err != nil {
		t.Fatalf("DispatchNextOfRecord: %v", err)
	}

	var creator, title string
	mustQueryRow(t, db, `SELECT created_by, title FROM coord.work_item WHERE id = $1::uuid`,
		res.CreatedWorkItemID).Scan(&creator, &title)
	if creator != dispatchCoordinator {
		t.Fatalf("P2 VIOLATED (this is the naive arm's failure): dispatched item created_by=%q, "+
			"want the coordinator %q — B must never author the next item", creator, dispatchCoordinator)
	}
	if strings.Contains(title, "B recommends") {
		t.Fatalf("P2 VIOLATED: dispatched title %q carries B's recommendation verbatim — the "+
			"recommendation is advisory and must not execute itself", title)
	}

	// The override provenance is in the §6.5 audit row.
	var audits int
	mustQueryRow(t, db, `
		SELECT count(*) FROM coord.audit_log
		 WHERE event_type = 'coordinator_dispatched' AND principal = $1
		   AND payload->>'advisory_followed' = 'false'
		   AND payload->>'created_work_item_id' = $2`,
		dispatchCoordinator, res.CreatedWorkItemID).Scan(&audits)
	if audits != 1 {
		t.Fatalf("expected exactly one coordinator_dispatched audit row with the override "+
			"provenance, found %d", audits)
	}
}

// --------------------------- D3 ---------------------------------------------
// P3: no custody transfer. The dispatched item's claim row is FRESH (unheld,
// fence 0) until worker C acquires it via the §6.2 claim — C's fence is its
// own bump, B's claim row on the source is never rewritten, and B's zombie
// re-acquire over C's live lease is rejected. Teeth: the naive "helpful" arm
// copies B's claim onto a dispatched item so C "inherits" B's custody — the
// fresh-claim scan must catch that shape.
func dispatchD3NoCustodyTransfer(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()

	resetProdDispatchSchema(t, db)
	sourceID, fenceB := seedCompletedSource(t, db, `{"findings":"done","recommended_next":[]}`, "")

	// Teeth: the naive custody handoff — B's (holder, run, fence, lease)
	// copied onto a dispatched item — must be catchable by the fresh-claim
	// scan below (the exact shape the scan rejects).
	var naiveID string
	mustQueryRow(t, db, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		VALUES ($1, 'naive-inherit-arm', 'todo', $2) RETURNING id::text`,
		dispatchProject, dispatchCoordinator).Scan(&naiveID)
	mustExec(t, db, `
		UPDATE coord.claim
		   SET holder_principal = $1, run_id = $2::uuid, fence_token = $3,
		       lease_expires_at = clock_timestamp() + interval '30 seconds'
		 WHERE work_item_id = $4::uuid`,
		dispatchWorkerB, dispatchRunB, fenceB, naiveID)
	if err := checkFreshUnheldClaim(db, naiveID); err == nil {
		t.Fatal("FALSIFICATION LOST ITS TEETH: the naive inherit-arm produced a claim the " +
			"fresh-claim scan accepts, so the scan proves nothing")
	}

	// The production arm: dispatch, then the fresh-claim invariant holds.
	disp, err := coord.NewProdDispatcher(db, nil)
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	res, err := disp.DispatchNextOfRecord(ctx, coord.DispatchDecision{
		CoordinatorPrincipal: dispatchCoordinator,
		SourceWorkItemID:     sourceID,
		SourceRunID:          dispatchRunB,
		Title:                "downstream of the completed dependency",
	})
	if err != nil {
		t.Fatalf("DispatchNextOfRecord: %v", err)
	}
	if err := checkFreshUnheldClaim(db, res.CreatedWorkItemID); err != nil {
		t.Fatalf("P3 VIOLATED: %v", err)
	}

	// C claims the DISPATCHED item via the production §6.2 claim — a fresh
	// acquire, never an inherited lease.
	pc, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	cItem, fenceC, ok, err := pc.AcquireSpecific(ctx, dispatchWorkerC, dispatchRunC, res.CreatedWorkItemID, "")
	if err != nil || !ok {
		t.Fatalf("worker C fresh claim: ok=%v err=%v", ok, err)
	}
	if cItem != res.CreatedWorkItemID {
		t.Fatalf("C claimed %s, want the dispatched item %s", cItem, res.CreatedWorkItemID)
	}
	if fenceC != 1 {
		t.Fatalf("C's fence = %d, want 1 (fresh provisioned 0 + C's own bump) — C must never "+
			"inherit B's fence %d", fenceC, fenceB)
	}

	// B's claim row on the source is untouched: still names B, at B's fence.
	var srcHolder string
	var srcRun string
	var srcFence int64
	mustQueryRow(t, db, `
		SELECT c.holder_principal, c.run_id::text, c.fence_token
		  FROM coord.claim c JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE w.id = $1::uuid`, sourceID).Scan(&srcHolder, &srcRun, &srcFence)
	if srcHolder != dispatchWorkerB || srcRun != dispatchRunB || srcFence != fenceB {
		t.Fatalf("P3 VIOLATED: source claim row rewritten by the dispatch/C's claim — "+
			"(%q,%q,fence %d), want B (%q,%q,fence %d)",
			srcHolder, srcRun, srcFence, dispatchWorkerB, dispatchRunB, fenceB)
	}

	// And a zombie B write — B re-acquiring the DISPATCHED item while C holds
	// a live lease — is rejected by the §6.2 free-or-expired guard.
	if _, _, ok, err := pc.AcquireSpecific(ctx, dispatchWorkerB, dispatchRunB, res.CreatedWorkItemID, ""); err == nil && ok {
		t.Fatal("P3 VIOLATED: B re-acquired the dispatched item over C's live lease — " +
			"the free-or-expired guard did not bite")
	}
}

// checkFreshUnheldClaim asserts the P3 creation invariant for a dispatched
// item: exactly one claim row, unheld, fence 0 (F3 provision), lane todo.
func checkFreshUnheldClaim(db *sql.DB, itemID string) error {
	var state, holder sql.NullString
	var fence int64
	var claims int
	row := db.QueryRow(`
		SELECT w.state, c.holder_principal, c.fence_token,
		       (SELECT count(*) FROM coord.claim cc WHERE cc.work_item_id = w.id)
		  FROM coord.work_item w JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.id = $1::uuid`, itemID)
	if err := row.Scan(&state, &holder, &fence, &claims); err != nil {
		return err
	}
	if claims != 1 || holder.Valid || fence != 0 || state.String != "todo" {
		return fmt.Errorf("dispatched item %s is not FRESH (claims=%d holder=%v fence=%d lane=%s) — "+
			"custody must start at an unheld fence-0 provision, never be inherited",
			itemID, claims, holder, fence, state.String)
	}
	return nil
}

// --------------------------- D4 ---------------------------------------------
// §6.4 idempotency: M concurrent coordinators dispatching the same completed
// (source, run) converge on ONE created item. Teeth: without the marker, the
// same fan-out creates M duplicates.
func dispatchD4Idempotent(t *testing.T, dsn string) {
	db := openDB(t, dsn)
	ctx := context.Background()
	const coordinators = 16

	// Teeth — the naive no-marker fan-out duplicates the next item.
	resetProdDispatchSchema(t, db)
	sourceID, _ := seedCompletedSource(t, db, "", "")
	var before int
	mustQueryRow(t, db, `SELECT count(*) FROM coord.work_item`).Scan(&before)
	for i := 0; i < coordinators; i++ {
		mustExec(t, db, `
			INSERT INTO coord.work_item (project_id, title, state, created_by)
			VALUES ($1, 'naive next', 'todo', $2)`, dispatchProject, dispatchCoordinator)
	}
	var after int
	mustQueryRow(t, db, `SELECT count(*) FROM coord.work_item`).Scan(&after)
	if after-before != coordinators {
		t.Fatal("FALSIFICATION LOST ITS TEETH: the naive no-marker fan-out did not create one " +
			"item per coordinator, so the convergence assertion proves nothing")
	}

	// The production arm: every concurrent dispatcher gets the SAME item id,
	// exactly one item is created, and re-entry is a no-op returning it.
	resetProdDispatchSchema(t, db)
	sourceID, _ = seedCompletedSource(t, db, "", "")
	disp, err := coord.NewProdDispatcher(db, nil)
	if err != nil {
		t.Fatalf("NewProdDispatcher: %v", err)
	}
	mustQueryRow(t, db, `SELECT count(*) FROM coord.work_item`).Scan(&before)

	ids := make(chan string, coordinators)
	fresh := make(chan bool, coordinators)
	errs := make(chan error, coordinators)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < coordinators; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			res, err := disp.DispatchNextOfRecord(ctx, coord.DispatchDecision{
				CoordinatorPrincipal: dispatchCoordinator,
				SourceWorkItemID:     sourceID,
				SourceRunID:          dispatchRunB,
				Title:                "the one next item",
			})
			if err != nil {
				errs <- err
				return
			}
			ids <- res.CreatedWorkItemID
			fresh <- !res.AlreadyDispatched
		}()
	}
	close(start)
	wg.Wait()
	close(errs)
	close(ids)
	close(fresh)
	for err := range errs {
		t.Fatalf("concurrent DispatchNextOfRecord: %v", err)
	}
	first, gotAny := "", false
	creations := 0
	for id := range ids {
		if !gotAny {
			first, gotAny = id, true
		} else if id != first {
			t.Fatalf("D4 VIOLATED: concurrent dispatchers created different items (%s vs %s)", id, first)
		}
	}
	for f := range fresh {
		if f {
			creations++
		}
	}
	if !gotAny || creations != 1 {
		t.Fatalf("D4 VIOLATED: %d dispatchers reported a fresh creation (any item: %v), want exactly 1",
			creations, gotAny)
	}
	var total int
	mustQueryRow(t, db, `SELECT count(*) FROM coord.work_item`).Scan(&total)
	if total-before != 1 {
		t.Fatalf("D4 VIOLATED: %d items created for one (source, run) dispatch, want exactly 1",
			total-before)
	}

	// Sequential re-entry converges on the same item, reporting the dedupe.
	res, err := disp.DispatchNextOfRecord(ctx, coord.DispatchDecision{
		CoordinatorPrincipal: dispatchCoordinator,
		SourceWorkItemID:     sourceID,
		SourceRunID:          dispatchRunB,
		Title:                "re-driven decision — must not create a second item",
	})
	if err != nil {
		t.Fatalf("re-drive DispatchNextOfRecord: %v", err)
	}
	if !res.AlreadyDispatched || res.CreatedWorkItemID != first {
		t.Fatalf("D4 VIOLATED: re-drive returned (%s, already=%v), want (%s, already=true)",
			res.CreatedWorkItemID, res.AlreadyDispatched, first)
	}
	var markers, audits int
	mustQueryRow(t, db, `SELECT count(*) FROM coord.dispatch`).Scan(&markers)
	mustQueryRow(t, db, `
		SELECT count(*) FROM coord.audit_log WHERE event_type = 'coordinator_dispatched'`).Scan(&audits)
	if markers != 1 || audits != 1 {
		t.Fatalf("D4 VIOLATED: %d markers / %d dispatch audits, want exactly 1 and 1", markers, audits)
	}
}
