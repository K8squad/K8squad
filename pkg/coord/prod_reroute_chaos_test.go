//go:build chaos

// prod_reroute_chaos_test.go — the Story 2.10 / ISI-2882 rate-limit re-route
// chaos gate, executed by the same spine-chaos workflow run as TestSpine (the
// workflow's -run 'TestSpine' filter matches TestSpineReroute unanchored).
// Every case runs against a REAL Postgres with -race on, carrying the SHIPPED
// schema (0001 + 0009_rate_limit_reroute; + 0003 for the outbox case).
//
// The story's [no-P2P guardrail] proven here: a re-route is fenced
// RELEASE → RE-DISPATCH → CLAIM, never a lease handoff —
//
//	T1 release/re-dispatch/hold     one transaction: holder cleared, fence
//	                                 bumped, item back in todo, hold + audit
//	T2 zombie fenced out            the original holder cannot act after the
//	                                 release (2.8 custody-gated write refused)
//	T3 different credential claims  the §6.2 pop delivers the item to a claim
//	                                 presenting a DIFFERENT credential (7.6)
//	T4 same credential refused      both claim paths reject the throttled
//	                                 credential while the hold is live
//	T5 window clears → hold inert   past resume_at the SAME credential claims
//	                                 again (3.7 auto-resume branch)
//	T6 idempotent re-release        a re-drive converges (AlreadyReleased),
//	                                 no second audit row, no fence change
//	T7 custody guard                a release for a holder/run the claim row
//	                                 does not name is refused, record intact
//	T8 outbox co-commit             the 12.1 event rides the release txn (and
//	                                 a refused release emits nothing)
//	T9 unheld claim refused         a release against an UNHELD claim row is
//	                                 ErrNotRerouteCustodian, not an infra error (F1)
//	T10 no head-of-line block       a held item at the FIFO head is skipped, the
//	                                 credentialed pop returns the next unheld item
//	T11 blind claim bypasses hold   ClaimNext (credential-blind) re-claims a held
//	                                 item by design — the F2 opt-in-guard decision
package coord_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
)

// The fixture identities: the paused Run (holder) on credential A, the
// coordinator, and the other Agent (claimant) on credential B.
const (
	rrHolder  = "principal:agent-a"
	rrRun     = "33333333-3333-3333-3333-333333333333"
	rrCoord   = "principal:coord"
	rrCredA   = "secret:squad/cred-a"
	rrCredB   = "secret:squad/cred-b"
	rrClaimer = "principal:agent-b"
	rrRunB    = "44444444-4444-4444-4444-444444444444"
)

