// Command covgate scores a Go coverage profile against the Story 14.5 (ISI-2744)
// L5 per-package thresholds and exits non-zero (fail-closed) if any package is
// under its floor or the profile is empty. It is the thin CLI over pkg/covgate the
// l5-code-quality.yml lane runs after `go test -race -coverprofile`; all gate LOGIC
// (and its teeth) live in pkg/covgate so this main stays trivial.
//
//	go run ./cmd/covgate coverage.out
//	go run ./cmd/covgate < coverage.out
package main

import (
	"fmt"
	"os"

	"github.com/K8squad/K8squad/pkg/covgate"
)

func main() {
	var r *os.File
	switch len(os.Args) {
	case 1:
		r = os.Stdin
	case 2:
		// The profile path is a CLI argument by design (the lane passes coverage.out);
		// this is a local dev/CI tool, not a network-exposed surface. (gosec G304/G703)
		f, err := os.Open(os.Args[1]) // #nosec G304 G703 -- caller-specified coverage profile path is intentional
		if err != nil {
			fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
			os.Exit(2)
		}
		defer f.Close()
		r = f
	default:
		fmt.Fprintln(os.Stderr, "usage: covgate [coverage.out]  (reads stdin if omitted)")
		os.Exit(2)
	}

	pkgs, err := covgate.ParseProfile(r)
	if err != nil {
		fmt.Fprintf(os.Stderr, "covgate: %v\n", err)
		os.Exit(1)
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
