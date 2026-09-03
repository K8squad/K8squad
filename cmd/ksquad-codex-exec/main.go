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

// Command ksquad-codex-exec is the prompt-delivery wrapper for the ChatGPT
// Codex runtime (Epic ISI-3647, arch §3.2/§5 item 3, seams B/C). Unlike the
// other v1 CLIs, `codex exec` reads its prompt from stdin/argv rather than an
// env envelope; ExecSpec has no Stdin field. This tiny static binary bridges
// that gap: the Codex adapter launches it (with adapter-supplied flags as
// argv) and env-injects the context envelope, exactly as every other runtime
// does. The wrapper reassembles the envelope from KSQUAD_SYSTEM_CONTEXT +
// KSQUAD_INPUT and hands it to `codex exec -` on stdin, so the prompt never
// appears on argv / the process table (NFR-SEC1). It inherits the process env,
// so credentials and model-route vars flow through untouched.
//
// It is also the future home for S9 auth.json staging.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

const (
	// envSystemContext + envInput are the context-envelope pair every KSquad
	// runtime reads its system context + work instruction from (spec §8.5).
	envSystemContext = "KSQUAD_SYSTEM_CONTEXT"
	envInput         = "KSQUAD_INPUT"
	// envCodexBin overrides the codex binary resolved on PATH; used by tests
	// (and any operator that ships codex under a non-default name).
	envCodexBin = "KSQUAD_CODEX_BIN"
)

// buildEnvelope reassembles the single prompt codex reads from stdin out of the
// two context env vars. The system context precedes the concrete instruction,
// separated by a blank line; either half may be empty.
func buildEnvelope(systemContext, input string) string {
	switch {
	case systemContext == "":
		return input
	case input == "":
		return systemContext
	default:
		return systemContext + "\n\n" + input
	}
}

// buildArgs is the codex argv: `exec -` (read the prompt from stdin) followed
// by the adapter-supplied passthrough flags. The prompt is NEVER placed here —
// that is the whole point of the wrapper (NFR-SEC1).
func buildArgs(passthrough []string) []string {
	args := make([]string, 0, len(passthrough)+2)
	args = append(args, "exec", "-")
	return append(args, passthrough...)
}

// run launches codex with the envelope on stdin and returns its exit code. It
// is factored out of main so the exec path is unit-testable with a fake codex.
func run(passthrough []string, getenv func(string) string, stdout, stderr io.Writer) int {
	bin := getenv(envCodexBin)
	if bin == "" {
		bin = "codex"
	}
	envelope := buildEnvelope(getenv(envSystemContext), getenv(envInput))

	// #nosec G204 G702 -- bin is operator-controlled (fixed "codex" or the
	// KSQUAD_CODEX_BIN override); passthrough is the codex adapter's constant
	// flags, not request input. The prompt rides stdin, never argv (NFR-SEC1).
	cmd := exec.Command(bin, buildArgs(passthrough)...)
	cmd.Stdin = strings.NewReader(envelope)
	cmd.Stdout = stdout
	cmd.Stderr = stderr

	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "ksquad-codex-exec: %v\n", err)
		return 1
	}
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
