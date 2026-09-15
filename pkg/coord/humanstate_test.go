// humanstate_test.go — white-box unit checks for the ISI-4455 phase-status
// model: the extended board-state enum and the authored, fail-closed
// phase-transition graph. Pure logic, no Postgres — the DB-level CHECK is proven
// by 0019_work_item_phase_states_test.sql, the wired store behaviour by the
// apiserver/taskio tests.
package coord

import "testing"

// the 10 human phase columns the ISI-4452 Kanban renders.
var phaseColumns = []string{
	"backlog", "todo",
	"design", "planning", "implementation",
	"code_review", "testing", "documentation",
	"done", "cancelled",
}

func TestHumanStates_PhaseColumnsAndEngineLanesValid(t *testing.T) {
	// All 10 phase columns must be human-targetable.
	for _, s := range phaseColumns {
		if !humanStates[s] {
			t.Errorf("phase column %q must be a valid (human-targetable) board state", s)
		}
	}
	// The transitional engine lanes stay valid so the dispatch engine and agent
	// self-reports keep working (additive, not a replace).
	for _, s := range []string{"in_progress", "in_review"} {
		if !humanStates[s] {
			t.Errorf("engine lane %q must remain a valid board state (additive migration)", s)
		}
	}
	// `blocked` is a condition, never a lane.
	if humanStates["blocked"] {
		t.Error("`blocked` must NOT be a board state — it is an orthogonal condition (blocked_reason)")
	}
}

func TestPhaseTransitionAllowed(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		// working lane → anywhere is permitted (reshuffle / complete / cancel).
		{"backlog", "todo", true},
		{"todo", "design", true},
		{"design", "implementation", true}, // skip-ahead
		{"implementation", "design", true}, // rework (backward)
		{"documentation", "done", true},
		{"implementation", "cancelled", true},
		{"in_progress", "in_review", true}, // agent self-report path
		{"in_progress", "done", true},
		{"backlog", "done", true}, // permissive by design; new client renders no-drop cues

		// terminal → working lane is a REOPEN, permitted (incl. the live 5-lane
		// board's done→in_progress drag).
		{"done", "todo", true},
		{"done", "in_progress", true},
		{"cancelled", "backlog", true},
		{"cancelled", "implementation", true},

		// the ONLY fail-closed family: a direct hop between the two terminals.
		{"done", "cancelled", false},
		{"cancelled", "done", false},

		// a no-op is not a transition (handled upstream as a conflict).
		{"done", "done", false},
		{"todo", "todo", false},
	}
	for _, c := range cases {
		if got := phaseTransitionAllowed(c.from, c.to); got != c.want {
			t.Errorf("phaseTransitionAllowed(%q, %q) = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
