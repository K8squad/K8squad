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
	"context"
	"fmt"

	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// SandboxLiveness is the production lapLiveness (ISI-7543): it reads the Run
// CR by UID through the uncached API reader (the same reader discipline as the
// heartbeat zombie gate — a liveness verdict must not ride a stale cache) and,
// only for a run with a BOUND sandbox (status.sandboxRef), asks the pod side
// whether that pod is still live. Unbound runs (mesh/stdio lanes) report
// not-dead: their follow target is not a pod, so pod evidence would condemn a
// live mesh follow. This is the re-attach-side answer to the bind-time
// errSandboxGone death path (ISI-7538): the crash-restart variant of a dead
// sandbox is caught BEFORE its Submit can re-acquire the BYO EndpointGate
// slot, and the lap is terminalized instead of wedging the queue.
type SandboxLiveness struct {
	// Runs lists Run CRs for the UID match (required; the uncached API
	// reader).
	Runs client.Reader
	// Pods answers the pod-liveness leg (required; a *LiveRuns over the same
	// uncached reader).
	Pods *LiveRuns
}

// DeadSandbox implements lapLiveness. Fail-safe on the unbound and CR-missing
// cases (false, nil): the guard only ever condemns POSITIVE evidence of a
// dead bound sandbox (never reap on a maybe). A list error is returned so the
// caller defers the lap instead of guessing.
func (s *SandboxLiveness) DeadSandbox(ctx context.Context, runID string) (bool, error) {
	if s == nil || s.Runs == nil || s.Pods == nil || runID == "" {
		return false, nil
	}
	runs := &ksquadv1alpha1.RunList{}
	if err := s.Runs.List(ctx, runs); err != nil {
		return false, fmt.Errorf("rundrive.SandboxLiveness: list runs for %s: %w", runID, err)
	}
	for i := range runs.Items {
		if string(runs.Items[i].UID) != runID {
			continue
		}
		ref := runs.Items[i].Status.SandboxRef
		if ref == nil || ref.Name == "" {
			// Mesh / stdio lane: no pod to be dead — pod liveness says
			// nothing about this lap's follow target.
			return false, nil
		}
		live, err := s.Pods.HasLiveSandbox(ctx, runID)
		if err != nil {
			return false, fmt.Errorf("rundrive.SandboxLiveness: pod liveness for run %s: %w", runID, err)
		}
		return !live, nil
	}
	// CR gone: the RunExists gate (ISI-5184) owns that case — not
	// dead-sandbox evidence.
	return false, nil
}
