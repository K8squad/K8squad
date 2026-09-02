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

package run

import (
	"context"
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/contextasm"
	"github.com/K8squad/K8squad/pkg/controller/contextsource"
)

// ContextAssemblers builds a per-namespace §8.5 context assembler over the
// production Sources (coord DB + Project CRD + memory service). The Run
// reconciler uses it to assemble+pin the context snapshot at Claiming →
// Running; the dispatcher uses the same seam to re-read the pinned snapshot.
// pkg/controller/contextsource.Deps implements this; tests fake it.
type ContextAssemblers interface {
	// For returns an assembler whose Sources resolve the Project CRD in
	// namespace (coord reads are namespace-agnostic, ADR-001).
	For(namespace string) *contextasm.Assembler
}

// ensureContextSnapshot assembles the §8.5 context envelope and pins its
// resolved-input snapshot on desired.ContextSnapshot (story S1, ISI-3600).
//
// It is a no-op when the side-channel is disabled (nil ContextAssemblers,
// non-regressing), when the Run is not yet being dispatched (phase is not
// Claiming/Running), or when a snapshot is already pinned (immutable for the
// Run's life — a re-drive reuses it, which is what makes resume
// deterministic). A Run with no dispatch agent is skipped (no model → no
// resolvable window); a declared-but-unreadable agent/Project fails closed.
func (r *Reconciler) ensureContextSnapshot(ctx context.Context, run *api.Run, desired *api.RunStatus) error {
	if r.ContextAssemblers == nil {
		return nil
	}
	if desired.Phase != api.RunPhaseClaiming && desired.Phase != api.RunPhaseRunning {
		return nil
	}
	if desired.ContextSnapshot != nil {
		return nil
	}
	if len(run.Spec.Agents) == 0 {
		return nil // no model to key the contextWindow off; nothing to assemble
	}

	agentRef := run.Spec.Agents[0]
	ns := agentRef.Namespace
	if ns == "" {
		ns = run.Namespace
	}
	var agent api.Agent
	if err := r.Get(ctx, client.ObjectKey{Namespace: ns, Name: agentRef.Name}, &agent); err != nil {
		return fmt.Errorf("read Agent %s/%s for run %s/%s context assembly: %w", ns, agentRef.Name, run.Namespace, run.Name, err)
	}

	projNS := run.Spec.ProjectRef.Namespace
	if projNS == "" {
		projNS = run.Namespace
	}
	var project api.Project
	if err := r.Get(ctx, client.ObjectKey{Namespace: projNS, Name: run.Spec.ProjectRef.Name}, &project); err != nil {
		return fmt.Errorf("read Project %s/%s for run %s/%s context assembly: %w", projNS, run.Spec.ProjectRef.Name, run.Namespace, run.Name, err)
	}

	window := contextsource.WindowForModel(agent.Spec.Model)
	res, err := r.ContextAssemblers.For(run.Namespace).Assemble(ctx, contextasm.AssembleRequest{
		Run:           run,
		Agent:         &agent,
		Project:       &project,
		TeamID:        run.Spec.TeamRef.Name,
		ContextWindow: window,
	})
	if err != nil {
		return fmt.Errorf("assemble context for run %s/%s: %w", run.Namespace, run.Name, err)
	}

	snap := res.Snapshot
	now := metav1.Now()
	if r.Now != nil {
		now = r.Now()
	}
	snap.AssembledAt = &now
	desired.ContextSnapshot = snap
	return nil
}
