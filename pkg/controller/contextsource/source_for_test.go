package contextsource

import "testing"

// Deps.For is the single gather-site wiring both the Run reconciler and the
// dispatcher share. It must construct without dereferencing its dependencies
// (nil Memory is the operator's documented memory-tier-off shape), pass the
// namespace through to the Source, and let a TopK <= 0 default inside the
// assembler rather than panicking here.
func TestDepsFor(t *testing.T) {
	a := Deps{}.For("run-ns-1")
	if a == nil {
		t.Fatal("For must return a non-nil assembler")
	}
	if a.TopK != 8 {
		t.Fatalf("TopK <= 0 must default to 8 inside the assembler, got %d", a.TopK)
	}
	if got := (Deps{TopK: 5}).For("ns-2"); got.TopK != 5 {
		t.Fatalf("explicit TopK must pass through, got %d want 5", got.TopK)
	}
}
