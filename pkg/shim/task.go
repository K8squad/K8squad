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
	"sync"
	"time"

	"github.com/K8squad/K8squad/pkg/a2a"
)

// task is one in-flight A2A task's server-side record: its current §3.1 state
// and its append-only, gap-free SSE event log (the resume source of truth, C4).
type task struct {
	id       string
	workItem string
	now      func() time.Time

	mu      sync.Mutex
	state   a2a.TaskState
	reason  string
	lastSeq uint64

	cancel context.CancelFunc
	stream *taskStream
}

// status returns the current V3 status snapshot.
func (t *task) status() a2a.Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	return a2a.Status{State: t.state, Reason: t.reason, LastSeq: t.lastSeq}
}

// nextEvent allocates the next monotonic seq under the task lock and builds the
// event shell. Callers hold no lock; this method takes t.mu.
func (t *task) nextEvent(typ a2a.EventType, payload any) a2a.Event {
	t.lastSeq++
	return a2a.Event{
		Seq:       t.lastSeq,
		A2ATaskID: t.id,
		TS:        t.now(),
		Type:      typ,
		Payload:   payload,
	}
}

// setState transitions the task and appends a §4 status event. It is a no-op
// once the task is terminal, so a late runtime status cannot resurrect a
// canceled/failed task.
func (t *task) setState(state a2a.TaskState, reason string) {
	// t.mu is held across append so seq assignment (nextEvent) and the
	// stream append are atomic: a racing emit cannot slot a lower-seq event
	// into the log after a higher-seq one (C4 gap-free monotonic ordering).
	// Lock order t.mu→ts.mu is consistent and subscribe never takes t.mu, so
	// no deadlock.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.IsTerminal() {
		return
	}
	t.state = state
	t.reason = reason
	ev := t.nextEvent(a2a.EventStatus, a2a.StatusPayload{State: state, Reason: reason})
	t.stream.append(ev)
}

// terminate settles the task on a terminal state exactly once, appends the
// terminal status event and closes the SSE stream. Repeat calls (e.g. the
// runner completing and CancelTask racing) are no-ops (C8) — the first winner
// owns the terminal state.
func (t *task) terminate(state a2a.TaskState, reason string) {
	// t.mu held across append+finish so the terminal event's seq and its log
	// position are atomic vs a racing emitProgress (see setState). Without
	// this, a concurrent progress emit could append its lower-seq event after
	// the terminal event, replaying out of order on resume.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.IsTerminal() {
		return
	}
	t.state = state
	t.reason = reason
	ev := t.nextEvent(a2a.EventStatus, a2a.StatusPayload{State: state, Reason: reason})
	t.stream.append(ev)
	t.stream.finish()
}

// emitProgress appends one runtime progress unit as a sequenced §4 event. A
// progress emit after the task is terminal is dropped (the stream is closed).
func (t *task) emitProgress(p Progress) {
	// t.mu held across append so this progress event's seq and its log slot
	// are atomic vs a racing terminate (see setState) — preserves C4 ordering.
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.state.IsTerminal() {
		return
	}
	var (
		typ     = p.Kind
		payload any
	)
	switch p.Kind {
	case a2a.EventMessage:
		payload = deref(p.Message)
	case a2a.EventTool:
		payload = deref(p.Tool)
	case a2a.EventUsage:
		payload = deref(p.Usage)
	case a2a.EventArtifactRef:
		payload = deref(p.Artifact)
	case a2a.EventAuthRequired:
		payload = deref(p.Auth)
	default:
		typ = a2a.EventMessage
		payload = a2a.MessagePayload{Role: "agent", Text: "", Trust: "untrusted"}
	}
	ev := t.nextEvent(typ, payload)
	t.stream.append(ev)
}

func deref[T any](p *T) any {
	if p == nil {
		var zero T
		return zero
	}
	return *p
}

// taskStream is the append-only, fan-out SSE log for one task. It buffers every
// event so a late or reconnecting subscriber replays from any seq gap-free (C4)
// and closes all subscribers when the task settles.
type taskStream struct {
	mu   sync.Mutex
	cond *sync.Cond
	log  []a2a.Event
	done bool
}

func newTaskStream() *taskStream {
	ts := &taskStream{}
	ts.cond = sync.NewCond(&ts.mu)
	return ts
}

func (ts *taskStream) append(ev a2a.Event) {
	ts.mu.Lock()
	ts.log = append(ts.log, ev)
	ts.cond.Broadcast()
	ts.mu.Unlock()
}

func (ts *taskStream) finish() {
	ts.mu.Lock()
	ts.done = true
	ts.cond.Broadcast()
	ts.mu.Unlock()
}

// subscribe returns a channel that replays every buffered event with seq >
// fromSeq in order, then tails live events until the task is terminal, then
// closes. It honors ctx cancellation so a disconnected consumer's goroutine
// exits promptly.
func (ts *taskStream) subscribe(ctx context.Context, fromSeq uint64) <-chan a2a.Event {
	ch := make(chan a2a.Event)
	go func() {
		defer close(ch)
		// Wake the cond wait when the caller's context is canceled.
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				ts.mu.Lock()
				ts.cond.Broadcast()
				ts.mu.Unlock()
			case <-stop:
			}
		}()

		i := 0
		for {
			ts.mu.Lock()
			for i < len(ts.log) && ts.log[i].Seq <= fromSeq {
				i++
			}
			for i >= len(ts.log) && !ts.done && ctx.Err() == nil {
				ts.cond.Wait()
			}
			if ctx.Err() != nil {
				ts.mu.Unlock()
				return
			}
			if i >= len(ts.log) && ts.done {
				ts.mu.Unlock()
				return
			}
			ev := ts.log[i]
			i++
			ts.mu.Unlock()

			select {
			case ch <- ev:
			case <-ctx.Done():
				return
			}
		}
	}()
	return ch
}
