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
	"sync/atomic"
	"testing"
	"time"

	apiv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// fakeRunner is a deterministic Runner: it emits a fixed progress script, then
// (optionally) blocks until release/ctx before settling on outcome.
type fakeRunner struct {
	calls   int32
	emits   []Progress
	outcome Outcome
	block   chan struct{}
}

func (f *fakeRunner) Run(ctx context.Context, _ runtimes.ExecSpec, emit func(Progress)) (Outcome, error) {
	atomic.AddInt32(&f.calls, 1)
	for _, p := range f.emits {
		emit(p)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return Outcome{}, ctx.Err()
		}
	}
	return f.outcome, nil
}

func msg(text string) Progress {
	return Progress{Kind: a2a.EventMessage, Message: &a2a.MessagePayload{Role: "agent", Text: text, Trust: "untrusted"}}
}

func testEngine(t *testing.T, runner Runner) *Engine {
	t.Helper()
	rt, err := runtimes.Get(apiv1alpha1.RuntimeTypeOpenClaw)
	if err != nil {
		t.Fatal(err)
	}
	return New(rt, runner, Config{
		Identity:    Identity{Name: "coder-1", Squad: "alpha", Project: "demo"},
		Model:       "claude-sonnet-4",
		ShimVersion: "test",
	})
}

func drain(t *testing.T, ch <-chan a2a.Event) []a2a.Event {
	t.Helper()
	var out []a2a.Event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-ch:
			if !ok {
				return out
			}
			out = append(out, ev)
		case <-timeout:
			t.Fatal("timed out draining event stream")
		}
	}
}

// TestSubmitLifecycle drives a full Run and asserts the §3.1 lifecycle over a
// gap-free, monotonic SSE stream (C4): submitted → working → message → completed.
func TestSubmitLifecycle(t *testing.T) {
	runner := &fakeRunner{emits: []Progress{msg("hello"), msg("world")}, outcome: Outcome{State: a2a.TaskCompleted}}
	e := testEngine(t, runner)

	st, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-1", WorkItemID: "wi-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != a2a.TaskSubmitted {
		t.Errorf("initial submit status = %s, want submitted", st.State)
	}

	ch, err := e.StreamEvents(context.Background(), "run-1", 0)
	if err != nil {
		t.Fatal(err)
	}
	events := drain(t, ch)

	// Monotonic gap-free seq starting at 1.
	for i, ev := range events {
		if ev.Seq != uint64(i+1) {
			t.Fatalf("event %d has seq %d, want %d (gap-free monotonic)", i, ev.Seq, i+1)
		}
		if ev.A2ATaskID != "run-1" {
			t.Errorf("event %d task id = %q", i, ev.A2ATaskID)
		}
	}

	states := statusStates(events)
	wantStates := []a2a.TaskState{a2a.TaskSubmitted, a2a.TaskWorking, a2a.TaskCompleted}
	if len(states) != len(wantStates) {
		t.Fatalf("status states = %v, want %v", states, wantStates)
	}
	for i, s := range wantStates {
		if states[i] != s {
			t.Errorf("status[%d] = %s, want %s", i, states[i], s)
		}
	}

	final, _ := e.GetStatus(context.Background(), "run-1")
	if final.State != a2a.TaskCompleted {
		t.Errorf("final state = %s, want completed", final.State)
	}
	if final.LastSeq != uint64(len(events)) {
		t.Errorf("final LastSeq = %d, want %d", final.LastSeq, len(events))
	}
}

// TestSubmitReattachDedup asserts a second submit with a known task id
// reattaches and does NOT start a second execution (C1).
func TestSubmitReattachDedup(t *testing.T) {
	runner := &fakeRunner{emits: []Progress{msg("x")}, outcome: Outcome{State: a2a.TaskCompleted}}
	e := testEngine(t, runner)

	if _, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	ch, _ := e.StreamEvents(context.Background(), "run-1", 0)
	drain(t, ch) // let it settle

	// Re-submit the same id: must be a no-op reattach.
	st, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-1"})
	if err != nil {
		t.Fatal(err)
	}
	if st.State != a2a.TaskCompleted {
		t.Errorf("reattach status = %s, want the terminal completed", st.State)
	}
	if got := atomic.LoadInt32(&runner.calls); got != 1 {
		t.Errorf("runner invoked %d times, want exactly 1 (no second execution, C1)", got)
	}
}

// TestResumeFromSeq asserts a re-stream from a mid-stream seq replays only
// events with seq > fromSeq, gap-free (C4).
func TestResumeFromSeq(t *testing.T) {
	runner := &fakeRunner{emits: []Progress{msg("a"), msg("b"), msg("c")}, outcome: Outcome{State: a2a.TaskCompleted}}
	e := testEngine(t, runner)
	if _, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-1"}); err != nil {
		t.Fatal(err)
	}

	full := drain(t, mustStream(t, e, "run-1", 0))
	if len(full) < 4 {
		t.Fatalf("expected at least 4 events, got %d", len(full))
	}

	resumeFrom := full[2].Seq
	resumed := drain(t, mustStream(t, e, "run-1", resumeFrom))
	if len(resumed) != len(full)-3 {
		t.Fatalf("resume from seq %d yielded %d events, want %d", resumeFrom, len(resumed), len(full)-3)
	}
	for i, ev := range resumed {
		if ev.Seq <= resumeFrom {
			t.Errorf("resumed event %d has seq %d <= fromSeq %d", i, ev.Seq, resumeFrom)
		}
		if ev.Seq != full[3+i].Seq {
			t.Errorf("resumed event %d seq %d != full seq %d (not gap-free)", i, ev.Seq, full[3+i].Seq)
		}
	}
}

