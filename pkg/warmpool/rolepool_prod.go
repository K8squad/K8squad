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

// rolepool_prod.go — the production adapters of the P1.3 RolePool scheduler
// (ISI-5824): the CR lister that resolves RolePool → (Role, AgentRuntime)
// envelope facts, the coord.mesh_endpoint standing counter, and the
// controller-runtime status writer. Pure logic lives in rolepool.go against
// the small interfaces; only the k8s/Postgres touches live here.
package warmpool

import (
	"context"
	"fmt"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CRLister resolves the cluster's RolePool CRs into scheduler-ready
// RolePoolConfigs: for each CR it dereferences spec.roleRef (model, skills,
// coordinator) and spec.runtimeRef (runtime type → image). A CR whose
// references do not resolve is reported in the unresolved map keyed
// "namespace/name" — a half-configured pool must never boot a wrong-shaped
// pod, and must never block the pools that do resolve.
type CRLister struct {
	Client client.Client
	// ImageFor resolves an AgentRuntime.spec.type to the sandbox image
	// (rundrive.RuntimeImages.ImageFor — injected to keep rundrive out of
	// this package's import graph).
	ImageFor func(runtimeType string) string
}

// ListRolePools lists every RolePool across namespaces and resolves each.
// The team is the CR's namespace (chapter §3.1: the team namespace IS the
// isolation boundary).
func (l *CRLister) ListRolePools(ctx context.Context) ([]RolePoolConfig, map[string]error, error) {
	var list ksquadv1alpha1.RolePoolList
	if err := l.Client.List(ctx, &list); err != nil {
		return nil, nil, fmt.Errorf("warmpool.CRLister: list RolePools: %w", err)
	}

	pools := make([]RolePoolConfig, 0, len(list.Items))
	unresolved := map[string]error{}
	for i := range list.Items {
		rp := &list.Items[i]
		key := rp.Namespace + "/" + rp.Name

		var role ksquadv1alpha1.Role
		if err := l.Client.Get(ctx, client.ObjectKey{Namespace: rp.Namespace, Name: rp.Spec.RoleRef.Name}, &role); err != nil {
			unresolved[key] = fmt.Errorf("resolve roleRef %q: %w", rp.Spec.RoleRef.Name, err)
			continue
		}
		var rt ksquadv1alpha1.AgentRuntime
		if err := l.Client.Get(ctx, client.ObjectKey{Namespace: rp.Namespace, Name: rp.Spec.RuntimeRef.Name}, &rt); err != nil {
			unresolved[key] = fmt.Errorf("resolve runtimeRef %q: %w", rp.Spec.RuntimeRef.Name, err)
			continue
		}
		image := ""
		if l.ImageFor != nil {
			image = l.ImageFor(rt.Spec.Type)
		}

		skills := make([]string, 0, len(role.Spec.DefaultSkills))
		for _, s := range role.Spec.DefaultSkills {
			skills = append(skills, s.Name)
		}

		pools = append(pools, RolePoolConfig{
			Namespace:   rp.Namespace,
			Name:        rp.Name,
			Role:        rp.Spec.RoleRef.Name,
			RuntimeType: rt.Spec.Type,
			Image:       image,
			Min:         int(rp.Spec.Standing.Min),
			Max:         int(rp.Spec.Standing.Max),
			Coordinator: role.Spec.Coordinator,
			Model:       role.Spec.Model,
			Skills:      skills,
			Lifecycle: MeshLifecycleSpec{
				CheckInterval:   rp.Spec.Lifecycle.CheckInterval,
				IdleToHibernate: rp.Spec.Lifecycle.IdleToHibernate,
				HibernateToStop: rp.Spec.Lifecycle.HibernateToStop,
				AlwaysUp:        rp.Spec.Lifecycle.AlwaysUp,
			},
			Generation: rp.Generation,
		})
	}
	return pools, unresolved, nil
}

// RegistryStandingCounter adapts coord.MeshRegistry to the scheduler's
// StandingCounter: the LIVE registered endpoint count for (team, role) —
// TTL-swept, gateway-heartbeated, and authoritative for routing, so it is
// the honest read of what is actually standing.
type RegistryStandingCounter struct {
	Registry *coord.MeshRegistry
}

// StandingProcesses counts the live registered endpoints for the pool.
func (c *RegistryStandingCounter) StandingProcesses(ctx context.Context, team, role string) (int, error) {
	eps, err := c.Registry.LookupTeamRole(ctx, team, role)
	if err != nil {
		return 0, err
	}
	return len(eps), nil
}

// ClientStatusWriter persists the bounded report through the CRD's status
// subresource. It patches ONLY the status fields it owns; a conflict (another
// writer won) is returned as an error and the next tick retries — the report
// is level-triggered, never incrementally merged.
type ClientStatusWriter struct {
	Client client.Client
}

// WriteRolePoolStatus writes one RolePool's status.
func (w *ClientStatusWriter) WriteRolePoolStatus(ctx context.Context, namespace, name string, report RolePoolReport) error {
	var rp ksquadv1alpha1.RolePool
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, &rp); err != nil {
		return fmt.Errorf("warmpool.ClientStatusWriter: get %s/%s: %w", namespace, name, err)
	}
	rp.Status = ksquadv1alpha1.RolePoolStatus{
		Target:             report.Target,
		Pod:                report.Pod,
		ObservedGeneration: report.ObservedGeneration,
	}
	if report.StandingKnown {
		rp.Status.Standing = report.Standing
	}
	if err := w.Client.Status().Update(ctx, &rp); err != nil {
		return fmt.Errorf("warmpool.ClientStatusWriter: update %s/%s status: %w", namespace, name, err)
	}
	return nil
}

// KubeEnsurer adapts KubeProvisioner.BootMeshPool (idempotent
// create-if-missing) to the scheduler's PoolEnsurer seam.
type KubeEnsurer struct{ Provisioner *KubeProvisioner }

// EnsureMeshPool boots the standing gateway pod if absent.
func (e *KubeEnsurer) EnsureMeshPool(ctx context.Context, spec MeshPoolSpec) error {
	return e.Provisioner.BootMeshPool(ctx, spec)
}

// Compile-time proof the adapters satisfy the scheduler seams.
var (
	_ StandingCounter      = (*RegistryStandingCounter)(nil)
	_ RolePoolStatusWriter = (*ClientStatusWriter)(nil)
	_ PoolEnsurer          = (*KubeEnsurer)(nil)
)
