/*
Copyright 2026 The K8squad Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Command instscore scores an offline OTLP JSON dump against the
// instrumentation-score rule set (ISI-7741 plan, phase 2). It is the $0 CI
// lane twin of cmd/conformance: no live collector, no network, no paid
// credential — the dump is telemetry captured at the collector wire
// boundary, and scoring is pure and deterministic.
//
//	instscore dump.json                  # score every service in the dump
//	instscore -service payments dump.json
//	instscore -json dump.json            # machine-readable report
//
// Exit code 0 means the dump was scored (failing rules are findings in the
// data, not tool failures — the v1 deliverable is report-only); non-zero
// means usage, I/O, or parse failure.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"text/tabwriter"

	"github.com/K8squad/K8squad/pkg/telemetry/instscore"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "instscore:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("instscore", flag.ContinueOnError)
	fs.SetOutput(stdout)
	var (
		serviceName = fs.String("service", "", "score only this service.name (default: every service in the dump)")
		asJSON      = fs.Bool("json", false, "emit the report as JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return fmt.Errorf("usage: instscore [-service name] [-json] dump.json [more-dumps.json ...]")
	}

	acc := instscore.NewAccumulator()
	for _, path := range fs.Args() {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if err := instscore.LoadDumpInto(acc, data); err != nil {
			return fmt.Errorf("load %s: %w", path, err)
		}
	}

	windows := acc.Windows()
	if *serviceName != "" {
		w := acc.Window(*serviceName)
		if w == nil {
			return fmt.Errorf("service %q not present in the dump(s)", *serviceName)
		}
		windows = []*instscore.Window{w}
	}
	if len(windows) == 0 {
		return fmt.Errorf("dump(s) contain no telemetry")
	}

	results := make([]instscore.Result, 0, len(windows))
	for _, w := range windows {
		results = append(results, instscore.Evaluate(w))
	}
	sort.Slice(results, func(i, j int) bool { return results[i].ServiceName < results[j].ServiceName })

	if *asJSON {
		return writeJSON(stdout, results)
	}
	return writeHuman(stdout, results)
}

// report mirrors instscore.Result with stable snake_case field names for
// the machine-readable output.
type report struct {
	Services []serviceReport `json:"services"`
}

type serviceReport struct {
	ServiceName  string        `json:"service_name"`
	Score        float64       `json:"score"`
	Category     string        `json:"category"`
	Implemented  int           `json:"implemented"`
	Total        int           `json:"total"`
	NotEvaluated []string      `json:"not_evaluated"`
	Rules        []ruleVerdict `json:"rules"`
}

type ruleVerdict struct {
	ID       string `json:"id"`
	Status   string `json:"status"`
	Evidence string `json:"evidence"`
}

func writeJSON(w io.Writer, results []instscore.Result) error {
	rep := report{Services: make([]serviceReport, 0, len(results))}
	for _, r := range results {
		sr := serviceReport{
			ServiceName:  r.ServiceName,
			Score:        r.Score,
			Category:     string(r.Category),
			Implemented:  r.Implemented,
			Total:        r.Total,
			NotEvaluated: r.NotEvaluated,
		}
		if sr.NotEvaluated == nil {
			sr.NotEvaluated = []string{}
		}
		for _, v := range r.Verdicts {
			sr.Rules = append(sr.Rules, ruleVerdict{ID: v.Rule, Status: string(v.Status), Evidence: v.Evidence})
		}
		rep.Services = append(rep.Services, sr)
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(rep)
}

func writeHuman(w io.Writer, results []instscore.Result) error {
	for _, r := range results {
		name := r.ServiceName
		if name == "" {
			name = "(no service.name)"
		}
		fmt.Fprintf(w, "service %s\n", name)
		fmt.Fprintf(w, "  score:       %.2f / 100 (%s)\n", r.Score, r.Category)
		fmt.Fprintf(w, "  implemented: %d/%d rules", r.Implemented, r.Total)
		if len(r.NotEvaluated) > 0 {
			fmt.Fprintf(w, " — not evaluated (excluded from the formula): %s", strings.Join(r.NotEvaluated, ", "))
		}
		fmt.Fprintln(w)
		tw := tabwriter.NewWriter(w, 0, 2, 2, ' ', 0)
		for _, v := range r.Verdicts {
			fmt.Fprintf(tw, "    %s\t%s\t%s\n", statusMark(v.Status), v.Rule, firstSentence(v.Evidence))
		}
		tw.Flush()
		fmt.Fprintln(w)
	}
	return nil
}

func statusMark(s instscore.Status) string {
	switch s {
	case instscore.StatusPass:
		return "pass"
	case instscore.StatusFail:
		return "FAIL"
	default:
		return "n/a "
	}
}

// firstSentence trims evidence to its first sentence so the human table
// stays scannable; the JSON report carries the full string. A sentence ends
// at ". " (a bare dot is usually an attribute name like service.name).
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 && i < 100 {
		return s[:i+1]
	}
	if strings.HasSuffix(s, ".") && len(s) <= 100 {
		return s
	}
	if len(s) > 100 {
		return s[:97] + "…"
	}
	return s
}
