package coord

import (
	"context"
	"testing"
	"time"
)

// fakePhaseMetrics captures emissions for assertion (the SweeperMetrics fake
// discipline).
type fakePhaseMetrics struct {
	transitions [][3]string   // {from, to, initiator}
	durations   []phaseSample // {phase, initiator, seconds}
}

type phaseSample struct {
	phase, initiator string
	seconds          float64
}

func (f *fakePhaseMetrics) IncPhaseTransition(from, to, initiator string) {
	f.transitions = append(f.transitions, [3]string{from, to, initiator})
}
func (f *fakePhaseMetrics) ObservePhaseDuration(phase, initiator string, seconds float64) {
	f.durations = append(f.durations, phaseSample{phase, initiator, seconds})
}
func (f *fakePhaseMetrics) Signals() []string { return nil }

// withFakePhaseMetrics swaps the process emitter for the duration of fn and
// restores it (the emitter is process-global; tests run serially per package).
func withFakePhaseMetrics(t *testing.T, fn func(*fakePhaseMetrics)) {
	t.Helper()
	prev := defaultPhaseMetrics
	fake := &fakePhaseMetrics{}
	defaultPhaseMetrics = fake
	defer func() { defaultPhaseMetrics = prev }()
	fn(fake)
}

// A well-formed transition emits exactly one counter and one duration sample,
// with the from-phase as the histogram key.
func TestEmitPhaseTransitionEmits(t *testing.T) {
	withFakePhaseMetrics(t, func(f *fakePhaseMetrics) {
		EmitPhaseTransition(context.Background(), PhaseTransitionEvent{
			WorkItemRef: "wi-1",
			From:        "design",
			To:          "planning",
			Initiator:   "coordinator",
			TimeInPhase: 90 * time.Second,
		})
		if len(f.transitions) != 1 || f.transitions[0] != [3]string{"design", "planning", "coordinator"} {
			t.Fatalf("transitions = %v, want one design→planning/coordinator", f.transitions)
		}
		if len(f.durations) != 1 || f.durations[0] != (phaseSample{"design", "coordinator", 90}) {
			t.Fatalf("durations = %v, want one {design,coordinator,90}", f.durations)
		}
	})
}

// A blank ref/from/to is not a phase transition — nothing is emitted (an empty
// label would pollute the bounded metric).
func TestEmitPhaseTransitionSkipsIncomplete(t *testing.T) {
	withFakePhaseMetrics(t, func(f *fakePhaseMetrics) {
		EmitPhaseTransition(context.Background(), PhaseTransitionEvent{From: "design", To: "planning"})    // no ref
		EmitPhaseTransition(context.Background(), PhaseTransitionEvent{WorkItemRef: "wi", To: "planning"}) // no from
		EmitPhaseTransition(context.Background(), PhaseTransitionEvent{WorkItemRef: "wi", From: "design"}) // no to
		if len(f.transitions) != 0 || len(f.durations) != 0 {
			t.Fatalf("emitted on an incomplete event: %v / %v", f.transitions, f.durations)
		}
	})
}

// A negative duration (clock skew) clamps to 0; a blank initiator normalizes.
func TestEmitPhaseTransitionClampsAndDefaults(t *testing.T) {
	withFakePhaseMetrics(t, func(f *fakePhaseMetrics) {
		EmitPhaseTransition(context.Background(), PhaseTransitionEvent{
			WorkItemRef: "wi-1",
			From:        "testing",
			To:          "done",
			TimeInPhase: -5 * time.Second,
		})
		if len(f.durations) != 1 || f.durations[0].seconds != 0 {
			t.Fatalf("durations = %v, want clamped 0", f.durations)
		}
		if f.durations[0].initiator != "unknown" || f.transitions[0][2] != "unknown" {
			t.Fatalf("blank initiator not defaulted: %v / %v", f.durations, f.transitions)
		}
	})
}
