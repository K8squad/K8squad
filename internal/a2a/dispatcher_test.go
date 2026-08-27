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

package a2a_test

import (
	"context"
	"errors"
	"testing"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/coord"
)

// The Dispatcher is the drop-in replacement for coord's nil ledger-only
// dispatcher, so it must satisfy the coord.TaskDispatcher port exactly.
var _ coord.TaskDispatcher = (*clienta2a.Dispatcher)(nil)

func TestDispatcherAsyncFollowSettles(t *testing.T) {
	fs := &fakeShim{
		autoClose: true,
		terminal:  wire.Status{State: wire.TaskCompleted, LastSeq: 3},
		events: []wire.Event{
			statusEvent(1, wire.TaskWorking),
			artifactEvent(2, "s3://d/1"),
			statusEvent(3, wire.TaskCompleted),
		},
	}
	sink := &collectSink{}

	var builtTask, builtRun string
	var done struct {
		runID string
		res   clienta2a.Result
		err   error
	}
	d := &clienta2a.Dispatcher{
		Client: clienta2a.New(clienta2a.NewEngineTransport(fs)),
		Builder: func(_ context.Context, a2aTaskID, runID string) (wire.Task, error) {
			builtTask, builtRun = a2aTaskID, runID
			return wire.Task{A2ATaskID: a2aTaskID, WorkItemID: "wi-1"}, nil
		},
		Sink: sink,
		OnDone: func(runID string, res clienta2a.Result, err error) {
			done.runID, done.res, done.err = runID, res, err
		},
	}

	if err := d.Submit(context.Background(), "run-1", "run-1"); err != nil {
		t.Fatalf("Submit: %v", err)
	}
	d.Wait() // background follow completes (OnDone runs before Done())

	if builtTask != "run-1" || builtRun != "run-1" {
		t.Fatalf("builder got (%q,%q), want (run-1,run-1)", builtTask, builtRun)
	}
	if done.err != nil {
		t.Fatalf("follow err: %v", done.err)
	}
	if done.runID != "run-1" {
		t.Fatalf("OnDone runID = %q, want run-1", done.runID)
	}
	if done.res.Status.State != wire.TaskCompleted {
		t.Fatalf("OnDone state = %q, want completed", done.res.Status.State)
	}
	if len(done.res.Artifacts) != 1 || done.res.Artifacts[0].URI != "s3://d/1" {
		t.Fatalf("OnDone artifacts = %+v", done.res.Artifacts)
	}
	if len(sink.events) != 3 {
		t.Fatalf("sink saw %d events, want 3", len(sink.events))
	}
}

func TestDispatcherBuilderErrorSurfaces(t *testing.T) {
	boom := errors.New("no card")
	d := &clienta2a.Dispatcher{
		Client:  clienta2a.New(clienta2a.NewEngineTransport(&fakeShim{})),
		Builder: func(context.Context, string, string) (wire.Task, error) { return wire.Task{}, boom },
	}
	err := d.Submit(context.Background(), "run-1", "run-1")
	if !errors.Is(err, boom) {
		t.Fatalf("Submit err = %v, want wrap of %v", err, boom)
	}
}

func TestDispatcherRequiresClientAndBuilder(t *testing.T) {
	if err := (&clienta2a.Dispatcher{}).Submit(context.Background(), "t", "r"); err == nil {
		t.Fatalf("Submit with no Client must error")
	}
	d := &clienta2a.Dispatcher{Client: clienta2a.New(clienta2a.NewEngineTransport(&fakeShim{}))}
	if err := d.Submit(context.Background(), "t", "r"); err == nil {
		t.Fatalf("Submit with no Builder must error")
	}
}

// TestDispatcherSinkForResolvesPerRun: the operator decorates the run-event
// path with per-Run labels (TelemetrySink), so SinkFor must be the sink the
// follow uses — one per Run, not the static Sink (ISI-3352).
func TestDispatcherSinkForResolvesPerRun(t *testing.T) {
	fs := &fakeShim{
		autoClose: true,
		events:    []wire.Event{statusEvent(1, wire.TaskCompleted)},
	}
	byRun := map[string][]wire.Event{}
	static := &collectSink{}
	d := &clienta2a.Dispatcher{
		Client: clienta2a.New(clienta2a.NewEngineTransport(fs)),
		Builder: func(_ context.Context, a2aTaskID, _ string) (wire.Task, error) {
			return wire.Task{A2ATaskID: a2aTaskID}, nil
		},
		Sink: static, // must stay untouched when SinkFor is set
		SinkFor: func(runID string) clienta2a.EventSink {
			return clienta2a.SinkFunc(func(_ context.Context, ev wire.Event) error {
				byRun[runID] = append(byRun[runID], ev)
				return nil
			})
		},
	}
	if err := d.Submit(context.Background(), "run-1", "run-1"); err != nil {
		t.Fatalf("Submit run-1: %v", err)
	}
	if err := d.Submit(context.Background(), "run-2", "run-2"); err != nil {
		t.Fatalf("Submit run-2: %v", err)
	}
	d.Wait()

	if len(static.events) != 0 {
		t.Fatalf("static Sink saw %d events, want 0 when SinkFor is set", len(static.events))
	}
	for _, run := range []string{"run-1", "run-2"} {
		if len(byRun[run]) != 1 {
			t.Fatalf("SinkFor(%q) saw %d events, want 1", run, len(byRun[run]))
		}
	}
}
