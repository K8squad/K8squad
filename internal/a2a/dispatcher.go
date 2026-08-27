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

package a2a

import (
	"context"
	"fmt"
	"sync"

	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// TaskBuilder resolves the deterministic A2A Task for a Run from its identifiers
// plus the resolved Agent Card + run context (envelope, fence token, model
// route — §3 V1). It is the seam between coord's id-only dispatch port and the
// full Task the wire needs; the reconciler supplies it so this package stays
// free of coord / DB dependencies. It MUST be deterministic on (a2aTaskID,
// runID) — the same inputs rebuild the same Task so a re-drive reattaches (C1).
type TaskBuilder func(ctx context.Context, a2aTaskID, runID string) (wire.Task, error)

// Dispatcher adapts the A2A Client to the coord.TaskDispatcher port (§10.1,
// stories 3.5 + 5.1): the drop-in replacement for the nil ledger-only
// dispatcher (pkg/coord). Submit builds the task, submits it over A2A, and
// follows the SSE stream to the Sink in the BACKGROUND so the reconcile step
// returns once the task is accepted — dispatch STARTS execution, it does not
// block the reconciler until the task settles. It is idempotent on a2aTaskID via
// the transport's reattach (C1); ProdEffects additionally gates it once per task
// via the coord.a2a_dispatch marker.
//
// Dispatcher satisfies coord.TaskDispatcher structurally (Submit(ctx, a2aTaskID,
// runID) error); it does not import coord, keeping the dependency one-way.
type Dispatcher struct {
	// Client is the A2A caller over the chosen transport.
	Client *Client
	// Builder resolves the Task for a Run (required).
	Builder TaskBuilder
	// Sink receives the followed task's SSE events (run_events). Nil discards.
	Sink EventSink
	// SinkFor, when set, resolves the event sink per followed Run instead of
	// the static Sink. The operator's TelemetrySink decorates the run-event
	// path with per-Run labels (Run/Agent — plan §2.4), which one process-wide
	// Sink cannot carry: set SinkFor to wrap each Run's sink with its own
	// labels. Nil falls back to Sink for every Run.
	SinkFor func(runID string) EventSink
	// OnDone, if set, is invoked with the terminal Result (or follow error) when
	// a background follow completes — the seam for run-status settlement and
	// usage metering. It runs on the follow goroutine.
	OnDone func(runID string, res Result, err error)

	follows sync.WaitGroup
}

// Submit builds the task for (a2aTaskID, runID), submits it over A2A, and spawns
// a background follow that streams SSE to the Sink and settles via OnDone. It
// returns once the shim accepts the task (or fails to). The follow runs on a
// context detached from ctx (context.WithoutCancel) so the reconciler returning
// does not truncate an in-flight Run.
func (d *Dispatcher) Submit(ctx context.Context, a2aTaskID, runID string) error {
	if d.Client == nil {
		return fmt.Errorf("a2a: Dispatcher requires a Client")
	}
	if d.Builder == nil {
		return fmt.Errorf("a2a: Dispatcher requires a Builder")
	}
	task, err := d.Builder(ctx, a2aTaskID, runID)
	if err != nil {
		return fmt.Errorf("a2a: build task for run %s: %w", runID, err)
	}
	if task.A2ATaskID == "" {
		task.A2ATaskID = a2aTaskID
	}

	bg := context.WithoutCancel(ctx)
	sess, err := d.Client.Begin(bg, task)
	if err != nil {
		return err
	}

	d.follows.Add(1)
	go func() {
		defer d.follows.Done()
		res, ferr := d.Client.Follow(bg, sess, d.sinkFor(runID))
		if d.OnDone != nil {
			d.OnDone(runID, res, ferr)
		}
	}()
	return nil
}

// Wait blocks until every in-flight background follow finishes. It is the
// graceful-shutdown / test barrier; the controller calls it on drain so a Run's
// SSE is fully flushed to run_events before the process exits.
func (d *Dispatcher) Wait() { d.follows.Wait() }

func (d *Dispatcher) sink() EventSink {
	if d.Sink == nil {
		return DiscardSink
	}
	return d.Sink
}

// sinkFor resolves the follow sink for one Run: SinkFor when set (per-Run
// labels), else the static sink()/DiscardSink default.
func (d *Dispatcher) sinkFor(runID string) EventSink {
	if d.SinkFor != nil {
		return d.SinkFor(runID)
	}
	return d.sink()
}
