// runphasetransition_unit_test.go — the NO-Postgres unit lane for the WS-1
// (ISI-5746) Run phase-transition journal write: the fail-closed input guards
// that return BEFORE any DB round-trip, plus the phase enum. Same split as
// changeref_unit_test.go — the database-backed properties (append-only
// round-trip, enum CHECK, NULL from_phase) are exercised by the shipped 0037
// migration's _test.sql self-check.
package coord

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestRecordRunPhaseTransitionRejectsBadInput(t *testing.T) {
	db := offlineDB()
	cases := map[string]RunPhaseTransition{
		"no run uid":     {RunUID: "", ProjectID: "p", ToPhase: RunPhaseClaiming},
		"no project":     {RunUID: "r", ProjectID: "", ToPhase: RunPhaseClaiming},
		"no to phase":    {RunUID: "r", ProjectID: "p", ToPhase: ""},
		"bad to phase":   {RunUID: "r", ProjectID: "p", ToPhase: "Succeeded"},
		"bad from phase": {RunUID: "r", ProjectID: "p", FromPhase: "bogus", ToPhase: RunPhaseRunning},
	}
	for name, row := range cases {
		t.Run(name, func(t *testing.T) {
			err := RecordRunPhaseTransition(context.Background(), db, row)
			if !errors.Is(err, ErrInvalidRunPhaseTransition) {
				t.Fatalf("got %v, want ErrInvalidRunPhaseTransition", err)
			}
		})
	}
}

func TestRecordRunPhaseTransitionNilDB(t *testing.T) {
	err := RecordRunPhaseTransition(context.Background(), nil,
		RunPhaseTransition{RunUID: "r", ProjectID: "p", ToPhase: RunPhaseClaiming})
	if err == nil {
		t.Fatal("nil db must be rejected")
	}
	// A nil-db error is infrastructure, NOT a validation error — must not be
	// conflated with the fail-closed input guard.
	if errors.Is(err, ErrInvalidRunPhaseTransition) {
		t.Fatalf("nil db should not report ErrInvalidRunPhaseTransition, got %v", err)
	}
}

// An empty FromPhase is VALID — the Run's first observed transition (stored
// NULL). It must pass the input guards (the call then fails only on the offline
// DB's connect, never on validation).
func TestRecordRunPhaseTransitionEmptyFromPhaseIsValid(t *testing.T) {
	err := RecordRunPhaseTransition(context.Background(), offlineDB(),
		RunPhaseTransition{RunUID: "r", ProjectID: "p", FromPhase: "", ToPhase: RunPhaseClaiming, TS: time.Now()})
	if errors.Is(err, ErrInvalidRunPhaseTransition) {
		t.Fatalf("empty from_phase (first transition) must pass validation, got %v", err)
	}
}

// An unresolved Team (empty TeamID) is VALID — stored NULL; it must not trip the
// input guard (project_id is the required tenancy key, not team_id).
func TestRecordRunPhaseTransitionEmptyTeamIsValid(t *testing.T) {
	err := RecordRunPhaseTransition(context.Background(), offlineDB(),
		RunPhaseTransition{RunUID: "r", ProjectID: "p", TeamID: "", ToPhase: RunPhaseRunning})
	if errors.Is(err, ErrInvalidRunPhaseTransition) {
		t.Fatalf("empty team_id (unresolved Team) must pass validation, got %v", err)
	}
}

// The phase enum is closed: the four lowercased chip tokens only.
func TestValidRunPhases(t *testing.T) {
	for _, ok := range []string{RunPhaseClaiming, RunPhaseRunning, RunPhaseComplete, RunPhaseFailed} {
		if !ValidRunPhases[ok] {
			t.Fatalf("%q should be a valid run phase", ok)
		}
	}
	for _, bad := range []string{"", "Claiming", "Running", "Succeeded", "succeeded", "completed", "cancelled", "pending"} {
		if ValidRunPhases[bad] {
			t.Fatalf("%q should be an invalid run phase", bad)
		}
	}
}
