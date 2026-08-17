// Package covgate is the Go landing of the Story 14.5 (ISI-2744) L5 code-quality
// & coverage GATE — the per-package coverage threshold evaluator the
// l5-code-quality.yml lane runs after `go test -race -coverprofile`.
//
// It follows the same shape as pkg/perfgate (Story 14.3): the thing under test is
// NOT a coverage number, it is the GATE LOGIC. Given a coverage profile, does the
// evaluator FAIL THE BUILD when a correctness-critical package is under-covered and
// stay GREEN when every package clears its floor? The thresholds below are the only
// thing the gate hard-codes; everything else is measured from the profile in-job.
//
// Per the AC (04-epics-and-stories.md 14.5) the coverage bar is PER-PACKAGE so the
// spine cannot hide behind trivial packages inflating a whole-module average:
//
//	≥80% every Go package (DefaultFloor)
//	≥90% pkg/coord — the coordination spine (SpineFloor)
//	≥70% console — enforced in the node lane by the JS coverage reporter, not here.
//
// The evaluator is fail-closed: a missing/empty profile is a build FAILURE
// (ErrNoProfile, the "can't measure = fail" tooth), never a silent green, exactly
// as perfgate.RequireBaseline fails a baseline-less perf run.
package covgate

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// Per-package coverage floors (percent). PER-PACKAGE, not whole-module: a 30%
// package cannot be masked by a 100% trivial neighbour in an averaged number.
// Do NOT lower these; the interim ratchet toward them lives on the PR path in
// ci.yml (whole-module floor, ISI-2714). This lane enforces the TARGET.
const (
	// DefaultFloor — every authored Go package must clear this.
	DefaultFloor = 80.0
	// SpineFloor — pkg/coord, the correctness-critical coordination spine
	// (no-double-claim / lease-fencing), carries a higher bar.
	SpineFloor = 90.0
	// ConsoleFloor — the Next.js console target, enforced by the JS coverage
	// reporter in the node lane (documented here so the bar lives in one place).
	ConsoleFloor = 70.0
)

// spinePrefix identifies the coordination-spine package(s) that carry SpineFloor.
// Matched on the import-path suffix so it is module-path agnostic (works whether
// the profile prints github.com/K8squad/K8squad/pkg/coord or a trimmed path).
const spinePrefix = "pkg/coord"

// ErrNoProfile is the build-FAILURE signal: an L5 coverage run with no parseable
// statements must fail the build, never silent-green (the "can't measure = fail"
// tooth, analogue of perfgate.ErrMissingBaseline). A profile that mentions only
// generated files — all excluded — is equally a failure: the lane measured nothing.
var ErrNoProfile = errors.New("covgate: no coverage statements to score (missing/empty profile)")

// PackageCoverage is a single package's statement-coverage percentage, aggregated
// from every non-generated block that package contributed to the profile.
type PackageCoverage struct {
	// Package is the import path (e.g. github.com/K8squad/K8squad/pkg/coord).
	Package string
	// Percent is covered-statements / total-statements * 100 for the package.
	Percent float64
	// Statements is the package's total counted statements (excludes generated).
	Statements int
}

// ThresholdFor returns the required coverage floor for an import path. The
// coordination spine (pkg/coord) carries SpineFloor; every other package carries
// DefaultFloor. This is the single tooth the per-package bar rests on: widening it
// (e.g. returning DefaultFloor for pkg/coord) lets an under-covered spine merge and
// is exactly what TestCovGate/spine_below_90 kills.
func ThresholdFor(importPath string) float64 {
	// Normalise on the import-path suffix so a full or trimmed module path both hit.
	if importPath == spinePrefix ||
		strings.HasSuffix(importPath, "/"+spinePrefix) ||
		strings.Contains(importPath, "/"+spinePrefix+"/") {
		return SpineFloor
	}
	return DefaultFloor
}

// isGenerated reports whether a source file is machine-generated and must be
// excluded from the authored-code metric (matches the ci.yml zz_generated filter).
func isGenerated(file string) bool {
	base := path.Base(file)
	return strings.HasPrefix(base, "zz_generated") || strings.HasSuffix(base, ".pb.go")
}

