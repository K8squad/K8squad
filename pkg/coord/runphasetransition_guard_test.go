// runphasetransition_guard_test.go — the sqlmock lane for the ISI-7541
// backward-edge guard: the acceptance pair from the issue, pinned row by row.
//
//   - a crash-path flap (backward/same-band edge, NO fence bump since the last
//     journaled edge) emits NO row — ErrRunPhaseEdgeSuppressed, no INSERT;
//   - a genuine retry-lap regression (the SAME backward edge, but the claim
//     fence WAS bumped) still journals, stamped with the certifying fence.
//
// Plus the fail-closed corners (unknown fence, NULL last-edge fence, missing
// work item ref) and the forward-edge passthrough. The two acceptance cases
// replay the live ISI-7538 sequence (build ddf159d6): journal held
// claiming→running at fence N; the crash-path re-drive regressed the projection
// under the SAME fence N; the run CR ended Pending while the journal's last row
// stayed running.
package coord

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	sqlmock "github.com/DATA-DOG/go-sqlmock"
)

const (
	guardRun  = "11111111-1111-1111-1111-111111111111"
	guardItem = "22222222-2222-2222-2222-222222222222"
	guardProj = "33333333-3333-3333-3333-333333333333"
)

func i64(v int64) *int64 { return &v }

// lastEdgeRows shapes the guard's last-journaled-edge read (to_phase, fence).
func lastEdgeRows(to string, fence *int64) *sqlmock.Rows {
	return sqlmock.NewRows([]string{"to_phase", "fence"}).AddRow(to, fence)
}

// expectLastEdge wires the journal's last-row read for guardRun.
func expectLastEdge(m sqlmock.Sqlmock, to string, fence *int64) {
	m.ExpectQuery("SELECT to_phase, fence FROM coord.run_phase_transition").
		WithArgs(guardRun).
		WillReturnRows(lastEdgeRows(to, fence))
}

// expectClaimFence wires the oracle read; fence=nil models "no claim row"
// (sql.ErrNoRows — the empty-rows result).
func expectClaimFence(m sqlmock.Sqlmock, fence *int64) {
	rows := sqlmock.NewRows([]string{"fence_token"})
	if fence != nil {
		rows.AddRow(*fence)
	}
	m.ExpectQuery("SELECT fence_token FROM coord.claim WHERE work_item_id").
		WithArgs(sqlmock.AnyArg()).
		WillReturnRows(rows)
}

func expectInsert(m sqlmock.Sqlmock, toPhase string, fence int64) {
	m.ExpectExec("INSERT INTO coord.run_phase_transition").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(),
			toPhase, sqlmock.AnyArg(), sql.NullInt64{Int64: fence, Valid: true}).
		WillReturnResult(sqlmock.NewResult(1, 1))
}

// The run's FIRST journal edge is forward by definition: it journals and stamps
// the fence so the NEXT edge has an oracle to compare against.
func TestGuardFirstEdgeJournalsWithFenceStamp(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	m.ExpectQuery("SELECT to_phase, fence FROM coord.run_phase_transition").
		WithArgs(guardRun).
		WillReturnRows(sqlmock.NewRows([]string{"to_phase", "fence"})) // no history
	expectClaimFence(m, i64(4))
	expectInsert(m, RunPhaseClaiming, 4)

	if err := RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		ToPhase: RunPhaseClaiming, TS: time.Now(),
	}); err != nil {
		t.Fatalf("first edge must journal, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// ACCEPTANCE 1 — a crash-path flap emits no backward row: the journal holds
// running@4, the claim fence is STILL 4 (the crash-path re-drive of a live
// claim bumps nothing, RedriveClaim's no-op fast path), and the regressed
// running→claiming edge is suppressed. No INSERT may fire.
func TestGuardSuppressesCrashPathBackwardFlap(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	expectClaimFence(m, i64(4)) // same fence — no retry lap happened

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseRunning, ToPhase: RunPhaseClaiming, TS: time.Now(),
	})
	if !errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("crash-path backward flap must be suppressed, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT may fire on suppression: %v", err)
	}
}

// ACCEPTANCE 2 — a genuine retry-lap regression still journals: the same
// running→claiming edge, but the claim was re-acquired (fence 4→5, the §6.2/§6.4
// bump every retry lap takes), journals with the certifying fence stamped.
func TestGuardJournalsFencedRetryRegression(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	expectClaimFence(m, i64(5)) // bumped — a real retry lap re-claimed
	expectInsert(m, RunPhaseClaiming, 5)

	if err := RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseRunning, ToPhase: RunPhaseClaiming, TS: time.Now(),
	}); err != nil {
		t.Fatalf("fenced retry regression must journal, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// The flap's re-advance is same-band noise: the journal already holds running,
// the suppressed regression never journaled claiming, so claiming→running
// re-enters the band it never durably left — suppressed without a fence bump.
func TestGuardSuppressesSameBandReEntry(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	expectClaimFence(m, i64(4))

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseClaiming, ToPhase: RunPhaseRunning, TS: time.Now(),
	})
	if !errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("same-band re-entry must be suppressed without a bump, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT may fire on suppression: %v", err)
	}
}

