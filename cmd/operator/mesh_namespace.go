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
	kubepool "github.com/K8squad/K8squad/pkg/warmpool"
	"github.com/go-logr/logr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// resolveTeamNamespaces lists every Team ONCE and returns a team-name →
// reconciled-squad-namespace (Team.Status.Namespace) map — the single cluster
// snapshot every flagged pool resolves from (one deep-copying List, not one per
// team). Teams whose namespace has not reconciled yet (empty Status.Namespace)
// are absent — an unresolved team, never a guessed placement.
//
// Team is namespaced, so the same name can appear in two home namespaces; a
// name that resolves to DIFFERENT namespaces is AMBIGUOUS and dropped from the
// map (treated unresolved) rather than silently resolved to whichever the
// unordered informer index happened to list first — a mis-placement reported as
// authoritative is worse than an honest skip.
func resolveTeamNamespaces(ctx context.Context, reader client.Reader) (map[string]string, error) {
	var teams ksquadv1alpha1.TeamList
	if err := reader.List(ctx, &teams); err != nil {
		return nil, fmt.Errorf("resolve team namespaces: list teams: %w", err)
	}
	out := make(map[string]string, len(teams.Items))
	ambiguous := map[string]struct{}{}
	for i := range teams.Items {
		t := &teams.Items[i]
		if t.Status.Namespace == "" {
			continue
		}
		if prev, seen := out[t.Name]; seen {
			if prev != t.Status.Namespace {
				ambiguous[t.Name] = struct{}{}
			}
			continue
		}
		out[t.Name] = t.Status.Namespace
	}
	for name := range ambiguous {
		delete(out, name)
	}
	return out, nil
}

// meshPoolBooter is the boot seam bootMeshPools drives: KubeProvisioner.
// BootMeshPool (idempotent create-if-missing) in production, a recorder in
// tests — so the apply-the-resolution logic (that the resolved namespace
// actually reaches the booted spec) is unit-testable without a live cluster.
type meshPoolBooter interface {
	BootMeshPool(ctx context.Context, spec kubepool.MeshPoolSpec) error
}

// meshPoolCleaner deletes a mis-located standing pool pod — the ISI-7692
// migration leg. KubeProvisioner.DeleteMislocatedMeshPool in production.
type meshPoolCleaner interface {
	DeleteMislocatedMeshPool(ctx context.Context, ns, team, role string) error
}

// bootMeshPools resolves each flagged team's own namespace and boots its
// standing pool pod there, deleting any same-named pod left in the fallback
// namespace by an earlier mis-placed boot (ISI-7692 finding 2: the orphan keeps
// heartbeating under the namespace-free (team, role) registry key, so dispatch
// can still route to it). It is idempotent (BootMeshPool/DeleteMislocated are
// create/delete-if-present) and PURE over its seams, so the boot Runnable can
// re-invoke it every tick until every team resolves.
//
// It returns the teams that are NOT done yet — unresolved (and un-fallback-able)
// or failed-to-boot — so the caller retries only those. A team is "done" when
// its pod is booted (including via the single-team fallback); an empty return
// means every flagged pool is placed.
//
// Fallback policy (ISI-7692 finding 3): the KSQUAD_WARM_POOL_NAMESPACE fallback
// re-introduces the defect for multi-team operators (it is one shared ns), and
// a pod booted there can never be claimed anyway — Runs are never written to it
// (intake hard-skips an unreconciled team, never falls back to an env ns). So
// the fallback is honored ONLY when allowFallback is set (the single-team case);
// otherwise an unresolved team SKIPS the boot and is retried, matching the Run
// side's "honest skip, next tick retries".
func bootMeshPools(
	ctx context.Context,
	reader client.Reader,
	booter meshPoolBooter,
	cleaner meshPoolCleaner,
	specs map[string]kubepool.MeshPoolSpec,
	teams []string,
	role, fallbackNS string,
	allowFallback bool,
	log logr.Logger,
) (remaining []string) {
	nsByTeam, err := resolveTeamNamespaces(ctx, reader)
	if err != nil {
		// A List failure is NOT a licence to guess a namespace: leave every
		// team for the next pass rather than fan them all into the fallback ns
		// (the original ISI-7692 defect) with no diagnostic.
		log.Error(err, "AgentMesh pool namespace resolution failed; no pools booted this pass (retrying)", "teams", teams)
		return append([]string(nil), teams...)
	}
	for _, team := range teams {
		podName := kubepool.MeshPoolPodName(team, role)
		spec := specs[podName]
		ns, resolved := nsByTeam[team]
		fromTeamCR := resolved
		if !resolved {
			if allowFallback && fallbackNS != "" {
				ns = fallbackNS
				log.Info("AgentMesh pool namespace unresolved; using single-team KSQUAD_WARM_POOL_NAMESPACE fallback (pool may be mis-placed until the Team reconciles)",
					"team", team, "role", role, "namespace", ns)
			} else {
				log.Info("AgentMesh pool namespace unresolved (Team.Status.Namespace empty/ambiguous); pool NOT booted this pass, retrying — dispatch stays on the legacy path",
					"team", team, "role", role)
				remaining = append(remaining, team)
				continue
			}
		}
		// ISI-7692 finding 2: an earlier operator booted this pool into the
		// shared fallback namespace; create-if-missing in the correct namespace
		// leaves that orphan running and registering, so dispatch can still
		// route to it. Delete it so its coord.mesh_endpoint row TTL-expires and
		// only the co-located pod serves the pool. Best-effort: a failed cleanup
		// does not block booting the correct pod.
		if fallbackNS != "" && ns != fallbackNS {
			if cerr := cleaner.DeleteMislocatedMeshPool(ctx, fallbackNS, team, role); cerr != nil {
				log.Error(cerr, "AgentMesh mis-located pool cleanup failed (orphan may keep registering)", "team", team, "role", role, "staleNamespace", fallbackNS)
			}
		}
		spec.Namespace = ns
		if berr := booter.BootMeshPool(ctx, spec); berr != nil {
			// Loud but non-fatal: a failed pool boot must not take the operator
			// down — the canary's dispatch falls back to the legacy path while
			// the pool is absent. Retry it next pass.
			log.Error(berr, "AgentMesh pool boot failed; dispatch falls back to the legacy path (retrying)", "team", team, "role", role, "namespace", ns)
			remaining = append(remaining, team)
			continue
		}
		log.Info("AgentMesh standing pool pod booted", "team", team, "role", role, "namespace", ns, "namespaceFromTeamCR", fromTeamCR)
	}
	return remaining
}
