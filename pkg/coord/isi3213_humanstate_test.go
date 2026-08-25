package coord

// isi3213_humanstate_test.go — ISI-3213 (ratchet Go unit-test coverage 35→80).
//
// DB-free unit coverage for the human board-lane transition custody op
// (humanstate.go, Story 8.14a / ISI-2909): the constructor's nil-db guard, the
// board-lane enum (§8.6 — `blocked` is a condition, never a lane), and the
// fail-closed validation branches of TransitionState that return BEFORE any
// BeginTx (required-arg guard + ErrInvalidState on a non-lane target/fromState).
// The DB-backed body (row lock, CAS UPDATE, §6.5 audit) is exercised by the
// chaos-tagged suites; these pure branches run in the ci.yml unit lane and lift
// the gated authored-coverage number. Same pattern as credpause_test.go.

import (
	"context"
	"errors"
	"testing"
)

func TestNewHumanStateStore_NilDB(t *testing.T) {
	s, err := NewHumanStateStore(nil)
	if err == nil {
		t.Fatal("NewHumanStateStore(nil) = nil error, want non-nil (fail-closed on nil db)")
	}
	if s != nil {
		t.Errorf("NewHumanStateStore(nil) store = %v, want nil", s)
	}
}

// humanStates is the exact §8.6 board-lane enum the 0001 work_item.state CHECK
// pins. `blocked` is deliberately excluded (it is a condition, not a lane).
func TestHumanStates_BoardLaneEnum(t *testing.T) {
	for _, lane := range []string{"backlog", "todo", "in_progress", "in_review", "done"} {
		if !humanStates[lane] {
			t.Errorf("humanStates missing board lane %q", lane)
		}
	}
	if humanStates["blocked"] {
		t.Error("humanStates includes \"blocked\" — it is a condition, not a lane (§8.6)")
	}
	if len(humanStates) != 5 {
		t.Errorf("humanStates has %d lanes, want exactly 5", len(humanStates))
	}
}

// The three validation branches below all return before s.db.BeginTx, so a
// zero-value store (nil db) never dereferences it — this is a pure-logic test.
func TestTransitionState_RequiredArgs(t *testing.T) {
	s := &HumanStateStore{}
	ctx := context.Background()
	cases := []struct {
		name                           string
		workItemID, targetState, princ string
	}{
		{"empty workItemID", "", "done", "alice"},
		{"empty targetState", "wi-1", "", "alice"},
		{"empty principal", "wi-1", "done", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := s.TransitionState(ctx, c.workItemID, "team-1", c.targetState, "", c.princ, "")
			if err == nil {
				t.Fatalf("TransitionState(%q,%q,%q) = nil error, want required-arg error",
					c.workItemID, c.targetState, c.princ)
			}
		})
	}
}

func TestTransitionState_InvalidTargetState(t *testing.T) {
	s := &HumanStateStore{}
	ctx := context.Background()
	for _, target := range []string{"blocked", "bogus", "DONE"} {
		t.Run(target, func(t *testing.T) {
			_, err := s.TransitionState(ctx, "wi-1", "team-1", target, "", "alice", "")
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("TransitionState target=%q err = %v, want ErrInvalidState", target, err)
			}
		})
	}
}

func TestTransitionState_InvalidFromState(t *testing.T) {
	s := &HumanStateStore{}
	ctx := context.Background()
	// target is a valid lane; the non-lane fromState guard must trip first,
	// before any DB work.
	for _, from := range []string{"blocked", "nonsense"} {
		t.Run(from, func(t *testing.T) {
			_, err := s.TransitionState(ctx, "wi-1", "team-1", "done", from, "alice", "")
			if !errors.Is(err, ErrInvalidState) {
				t.Fatalf("TransitionState fromState=%q err = %v, want ErrInvalidState", from, err)
			}
		})
	}
}
