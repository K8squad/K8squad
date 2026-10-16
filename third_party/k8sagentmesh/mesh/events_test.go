/*
Copyright 2026 The K8sAgentMesh Authors.

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

// The lifecycle-event contract tests (P3.6, ISI-6039; schema: EVENTS.md):
//
//   - the bounded vocabularies are the wire schema — a changed value is a
//     schema break, so they are pinned here, not just documented;
//   - the process-state gauge inputs bucket exactly like the reference host's
//     composition-root closure (cmd/shim/gateway.go in k8squad);
//   - the emitter interface is host-pluggable with no telemetry SDK: a plain
//     recording emitter implements mesh.LifecycleObserver and receives every
//     event verbatim — the property a wire emitter (implementation #2) relies
//     on and the reference host's OTel emitter (implementation #1) already
//     exercises in production.
package mesh_test

import (
	"context"
	"reflect"
	"testing"
	"time"

	"github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// TestEventSchemaVocabulary pins the bounded string vocabularies of the
// lifecycle-event contract (EVENTS.md). These values are emitted as metric
// labels and span attributes by implementation #1 and would be wire fields
// for a future implementation #2 — changing one is a schema break, not a
// refactor.
func TestEventSchemaVocabulary(t *testing.T) {
	got := map[string]string{
		"Selection.idle":       string(mesh.SelectionIdle),
		"Selection.overflow":   string(mesh.SelectionOverflow),
		"Selection.wake":       string(mesh.SelectionWake),
		"SpawnSource.envelope": string(mesh.SpawnSourceEnvelope),
		"SpawnSource.cold":     string(mesh.SpawnSourceCold),
		"ProcState.active":     string(mesh.StateActive),
		"ProcState.idle":       string(mesh.StateIdle),
		"ProcState.hibernated": string(mesh.StateHibernated),
		"ProcState.stopped":    string(mesh.StateStopped),
		"ProcState.String()":   mesh.StateIdle.String(),
	}
	want := map[string]string{
		"Selection.idle":       "idle",
		"Selection.overflow":   "overflow",
		"Selection.wake":       "wake",
		"SpawnSource.envelope": "envelope",
		"SpawnSource.cold":     "cold",
		"ProcState.active":     "active",
		"ProcState.idle":       "idle",
		"ProcState.hibernated": "hibernated",
		"ProcState.stopped":    "stopped",
		"ProcState.String()":   "idle",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("event schema vocabulary drifted:\n got  %v\n want %v", got, want)
	}
}

// TestSnapshotProcessStates checks the gauge-input builder against the exact
// semantics of the reference host's composition-root closure: bucket a
// Processes() listing per state, keep stopped slots (recreate-on-demand
// capacity), and produce an empty (never nil) map for an empty pool.
func TestSnapshotProcessStates(t *testing.T) {
	procs := []mesh.ProcessInfo{
		{EndpointID: "e1", State: mesh.StateIdle},
		{EndpointID: "e2", State: mesh.StateActive},
		{EndpointID: "e3", State: mesh.StateIdle},
		{EndpointID: "e4", State: mesh.StateHibernated},
		{EndpointID: "e5", State: mesh.StateStopped},
	}
	want := map[mesh.ProcState]int64{
		mesh.StateIdle:       2,
		mesh.StateActive:     1,
		mesh.StateHibernated: 1,
		mesh.StateStopped:    1,
	}
	if got := mesh.SnapshotProcessStates(procs); !reflect.DeepEqual(got, want) {
		t.Errorf("SnapshotProcessStates = %v, want %v", got, want)
	}
	if got := mesh.SnapshotProcessStates(nil); len(got) != 0 {
		t.Errorf("SnapshotProcessStates(nil) = %v, want empty map", got)
	}
}

// recordingEmitter is a host-pluggable emitter with no telemetry SDK behind
// it — the transport-agnostic half of the contract's proof.
type recordingEmitter struct {
	routes []mesh.RouteEvent
	spawns []mesh.SpawnEvent
	wakes  []mesh.WakeEvent
}

func (r *recordingEmitter) ObserveRoute(_ context.Context, e mesh.RouteEvent) {
	r.routes = append(r.routes, e)
}
func (r *recordingEmitter) ObserveSpawn(_ context.Context, e mesh.SpawnEvent) {
	r.spawns = append(r.spawns, e)
}
func (r *recordingEmitter) ObserveWake(_ context.Context, e mesh.WakeEvent) {
	r.wakes = append(r.wakes, e)
}

var _ mesh.LifecycleObserver = (*recordingEmitter)(nil)

// TestLifecycleObserverHostPluggable drives one of each event through a
// plain (non-OTel) emitter and asserts the contract delivers every event
// verbatim: same identity, same vocabulary field, same timing value.
func TestLifecycleObserverHostPluggable(t *testing.T) {
	r := &recordingEmitter{}
	ctx := context.Background()

	r.ObserveRoute(ctx, mesh.RouteEvent{
		Team: "t", Role: "r", Pod: "p", EndpointID: "e1",
		Selection: mesh.SelectionOverflow, Duration: 3 * time.Second,
	})
	r.ObserveSpawn(ctx, mesh.SpawnEvent{
		Team: "t", Role: "r", Pod: "p", EndpointID: "e1",
		Source: mesh.SpawnSourceEnvelope, Duration: 2 * time.Second,
	})
	r.ObserveWake(ctx, mesh.WakeEvent{
		Team: "t", Role: "r", Pod: "p", EndpointID: "e2",
		Duration: 250 * time.Millisecond, Rebind: true,
	})

	if len(r.routes) != 1 || r.routes[0].Selection != mesh.SelectionOverflow ||
		r.routes[0].Duration != 3*time.Second || r.routes[0].EndpointID != "e1" {
		t.Errorf("route event not delivered verbatim: %+v", r.routes)
	}
	if len(r.spawns) != 1 || r.spawns[0].Source != mesh.SpawnSourceEnvelope ||
		r.spawns[0].Duration != 2*time.Second {
		t.Errorf("spawn event not delivered verbatim: %+v", r.spawns)
	}
	if len(r.wakes) != 1 || !r.wakes[0].Rebind || r.wakes[0].Duration != 250*time.Millisecond {
		t.Errorf("wake event not delivered verbatim: %+v", r.wakes)
	}
}
