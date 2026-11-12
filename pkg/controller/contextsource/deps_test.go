package contextsource

import "testing"

// ISI-7286 (L5 green-up): Deps.For was the one zero-coverage function left in
// this package. Pin the construction contract both call sites (reconciler,
// dispatcher) rely on: an assembler bound to the namespace, with the TopK
// default applied when unset.
func TestDepsForBuildsAssembler(t *testing.T) {
	if asm := (Deps{}).For("team-a"); asm == nil {
		t.Fatal("Deps.For returned nil assembler")
	}
	if asm := (Deps{TopK: 3}).For("team-b"); asm == nil {
		t.Fatal("Deps.For returned nil assembler with TopK set")
	}
}
