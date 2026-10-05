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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// TestOSRunnerCancelKillsRuntimeProcessGroup is the ISI-4240 regression:
// cancelling the context before any terminal event must kill the runtime's
// whole process group. A child holding the stdout write end would otherwise
// keep the scanner from ever seeing EOF, and with no terminal line the
// settle timer is never armed — Run would wedge until the sweeper instead
// of returning ctx.Err() promptly.
func TestOSRunnerCancelKillsRuntimeProcessGroup(t *testing.T) {
	script := writeScript(t, `
echo '{"type":"text","part":{"type":"text","text":"working"}}'
sleep 300 &
sleep 300
`)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Cancel once the first line has been scanned, so the runtime is
	// mid-flight with a background pipe-holder alive at kill time.
	firstLine := make(chan struct{})
	var once sync.Once
	var lines []string
	emit := func(p Progress) {
		collect(&lines)(p)
		if p.Kind == a2a.EventMessage {
			once.Do(func() { close(firstLine) })
		}
	}
	go func() {
		<-firstLine
		cancel()
	}()
	runner := osRunner{killGrace: 5 * time.Second}
	start := time.Now()
	_, err := runner.Run(ctx, runtimes.ExecSpec{
		Path:       script,
		SettleLine: jsonSettleDetector("step_finish"),
	}, emit)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("cancel took %s; the process group was not killed", elapsed)
	}
	if len(lines) != 1 || lines[0] != `{"type":"text","part":{"type":"text","text":"working"}}` {
		t.Fatalf("emitted lines = %v, want the pre-terminal line", lines)
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

// TestOSRunnerGuardsOversizedEnvString is the ISI-5329 defensive guard: an env
// string at/over the kernel's MAX_ARG_STRLEN (128 KiB) would make exec fail
// with a raw E2BIG. The runner must reject it BEFORE launch with a legible
// TaskFailed reason naming the offending var — never as an opaque launch crash.
func TestOSRunnerGuardsOversizedEnvString(t *testing.T) {
	ok := writeScript(t, "echo done")
	oversized := "KSQUAD_SYSTEM_CONTEXT=" + strings.Repeat("x", maxEnvStrLen)
	outcome, err := osRunner{}.Run(context.Background(),
		runtimes.ExecSpec{Path: ok, Env: []string{oversized}}, func(Progress) {})
	if err != nil {
		t.Fatalf("guard must fail via Outcome, not error: %v", err)
	}
	if outcome.State != a2a.TaskFailed {
		t.Fatalf("oversized env must fail the run; outcome=%+v", outcome)
	}
	if !strings.Contains(outcome.Reason, "KSQUAD_SYSTEM_CONTEXT") || !strings.Contains(outcome.Reason, "MAX_ARG_STRLEN") {
		t.Errorf("reason must name the var and the limit; got %q", outcome.Reason)
	}
}

// TestOSRunnerAllowsNormalEnv guards against a false positive: an env string
// comfortably under the ceiling launches normally.
func TestOSRunnerAllowsNormalEnv(t *testing.T) {
	ok := writeScript(t, "echo done")
	outcome, err := osRunner{}.Run(context.Background(),
		runtimes.ExecSpec{Path: ok, Env: []string{"KSQUAD_INPUT=fix the bug"}}, func(Progress) {})
	if err != nil || outcome.State != a2a.TaskCompleted {
		t.Fatalf("normal env must run; outcome=%+v err=%v", outcome, err)
	}
}

// TestOSRunnerFirstOutputWatchdogFailsLoudly is the ISI-5036 regression: a
// runtime that accepts the task but never reaches its model call (the
// provider accepted the connection and never streamed) emits no stdout at
// all. The runner must not wedge until an external teardown; it must fail
// within the window with a reason naming the stall.
func TestOSRunnerFirstOutputWatchdogFailsLoudly(t *testing.T) {
	script := writeScript(t, "sleep 300\n")
	runner := osRunner{firstOutput: 300 * time.Millisecond, killGrace: 2 * time.Second}
	start := time.Now()
	outcome, err := runner.Run(context.Background(), runtimes.ExecSpec{
		Path:       script,
		SettleLine: jsonSettleDetector("step_finish"),
	}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskFailed {
		t.Fatalf("state = %s, want TaskFailed", outcome.State)
	}
	if !strings.Contains(outcome.Reason, "produced no output") {
		t.Fatalf("reason = %q, want a no-output/stall reason", outcome.Reason)
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Fatalf("watchdog took %s; the stall was not bounded", elapsed)
	}
}

// TestOSRunnerFirstOutputWatchdogStandsDownOnOutput guards the watchdog
// against false positives: a runtime whose first line arrives inside the
// window must ride the normal exit path, not be killed as a stall.
func TestOSRunnerFirstOutputWatchdogStandsDownOnOutput(t *testing.T) {
	script := writeScript(t, "sleep 0.4; echo done\n")
	runner := osRunner{firstOutput: 5 * time.Second, killGrace: time.Second}
	outcome, err := runner.Run(context.Background(), runtimes.ExecSpec{Path: script}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted || outcome.Reason != "" {
		t.Fatalf("outcome = %+v, want completed with no reason", outcome)
	}
}

// TestOSRunnerFirstOutputWatchdogDoesNotSpinAfterFirstLine is the ISI-5038
// review regression: the stand-down branch must nil the closed firstOutput
// channel, because a closed channel is permanently select-ready. Left armed,
// the Run select loop hot-spins from the first stdout line until exit. CPU
// time (RUSAGE_SELF) is measured across a quiet window: a spin burns roughly
// the whole window, the disarmed loop blocks in select.
func TestOSRunnerFirstOutputWatchdogDoesNotSpinAfterFirstLine(t *testing.T) {
	script := writeScript(t, "echo first\nsleep 1\n")
	runner := osRunner{firstOutput: 5 * time.Second, killGrace: 5 * time.Second}

	var before, after syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &before); err != nil {
		t.Fatalf("getrusage before: %v", err)
	}
	outcome, err := runner.Run(context.Background(), runtimes.ExecSpec{Path: script}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted {
		t.Fatalf("state = %s, want TaskCompleted", outcome.State)
	}
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &after); err != nil {
		t.Fatalf("getrusage after: %v", err)
	}
	spin := time.Duration(after.Utime.Nano()-before.Utime.Nano()) +
		time.Duration(after.Stime.Nano()-before.Stime.Nano())
	if spin > 500*time.Millisecond {
		t.Fatalf("Run consumed %s CPU across a ~1s quiet window; the firstOutput stand-down is busy-spinning", spin)
	}
}

// TestNewOSRunnerFirstOutputTimeoutEnv pins the operator-facing override:
// duration and bare-second forms set the window, 0 is the explicit opt-out.
func TestNewOSRunnerFirstOutputTimeoutEnv(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"", osFirstOutputTimeoutDefault},
		{"45s", 45 * time.Second},
		{"30", 30 * time.Second},
		{"0", 0}, // explicit opt-out: window disabled
	}
	for _, tc := range cases {
		t.Setenv("KSQUAD_RUNTIME_FIRST_OUTPUT_TIMEOUT", tc.val)
		got := NewOSRunner().(osRunner).firstOutputWindow()
		if got != tc.want {
			t.Fatalf("val %q: window = %s, want %s", tc.val, got, tc.want)
		}
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

// TestOSRunnerWarmsEndpointBeforeLaunch is the ISI-5085 core: when the spec
// carries a Warmup, the runner issues one tiny OpenAI-compatible completion
// against the endpoint+model BEFORE launching the CLI, so a cold/reloading
// model is resident before the first-output watchdog is armed.
func TestOSRunnerWarmsEndpointBeforeLaunch(t *testing.T) {
	var mu sync.Mutex
	var gotPath, gotModel, gotAuth string
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		hits++
		gotPath = r.URL.Path
		var doc struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&doc)
		gotModel = doc.Model
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":""}}]}`))
	}))
	defer srv.Close()

	script := writeScript(t, "echo done\n")
	outcome, err := osRunner{}.Run(context.Background(), runtimes.ExecSpec{
		Path:   script,
		Warmup: &runtimes.Warmup{Endpoint: srv.URL + "/v1", Model: "qwen3.8:latest"},
	}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted {
		t.Fatalf("outcome = %+v, want completed", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Fatalf("warm requests = %d, want exactly 1", hits)
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("warm path = %q, want /v1/chat/completions", gotPath)
	}
	if gotModel != "qwen3.8:latest" {
		t.Errorf("warm model = %q, want qwen3.8:latest", gotModel)
	}
	if gotAuth != "Bearer ollama" {
		t.Errorf("warm auth = %q, want the ollama placeholder", gotAuth)
	}
}

// TestOSRunnerWarmDisabledSkips pins the KSQUAD_RUNTIME_WARM_TIMEOUT=0
// opt-out: no warm request is issued and the CLI still runs.
func TestOSRunnerWarmDisabledSkips(t *testing.T) {
	var mu sync.Mutex
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	script := writeScript(t, "echo done\n")
	outcome, err := osRunner{warm: -1}.Run(context.Background(), runtimes.ExecSpec{
		Path:   script,
		Warmup: &runtimes.Warmup{Endpoint: srv.URL + "/v1", Model: "qwen3.8:latest"},
	}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskCompleted {
		t.Fatalf("outcome = %+v, want completed", outcome)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("warm requests = %d, want 0 when disabled", hits)
	}
}

// TestOSRunnerWarmFailureFailsLoud guards the fail-closed contract: an
// endpoint that cannot answer the warm request must fail the run with a warm
// reason, and the CLI must never launch.
func TestOSRunnerWarmFailureFailsLoud(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "cold", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	marker := filepath.Join(t.TempDir(), "launched")
	script := writeScript(t, "touch "+marker+"\n")
	outcome, err := osRunner{}.Run(context.Background(), runtimes.ExecSpec{
		Path:   script,
		Warmup: &runtimes.Warmup{Endpoint: srv.URL + "/v1", Model: "qwen3.8:latest"},
	}, func(Progress) {})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if outcome.State != a2a.TaskFailed {
		t.Fatalf("outcome = %+v, want TaskFailed", outcome)
	}
	if !strings.Contains(outcome.Reason, "warm-up failed") {
		t.Errorf("reason = %q, want a warm-up failure", outcome.Reason)
	}
	if _, statErr := os.Stat(marker); !os.IsNotExist(statErr) {
		t.Fatalf("CLI launched (marker exists) despite warm failure; stat err = %v", statErr)
	}
}

// TestNewOSRunnerWarmTimeoutEnv pins the operator-facing override forms:
// duration and bare seconds set the window, 0 disables it.
func TestNewOSRunnerWarmTimeoutEnv(t *testing.T) {
	cases := []struct {
		val  string
		want time.Duration
	}{
		{"", osWarmTimeoutDefault},
		{"120s", 120 * time.Second},
		{"45", 45 * time.Second},
		{"0", 0},
	}
	for _, tc := range cases {
		t.Setenv("KSQUAD_RUNTIME_WARM_TIMEOUT", tc.val)
		got := NewOSRunner().(osRunner).warmWindow()
		if got != tc.want {
			t.Fatalf("val %q: window = %s, want %s", tc.val, got, tc.want)
		}
	}
}

// TestAgentProxyEnv verifies the ISI-5477 forward-proxy injection: the shim
// translates the NAMESPACED KSQUAD_SANDBOX_*_PROXY config into the
// conventional HTTPS_PROXY/HTTP_PROXY/NO_PROXY names (both cases) for the
// agent child, and contributes nothing when the source is unset — the
// pre-ISI-5477 direct-egress posture.
func TestAgentProxyEnv(t *testing.T) {
	t.Run("all unset yields nil", func(t *testing.T) {
		get := func(string) string { return "" }
		if got := agentProxyEnv(get); got != nil {
			t.Fatalf("unset source: want nil, got %v", got)
		}
	})

	t.Run("blank source contributes nothing", func(t *testing.T) {
		get := map[string]string{
			"KSQUAD_SANDBOX_HTTPS_PROXY": "   ",
			"KSQUAD_SANDBOX_NO_PROXY":    "",
		}
		if got := agentProxyEnv(func(k string) string { return get[k] }); got != nil {
			t.Fatalf("blank source: want nil, got %v", got)
		}
	})

	t.Run("renders both cases for each set var", func(t *testing.T) {
		src := map[string]string{
			"KSQUAD_SANDBOX_HTTPS_PROXY": "http://egress.k8squad-system.svc:8888",
			"KSQUAD_SANDBOX_HTTP_PROXY":  "http://egress.k8squad-system.svc:8888",
			"KSQUAD_SANDBOX_NO_PROXY":    "localhost,.svc,10.0.0.185",
		}
		got := agentProxyEnv(func(k string) string { return src[k] })
		want := map[string]string{
			"HTTPS_PROXY": "http://egress.k8squad-system.svc:8888",
			"https_proxy": "http://egress.k8squad-system.svc:8888",
			"HTTP_PROXY":  "http://egress.k8squad-system.svc:8888",
			"http_proxy":  "http://egress.k8squad-system.svc:8888",
			"NO_PROXY":    "localhost,.svc,10.0.0.185",
			"no_proxy":    "localhost,.svc,10.0.0.185",
		}
		gotMap := map[string]string{}
		for _, kv := range got {
			parts := strings.SplitN(kv, "=", 2)
			if len(parts) != 2 {
				t.Fatalf("malformed env entry %q", kv)
			}
			gotMap[parts[0]] = parts[1]
		}
		if len(gotMap) != len(want) {
			t.Fatalf("env count = %d (%v), want %d", len(gotMap), got, len(want))
		}
		for k, v := range want {
			if gotMap[k] != v {
				t.Fatalf("%s = %q, want %q", k, gotMap[k], v)
			}
		}
	})

	t.Run("trims surrounding whitespace", func(t *testing.T) {
		src := map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "  http://p:8888  "}
		got := agentProxyEnv(func(k string) string { return src[k] })
		for _, kv := range got {
			if !strings.HasSuffix(kv, "=http://p:8888") {
				t.Fatalf("expected trimmed value, got %q", kv)
			}
		}
	})
}

// TestOSRunnerInjectsProxyEnvIntoChild is the ISI-5477 end-to-end proof for
// the runner path (not just agentProxyEnv in isolation): with the NAMESPACED
// KSQUAD_SANDBOX_*_PROXY set on the shim's own env, the launched agent child
// must see the CONVENTIONAL HTTPS_PROXY/HTTP_PROXY/NO_PROXY (both cases) that
// bun's fetch / curl / git honor — the whole mechanism that routes external
// model traffic through the per-squad egress proxy.
func TestOSRunnerInjectsProxyEnvIntoChild(t *testing.T) {
	t.Setenv("KSQUAD_SANDBOX_HTTPS_PROXY", "http://egress.k8squad-system.svc:8888")
	t.Setenv("KSQUAD_SANDBOX_HTTP_PROXY", "http://egress.k8squad-system.svc:8888")
	t.Setenv("KSQUAD_SANDBOX_NO_PROXY", "localhost,.svc,10.0.0.185")
	script := writeScript(t, `printf 'HTTPS=%s https=%s HTTP=%s NO=%s no=%s\n' \
  "$HTTPS_PROXY" "$https_proxy" "$HTTP_PROXY" "$NO_PROXY" "$no_proxy"`)

	var mu sync.Mutex
	var lines []string
	outcome, err := osRunner{}.Run(context.Background(),
		runtimes.ExecSpec{Path: script}, func(p Progress) {
			if p.Message != nil {
				mu.Lock()
				lines = append(lines, p.Message.Text)
				mu.Unlock()
			}
		})
	if err != nil || outcome.State != a2a.TaskCompleted {
		t.Fatalf("run: outcome=%+v err=%v", outcome, err)
	}
	got := strings.Join(lines, "\n")
	want := "HTTPS=http://egress.k8squad-system.svc:8888 " +
		"https=http://egress.k8squad-system.svc:8888 " +
		"HTTP=http://egress.k8squad-system.svc:8888 " +
		"NO=localhost,.svc,10.0.0.185 no=localhost,.svc,10.0.0.185"
	if !strings.Contains(got, want) {
		t.Fatalf("child env missing injected proxy vars.\n got: %q\nwant substring: %q", got, want)
	}
}

// TestOSRunnerNoProxyEnvWhenUnset guards the default: with no KSQUAD_SANDBOX_*
// proxy config, the child inherits NO conventional proxy var from this seam
// (the pre-ISI-5477 direct-egress posture).
func TestOSRunnerNoProxyEnvWhenUnset(t *testing.T) {
	// No namespaced source AND no ambient conventional value: the seam must
	// not synthesize one, and must not pass a CI-ambient HTTPS_PROXY through.
	t.Setenv("KSQUAD_SANDBOX_HTTPS_PROXY", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	script := writeScript(t, `printf 'HTTPS=[%s]\n' "$HTTPS_PROXY"`)
	var mu sync.Mutex
	var lines []string
	outcome, err := osRunner{}.Run(context.Background(),
		runtimes.ExecSpec{Path: script}, func(p Progress) {
			if p.Message != nil {
				mu.Lock()
				lines = append(lines, p.Message.Text)
				mu.Unlock()
			}
		})
	if err != nil || outcome.State != a2a.TaskCompleted {
		t.Fatalf("run: outcome=%+v err=%v", outcome, err)
	}
	if got := strings.Join(lines, "\n"); !strings.Contains(got, "HTTPS=[]") {
		t.Fatalf("unset proxy must leave child HTTPS_PROXY empty; got %q", got)
	}
}
