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

// mesh_namespace.go — the host-side placement resolution for AgentMesh standing
// pool pods (ISI-7692, child of the ISI-7686 follow_error root-cause).
//
// An AgentMesh standing pool pod serves ONE team's Runs. Those Runs cold-boot
// their sandbox into the team's OWN reconciled squad namespace: intake writes
// each Run to Team.Status.Namespace (pkg/controller/rundrive/intake.go), and
// kube.Boot places the sandbox pod in the Run's namespace (ADR-044 step 9,
// §12.1 — per-Run RBAC/NetworkPolicy/quota are namespace-scoped). So a standing
// pool pod is only CLAIMABLE when it is co-located in that same namespace: a
// pod parked in a foreign namespace (the observed defect — every pool booted
// into a single KSQUAD_WARM_POOL_NAMESPACE, the bmad team's ns) is unreachable
// across the namespace boundary for the team's Runs, which then always cold-boot
// (the force-multiplier the parent ISI-7686 found atop the missing-proxy
// ISI-7691 defect). The placement therefore mirrors the Run's: the team's own
// Team.Status.Namespace.
package main

import (
	"context"
	"fmt"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolveMeshPoolNamespace resolves the namespace the AgentMesh standing pool
// pod for teamName must boot in: the team's own reconciled squad namespace
// (Team.Status.Namespace), the SAME namespace that team's Runs cold-boot their
// sandbox pods into.
//
// The resolved team namespace always WINS over fallback — fallback
// (KSQUAD_WARM_POOL_NAMESPACE) is the legacy single-namespace behavior, used
// only when the Team CR cannot be resolved or has not reconciled a namespace
// yet, so existing single-team/dev operators keep working. The boolean return
// reports whether the namespace came from the Team CR (true) or the fallback
// (false) so the caller can log the distinction — a fallback boot may still
// mis-place a multi-team pool and is worth surfacing.
//
// When the team cannot be resolved AND no fallback is set, it errors: booting
// into a guessed namespace is exactly the ISI-7692 defect, so the caller must
// skip the boot rather than pick a wrong namespace.
func resolveMeshPoolNamespace(ctx context.Context, reader client.Reader, teamName, fallback string) (ns string, fromTeamCR bool, err error) {
	var teams ksquadv1alpha1.TeamList
	if listErr := reader.List(ctx, &teams); listErr != nil {
		if fallback != "" {
			return fallback, false, nil
		}
		return "", false, fmt.Errorf("resolve mesh pool namespace for team %q: list teams: %w", teamName, listErr)
	}
	for i := range teams.Items {
		t := &teams.Items[i]
		if t.Name == teamName && t.Status.Namespace != "" {
			return t.Status.Namespace, true, nil
		}
	}
	if fallback != "" {
		return fallback, false, nil
	}
	return "", false, fmt.Errorf(
		"resolve mesh pool namespace: team %q has no reconciled squad namespace "+
			"(Team.Status.Namespace empty) and no KSQUAD_WARM_POOL_NAMESPACE fallback set",
		teamName)
}
