/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package coord

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
)

// fakeRunSummaryMetrics captures emissions for assertion (the phasespan fake
// discipline).
type fakeRunSummaryMetrics struct {
	records []runSummarySample
}

type runSummarySample struct {
	outcome string
	source  string
	present bool
	length  int
}

func (f *fakeRunSummaryMetrics) RecordRunSummary(_ context.Context, outcome, source string, present bool, length int) {
	f.records = append(f.records, runSummarySample{outcome, source, present, length})
}
func (f *fakeRunSummaryMetrics) Signals() []string { return nil }

// withFakeRunSummaryMetrics swaps the process emitter for the duration of fn
// and restores it (the emitter is process-global; tests run serially).
func withFakeRunSummaryMetrics(t *testing.T, fn func(*fakeRunSummaryMetrics)) {
	t.Helper()
	prev := defaultRunSummaryMetrics
	fake := &fakeRunSummaryMetrics{}
	defaultRunSummaryMetrics = fake
	defer func() { defaultRunSummaryMetrics = prev }()
	fn(fake)
}

const (
	testWI     = "11111111-1111-1111-1111-111111111111"
	testRun    = "22222222-2222-2222-2222-222222222222"
	testEngine = "engine:operator"
	testAgent  = "agent:amelia"
)

// expectSummaryQuery registers the committed-state re-query and returns the
// chosen rows (nil → ErrNoRows).
func expectSummaryQuery(mock sqlmock.Sqlmock, rows *sqlmock.Rows, err error) {
	q := mock.ExpectQuery("FROM coord.comment").WithArgs(testWI, testRun)
	switch {
	case err != nil:
		q.WillReturnError(err)
	case rows != nil:
		q.WillReturnRows(rows)
	default:
		q.WillReturnRows(sqlmock.NewRows([]string{"body", "author_principal"}))
	}
}

// An agent-authored summary (author != engine principal) emits present=true,
// source=agent, and the rune length of the body. The length is COUNTED in
// runes, not bytes, so multibyte content is not over-reported.
func TestEmitRunSummaryObservationAgentAuthored(t *testing.T) {
	withFakeRunSummaryMetrics(t, func(f *fakeRunSummaryMetrics) {
		db, mock, err := sqlmock.New()
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		body := "Run summary — héllo 世界" // 20 runes, >20 bytes
		expectSummaryQuery(mock, sqlmock.NewRows([]string{"body", "author_principal"}).AddRow(body, testAgent), nil)

		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "succeeded", EnginePrincipal: testEngine,
		})

		want := runSummarySample{"succeeded", SummarySourceAgent, true, len([]rune(body))}
		if len(f.records) != 1 || f.records[0] != want {
			t.Fatalf("records = %v, want one %v", f.records, want)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatal(err)
		}
	})
}

// A summary authored under the engine principal is the platform-synthesized
// fallback → source=synthesized, still present=true.
func TestEmitRunSummaryObservationSynthesized(t *testing.T) {
	withFakeRunSummaryMetrics(t, func(f *fakeRunSummaryMetrics) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		expectSummaryQuery(mock, sqlmock.NewRows([]string{"body", "author_principal"}).AddRow("auto summary", testEngine), nil)

		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "failed", EnginePrincipal: testEngine,
		})

		want := runSummarySample{"failed", SummarySourceSynthesized, true, len("auto summary")}
		if len(f.records) != 1 || f.records[0] != want {
			t.Fatalf("records = %v, want one %v", f.records, want)
		}
	})
}

// THE REGRESSION GUARD: a completed run with NO summary row emits present=false,
// source=absent, length=0 — the series the missing-summary detector alerts on.
func TestEmitRunSummaryObservationAbsentIsTheRegression(t *testing.T) {
	withFakeRunSummaryMetrics(t, func(f *fakeRunSummaryMetrics) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		expectSummaryQuery(mock, nil, nil) // no rows → ErrNoRows

		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "cancelled", EnginePrincipal: testEngine,
		})

		want := runSummarySample{"cancelled", SummarySourceAbsent, false, 0}
		if len(f.records) != 1 || f.records[0] != want {
			t.Fatalf("records = %v, want one %v (the regression series)", f.records, want)
		}
	})
}

// A non-terminal step (a retry lap), an empty id, or a nil db is a no-op — the
// guard owes nothing where settle owes nothing, and never touches the db.
func TestEmitRunSummaryObservationSkips(t *testing.T) {
	withFakeRunSummaryMetrics(t, func(f *fakeRunSummaryMetrics) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		// No ExpectQuery registered: any db touch fails ExpectationsWereMet.

		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "running", EnginePrincipal: testEngine,
		}) // non-terminal
		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: "", RunID: testRun, TerminalStep: "succeeded", EnginePrincipal: testEngine,
		}) // no work item
		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: "", TerminalStep: "succeeded", EnginePrincipal: testEngine,
		}) // no run id
		EmitRunSummaryObservation(context.Background(), nil, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "succeeded", EnginePrincipal: testEngine,
		}) // nil db

		if len(f.records) != 0 {
			t.Fatalf("emitted on a no-op: %v", f.records)
		}
		if err := mock.ExpectationsWereMet(); err != nil {
			t.Fatalf("db was touched on a no-op: %v", err)
		}
	})
}

// A query error degrades to silence (best-effort): the board move already
// committed, so the guard emits nothing rather than a phantom reading.
func TestEmitRunSummaryObservationQueryErrorIsSilent(t *testing.T) {
	withFakeRunSummaryMetrics(t, func(f *fakeRunSummaryMetrics) {
		db, mock, _ := sqlmock.New()
		defer db.Close()
		expectSummaryQuery(mock, nil, errors.New("connection reset"))

		EmitRunSummaryObservation(context.Background(), db, RunSummaryObservation{
			WorkItemID: testWI, RunID: testRun, TerminalStep: "succeeded", EnginePrincipal: testEngine,
		})

		if len(f.records) != 0 {
			t.Fatalf("emitted on a query error: %v", f.records)
		}
	})
}

// The default (uninstalled) emitter is nop and names both signals — the
// contract is which signals EXIST, asserted independent of wiring.
func TestRunSummaryNopSignals(t *testing.T) {
	var m RunSummaryMetrics = nopRunSummaryMetrics{}
	got := m.Signals()
	want := map[string]bool{runSummaryPresentName: true, runSummaryLengthName: true}
	if len(got) != len(want) {
		t.Fatalf("signals = %v, want %v", got, want)
	}
	for _, s := range got {
		if !want[s] {
			t.Fatalf("unexpected signal %q", s)
		}
	}
	// A nop record must not panic (call sites never nil-check).
	m.RecordRunSummary(context.Background(), "succeeded", SummarySourceAgent, true, 10)
}

// SetRunSummaryMetrics ignores nil (never downgrades an installed emitter to a
// nil that would panic call sites).
func TestSetRunSummaryMetricsIgnoresNil(t *testing.T) {
	prev := defaultRunSummaryMetrics
	defer func() { defaultRunSummaryMetrics = prev }()
	fake := &fakeRunSummaryMetrics{}
	defaultRunSummaryMetrics = fake
	SetRunSummaryMetrics(nil)
	if defaultRunSummaryMetrics != fake {
		t.Fatal("SetRunSummaryMetrics(nil) clobbered the installed emitter")
	}
}