// seedRerouteSUT resets the coord schema (0001 [+0003 when outbox] + 0009),
// seeds ONE in_progress item whose claim is held by the paused Run at fence 7
// with a live lease (the 3.7 short-pause state: checkout retained, heartbeat
// current), and returns the db handle + item uuid.
func seedRerouteSUT(t *testing.T, ctx context.Context, dsn string, withOutbox bool) (*sql.DB, string) {
	t.Helper()
	db := openDB(t, dsn)
	if _, err := db.ExecContext(ctx, `DROP SCHEMA IF EXISTS coord CASCADE`); err != nil {
		t.Fatalf("reset coord schema: %v", err)
	}
	migrations := []string{"0001_coord_schema.sql"}
	if withOutbox {
		migrations = append(migrations, "0003_coord_outbox.sql")
	}
	migrations = append(migrations, "0009_rate_limit_reroute.sql")
	for _, name := range migrations {
		if _, err := db.ExecContext(ctx, migrationFile(t, name)); err != nil {
			t.Fatalf("apply %s: %v", name, err)
		}
	}
	var wi string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		     VALUES (gen_random_uuid(), 'reroute gate item', 'todo', 'principal:test')
		  RETURNING id`).Scan(&wi); err != nil {
		t.Fatalf("seed work_item: %v", err)
	}
	claim := coord.DefaultProdConfig()
	claimer, err := coord.NewProdClaimer(db, claim)
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	item, fence, ok, err := claimer.ClaimNext(ctx, rrHolder, rrRun, "")
	if err != nil || !ok || item != wi || fence != 1 {
		t.Fatalf("seed claim: item=%s fence=%d ok=%v err=%v (want %s, 1, true, nil)", item, fence, ok, err, wi)
	}
	return db, wi
}

// holdWindow is the fixture Retry-After window the hold expires at.
func holdWindow() time.Time { return time.Now().Add(5 * time.Minute) }

func TestSpineReroute(t *testing.T) {
	dsn := dsnOrFatal(t)
	ctx := context.Background()

	t.Run("T1_release_redispatch_hold", func(t *testing.T) { rerouteT1Release(t, ctx, dsn) })
	t.Run("T2_zombie_fenced_out", func(t *testing.T) { rerouteT2Zombie(t, ctx, dsn) })
	t.Run("T3_different_credential_claims", func(t *testing.T) { rerouteT3DifferentCred(t, ctx, dsn) })
	t.Run("T4_same_credential_refused", func(t *testing.T) { rerouteT4SameCred(t, ctx, dsn) })
	t.Run("T5_window_clears_hold_inert", func(t *testing.T) { rerouteT5WindowClears(t, ctx, dsn) })
	t.Run("T6_idempotent_rerelease", func(t *testing.T) { rerouteT6Idempotent(t, ctx, dsn) })
	t.Run("T7_custody_guard", func(t *testing.T) { rerouteT7Custody(t, ctx, dsn) })
	t.Run("T8_outbox_cocommit", func(t *testing.T) { rerouteT8Outbox(t, ctx, dsn) })
	// ISI-3083 chaos-gate gaps closed with the Cursor-review fixes.
	t.Run("T9_unheld_claim_refused", func(t *testing.T) { rerouteT9UnheldRefused(t, ctx, dsn) })
	t.Run("T10_no_head_of_line_block", func(t *testing.T) { rerouteT10NoHeadOfLine(t, ctx, dsn) })
	t.Run("T11_blind_claim_bypasses_hold", func(t *testing.T) { rerouteT11BlindBypass(t, ctx, dsn) })
}

// rerouteRelease drives the SUT release (attempt 2 = the policy's repeat
// escalation, window 5m out) and returns the result.
func rerouteRelease(t *testing.T, ctx context.Context, db *sql.DB, wi string, opts ...coord.ProdRerouteOption) coord.RerouteReleaseResult {
	t.Helper()
	s, err := coord.NewProdRerouteStore(db, opts...)
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	res, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRun, rrCredA, 2, holdWindow(), rrCoord)
	if err != nil {
		t.Fatalf("ReleaseForReroute: %v", err)
	}
	if res.AlreadyReleased {
		t.Fatal("first release reported AlreadyReleased")
	}
	return res
}

// rerouteClaimState reads the claim row + item lane back for assertions.
func rerouteClaimState(t *testing.T, ctx context.Context, db *sql.DB, wi string) (holder, state sql.NullString, fence int64) {
	t.Helper()
	err := db.QueryRowContext(ctx, `
		SELECT c.holder_principal, w.state, c.fence_token
		  FROM coord.claim c JOIN coord.work_item w ON w.id = c.work_item_id
		 WHERE c.work_item_id = $1::uuid`, wi).Scan(&holder, &state, &fence)
	if err != nil {
		t.Fatalf("read claim state: %v", err)
	}
	return holder, state, fence
}

func rerouteT1Release(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	res := rerouteRelease(t, ctx, db, wi)

	if res.Fence != 2 {
		t.Fatalf("released fence = %d, want 2 (claim acquired at 1, release bumps)", res.Fence)
	}
	holder, state, fence := rerouteClaimState(t, ctx, db, wi)
	if holder.Valid {
		t.Fatalf("holder = %q after release, want NULL (checkout released)", holder.String)
	}
	if state.String != "todo" {
		t.Fatalf("item state = %q, want todo (coordinator re-dispatch)", state.String)
	}
	if fence != 2 {
		t.Fatalf("claim fence = %d, want 2", fence)
	}

	// The hold row: names the throttled credential, the escalation attempt,
	// the window, the released run and the deciding coordinator.
	var cred string
	var attempt int
	var releasedRun, coordinator string
	if err := db.QueryRowContext(ctx, `
		SELECT throttled_credential, attempt, released_run::text, coordinator
		  FROM coord.rate_limit_reroute WHERE work_item_id = $1::uuid`, wi,
	).Scan(&cred, &attempt, &releasedRun, &coordinator); err != nil {
		t.Fatalf("read hold: %v", err)
	}
	if cred != rrCredA || attempt != 2 || releasedRun != rrRun || coordinator != rrCoord {
		t.Fatalf("hold = (%q, %d, %s, %s), want (%q, 2, %s, %s)",
			cred, attempt, releasedRun, coordinator, rrCredA, rrRun, rrCoord)
	}

	// The §6.5 audit row: claim_released, in_progress → todo, coordinator-driven.
	var eventType, principal, fromState, toState string
	var payload string
	if err := db.QueryRowContext(ctx, `
		SELECT event_type, principal, from_state, to_state, payload::text
		  FROM coord.audit_log
		 WHERE work_item_id = $1::uuid AND event_type = 'claim_released'`, wi,
	).Scan(&eventType, &principal, &fromState, &toState, &payload); err != nil {
		t.Fatalf("read audit: %v", err)
	}
	if principal != rrCoord || fromState != "in_progress" || toState != "todo" {
		t.Fatalf("audit = (%s, %s, %s→%s), want coordinator-driven in_progress→todo",
			eventType, principal, fromState, toState)
	}
	for _, want := range []string{`"rate_limited_reroute"`, `"throttled_credential"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("audit payload %q missing %s", payload, want)
		}
	}
}

