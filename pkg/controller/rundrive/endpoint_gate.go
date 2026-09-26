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

package rundrive

import (
	"errors"
	"sync"
	"time"
)

// endpoint_gate.go — the per-BYO-endpoint serialization gate (ISI-5037).
//
// Motivation: a shared BYO model endpoint (the LAN Ollama at
// http://10.0.0.185:11434, v0.32.15) serves only ONE long streaming chat
// completion at a time. Concurrent streams are accepted at TCP and queued
// server-side with an UNBOUNDED first-token wait (reproduced: the queued
// request's TTFT equals the incumbent stream's remaining duration). A second
// squad Run racing the same endpoint therefore streamed nothing within the
// shim's first-output watchdog window and died — the ~50% mid-stream run
// failures of ISI-5036. Since the endpoint cannot be made parallel from
// here, the operator serializes Runs per endpoint instead: the second Run
// waits in the drive loop (bounded requeue, legible on the CR) instead of
// dispatching into a stream that can never start in time.
//
// The gate is an in-process, leader-scoped permit map keyed by the resolved,
// normalized endpoint BaseURL (modelendpoint.Resolver already trims trailing
// slashes). In-process is sufficient: the operator is leader-elected
// single-writer (cmd/operator/main.go leader-elect) and reconcile passes are
// serialized, so the only writers are the drive loop and the dispatcher's
// follow goroutines. Correctness under restart is self-healing: the map
// starts empty and ReattachFollows re-runs buildTask for every unsettled
// lap, whose same-run Acquire re-populates the holder set. The narrow race
// (a fresh Run acquiring between leader-elect and re-attach) degrades to the
// pre-gate behavior — the loser waits or the reaper cleans up — never to a
// wrong execution.
//
// Lifecycle:
//   - Acquire: operatorDispatch.buildTask, right after the BYO endpoint
//     resolves (dispatch.go). Idempotent per run — a re-drive, retry lap, or
//     follow re-attach of the SAME run re-acquires its own permit as a no-op.
//   - Release (primary): the dispatcher's OnDone on a CLEANLY terminal
//     follow (cmd/operator/main.go) — the true "agent stopped using the
//     endpoint" moment. A follow ERROR is not proof of completion (the agent
//     may still be streaming), so an errored OnDone keeps the permit.
//   - Release (defensive): the driver's terminal FailEnter / cancelFinish
//     paths, for runs that die or are killed before (or without) a clean
//     follow — covers the acquire-then-never-dispatched and
//     follow-errored-then-budget-exhausted edges. Idempotent.

// errEndpointSlotBusy is the sentinel a buildTask wraps when its run's BYO
// endpoint is fully held by other run(s). It rides the same plumbing as
// errSandboxPending (sticky effects error → driver errors.Is branch) and
// converts to a quiet bounded requeue plus a legible CR condition — never a
// span exception, never a retry-lap burn.
var errEndpointSlotBusy = errors.New("rundrive: BYO model endpoint slot busy")

// endpointSlotWaitDelay paces the requeue of a run waiting on a busy
// endpoint. Longer than continueDelay on purpose: the holder is a full agent
// run (minutes), so a 2s poll would redo the whole buildTask read chain
// hundreds of times for nothing, while a freed slot is picked up within one
// delay — invisible against a multi-minute run.
const endpointSlotWaitDelay = 10 * time.Second

// EndpointGate bounds how many runs may concurrently hold a given BYO
// endpoint. The zero-value-ready constructor takes the per-endpoint slot
// count (1 = strict serialization, the Ollama mitigation default).
type EndpointGate struct {
	mu sync.Mutex
	// max is the per-endpoint slot count (>= 1; NewEndpointGate clamps).
	max int
	// holders maps a normalized endpoint BaseURL to the set of run uids
	// currently holding a slot on it. A run appears at most once per
	// endpoint; empty sets are deleted so the map does not grow unboundedly.
	holders map[string]map[string]struct{}
}

// NewEndpointGate returns a gate allowing maxConcurrent runs per endpoint
// (values < 1 clamp to 1 — strict serialization).
func NewEndpointGate(maxConcurrent int) *EndpointGate {
	if maxConcurrent < 1 {
		maxConcurrent = 1
	}
	return &EndpointGate{max: maxConcurrent, holders: map[string]map[string]struct{}{}}
}

// Acquire takes a slot on endpoint for runID. It is idempotent for a run
// that already holds one (re-drive / retry lap / follow re-attach): the same
// run re-acquiring its own permit is always granted. On refusal it returns
// one current holder's run uid (for the wait message) and ok=false.
func (g *EndpointGate) Acquire(endpoint, runID string) (holder string, ok bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	holders := g.holders[endpoint]
	if _, mine := holders[runID]; mine {
		return runID, true
	}
	if len(holders) >= g.max {
		for h := range holders {
			return h, false
		}
	}
	if holders == nil {
		holders = map[string]struct{}{}
		g.holders[endpoint] = holders
	}
	holders[runID] = struct{}{}
	return runID, true
}

// ReleaseByRun drops every slot runID holds (at most one per endpoint, any
// number of endpoints). Idempotent and safe to call for a run that holds
// nothing — the defensive terminal paths call it unconditionally.
func (g *EndpointGate) ReleaseByRun(runID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for endpoint, holders := range g.holders {
		if _, ok := holders[runID]; !ok {
			continue
		}
		delete(holders, runID)
		if len(holders) == 0 {
			delete(g.holders, endpoint)
		}
	}
}
