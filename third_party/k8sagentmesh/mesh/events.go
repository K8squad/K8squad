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

// The lifecycle-event contract (P3.6, ISI-6039): the NATS-emission concept of
// the AgentMesh chapter formalized as the module's lifecycle-event seam. The
// Gateway is the SOLE producer; it emits through the host-pluggable emitter
// interface — LifecycleObserver (lifecycle.go) — and this file adds the
// process-state gauge inputs, the other half of the schema. The full event
// schema (events, fields, bounded vocabularies, cardinality rules,
// implementation registry) is documented in EVENTS.md next to this file.
//
// Implementation #1 is the reference host's: k8squad pkg/telemetry/meshobs
// renders the contract onto the OTel spine (mesh.route/mesh.spawn/mesh.wake
// spans, ksquad.mesh.* metrics) — host-side, unchanged in behavior. A NATS
// JetStream emitter is a possible implementation #2 later (not in P3); the
// contract is deliberately transport-agnostic: events carry identity,
// bounded-vocabulary fields and timings, never a telemetry-SDK type.
package mesh

// ProcessStateSnapshot is the process-state gauge input of the lifecycle-event
// contract: a cheap, non-blocking callback returning the count of standing
// processes per lifecycle state (§6, D4.1). The gateway is the producer; an
// emitter host polls it on its own cadence — an OTel gauge's collection cycle
// (implementation #1, RegisterProcessesGauge), a periodic publish for a
// future wire emitter.
//
// The callback MUST be cheap and MUST NOT call into a live process: back it
// with the gateway's in-memory Processes() listing via SnapshotProcessStates,
// never an adapter op. A snapshot MAY omit zero-count states.
type ProcessStateSnapshot func() map[ProcState]int64

// SnapshotProcessStates buckets a Gateway.Processes() listing into the
// ProcessStateSnapshot shape — the canonical gauge-input builder. The
// composition root wires it over the live gateway:
//
//	snapshot := mesh.ProcessStateSnapshot(func() map[mesh.ProcState]int64 {
//		return mesh.SnapshotProcessStates(gw.Processes())
//	})
//
// Stopped slots stay in the count: they are recreate-on-demand capacity, part
// of the pool's shape, not a leak (§8.3).
func SnapshotProcessStates(procs []ProcessInfo) map[ProcState]int64 {
	counts := make(map[ProcState]int64, len(procs))
	for _, p := range procs {
		counts[p.State]++
	}
	return counts
}
