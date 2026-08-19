// Command covgate scores a Go coverage profile against the Story 14.5 (ISI-2744)
// L5 per-package thresholds and exits non-zero (fail-closed) if any package is
// under its floor or the profile is empty. It is the thin CLI over pkg/covgate the
// l5-code-quality.yml lane runs after `go test -race -coverprofile`; all gate LOGIC
// (and its teeth) live in pkg/covgate so this main stays trivial.
//
// Target mode (default — the 80%/90% promotion gate):
//
//	go run ./cmd/covgate coverage.out
//	go run ./cmd/covgate < coverage.out
//
// Ratchet mode (the L5 lane's interim posture, ISI-2852): every package must
// stay >= its pinned baseline entry, and any package missing from the baseline
// fails (regenerate deliberately, in the PR that adds the code):
//
//	go run ./cmd/covgate -ratchet covgate.baseline.json coverage.out
//
// Regenerate the baseline after coverage IMPROVES (moves up only — review
// rejects downward edits, same convention as ci.yml's COVERAGE_FLOOR):
//
//	go run ./cmd/covgate -regen covgate.baseline.json coverage.out
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/K8squad/K8squad/pkg/covgate"
)

func main() {
	var (
		ratchetPath = flag.String("ratchet", "", "ratchet-mode gate: score against this baseline JSON instead of the 80/90 target floors")
		regenPath   = flag.String("regen", "", "regenerate this baseline JSON from the profile (floors round DOWN to 1dp), then exit")
	)
	flag.Parse()

	r, err := profileReader(flag.Args())
	if err != nil {
		fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
		os.Exit(2)
	}
	defer r.Close()

	pkgs, err := covgate.ParseProfile(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
		os.Exit(1)
	}

	if *regenPath != "" {
		if err := covgate.WriteRatchetBaseline(*regenPath, pkgs); err != nil {
			fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
			os.Exit(1)
		}
		fmt.Printf("covgate: ratchet baseline regenerated at %s (%d packages)\n", *regenPath, len(pkgs))
		return
	}

	if *ratchetPath != "" {
		baseline, err := covgate.LoadRatchetBaseline(*ratchetPath)
		if err != nil {
			fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
			os.Exit(1)
		}
		// Audit trail: measured coverage vs the ratchet floor AND the eventual
		// target floor, so every nightly run shows the remaining distance.
		fmt.Printf("%-60s %8s  %9s  %s\n", "PACKAGE", "COVER", "RATCHET", "TARGET")
		for _, p := range pkgs {
			fmt.Printf("%-60s %7.1f%%  %8.1f%%  %.0f%%\n", p.Package, p.Percent, baseline[p.Package], covgate.ThresholdFor(p.Package))
		}
		passed, failures, stale, err := covgate.EvaluateRatchet(pkgs, baseline)
		if err != nil {
			fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
			os.Exit(1)
		}
		for _, s := range stale {
			fmt.Printf("covgate: notice: baseline entry %s no longer appears in the profile (package deleted?) — drop it on the next -regen\n", s)
		}
		if !passed {
			fmt.Fprintln(os.Stderr, "\ncovgate: L5 per-package coverage RATCHET FAILED:")
			for _, f := range failures {
				fmt.Fprintf(os.Stderr, "  - %s\n", f)
			}
			os.Exit(1)
		}
		fmt.Println("\ncovgate: L5 per-package coverage ratchet PASSED (no package below its baseline)")
		return
	}

	// Print the full per-package report (audit trail), then the verdict.
	fmt.Printf("%-60s %8s  %s\n", "PACKAGE", "COVER", "FLOOR")
	for _, p := range pkgs {
		fmt.Printf("%-60s %7.1f%%  %.0f%%\n", p.Package, p.Percent, covgate.ThresholdFor(p.Package))
	}

	passed, failures, err := covgate.Evaluate(pkgs)
	if err != nil {
		fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
		os.Exit(1)
	}
	if !passed {
		fmt.Fprintln(os.Stderr, "\ncovgate: L5 per-package coverage gate FAILED:")
		for _, f := range failures {
			fmt.Fprintf(os.Stderr, "  - %s\n", f)
		}
		os.Exit(1)
	}
	fmt.Println("\ncovgate: L5 per-package coverage gate PASSED")
}

// profileReader resolves the profile input: a positional path, else stdin.
// The path is a CLI argument by design (the lane passes coverage.out); this
// is a local dev/CI tool, not a network-exposed surface. (gosec G304/G703)
func profileReader(args []string) (*os.File, error) {
	switch len(args) {
	case 0:
		return os.Stdin, nil
	case 1:
		f, err := os.Open(args[0]) // #nosec G304 G703 -- caller-specified coverage profile path is intentional
		if err != nil {
			return nil, err
		}
		return f, nil
	default:
		return nil, fmt.Errorf("usage: covgate [-ratchet baseline.json | -regen baseline.json] [coverage.out]  (reads stdin if no path)")
	}
}
