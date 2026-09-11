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
	"syscall"
	"time"

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
	// ToolArgs is the INTERNAL raw-arguments seam for a Kind==EventTool
	// progress (Epic D, plan §2.4): the runtime adapter hands the raw tool
	// call arguments here, and the engine's funnel hashes them onto
	// Tool.ArgsSHA256 before the event is funneled any further — raw args
	// never reach the wire payload, the SSE log, or the telemetry mapper.
	// +optional
	ToolArgs string
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
type osRunner struct {
	// settleQuiet overrides the after-terminal-event quiet window (tests);
	// zero uses osSettleQuietDefault.
	settleQuiet time.Duration
	// killGrace overrides the SIGTERM→SIGKILL grace in the force-settle
	// path (tests); zero uses osSettleKillGraceDefault.
	killGrace time.Duration
}

// NewOSRunner returns the production os/exec-backed Runner.
func NewOSRunner() Runner { return osRunner{} }

const (
	// osSettleQuietDefault is how long stdout must stay silent after a
	// SettleLine terminal event before the runner concludes the runtime's
	// session went idle and force-settles the task (ISI-4224). A continuing
	// session emits its next line when the next step's first part arrives,
	// so the quiet gap equals one model round-trip; three minutes covers
	// slow big-context first-token latency without truncating a session
	// that is still deciding whether to continue, while still bounding the
	// settle latency the M1.6 smoke exposed (Run stuck in Claiming for ~1h
	// until the sweeper fired).
	osSettleQuietDefault = 3 * time.Minute
	// osSettleKillGraceDefault is the SIGTERM→SIGKILL grace when
	// force-settling: a moment for the runtime to flush buffers, then the
	// hard cut — the work already completed, the process is only lingering.
	osSettleKillGraceDefault = 10 * time.Second
)

func (r osRunner) quietWindow() time.Duration {
	if r.settleQuiet > 0 {
		return r.settleQuiet
	}
	return osSettleQuietDefault
}

func (r osRunner) graceWindow() time.Duration {
	if r.killGrace > 0 {
		return r.killGrace
	}
	return osSettleKillGraceDefault
}

func (r osRunner) Run(ctx context.Context, spec runtimes.ExecSpec, emit func(Progress)) (Outcome, error) {
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
	// Own process group: the runtime spawns tool children that inherit the
	// stdout pipe, so a leader-only kill would leave a grandchild holding
	// the write end — no EOF, no settle. The group lets forceSettle cut
	// the whole tree (ISI-4224).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

	// The scan loop runs on its own goroutine so the settle timer (ISI-4224)
	// can fire while Scan blocks on a pipe that may never EOF. scanned
	// closes when the scan ends — process exit and force-settle alike both
	// end here, after which exactly one cmd.Wait follows. activity carries
	// "a line arrived at/after the terminal event": each ping (re)arms the
	// quiet window, so a multi-step session that keeps emitting never
	// force-settles mid-flight.
	scanned := make(chan struct{})
	activity := make(chan struct{}, 16)
	go func() {
		defer close(scanned)
		terminalSeen := false
		scanner := bufio.NewScanner(stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scanner.Scan() {
			line := scanner.Text()
			emit(Progress{
				Kind: a2a.EventMessage,
				Message: &a2a.MessagePayload{
					Role:  "agent",
					Text:  line,
					Trust: "untrusted",
				},
			})
			if spec.SettleLine != nil && !terminalSeen && spec.SettleLine(line) {
				terminalSeen = true
			}
			if terminalSeen {
				select {
				case activity <- struct{}{}:
				default: // coalesce: a reset is a reset
				}
			}
		}
	}()

	var (
		settleTimer *time.Timer
		settleCh    <-chan time.Time
	)
	for {
		select {
		case <-scanned:
			// Fast path (and the only path when SettleLine is nil):
			// stdout EOF'd because the process exited.
			waitErr := cmd.Wait()
			if ctx.Err() != nil {
				// Context cancellation drives the canceled path in the
				// engine; report it as a wait error so the engine's own
				// cancel handling wins.
				return Outcome{}, ctx.Err()
			}
			if waitErr != nil {
				return Outcome{State: a2a.TaskFailed, Reason: waitErr.Error()}, nil
			}
			return Outcome{State: a2a.TaskCompleted}, nil
		case <-activity:
			quiet := r.quietWindow()
			if settleTimer == nil {
				settleTimer = time.NewTimer(quiet)
			} else {
				settleTimer.Reset(quiet)
			}
			settleCh = settleTimer.C
		case <-settleCh:
			// ISI-4224 settle path: the runtime's terminal event was seen,
			// then the stream went quiet for the whole window — the
			// session's work is done, only the process lingers (inotify
			// watcher, fetch retry, …). Terminate it and settle completed.
			if settleTimer != nil {
				settleTimer.Stop()
			}
			if ctx.Err() != nil {
				return Outcome{}, ctx.Err()
			}
			r.forceSettle(cmd, scanned)
			return Outcome{
				State:  a2a.TaskCompleted,
				Reason: "settled on runtime terminal output; process lingered and was terminated",
			}, nil
		}
	}
}

// forceSettle terminates a lingering runtime whose work already completed
// (ISI-4224): SIGTERM to the whole process group first — a grace period for
// the CLI to flush anything it still holds — then SIGKILL. It waits for the
// scanner goroutine to observe the pipe EOF either way, so no emit races the
// Run return and the single cmd.Wait that follows is ordered after the reads
// (os/exec pipe contract). Group signaling is load-bearing: tool children
// inherit the stdout pipe, and a leader-only kill would leave the pipe open.
func (r osRunner) forceSettle(cmd *exec.Cmd, scanned <-chan struct{}) {
	// A process that exited between the timer fire and the signal is fine:
	// the kill errors with ESRCH and scanned is already closing.
	pgid := -cmd.Process.Pid
	_ = syscall.Kill(pgid, syscall.SIGTERM)
	select {
	case <-scanned:
	case <-time.After(r.graceWindow()):
		_ = syscall.Kill(pgid, syscall.SIGKILL)
		<-scanned // a killed group always closes the pipe
	}
	// Reap; the error is irrelevant by construction — WE ended the process,
	// and the task outcome was already decided by the terminal event.
	_ = cmd.Wait()
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
