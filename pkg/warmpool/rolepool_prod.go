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

// rolepool_prod.go — the production adapters of the P1.3 RolePool policy
// #1 (ISI-5824; P3.5 leak L3, ISI-6038): the CR lister that resolves
// RolePool → (Role, AgentRuntime) envelope facts, the pool-pod provisioner
// that boots the durable unit a plan runs inside, the coord.mesh_endpoint
// standing counter, and the controller-runtime status writer. Pure policy
// logic lives in rolepool.go; only the k8s/Postgres touches live here.
package warmpool

import (
	"context"
	"fmt"
	"time"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// CRLister resolves the cluster's RolePool CRs into policy-ready
// RolePoolConfigs: for each CR it dereferences spec.roleRef (model, skills,
// coordinator) and spec.runtimeRef (runtime type). A CR whose references do
// not resolve is reported in the unresolved map keyed "namespace/name" — a
// half-configured pool must never reach the scheduler, and must never block
// the pools that do resolve.
type CRLister struct {
	Client client.Client
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

		skills := make([]string, 0, len(role.Spec.DefaultSkills))
		for _, s := range role.Spec.DefaultSkills {
			skills = append(skills, s.Name)
		}

		pools = append(pools, RolePoolConfig{
			Namespace:   rp.Namespace,
			Name:        rp.Name,
			Role:        rp.Spec.RoleRef.Name,
			RuntimeType: rt.Spec.Type,
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

// KubeEnsurer adapts KubeProvisioner.BootMeshPool (idempotent
// create-if-missing) to the mesh scheduler's PoolProvisioner seam: it
// turns one generic PoolPlan into the standing pool POD — the k8squad
// durable unit — resolving the sandbox image from the plan's runtime type
// and stamping the operator's pod namespace, Supervisor ingress and
// per-Project workspace PVC the host owns.
type KubeEnsurer struct {
	Provisioner *KubeProvisioner
	// ImageFor resolves the plan's runtime type to the sandbox image
	// (rundrive.RuntimeImages.ImageFor — injected to keep rundrive out of
	// this package's import graph). Empty result fails the boot — a pool
	// pod without an image is a wrong-shaped pod, never a silent default.
	ImageFor func(runtimeType string) string
	// PodNS is the namespace pool pods boot in (the operator's sandbox
	// namespace — NOT the team namespace: the provisioner's RBAC is
	// scoped there).
	PodNS string
	// Ingress is the Supervisor registration ingress stamped on the pod.
	Ingress string
	// WorkPVC is the optional per-Project workspace PVC.
	WorkPVC string
}

// EnsurePool boots the standing gateway pod for the plan if absent.
func (e *KubeEnsurer) EnsurePool(ctx context.Context, plan agentmesh.PoolPlan) error {
	return e.Provisioner.BootMeshPool(ctx, e.specFor(plan))
}

// specFor maps one generic plan onto the k8squad pool-pod spec: identity
// and band from the plan, substrate placement (namespace, ingress, PVC,
// image) from the host wiring the composition root injected here.
func (e *KubeEnsurer) specFor(plan agentmesh.PoolPlan) MeshPoolSpec {
	return MeshPoolSpec{
		Team:        plan.Team,
		Role:        plan.Role,
		Image:       e.ImageFor(plan.Envelope.RuntimeType),
		Namespace:   e.PodNS,
		RuntimeType: plan.Envelope.RuntimeType,
		Model:       plan.Envelope.Model,
		Skills:      plan.Envelope.Skills,
		Min:         plan.Min,
		Max:         plan.Max,
		Lifecycle:   meshLifecycleSpec(plan.Lifecycle),
		IngressURL:  e.Ingress,
		ProjectPVC:  e.WorkPVC,
	}
}

// meshLifecycleSpec converts the plan's generic lifecycle knobs back onto
// the CRD-shaped stamp the pod spec carries (the gateway parses them from
// env; zero durations stay empty — the operator default). The Hibernate
// (S4) arm is deliberately NOT carried: it is the operator-wide rollout
// switch (KSQUAD_MESH_HIBERNATE_ENABLED), never a per-pool fact.
func meshLifecycleSpec(cfg *agentmesh.LifecycleConfig) *MeshLifecycleSpec {
	if cfg == nil {
		return nil
	}
	return &MeshLifecycleSpec{
		CheckInterval:   durationString(cfg.CheckInterval),
		IdleToHibernate: durationString(cfg.IdleToHibernate),
		HibernateToStop: durationString(cfg.HibernateToStop),
		AlwaysUp:        cfg.AlwaysUp,
	}
}

// durationString renders one knob; zero stays empty (the gateway default).
func durationString(d time.Duration) string {
	if d <= 0 {
		return ""
	}
	return d.String()
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
// subresource. It patches ONLY the status fields it owns; a conflict
// (another writer won) is returned as an error and the next tick retries —
// the report is level-triggered, never incrementally merged.
type ClientStatusWriter struct {
	Client client.Client
}

// WritePoolStatus writes one RolePool's status for the plan's pool.
func (w *ClientStatusWriter) WritePoolStatus(ctx context.Context, plan agentmesh.PoolPlan, report agentmesh.PoolStatusReport) error {
	var rp ksquadv1alpha1.RolePool
	if err := w.Client.Get(ctx, client.ObjectKey{Namespace: plan.Team, Name: plan.Name}, &rp); err != nil {
		return fmt.Errorf("warmpool.ClientStatusWriter: get %s/%s: %w", plan.Team, plan.Name, err)
	}
	rp.Status = ksquadv1alpha1.RolePoolStatus{
		Target:             report.Target,
		Pod:                MeshPoolPodName(plan.Team, plan.Role),
		ObservedGeneration: report.ObservedGeneration,
	}
	if report.StandingKnown {
		rp.Status.Standing = report.Standing
	}
	if err := w.Client.Status().Update(ctx, &rp); err != nil {
		return fmt.Errorf("warmpool.ClientStatusWriter: update %s/%s status: %w", plan.Team, plan.Name, err)
	}
	return nil
}

// Compile-time proof the adapters satisfy the mesh scheduler seams (leak
// L3): the host plugs into the module's scheduling interface, never the
// other way round.
var (
	_ agentmesh.SchedulingPolicy = (*RolePoolPolicy)(nil)
	_ agentmesh.PoolProvisioner  = (*KubeEnsurer)(nil)
	_ agentmesh.StandingCounter  = (*RegistryStandingCounter)(nil)
	_ agentmesh.PoolStatusWriter = (*ClientStatusWriter)(nil)
	_ RolePoolLister             = (*CRLister)(nil)
)