// A fenced same-band edge journals: after a GENUINE regression journaled
// claiming@5, the retry lap's re-advance (claiming→running) is forward against
// the last row — but even a same-band edge with a bumped fence certifies.
func TestGuardSameBandWithBumpJournals(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	expectClaimFence(m, i64(6))
	expectInsert(m, RunPhaseRunning, 6)

	if err := RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseClaiming, ToPhase: RunPhaseRunning, TS: time.Now(),
	}); err != nil {
		t.Fatalf("bumped-fence edge must journal, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// Fail-closed: no claim row (oracle unknown) — a backward edge cannot be
// certified, so it is suppressed rather than journaled on trust.
func TestGuardFailClosedWhenFenceUnknown(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	expectClaimFence(m, nil) // no claim row

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseRunning, ToPhase: RunPhaseClaiming, TS: time.Now(),
	})
	if !errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("unknown fence must suppress fail-closed, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT may fire on suppression: %v", err)
	}
}

// Fail-cold-start: a pre-guard last row carries NULL fence — the bump cannot be
// verified against it, so the backward edge is suppressed (at most ONE backward
// row is lost per run that straddles the deploy; runs born after it never hit
// this, and forward edges are unaffected).
func TestGuardFailClosedWhenLastFenceNull(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, nil) // pre-guard row
	expectClaimFence(m, i64(5))

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseRunning, ToPhase: RunPhaseClaiming, TS: time.Now(),
	})
	if !errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("NULL last fence must suppress fail-closed, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT may fire on suppression: %v", err)
	}
}

// No work-item ref (the caller did not wire the oracle key): backward edges are
// suppressed fail-closed — and the oracle read must not fire at all.
func TestGuardFailClosedWithoutWorkItemID(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseRunning, i64(4))
	// NOTE: no ExpectQuery on coord.claim — WorkItemID is empty.

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj,
		FromPhase: RunPhaseRunning, ToPhase: RunPhaseClaiming, TS: time.Now(),
	})
	if !errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("missing oracle key must suppress fail-closed, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT and no claim read may fire: %v", err)
	}
}

// A FORWARD edge never needs the oracle's blessing — it journals regardless of
// the fence (the guard exists to protect history from regressions, not to gate
// progress), still stamping the fence for the next comparison.
func TestGuardForwardEdgeJournalsWithoutBump(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	expectLastEdge(m, RunPhaseClaiming, i64(4))
	expectClaimFence(m, i64(4))
	expectInsert(m, RunPhaseRunning, 4)

	if err := RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, TeamID: "team", WorkItemID: guardItem,
		FromPhase: RunPhaseClaiming, ToPhase: RunPhaseRunning, TS: time.Now(),
	}); err != nil {
		t.Fatalf("forward edge must journal without a bump, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("expectations: %v", err)
	}
}

// An infra failure reading the last edge is an ERROR (requeueable at the
// caller), never a silent suppression and never a blind insert.
func TestGuardLastEdgeReadErrorPropagates(t *testing.T) {
	db, m, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer db.Close()
	m.ExpectQuery("SELECT to_phase, fence FROM coord.run_phase_transition").
		WithArgs(guardRun).
		WillReturnError(errors.New("db stall"))

	err = RecordRunPhaseTransition(context.Background(), db, RunPhaseTransition{
		RunUID: guardRun, ProjectID: guardProj, WorkItemID: guardItem,
		FromPhase: RunPhaseClaiming, ToPhase: RunPhaseRunning, TS: time.Now(),
	})
	if err == nil || errors.Is(err, ErrRunPhaseEdgeSuppressed) {
		t.Fatalf("last-edge read failure must propagate as infra error, got %v", err)
	}
	if err := m.ExpectationsWereMet(); err != nil {
		t.Fatalf("no INSERT may fire on read failure: %v", err)
	}
}

// The band order the guard keys on: claiming < running < terminal(=complete=failed).
func TestRunPhaseRank(t *testing.T) {
	if runPhaseRank(RunPhaseClaiming) >= runPhaseRank(RunPhaseRunning) {
		t.Fatalf("claiming must rank below running")
	}
	if runPhaseRank(RunPhaseRunning) >= runPhaseRank(RunPhaseComplete) {
		t.Fatalf("running must rank below terminal")
	}
	if runPhaseRank(RunPhaseComplete) != runPhaseRank(RunPhaseFailed) {
		t.Fatalf("complete and failed share the terminal rank (settlement revision, not regression)")
	}
}
