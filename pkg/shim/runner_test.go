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
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// writeScript materializes an executable /bin/sh script standing in for a
// coding-agent CLI whose stdout behavior the runner must handle (ISI-4224
// regression tests): it can stream events, linger after its terminal event,
// spawn pipe-inheriting children, or exit dirty.
func writeScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fake-runtime.sh")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0o750); err != nil {
		t.Fatalf("write script: %v", err)
	}
	return path
}

// jsonSettleDetector mirrors the opencode adapter's SettleLine (JSON line
// whose type is the runtime's per-step terminal event) without coupling this
// package's tests to the runtimes package internals.
func jsonSettleDetector(terminalType string) func(string) bool {
	return func(line string) bool {
		var ev struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			return false
		}
		return ev.Type == terminalType
	}
}

// collect gathers emitted progress texts.
func collect(out *[]string) func(Progress) {
	return func(p Progress) {
		if p.Kind == a2a.EventMessage && p.Message != nil {
			*out = append(*out, p.Message.Text)
		}
	}
}

// TestOSRunnerSettlesAfterTerminalEventDespiteLingeringProcess is the ISI-4224
// regression: a runtime that prints its terminal event and then keeps a
// background handle alive (here: a child sleep inheriting the stdout pipe)
// must still settle Completed via the quiet-window force path, not wait for a
// stdout EOF that never comes.
func TestOSRunnerSettlesAfterTerminalEventDespiteLingeringProcess(t *testing.T) {
	script := writeScript(t, `
echo '{"type":"text","part":{"type":"text","text":"working"}}'
echo '{"type":"step_finish","part":{"type":"step-finish"}}'
sleep 300
`)
	runner := osRunner{settleQuiet: 300 * time.Millisecond, killGrace: 5 * time.Second}
	var lines []string
	start := time.Now()
	outcome, err := runner.Run(context.Background(), runtimes.ExecSpec{
		Path:       script,
		SettleLine: jsonSettleDetector("step_finish"),
	}, collect(&lines))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted {
		t.Fatalf("state = %s, want TaskCompleted (reason %q)", outcome.State, outcome.Reason)
	}
	if outcome.Reason == "" {
		t.Fatal("settled outcome should carry a reason naming the force path")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("settle took %s; the linger was not force-cut", elapsed)
	}
	if len(lines) != 2 || lines[1] != `{"type":"step_finish","part":{"type":"step-finish"}}` {
		t.Fatalf("emitted lines = %v, want both stdout lines incl. the terminal event", lines)
	}
}

// TestOSRunnerSettleResetsWhileSessionContinues guards the quiet window
// against truncation: a session that keeps emitting after a step_finish (a
// multi-step agent loop) must ride the ordinary EOF fast path, not the force
// settle — every line emitted, no forced-terminate reason.
func TestOSRunnerSettleResetsWhileSessionContinues(t *testing.T) {
	script := writeScript(t, `
echo '{"type":"step_finish","part":{"type":"step-finish"}}'
sleep 0.3
echo '{"type":"step_start","part":{"type":"step-start"}}'
`)
	runner := osRunner{settleQuiet: 2 * time.Second, killGrace: time.Second}
	var lines []string
	outcome, err := runner.Run(context.Background(), runtimes.ExecSpec{
		Path:       script,
		SettleLine: jsonSettleDetector("step_finish"),
	}, collect(&lines))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted {
		t.Fatalf("state = %s, want TaskCompleted", outcome.State)
	}
	if outcome.Reason != "" {
		t.Fatalf("reason = %q, want empty: a self-exiting runtime must not look force-settled", outcome.Reason)
	}
	if len(lines) != 2 {
		t.Fatalf("emitted lines = %v, want both steps' lines", lines)
	}
}

