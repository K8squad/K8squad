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
	"sync"
	"testing"
	"time"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/shim"
	"github.com/K8squad/K8squad/pkg/shim/runtimes"
)

// fakeShim is a scripted in-process wire.Shim: it replays a fixed event list on
// StreamEvents and records Submit/Cancel calls. When autoClose is set the stream
// closes after the scripted events (a terminal task); otherwise it holds open
// until ctx is canceled (a long-running task), so the cancel / sink-error paths
// can be exercised deterministically.
type fakeShim struct {
	events    []wire.Event
	terminal  wire.Status
	autoClose bool

	mu          sync.Mutex
	submitCalls int
	canceled    []string
}

func (f *fakeShim) SubmitTask(ctx context.Context, t wire.Task) (wire.Status, error) {
	f.mu.Lock()
	f.submitCalls++
	f.mu.Unlock()
	return wire.Status{State: wire.TaskSubmitted}, nil
}

func (f *fakeShim) StreamEvents(ctx context.Context, taskID string, fromSeq uint64) (<-chan wire.Event, error) {
	ch := make(chan wire.Event)
	go func() {
		defer close(ch)
		for _, ev := range f.events {
			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
		if f.autoClose {
			return
		}
		<-ctx.Done() // hold the task open until the caller lets go
	}()
	return ch, nil
}

func (f *fakeShim) GetStatus(ctx context.Context, taskID string) (wire.Status, error) {
	return f.terminal, nil
}

func (f *fakeShim) CancelTask(ctx context.Context, taskID, reason string) error {
	f.mu.Lock()
	f.canceled = append(f.canceled, reason)
	f.mu.Unlock()
	return nil
}

func (f *fakeShim) GetAgentCard(ctx context.Context) (wire.AgentCard, error) {
	return wire.AgentCard{}, nil
}

func (f *fakeShim) cancelCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.canceled)
}

func statusEvent(seq uint64, state wire.TaskState) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: "run-1", Type: wire.EventStatus, Payload: wire.StatusPayload{State: state}}
}

func msgEvent(seq uint64, text string) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: "run-1", Type: wire.EventMessage, Payload: wire.MessagePayload{Role: "agent", Text: text, Trust: "untrusted"}}
}

func artifactEvent(seq uint64, uri string) wire.Event {
	return wire.Event{Seq: seq, A2ATaskID: "run-1", Type: wire.EventArtifactRef, Payload: wire.ArtifactRef{Kind: "patch", WorkItemID: "wi-1", URI: uri, SHA256: "sha-" + uri}}
}

// collectSink records every event it is handed, in order.
type collectSink struct {
	mu     sync.Mutex
	events []wire.Event
}

func (s *collectSink) Event(ctx context.Context, ev wire.Event) error {
	s.mu.Lock()
	s.events = append(s.events, ev)
	s.mu.Unlock()
	return nil
}

func TestClientDispatchStreamsAndCollectsArtifacts(t *testing.T) {
	fs := &fakeShim{
		autoClose: true,
		terminal:  wire.Status{State: wire.TaskCompleted, LastSeq: 5},
		events: []wire.Event{
			statusEvent(1, wire.TaskSubmitted),
			statusEvent(2, wire.TaskWorking),
			artifactEvent(3, "s3://a/1"),
			artifactEvent(4, "s3://a/2"),
			statusEvent(5, wire.TaskCompleted),
		},
	}
	client := clienta2a.New(clienta2a.NewEngineTransport(fs))
	sink := &collectSink{}

	res, err := client.Dispatch(context.Background(), wire.Task{A2ATaskID: "run-1"}, sink)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if got, want := len(sink.events), 5; got != want {
		t.Fatalf("sink saw %d events, want %d", got, want)
	}
	for i := 1; i < len(sink.events); i++ {
		if sink.events[i].Seq <= sink.events[i-1].Seq {
			t.Fatalf("events not gap-free/ordered at %d: %v", i, sink.events)
		}
	}
	if got, want := len(res.Artifacts), 2; got != want {
		t.Fatalf("collected %d artifacts, want %d", got, want)
	}
	if res.Artifacts[0].URI != "s3://a/1" || res.Artifacts[1].URI != "s3://a/2" {
		t.Fatalf("artifact URIs wrong: %+v", res.Artifacts)
	}
	if res.Artifacts[0].SHA256 == "" {
		t.Fatalf("artifact sha not carried: %+v", res.Artifacts[0])
	}
	if res.Status.State != wire.TaskCompleted {
		t.Fatalf("terminal state = %q, want completed", res.Status.State)
	}
	if res.LastSeq != 5 {
		t.Fatalf("LastSeq = %d, want 5", res.LastSeq)
	}
}