func rerouteT2Zombie(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	rerouteRelease(t, ctx, db, wi)

	// The original holder's custody-gated write (2.8 handoff — same principal,
	// same run, SAME FENCE as the released episode) must be refused: the fence
	// advanced and the holder is gone. The zombie cannot resume-clobber.
	w, err := coord.NewProdHandoffWriter(db)
	if err != nil {
		t.Fatalf("NewProdHandoffWriter: %v", err)
	}
	_, err = w.WriteHandoff(ctx, wi, rrHolder, rrRun, "", 1, coord.HandoffDoc{Did: []string{"zombie"}})
	if !errors.Is(err, coord.ErrNotHandoffCustodian) {
		t.Fatalf("zombie handoff after release: err=%v, want ErrNotHandoffCustodian", err)
	}
}

func rerouteT3DifferentCred(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	rerouteRelease(t, ctx, db, wi)

	claim := coord.DefaultProdConfig()
	claimer, err := coord.NewProdClaimer(db, claim)
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	// The other Agent pops the re-dispatched item presenting ITS OWN
	// credential — different from the throttled one.
	item, fence, ok, err := claimer.ClaimNextCredentialed(ctx, rrClaimer, rrRunB, rrCredB, "")
	if err != nil || !ok {
		t.Fatalf("different-credential claim: item=%s ok=%v err=%v, want ok", item, ok, err)
	}
	if item != wi {
		t.Fatalf("claimed item = %s, want the re-routed item %s", item, wi)
	}
	if fence != 3 {
		t.Fatalf("re-claim fence = %d, want 3 (1 acquire + 2 release + 1 re-acquire)", fence)
	}
	holder, state, fence := rerouteClaimState(t, ctx, db, wi)
	if !holder.Valid || holder.String != rrClaimer || state.String != "in_progress" || fence != 3 {
		t.Fatalf("post-claim state = (%q, %s, %d), want (%s, in_progress, 3)",
			holder.String, state.String, fence, rrClaimer)
	}
}

func rerouteT4SameCred(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	rerouteRelease(t, ctx, db, wi)

	claim := coord.DefaultProdConfig()
	claimer, err := coord.NewProdClaimer(db, claim)
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}

	// Pop path: the throttled credential is INVISIBLE to the credentialed pop —
	// the item stays claimable but not for THIS credential.
	if _, _, ok, err := claimer.ClaimNextCredentialed(ctx, rrHolder, rrRun, rrCredA, ""); err != nil || ok {
		t.Fatalf("throttled-credential pop: ok=%v err=%v, want ok=false (hold live)", ok, err)
	}
	if n, err := claimer.ClaimableCount(ctx); err != nil || n != 1 {
		t.Fatalf("claimable count = %d err=%v, want 1 (item NOT consumed, just guarded)", n, err)
	}

	// Directed path: the specific acquire is refused for the throttled
	// credential, accepted for a different one.
	if _, _, ok, err := claimer.AcquireSpecificCredentialed(ctx, rrHolder, rrRun, wi, rrCredA, ""); err != nil || ok {
		t.Fatalf("throttled-credential specific acquire: ok=%v err=%v, want ok=false", ok, err)
	}
	if _, f, ok, err := claimer.AcquireSpecificCredentialed(ctx, rrClaimer, rrRunB, wi, rrCredB, ""); err != nil || !ok || f != 3 {
		t.Fatalf("different-credential specific acquire: fence=%d ok=%v err=%v, want 3/true/nil", f, ok, err)
	}
}

