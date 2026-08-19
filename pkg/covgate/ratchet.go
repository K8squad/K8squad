// Ratchet mode — the L5 lane's per-package INTERIM posture (ISI-2852).
//
// The TARGET floors (DefaultFloor 80% / SpineFloor 90%) are not yet
// satisfiable on the merged tree: the authored baseline is ~35% module-wide
// (see ci.yml's ISI-2714 ratchet log) and the L5 lane measures a DB-less
// `go test ./...` run, so DB-backed code (pkg/coord, pkg/events) cannot
// reach its floor there until the ISI-2714 DB-backed coverage lane lands.
// Scoring the nightly at the unattainable target red-walled the lane on
// EVERY run (runs 1–3, 2026-08-18/19) — a permanently-red gate carries no
// regression signal at all.
//
// The ratchet restores the signal without weakening the mechanism:
//
//   - every package must stay >= its baseline entry (measured on the runner,
//     pinned in covgate.baseline.json) — a regression FAILS the build;
//   - a package present in the profile but ABSENT from the baseline FAILS
//     the build (a new package must be consciously baselined via -regen in
//     the PR that adds it — no silent-green for unknown code);
//   - baselines only ever move UP (review enforces; same convention as
//     ci.yml's COVERAGE_FLOOR: "Do NOT lower this number; raise it as
//     coverage improves");
//   - the target floors stay hard-coded above and are re-armed (flagless
//     target mode) once the ISI-2714 ratchet reaches 80/90 — promotion is
//     a one-line lane change plus a green target-mode dry run.
package covgate

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
)

// RatchetBaseline maps a package import path to its pinned per-package
// coverage floor (percent). Entries are the one-way ratchet: review must
// reject any baseline that moves down.
type RatchetBaseline map[string]float64

// RatchetFailure is one package that violated the ratchet: either measured
// below its baseline entry, or measured at all while absent from the
// baseline (unbaselined = the no-silent-green tooth for new code).
type RatchetFailure struct {
	Package string
	Percent float64
	Floor   float64
	// Unbaselined marks a package missing from the baseline entirely.
	Unbaselined bool
}

func (f RatchetFailure) String() string {
	if f.Unbaselined {
		return fmt.Sprintf("%s: %.1f%% measured but no ratchet baseline entry — cover it and regenerate the baseline in the same PR (go run ./cmd/covgate -regen covgate.baseline.json coverage.out)", f.Package, f.Percent)
	}
	return fmt.Sprintf("%s: %.1f%% < %.1f%% ratchet baseline", f.Package, f.Percent, f.Floor)
}

// LoadRatchetBaseline reads the JSON baseline file (map of import path to
// floor percent). A missing file is a hard error: the lane must never fall
// back to target-less scoring.
func LoadRatchetBaseline(path string) (RatchetBaseline, error) {
	raw, err := os.ReadFile(path) // #nosec G304 -- caller-specified baseline path is intentional (CI tool)
	if err != nil {
		return nil, fmt.Errorf("covgate: reading ratchet baseline: %w", err)
	}
	var b RatchetBaseline
	if err := json.Unmarshal(raw, &b); err != nil {
		return nil, fmt.Errorf("covgate: parsing ratchet baseline %s: %w", path, err)
	}
	if len(b) == 0 {
		return nil, fmt.Errorf("covgate: ratchet baseline %s is empty", path)
	}
	return b, nil
}

// WriteRatchetBaseline regenerates the baseline file from a scored profile,
// rounding every floor DOWN to one decimal so the pinned value can never
// exceed the measurement that produced it (a display-rounded 89.6 measured
// as 89.55 would otherwise false-RED the next identical run). Output is
// deterministic (sorted keys, stable indentation) so baseline diffs review
// cleanly.
func WriteRatchetBaseline(path string, pkgs []PackageCoverage) error {
	b := make(RatchetBaseline, len(pkgs))
	for _, p := range pkgs {
		b[p.Package] = floor1(p.Percent)
	}
	raw, err := json.MarshalIndent(b, "", "  ")
	if err != nil {
		return fmt.Errorf("covgate: encoding ratchet baseline: %w", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return fmt.Errorf("covgate: writing ratchet baseline: %w", err)
	}
	return nil
}

// floor1 rounds down to one decimal (89.57 -> 89.5).
func floor1(v float64) float64 {
	return float64(int64(v*10)) / 10
}

// EvaluateRatchet scores packages against the baseline. Fail-closed in the
// same spirit as Evaluate: RED when any measured package is below its entry
// or absent from the baseline; ErrNoProfile still applies (nothing measured
// = build failure). Baseline entries whose package no longer appears in the
// profile are returned as stale — reported as notices, not failures, since
// deleted code cannot regress. A package exactly at its baseline PASSES
// (inclusive boundary, matching Evaluate).
func EvaluateRatchet(pkgs []PackageCoverage, baseline RatchetBaseline) (passed bool, failures []RatchetFailure, stale []string, err error) {
	if len(pkgs) == 0 {
		return false, nil, nil, ErrNoProfile
	}
	seen := make(map[string]bool, len(pkgs))
	for _, p := range pkgs {
		seen[p.Package] = true
		floor, ok := baseline[p.Package]
		if !ok {
			failures = append(failures, RatchetFailure{Package: p.Package, Percent: p.Percent, Unbaselined: true})
			continue
		}
		if p.Percent+1e-9 < floor {
			failures = append(failures, RatchetFailure{Package: p.Package, Percent: p.Percent, Floor: floor})
		}
	}
	for pkg := range baseline {
		if !seen[pkg] {
			stale = append(stale, pkg)
		}
	}
	sort.Strings(stale)
	return len(failures) == 0, failures, stale, nil
}