// ParseProfile reads a Go coverage profile (`go test -coverprofile` /
// `-covermode=atomic` output) and aggregates per-package statement coverage,
// excluding generated files. It parses the raw block format rather than
// `go tool cover -func` because only the raw profile carries per-block statement
// COUNTS — the correct statement-weighted denominator for a true per-package
// percentage (a function-percentage average would misweight tiny functions).
//
// Profile line format (after the `mode:` header):
//
//	<import-path>/<file>.go:<sL>.<sC>,<eL>.<eC> <numStmts> <hitCount>
//
// A block counts as covered when hitCount > 0. Package = path.Dir(import-path/file).
func ParseProfile(r io.Reader) ([]PackageCoverage, error) {
	type acc struct {
		total   int
		covered int
	}
	byPkg := map[string]*acc{}

	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	first := true
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if first {
			first = false
			if strings.HasPrefix(line, "mode:") {
				continue
			}
			// No mode header — fall through and try to parse this line as a block.
		}

		fields := strings.Fields(line)
		if len(fields) != 3 {
			return nil, fmt.Errorf("covgate: malformed profile line %q", line)
		}
		// fields[0] = "<path>.go:<sL>.<sC>,<eL>.<eC>"
		colon := strings.LastIndex(fields[0], ":")
		if colon < 0 {
			return nil, fmt.Errorf("covgate: malformed profile block %q", fields[0])
		}
		file := fields[0][:colon]
		numStmts, err := strconv.Atoi(fields[1])
		if err != nil {
			return nil, fmt.Errorf("covgate: bad statement count in %q: %w", line, err)
		}
		hits, err := strconv.Atoi(fields[2])
		if err != nil {
			return nil, fmt.Errorf("covgate: bad hit count in %q: %w", line, err)
		}
		if isGenerated(file) {
			continue
		}
		pkg := path.Dir(file)
		a := byPkg[pkg]
		if a == nil {
			a = &acc{}
			byPkg[pkg] = a
		}
		a.total += numStmts
		if hits > 0 {
			a.covered += numStmts
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("covgate: reading profile: %w", err)
	}

	out := make([]PackageCoverage, 0, len(byPkg))
	for pkg, a := range byPkg {
		if a.total == 0 {
			continue // a package with zero counted statements contributes no signal.
		}
		out = append(out, PackageCoverage{
			Package:    pkg,
			Percent:    100.0 * float64(a.covered) / float64(a.total),
			Statements: a.total,
		})
	}
	// Deterministic order so the printed report and any golden test are stable.
	sort.Slice(out, func(i, j int) bool { return out[i].Package < out[j].Package })
	return out, nil
}

// Failure is one package that fell below its floor.
type Failure struct {
	Package   string
	Percent   float64
	Threshold float64
}

func (f Failure) String() string {
	return fmt.Sprintf("%s: %.1f%% < %.0f%% floor", f.Package, f.Percent, f.Threshold)
}

// Evaluate is fail-closed: it returns passed=false with the list of offending
// packages if ANY package is below its ThresholdFor floor, and ErrNoProfile if
// there is nothing to score (missing/empty profile — the build must fail, never
// silent-green). A package exactly at its floor PASSES (boundary is inclusive).
func Evaluate(pkgs []PackageCoverage) (passed bool, failures []Failure, err error) {
	if len(pkgs) == 0 {
		return false, nil, ErrNoProfile
	}
	for _, p := range pkgs {
		floor := ThresholdFor(p.Package)
		if p.Percent+1e-9 < floor {
			failures = append(failures, Failure{Package: p.Package, Percent: p.Percent, Threshold: floor})
		}
	}
	return len(failures) == 0, failures, nil
}

// EvaluateProfile is the one-call driver the lane uses: parse then evaluate.
func EvaluateProfile(r io.Reader) (passed bool, failures []Failure, err error) {
	pkgs, err := ParseProfile(r)
	if err != nil {
		return false, nil, err
	}
	return Evaluate(pkgs)
}