// TestCancelIdempotent asserts CancelTask is a no-op on unknown/terminal tasks
// and drains a live task to canceled (C8).
func TestCancelIdempotent(t *testing.T) {
	// Unknown task: no-op success.
	e0 := testEngine(t, &fakeRunner{})
	if err := e0.CancelTask(context.Background(), "nope", "x"); err != nil {
		t.Errorf("cancel unknown task should be a no-op, got %v", err)
	}

	// Live task: block until canceled.
	runner := &fakeRunner{block: make(chan struct{}), outcome: Outcome{State: a2a.TaskCompleted}}
	e := testEngine(t, runner)
	if _, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-1"}); err != nil {
		t.Fatal(err)
	}
	waitForState(t, e, "run-1", a2a.TaskWorking)

	if err := e.CancelTask(context.Background(), "run-1", "operator abort"); err != nil {
		t.Fatal(err)
	}
	st, _ := e.GetStatus(context.Background(), "run-1")
	if st.State != a2a.TaskCanceled {
		t.Fatalf("state after cancel = %s, want canceled", st.State)
	}

	// Second cancel: no-op, state unchanged.
	if err := e.CancelTask(context.Background(), "run-1", "again"); err != nil {
		t.Fatal(err)
	}
	st2, _ := e.GetStatus(context.Background(), "run-1")
	if st2.State != a2a.TaskCanceled || st2.LastSeq != st.LastSeq {
		t.Errorf("second cancel mutated terminal task: %+v -> %+v", st, st2)
	}
}

// TestTwoRuntimesOneSquad is the story 5.5 acceptance: an OpenClaw agent and a
// Hermes agent both run real Runs in the same squad.
func TestTwoRuntimesOneSquad(t *testing.T) {
	const squad = "alpha"
	for _, typ := range []string{apiv1alpha1.RuntimeTypeOpenClaw, apiv1alpha1.RuntimeTypeHermes} {
		rt, err := runtimes.Get(typ)
		if err != nil {
			t.Fatal(err)
		}
		e := New(rt, &fakeRunner{emits: []Progress{msg("working on " + typ)}, outcome: Outcome{State: a2a.TaskCompleted}},
			Config{Identity: Identity{Name: typ + "-agent", Squad: squad, Project: "demo"}, ShimVersion: "test"})

		if _, err := e.SubmitTask(context.Background(), a2a.Task{A2ATaskID: "run-" + typ}); err != nil {
			t.Fatalf("%s submit: %v", typ, err)
		}
		drain(t, mustStream(t, e, "run-"+typ, 0))
		st, _ := e.GetStatus(context.Background(), "run-"+typ)
		if st.State != a2a.TaskCompleted {
			t.Errorf("%s in squad %s ended %s, want completed", typ, squad, st.State)
		}
		if e.AgentCard().Agent.Squad != squad {
			t.Errorf("%s card squad = %q, want %q", typ, e.AgentCard().Agent.Squad, squad)
		}
	}
}

func TestAgentCardPinsProtocolAndCapabilities(t *testing.T) {
	e := testEngine(t, &fakeRunner{})
	card := e.AgentCard()
	if card.SchemaVersion != a2a.SchemaVersion {
		t.Errorf("schemaVersion = %q, want %q", card.SchemaVersion, a2a.SchemaVersion)
	}
	if card.Protocol.A2A == "" || card.Protocol.MCP == "" {
		t.Errorf("card must advertise pinned protocol revisions, got %+v", card.Protocol)
	}
	if !card.Capabilities.Streaming {
		t.Error("openclaw card must advertise streaming")
	}
	if card.Runtime.Type != apiv1alpha1.RuntimeTypeOpenClaw {
		t.Errorf("card runtime type = %q", card.Runtime.Type)
	}
	if card.Model.ID != "claude-sonnet-4" {
		t.Errorf("card model = %q, want the configured override", card.Model.ID)
	}
}

func TestGetStatusUnknownTaskErrors(t *testing.T) {
	e := testEngine(t, &fakeRunner{})
	if _, err := e.GetStatus(context.Background(), "ghost"); err == nil {
		t.Error("GetStatus on unknown task should error")
	}
	if _, err := e.StreamEvents(context.Background(), "ghost", 0); err == nil {
		t.Error("StreamEvents on unknown task should error")
	}
}

// --- helpers ---

func statusStates(events []a2a.Event) []a2a.TaskState {
	var out []a2a.TaskState
	for _, ev := range events {
		if ev.Type == a2a.EventStatus {
			if sp, ok := ev.Payload.(a2a.StatusPayload); ok {
				out = append(out, sp.State)
			}
		}
	}
	return out
}

func mustStream(t *testing.T, e *Engine, id string, from uint64) <-chan a2a.Event {
	t.Helper()
	ch, err := e.StreamEvents(context.Background(), id, from)
	if err != nil {
		t.Fatal(err)
	}
	return ch
}

func waitForState(t *testing.T, e *Engine, id string, want a2a.TaskState) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		st, err := e.GetStatus(context.Background(), id)
		if err == nil && st.State == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("task %s never reached %s", id, want)
		case <-time.After(5 * time.Millisecond):
		}
	}
}
