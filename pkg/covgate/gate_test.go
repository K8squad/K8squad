package covgate

import (
	"errors"
	"strings"
	"testing"
)

// TestCovGate is the L5 gate's teeth — one subtest per scenario pinning the SAME
// GREEN(build passes)/RED(build fails) verdict the AC demands, the way TestPerfGate
// pins the perf anchor's verdict table. No wall-clock, no RNG, no Postgres: every
// input is a fixed profile string, so this runs in EVERY go leg (`go test ./...`)
// and cannot be dodged even though the full per-package MEASUREMENT lane is nightly.
//
// If a tooth is filed off in gate.go the matching subtest goes RED:
//   - widen ThresholdFor so pkg/coord returns 80 → spine_below_90 flips GREEN.
//   - drop the ErrNoProfile guard in Evaluate → empty_profile stops failing.
//   - stop excluding zz_generated in ParseProfile → generated_excluded shifts.
//   - make the boundary exclusive → at_floor_exact flips RED.

// buildProfile assembles a synthetic atomic coverage profile. Each block is one
// statement; `hits` toggles covered vs not, so a package's percent is directly the
// covered-block fraction. Import paths use the real module prefix to prove the
// spine matcher is module-path aware.
const mod = "github.com/K8squad/K8squad/"

func profile(lines ...string) string {
	return "mode: atomic\n" + strings.Join(lines, "\n") + "\n"
}

// block emits one 1-statement coverage block for file, hit or not.
func block(file string, hit int) string {
	return mod + file + ":1.1,2.1 1 " + itoa(hit)
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	return "1"
}

// pkgProfile builds a profile for a single package at exactly covered/total ratio.
func pkgProfile(pkgFile string, covered, total int) string {
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		hit := 0
		if i < covered {
			hit = 1
		}
		lines = append(lines, block(pkgFile, hit))
	}
	return profile(lines...)
}

func TestThresholdFor(t *testing.T) {
	cases := map[string]float64{
		mod + "pkg/coord":           SpineFloor,   // exact spine root
		mod + "pkg/coord/store":     SpineFloor,   // spine subpackage
		mod + "pkg/warmpool":        DefaultFloor, // ordinary package
		mod + "internal/memory":     DefaultFloor,
		mod + "cmd/operator":        DefaultFloor,
		"pkg/coord":                 SpineFloor,   // trimmed-path form
		mod + "pkg/coordinator/foo": DefaultFloor, // must NOT prefix-match pkg/coord
	}
	for pkg, want := range cases {
		if got := ThresholdFor(pkg); got != want {
			t.Errorf("ThresholdFor(%q) = %.0f, want %.0f", pkg, got, want)
		}
	}
}