func rerouteT5WindowClears(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	// Release with the window ALREADY CLEARED (the 3.7 auto-resume instant):
	// the hold is inert by predicate, so even the SAME credential may claim —
	// "the item stays paused until the Retry-After window clears", then normal
	// §6.2 custody resumes.
	s, err := coord.NewProdRerouteStore(db)
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	if _, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRun, rrCredA, 2, time.Now().Add(-time.Second), rrCoord); err != nil {
		t.Fatalf("ReleaseForReroute (window cleared): %v", err)
	}

	claim := coord.DefaultProdConfig()
	claimer, err := coord.NewProdClaimer(db, claim)
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	if _, _, ok, err := claimer.ClaimNextCredentialed(ctx, rrHolder, rrRun, rrCredA, ""); err != nil || !ok {
		t.Fatalf("same-credential claim after window clear: ok=%v err=%v, want ok (hold inert)", ok, err)
	}
}

func rerouteT6Idempotent(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	first := rerouteRelease(t, ctx, db, wi)

	// A re-drive (crash after commit, coordinator retry) converges on the
	// recorded outcome: AlreadyReleased, no state change, no second audit row.
	s, err := coord.NewProdRerouteStore(db)
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	second, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRun, rrCredA, 2, holdWindow(), rrCoord)
	if err != nil {
		t.Fatalf("re-release: %v", err)
	}
	if !second.AlreadyReleased || second.Fence != first.Fence {
		t.Fatalf("re-release = (%d, %v), want (%d, true) — convergent no-op", second.Fence, second.AlreadyReleased, first.Fence)
	}

	var audits int
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.audit_log
		 WHERE work_item_id = $1::uuid AND event_type = 'claim_released'`, wi,
	).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Fatalf("claim_released rows = %d, want exactly 1 after re-drive", audits)
	}
	_, state, fence := rerouteClaimState(t, ctx, db, wi)
	if state.String != "todo" || fence != first.Fence {
		t.Fatalf("post-re-drive state = (%s, %d), want (todo, %d) — untouched", state.String, fence, first.Fence)
	}
}

func rerouteT7Custody(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)

	s, err := coord.NewProdRerouteStore(db)
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	// Wrong run: the claim row does not name this (holder, run).
	if _, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRunB, rrCredA, 2, holdWindow(), rrCoord); !errors.Is(err, coord.ErrNotRerouteCustodian) {
		t.Fatalf("wrong-run release: err=%v, want ErrNotRerouteCustodian", err)
	}
	// Wrong holder.
	if _, err := s.ReleaseForReroute(ctx, wi, rrClaimer, rrRun, rrCredA, 2, holdWindow(), rrCoord); !errors.Is(err, coord.ErrNotRerouteCustodian) {
		t.Fatalf("wrong-holder release: err=%v, want ErrNotRerouteCustodian", err)
	}

	// The record is intact: still held, still in_progress, no hold row, no audit.
	holder, state, fence := rerouteClaimState(t, ctx, db, wi)
	if !holder.Valid || holder.String != rrHolder || state.String != "in_progress" || fence != 1 {
		t.Fatalf("record after refused releases = (%q, %s, %d), want untouched (%s, in_progress, 1)",
			holder.String, state.String, fence, rrHolder)
	}
	var holds, audits int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM coord.rate_limit_reroute WHERE work_item_id=$1::uuid`, wi).Scan(&holds); err != nil {
		t.Fatalf("count holds: %v", err)
	}
	if err := db.QueryRowContext(ctx, `
		SELECT count(*) FROM coord.audit_log
		 WHERE work_item_id=$1::uuid AND event_type='claim_released'`, wi).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if holds != 0 || audits != 0 {
		t.Fatalf("refused release wrote rows: holds=%d audits=%d, want 0/0", holds, audits)
	}

	// Argument validation fails closed without touching the record.
	if _, err := s.ReleaseForReroute(ctx, wi, "", rrRun, rrCredA, 2, holdWindow(), rrCoord); err == nil {
		t.Fatal("empty holder accepted, want rejection")
	}
	if _, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRun, rrCredA, 0, holdWindow(), rrCoord); err == nil {
		t.Fatal("attempt=0 accepted, want rejection")
	}
	if _, err := s.ReleaseForReroute(ctx, wi, rrHolder, rrRun, rrCredA, 2, time.Time{}, rrCoord); err == nil {
		t.Fatal("zero resumeAt accepted, want rejection")
	}
}

