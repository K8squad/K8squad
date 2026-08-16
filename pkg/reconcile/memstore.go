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

package reconcile

import "fmt"

// MemStore is an in-memory model of the coord Postgres reconcile_step + fence
// (arch §6.4) — the source of truth the falsification drives. Its Advance models
// the single conditional UPDATE co-committing the §6.5 audit row and §6.6 outbox
// event; a pass that loses the guard commits NEITHER (no phantom event, AC6).
type MemStore struct {
	step    Step
	fence   int64
	auditN  int // §6.5 coord.audit_log rows, one per COMMITTED transition
	outboxN int // §6.6 transactional-outbox events, co-committed with the advance
}

// NewMemStore returns a store positioned at the initial pending step with fence 1
// (the admission state — arch §8 Pending).
func NewMemStore() *MemStore { return &MemStore{step: StepPending, fence: 1} }

// Step returns the committed durable reconcile_step.
func (m *MemStore) Step() Step { return m.step }

// Fence returns the current monotonic fence token.
func (m *MemStore) Fence() int64 { return m.fence }

// SetStep re-points the durable step without a guard (§8 Failed→Claiming retry
// re-entry only).
func (m *MemStore) SetStep(s Step) { m.step = s }

// Reclaim is the §6.3 fence-first reclaim: monotonically raise the fence to fence
// the dead pod BEFORE the failover leader re-enters. Returns false if newFence
// would not raise the token.
func (m *MemStore) Reclaim(newFence int64) bool {
	if newFence > m.fence {
		m.fence = newFence
		return true
	}
	return false
}

// Advance is the conditional step-advance (arch §6.4). Two independent guards:
//   - fence == m.fence — the fence-EQUALITY guard (§6.3): a zombie whose fence
//     was bumped out by a reclaim no longer matches and loses (fencing = safety
//     for the reclaim zombie).
//   - m.step == expected — the step-CAS: on a same-fence split-brain (claim not
//     reclaimed) fencing cannot tell two equal-fence writers apart, so this guard
//     serializes them to exactly one advance (step-CAS = safety for the
//     same-fence competitor).
//
// The §6.5 audit row + §6.6 outbox event are appended INSIDE the committed branch
// only (AC6): a lost pass writes neither. A nil fence models the naive reconciler
// that takes no fence guard.
func (m *MemStore) Advance(expected, next Step, fence *int64) bool {
	if m.step == expected && (fence == nil || *fence == m.fence) {
		m.step = next
		m.auditN++
		m.outboxN++
		return true
	}
	return false
}

// AuditRows returns the count of co-committed §6.5 audit rows.
func (m *MemStore) AuditRows() int { return m.auditN }

// OutboxRows returns the count of co-committed §6.6 outbox events.
func (m *MemStore) OutboxRows() int { return m.outboxN }

var _ Store = (*MemStore)(nil)

// World is an in-memory model of the side-effecting outside world (arch §6.4) —
// the things that must happen at most once. The falsification asserts each of
// these collections has exactly the expected content after every crash window.
type World struct {
	// SandboxBinds holds one entry per sandbox PROVISIONED (claiming_sandbox).
	SandboxBinds []string
	// AgentExecutions holds one entry per REAL agent run started (dispatch).
	AgentExecutions []string
	// Artifacts is the content-addressed artifact set (collect).
	Artifacts map[string]string
	// TerminalTransitions holds one entry per terminal advance.
	TerminalTransitions []Step
}

// NewWorld returns an empty world.
func NewWorld() *World { return &World{Artifacts: map[string]string{}} }

// BindSandbox provisions the warm-pool sandbox for runID. keyed=true reattaches
// to an existing bind for runID (a re-entry never double-provisions, story
// §122-125); keyed=false provisions afresh every call (the naive behaviour).
func (w *World) BindSandbox(runID string, keyed bool) {
	if keyed {
		for _, b := range w.SandboxBinds {
			if b == runID {
				return // reattach to the existing sandbox; NO second provision
			}
		}
	}
	w.SandboxBinds = append(w.SandboxBinds, runID)
}

// Dispatch submits an A2A task to the shim. dedup=true makes the shim reattach to
// an in-flight taskID (no second agent execution); dedup=false always starts a
// new execution — so a naive re-drive double-dispatches.
func (w *World) Dispatch(taskID string, dedup bool) {
	if dedup {
		for _, t := range w.AgentExecutions {
			if t == taskID {
				return // shim reattaches; NO second execution
			}
		}
		w.AgentExecutions = append(w.AgentExecutions, taskID)
		return
	}
	// Naive: no dedup — every pass is a fresh, distinct execution.
	w.AgentExecutions = append(w.AgentExecutions, fmt.Sprintf("%s-attempt-%d", taskID, len(w.AgentExecutions)))
}

// Collect registers an artifact. upsert=true republishes the same
// content-addressed key idempotently (a re-entry is a no-op); upsert=false
// appends a fresh duplicate key each time (the naive behaviour).
func (w *World) Collect(key, content string, upsert bool) {
	if upsert {
		w.Artifacts[key] = content // idempotent republish on UNIQUE(work_item,run,kind)
		return
	}
	w.Artifacts[fmt.Sprintf("%s#%d", key, len(w.Artifacts))] = content // naive: appends dupes
}

// Terminal records a terminal transition.
func (w *World) Terminal(s Step) { w.TerminalTransitions = append(w.TerminalTransitions, s) }

var _ Effects = (*World)(nil)