// TestOSRunnerWithoutSettleLineKeepsExitOnlySemantics pins the legacy
// contract for adapters that do not advertise SettleLine: completion stays
// process-exit only, and a canceled context surfaces as ctx.Err().
func TestOSRunnerWithoutSettleLineKeepsExitOnlySemantics(t *testing.T) {
	script := writeScript(t, `
echo '{"type":"step_finish","part":{"type":"step-finish"}}'
exec sleep 300
`)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	runner := osRunner{}
	_, err := runner.Run(ctx, runtimes.ExecSpec{Path: script}, func(Progress) {})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
}

// TestOSRunnerMapsExitCodes asserts the exit-code mapping is unchanged by the
// settle path: zero → completed, non-zero → failed with the wait error.
func TestOSRunnerMapsExitCodes(t *testing.T) {
	ok := writeScript(t, "echo done")
	outcome, err := osRunner{}.Run(context.Background(), runtimes.ExecSpec{Path: ok}, func(Progress) {})
	if err != nil || outcome.State != a2a.TaskCompleted || outcome.Reason != "" {
		t.Fatalf("clean exit: outcome %+v err %v, want completed", outcome, err)
	}

	dirty := writeScript(t, "echo boom; exit 3")
	outcome, err = osRunner{}.Run(context.Background(), runtimes.ExecSpec{Path: dirty}, func(Progress) {})
	if err != nil || outcome.State != a2a.TaskFailed || outcome.Reason == "" {
		t.Fatalf("dirty exit: outcome %+v err %v, want failed with reason", outcome, err)
	}
}

// settleFakeRuntime is a Runtime whose Command points at the test script —
// enough adapter for the engine to drive a real osRunner through the ISI-4224
// scenario end-to-end.
type settleFakeRuntime struct{ path string }

func (s settleFakeRuntime) Type() string                   { return "settle-fake" }
func (s settleFakeRuntime) CLIVersion() string             { return "test" }
func (s settleFakeRuntime) Capabilities() a2a.Capabilities { return a2a.Capabilities{Streaming: true} }
func (s settleFakeRuntime) DefaultModel() a2a.ModelInfo    { return a2a.ModelInfo{ID: "test"} }
func (s settleFakeRuntime) CredentialShape() runtimes.CredentialShape {
	return runtimes.ShapeAPIKey
}
func (s settleFakeRuntime) Command(runtimes.LaunchContext) (runtimes.ExecSpec, error) {
	return runtimes.ExecSpec{Path: s.path, SettleLine: jsonSettleDetector("step_finish")}, nil
}

// TestEngineSettlesLingeringRuntimeToEnd reproduces the M1.6 smoke symptom
// through the full in-process path (ISI-4224): a runtime that emits its
// terminal event and then lingers must still drive the engine to a terminal
// task state and CLOSE the event stream — the close is what ends the
// supervisor's NDJSON response and lets the operator-side dispatcher settle
// the Run instead of sticking in Claiming.
func TestEngineSettlesLingeringRuntimeToEnd(t *testing.T) {
	script := writeScript(t, `
echo '{"type":"step_finish","part":{"type":"step-finish"}}'
sleep 300
`)
	eng := New(settleFakeRuntime{path: script}, osRunner{settleQuiet: 300 * time.Millisecond, killGrace: 5 * time.Second}, Config{})
	const taskID = "run-intake-e14bc9c0"
	if _, err := eng.SubmitTask(context.Background(), a2a.Task{A2ATaskID: taskID}); err != nil {
		t.Fatalf("SubmitTask: %v", err)
	}
	events, err := eng.StreamEvents(context.Background(), taskID, 0)
	if err != nil {
		t.Fatalf("StreamEvents: %v", err)
	}

	var last a2a.Event
	for ev := range events {
		last = ev
	}
	if last.Type != a2a.EventStatus {
		t.Fatalf("last event type = %s, want a terminal status event", last.Type)
	}
	st, ok := last.Payload.(a2a.StatusPayload)
	if !ok {
		t.Fatalf("terminal payload = %T, want a2a.StatusPayload", last.Payload)
	}
	if st.State != a2a.TaskCompleted {
		t.Fatalf("terminal state = %s, want TaskCompleted (reason %q)", st.State, st.Reason)
	}
}