func rerouteT8Outbox(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, true)

	// A refused release (custody guard) must emit NO event.
	s, err := coord.NewProdRerouteStore(db, coord.WithRerouteOutboxCapture())
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	if _, err := s.ReleaseForReroute(ctx, wi, rrClaimer, rrRun, rrCredA, 2, holdWindow(), rrCoord); !errors.Is(err, coord.ErrNotRerouteCustodian) {
		t.Fatalf("wrong-holder release: err=%v, want ErrNotRerouteCustodian", err)
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM coord.outbox WHERE event_type='reroute_released'`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 0 {
		t.Fatalf("outbox rows after refused release = %d, want 0", n)
	}

	// The successful release co-commits exactly one event.
	rerouteRelease(t, ctx, db, wi, coord.WithRerouteOutboxCapture())
	if err := db.QueryRowContext(ctx,
		`SELECT count(*) FROM coord.outbox WHERE event_type='reroute_released'`).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	if n != 1 {
		t.Fatalf("outbox rows after release = %d, want exactly 1", n)
	}
}

// insertWorkItem seeds a bare work_item (its claim row is auto-provisioned,
// holder NULL) and returns its uuid.
func insertWorkItem(t *testing.T, ctx context.Context, db *sql.DB, title, state string) string {
	t.Helper()
	var id string
	if err := db.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, title, state, created_by)
		     VALUES (gen_random_uuid(), $1, $2, 'principal:test')
		  RETURNING id`, title, state).Scan(&id); err != nil {
		t.Fatalf("seed work_item %q: %v", title, err)
	}
	return id
}

// rerouteT9UnheldRefused pins F1 (ISI-3083): a release against an UNHELD claim
// row (holder_principal / run_id NULL — a never-claimed item here) is the most
// ordinary refusal and MUST return ErrNotRerouteCustodian, not the infra
// NULL-scan error the plain-string scan produced before the fix. That error
// made errors.Is(err, ErrNotRerouteCustodian) false, so a caller retrying infra
// failures would retry a permanent refusal forever.
func rerouteT9UnheldRefused(t *testing.T, ctx context.Context, dsn string) {
	db, _ := seedRerouteSUT(t, ctx, dsn, false)
	fresh := insertWorkItem(t, ctx, db, "never-claimed item", "todo")

	s, err := coord.NewProdRerouteStore(db)
	if err != nil {
		t.Fatalf("NewProdRerouteStore: %v", err)
	}
	if _, err := s.ReleaseForReroute(ctx, fresh, rrHolder, rrRun, rrCredA, 2, holdWindow(), rrCoord); !errors.Is(err, coord.ErrNotRerouteCustodian) {
		t.Fatalf("release against unheld claim row: err=%v, want ErrNotRerouteCustodian (F1)", err)
	}
}

// rerouteT10NoHeadOfLine pins the "no head-of-line block" property Cursor could
// not break but which the single-item fixture left untested: with the held,
// re-routed item at the HEAD of the FIFO, the credentialed pop for the throttled
// credential skips it (the hold's NOT EXISTS applies before LIMIT 1) and returns
// the next, unheld item instead of stalling the lane.
func rerouteT10NoHeadOfLine(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	rerouteRelease(t, ctx, db, wi) // wi → todo with a LIVE hold pinning rrCredA, at the FIFO head

	second := insertWorkItem(t, ctx, db, "second lane item", "todo")

	claimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	// Pop presenting the THROTTLED credential: the held head (wi) is invisible,
	// the unheld second item is claimed — no head-of-line block.
	item, _, ok, err := claimer.ClaimNextCredentialed(ctx, rrClaimer, rrRunB, rrCredA, "")
	if err != nil || !ok {
		t.Fatalf("credentialed pop past held head: item=%s ok=%v err=%v, want ok", item, ok, err)
	}
	if item != second {
		t.Fatalf("claimed %s, want the unheld second item %s (held head must be skipped, not block the lane)", item, second)
	}
}

// rerouteT11BlindBypass pins the F2 decision (ISI-3083): the credential-BLIND
// ClaimNext bypasses the re-route hold by design — it re-claims the re-routed
// item even for the throttled credential. Recording it as a chaos case makes the
// opt-in-per-call-site guard boundary intentional and test-visible: adding a
// guard to ClaimNext later would be a deliberate change that flips this case.
func rerouteT11BlindBypass(t *testing.T, ctx context.Context, dsn string) {
	db, wi := seedRerouteSUT(t, ctx, dsn, false)
	rerouteRelease(t, ctx, db, wi) // wi → todo, LIVE hold pins rrCredA

	claimer, err := coord.NewProdClaimer(db, coord.DefaultProdConfig())
	if err != nil {
		t.Fatalf("NewProdClaimer: %v", err)
	}
	item, _, ok, err := claimer.ClaimNext(ctx, rrHolder, rrRun, "")
	if err != nil || !ok {
		t.Fatalf("blind ClaimNext after re-route: item=%s ok=%v err=%v, want ok (hold bypassed by design)", item, ok, err)
	}
	if item != wi {
		t.Fatalf("blind claim got %s, want the re-routed item %s (blind pop ignores the hold)", item, wi)
	}
}
