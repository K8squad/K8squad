// Command ksquad-codex-exec is the codex-specific entrypoint wrapper (ISI-3646 §3.2, D4).
//
// The K8squad shim delivers the prompt envelope through the environment
// (KSQUAD_SYSTEM_CONTEXT + KSQUAD_INPUT) and never on argv (NFR-SEC1: no prompt in the
// process table). The real `codex exec` reads its prompt from argv or stdin — not from
// those env vars — and ExecSpec carries no Stdin field (pkg/shim/runtimes/runtime.go).
// This wrapper bridges the gap: it assembles the envelope from the environment and runs
//
//	codex exec - <passed-through flags>
//
// with the envelope on stdin (`-` = read prompt from stdin), keeping the prompt off argv.
// The codex adapter's ExecSpec.Path targets this wrapper; ExecSpec.Args are the codex
// flags (--json, -m, -C, -s workspace-write, -a never, …) which pass straight through.
//
// STATUS — packaging stub (ISI-3653 / Codex S1). This minimal, compiling implementation
// exists so the ksquad-shim-codex image builds with `codex` and `ksquad-codex-exec` on
// $PATH (AC1). Full envelope semantics — richer system/context framing, structured error
// mapping, and human-seat $CODEX_HOME/auth.json staging (§3.4) — are wired by ISI-3647
// Story 3 (the S3 wrapper story). Keep the exec-on-stdin contract stable across that work.
package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// codexBin is the pinned static-musl codex CLI staged onto $PATH by Dockerfile.shim
// (RUNTIME=codex). Resolved via $PATH so the image layout owns the location.
const codexBin = "codex"

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "ksquad-codex-exec:", err)
		os.Exit(1)
	}
}

// run assembles the prompt envelope from the shim's env transport and execs
// `codex exec - <args>` with the envelope on stdin, propagating codex's exit code.
func run(args []string) error {
	// #nosec G204 G702 -- codexBin is a fixed binary name; args are the codex flags the
	// registered codex adapter constructs from constants (pkg/shim/runtimes/codex.go),
	// not untrusted input. The prompt rides stdin/env (buildEnvelope), never argv.
	cmd := exec.Command(codexBin, append([]string{"exec", "-"}, args...)...)
	cmd.Stdin = strings.NewReader(buildEnvelope(os.Getenv))
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()

	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			// Preserve codex's exit status so the shim sees the true terminal state.
			os.Exit(ee.ExitCode())
		}
		return err
	}
	return nil
}

// buildEnvelope folds KSQUAD_SYSTEM_CONTEXT (framing, when present) and KSQUAD_INPUT (the
// task) into a single prompt for codex's stdin. getenv is injected for testability.
func buildEnvelope(getenv func(string) string) string {
	var b strings.Builder
	if sys := getenv("KSQUAD_SYSTEM_CONTEXT"); sys != "" {
		b.WriteString(sys)
		b.WriteString("\n\n")
	}
	b.WriteString(getenv("KSQUAD_INPUT"))
	return b.String()
}
