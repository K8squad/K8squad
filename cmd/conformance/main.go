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

// Command conformance runs the KSquad A2A shim conformance suite (story 5.6)
// against the registered v1 shim set and prints a verdict, exiting non-zero if
// any runtime fails any check. It is the vendor-facing "runnable conformance
// suite I can execute independently":
//
//	conformance                     # every registered runtime, default lane
//	conformance -runtime opencode   # one runtime
//	conformance -lane ollama        # the $0 Ollama lane (story 5.7/5.8)
//	conformance -json               # machine-readable report
//
// The suite drives the real pkg/shim engine through a scripted runner, so it
// needs no live coding-agent CLI and the Ollama lane runs at $0 with no live
// server — it asserts the model-wire shape, not endpoint reachability.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/K8squad/K8squad/conformance"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "conformance:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout *os.File) error {
	fs := flag.NewFlagSet("conformance", flag.ContinueOnError)
	fs.SetOutput(stdout)
	var (
		runtimeName    = fs.String("runtime", "", "runtime type to certify (default: all registered)")
		laneName       = fs.String("lane", "default", "model-provider lane: default | ollama")
		ollamaEndpoint = fs.String("ollama-endpoint", "", "BYO Ollama endpoint for the ollama lane (default http://ollama:11434/v1)")
		ollamaModel    = fs.String("ollama-model", "", "model id served at the Ollama endpoint (default qwen3)")
		asJSON         = fs.Bool("json", false, "emit the report as JSON")
	)
	if err := fs.Parse(args); err != nil {
		return err
	}

	lane := conformance.Lane(*laneName)
	if lane != conformance.LaneDefault && lane != conformance.LaneOllama {
		return fmt.Errorf("unknown lane %q (want default | ollama)", *laneName)
	}
	opts := conformance.Options{Lane: lane, OllamaEndpoint: *ollamaEndpoint, OllamaModel: *ollamaModel}

	targets := runtimes.Registered()
	if *runtimeName != "" {
		if _, err := runtimes.Get(*runtimeName); err != nil {
			return err
		}
		targets = []string{*runtimeName}
	}

	var reports []conformance.Report
	allOK := true
	for _, name := range targets {
		rt, err := runtimes.Get(name)
		if err != nil {
			return err
		}
		rep := conformance.VerifyRuntime(rt, opts)
		reports = append(reports, rep)
		if !rep.OK() {
			allOK = false
		}
	}

	if *asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(reports); err != nil {
			return err
		}
	} else {
		for _, rep := range reports {
			fmt.Fprint(stdout, rep.String())
		}
		fmt.Fprintf(stdout, "\n%d runtime(s) checked on the %s lane: %s\n", len(reports), lane, verdict(allOK))
	}

	if !allOK {
		os.Exit(1)
	}
	return nil
}

func verdict(ok bool) string {
	if ok {
		return "ALL CONFORMANT"
	}
	return "NON-CONFORMANT"
}