func TestClientDispatchSinkErrorCancels(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fs := &fakeShim{events: []wire.Event{msgEvent(1, "a"), msgEvent(2, "b"), msgEvent(3, "c")}}
	client := clienta2a.New(clienta2a.NewEngineTransport(fs))

	boom := errors.New("sink down")
	var seen int
	sink := clienta2a.SinkFunc(func(_ context.Context, _ wire.Event) error {
		seen++
		if seen == 2 {
			return boom
		}
		return nil
	})

	_, err := client.Dispatch(ctx, wire.Task{A2ATaskID: "run-1"}, sink)
	if !errors.Is(err, boom) {
		t.Fatalf("Dispatch err = %v, want wrap of %v", err, boom)
	}
	if fs.cancelCount() == 0 {
		t.Fatalf("a sink error must cancel the live task; cancel not called")
	}
}

func TestClientDispatchContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	fs := &fakeShim{events: []wire.Event{msgEvent(1, "only")}} // then holds open
	client := clienta2a.New(clienta2a.NewEngineTransport(fs))

	sink := clienta2a.SinkFunc(func(_ context.Context, _ wire.Event) error {
		cancel() // cancel as soon as the first event lands
		return nil
	})

	_, err := client.Dispatch(ctx, wire.Task{A2ATaskID: "run-1"}, sink)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Dispatch err = %v, want context.Canceled", err)
	}
	if fs.cancelCount() == 0 {
		t.Fatalf("a dropped follow must cancel the live task; cancel not called")
	}
}

// fakeRunner is a scripted shim.Runner: it emits the given progress then settles.
type fakeRunner struct {
	emit    []shim.Progress
	outcome shim.Outcome
}

func (r fakeRunner) Run(ctx context.Context, _ runtimes.ExecSpec, emit func(shim.Progress)) (shim.Outcome, error) {
	for _, p := range r.emit {
		emit(p)
	}
	return r.outcome, nil
}

// TestEngineTransportDrivesRealShim proves the client speaks the real pkg/shim
// engine end-to-end (submit → SSE → artifact collection → terminal), not just a
// hand-rolled fake — the story 5.1 southbound path over the in-process seam.
func TestEngineTransportDrivesRealShim(t *testing.T) {
	rt, err := runtimes.Get("openclaw")
	if err != nil {
		t.Skipf("no openclaw runtime registered: %v", err)
	}
	art := &wire.ArtifactRef{Kind: "patch", WorkItemID: "wi-1", URI: "s3://real/1", SHA256: "deadbeef"}
	runner := fakeRunner{
		emit:    []shim.Progress{{Kind: wire.EventArtifactRef, Artifact: art}},
		outcome: shim.Outcome{State: wire.TaskCompleted},
	}
	eng := shim.New(rt, runner, shim.Config{Identity: shim.Identity{Name: "a", Squad: "s", Project: "p"}})

	client := clienta2a.New(clienta2a.NewEngineTransport(eng))
	sink := &collectSink{}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := client.Dispatch(ctx, wire.Task{A2ATaskID: "run-1", WorkItemID: "wi-1"}, sink)
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if res.Status.State != wire.TaskCompleted {
		t.Fatalf("terminal state = %q, want completed", res.Status.State)
	}
	if len(res.Artifacts) != 1 || res.Artifacts[0].URI != "s3://real/1" {
		t.Fatalf("artifacts = %+v, want the one emitted ref", res.Artifacts)
	}
	if len(sink.events) == 0 {
		t.Fatalf("sink received no SSE events from the real engine")
	}
}
