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

package shim

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// Progress is one normalized unit of runtime output the engine turns into a
// sequenced A2A event (spec §4). A runner emits Progress; the engine assigns
// the seq, timestamp and task id. Exactly one field is set per Progress,
// selected by Kind.
type Progress struct {
	Kind        a2a.EventType
	Message     *a2a.MessagePayload
	Tool        *a2a.ToolPayload
	SkillLoad   *a2a.SkillLoadPayload
	Usage       *a2a.UsagePayload
	Artifact    *a2a.ArtifactRef
	Auth        *a2a.AuthRequiredPayload
	RateLimited *a2a.RateLimitedPayload
}

// Outcome is the terminal result of a runtime process (spec §3.1). State is
// TaskCompleted or TaskFailed; the engine maps a canceled context to
// TaskCanceled itself, so a runner never reports canceled.
type Outcome struct {
	State  a2a.TaskState
	Reason string
}

// Runner executes a runtime ExecSpec to completion, emitting progress via emit
// and returning the terminal Outcome. It is the single I/O seam of the shim:
// the production osRunner uses os/exec, and tests inject a deterministic fake,
// so the engine's lifecycle + sequencing logic is exercised without a real CLI.
type Runner interface {
	Run(ctx context.Context, spec runtimes.ExecSpec, emit func(Progress)) (Outcome, error)
}

// osRunner is the production Runner: it launches the CLI, streams each stdout
// line as an untrusted agent message (F16 — displayed, never executed), and
// maps the exit code onto the terminal state. It deliberately never logs
// spec.Env, which carries the mapped credential (NFR-SEC3).
type osRunner struct{}

// NewOSRunner returns the production os/exec-backed Runner.
func NewOSRunner() Runner { return osRunner{} }

func (osRunner) Run(ctx context.Context, spec runtimes.ExecSpec, emit func(Progress)) (Outcome, error) {
	// Epic C: rendered native MCP configs materialize in the workdir
	// BEFORE the CLI starts (ADR-044: race-free — the runtime reads its
	// config at start, the files exist by then). Credentials inside the
	// rendered documents are env-NAME references, resolved by the CLI
	// process env; nothing secret is written here.
	if err := materializeWorkDirFiles(spec.WorkDir, spec.WorkDirFiles); err != nil {
		return Outcome{}, err
	}
	// #nosec G204 -- spec.Path and spec.Args are constructed entirely by the
	// registered runtime adapter from fixed binary names + constant flags
	// (pkg/shim/runtimes); the untrusted Run input rides ExecSpec.Env, never
	// argv (see envelopeEnv). Launching the coding-agent CLI is the shim's job.
	cmd := exec.CommandContext(ctx, spec.Path, spec.Args...)
	cmd.Dir = spec.WorkDir
	// Start from the ambient environment (the reconciler-injected secret env,
	// PATH, etc.) and layer the runtime's mapped env on top.
	cmd.Env = append(os.Environ(), spec.Env...)

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Outcome{}, fmt.Errorf("shim: stdout pipe: %w", err)
	}
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return Outcome{}, fmt.Errorf("shim: launch %s: %w", spec.Path, err)
	}

	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		emit(Progress{
			Kind: a2a.EventMessage,
			Message: &a2a.MessagePayload{
				Role:  "agent",
				Text:  scanner.Text(),
				Trust: "untrusted",
			},
		})
	}

	waitErr := cmd.Wait()
	if ctx.Err() != nil {
		// Context cancellation drives the canceled path in the engine; report
		// it as a wait error so the engine's own cancel handling wins.
		return Outcome{}, ctx.Err()
	}
	if waitErr != nil {
		return Outcome{State: a2a.TaskFailed, Reason: waitErr.Error()}, nil
	}
	return Outcome{State: a2a.TaskCompleted}, nil
}

// materializeWorkDirFiles writes the adapter-rendered config files into the
// workdir. Path-traversal safe: file names are adapter constants, never Run
// input; the guard keeps that invariant fail-closed against future adapters
// (hidden files like .mcp.json are legal; separators and traversal are not).
func materializeWorkDirFiles(workDir string, files []runtimes.WorkDirFile) error {
	for _, f := range files {
		if f.Name == "" || strings.ContainsAny(f.Name, "/\\") || strings.Contains(f.Name, "..") {
			return fmt.Errorf("shim: refusing workdir file %q: adapter file names must be plain names", f.Name)
		}
		if workDir != "" {
			if err := os.MkdirAll(workDir, 0o750); err != nil {
				return fmt.Errorf("shim: mkdir workdir: %w", err)
			}
			if err := os.WriteFile(filepath.Join(workDir, f.Name), f.Content, 0o600); err != nil {
				return fmt.Errorf("shim: write %s: %w", f.Name, err)
			}
			continue
		}
		if err := os.WriteFile(f.Name, f.Content, 0o600); err != nil {
			return fmt.Errorf("shim: write %s: %w", f.Name, err)
		}
	}
	return nil
}