func TestCovGate(t *testing.T) {
	// -- positive control: an ordinary package at 80% and the spine at 90% -> GREEN --
	t.Run("all_clear", func(t *testing.T) {
		p := profile(
			// pkg/warmpool: 8/10 = 80.0% (== DefaultFloor)
			pkgLines("pkg/warmpool/pool.go", 8, 10)...,
		)
		p += strings.TrimPrefix(pkgProfile("pkg/coord/claim.go", 9, 10), "mode: atomic\n") // 90%
		ok, fails, err := EvaluateProfile(strings.NewReader(p))
		mustGreen(t, ok, err, fails)
	})

	// -- the teeth: pkg/coord at 85% is < 90% -> RED even though it clears 80% --
	t.Run("spine_below_90", func(t *testing.T) {
		ok, fails, err := EvaluateProfile(strings.NewReader(pkgProfile("pkg/coord/lease.go", 85, 100)))
		mustRed(t, ok, err, fails)
		if len(fails) != 1 || !strings.Contains(fails[0].Package, "pkg/coord") {
			t.Fatalf("expected pkg/coord failure, got %v", fails)
		}
		if fails[0].Threshold != SpineFloor {
			t.Fatalf("spine failure must report the 90%% floor, got %.0f", fails[0].Threshold)
		}
	})

	// -- an ordinary package at 79% is < 80% -> RED --
	t.Run("ordinary_below_80", func(t *testing.T) {
		ok, fails, err := EvaluateProfile(strings.NewReader(pkgProfile("pkg/warmpool/pool.go", 79, 100)))
		mustRed(t, ok, err, fails)
	})

	// -- boundary: exactly at floor PASSES (inclusive) --
	t.Run("at_floor_exact", func(t *testing.T) {
		ok, _, err := EvaluateProfile(strings.NewReader(pkgProfile("pkg/warmpool/pool.go", 80, 100)))
		mustGreen(t, ok, err, nil)
		ok, _, err = EvaluateProfile(strings.NewReader(pkgProfile("pkg/coord/x.go", 90, 100)))
		mustGreen(t, ok, err, nil)
	})

	// -- can't-measure = build FAILURE (ErrNoProfile), never silent-green --
	t.Run("empty_profile", func(t *testing.T) {
		_, _, err := EvaluateProfile(strings.NewReader("mode: atomic\n"))
		if !errors.Is(err, ErrNoProfile) {
			t.Fatalf("empty profile must fail the build with ErrNoProfile, got err=%v", err)
		}
	})

	// -- a profile mentioning ONLY generated files is empty after exclusion -> FAIL --
	t.Run("only_generated", func(t *testing.T) {
		p := profile(block("api/v1alpha1/zz_generated.deepcopy.go", 1))
		_, _, err := EvaluateProfile(strings.NewReader(p))
		if !errors.Is(err, ErrNoProfile) {
			t.Fatalf("all-generated profile must fail with ErrNoProfile, got %v", err)
		}
	})

	// -- generated code is excluded from an otherwise-real package's metric --
	t.Run("generated_excluded", func(t *testing.T) {
		// pkg/warmpool: 8/10 authored covered = 80%; the generated block (uncovered)
		// would drag it to 8/11 = 72.7% and false-RED if not excluded.
		p := profile(append(
			pkgLines("pkg/warmpool/pool.go", 8, 10),
			block("pkg/warmpool/zz_generated.deepcopy.go", 0),
		)...)
		ok, fails, err := EvaluateProfile(strings.NewReader(p))
		mustGreen(t, ok, err, fails)
	})

	// -- multiple offenders are ALL reported (no early return hides debt) --
	t.Run("reports_every_offender", func(t *testing.T) {
		p := profile(append(append(
			pkgLines("pkg/a/a.go", 5, 10),     // 50%
			pkgLines("pkg/b/b.go", 6, 10)...), // 60%
			pkgLines("pkg/c/c.go", 9, 10)..., // 90% (clears 80)
		)...)
		ok, fails, err := EvaluateProfile(strings.NewReader(p))
		mustRed(t, ok, err, fails)
		if len(fails) != 2 {
			t.Fatalf("expected 2 offenders (a,b), got %d: %v", len(fails), fails)
		}
	})

	// -- malformed profile line is a hard parse error, not a silent skip --
	t.Run("malformed_is_error", func(t *testing.T) {
		_, _, err := EvaluateProfile(strings.NewReader("mode: atomic\ngarbage line without blocks\n"))
		if err == nil || errors.Is(err, ErrNoProfile) {
			t.Fatalf("malformed profile must be a parse error, got %v", err)
		}
	})
}

// pkgLines returns covered/total 1-statement blocks for a single file (helper for
// building multi-package fixtures inline).
func pkgLines(file string, covered, total int) []string {
	lines := make([]string, 0, total)
	for i := 0; i < total; i++ {
		hit := 0
		if i < covered {
			hit = 1
		}
		lines = append(lines, block(file, hit))
	}
	return lines
}

func mustGreen(t *testing.T, ok bool, err error, fails []Failure) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected GREEN, gate errored: %v", err)
	}
	if !ok {
		t.Fatalf("expected GREEN, gate said RED: %v", fails)
	}
}

func mustRed(t *testing.T, ok bool, err error, fails []Failure) {
	t.Helper()
	if err != nil {
		t.Fatalf("expected RED (gate false), gate errored: %v", err)
	}
	if ok {
		t.Fatalf("expected RED, gate said GREEN")
	}
	if len(fails) == 0 {
		t.Fatalf("expected RED to name at least one offending package")
	}
}
