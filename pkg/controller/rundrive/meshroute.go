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

// meshroute.go — the AgentMesh route-to-endpoint dispatch leg (chapter §5.2
// D3.2, strangler S3; ISI-5822 P1.1). The dispatch transport becomes
// pluggable exactly here (§13.2 I4): spawn-sandbox (legacy, the bound pod's
// supervisor) ∥ route-to-endpoint (the mesh, a standing process looked up in
// coord.mesh_endpoint). The Reconcile loop stays single and path-agnostic —
// only the transport picks.
//
// What is KEPT, deliberately (§5.4 D3.3):
//   - board custody: coord.claim via AcquireSpecific — unchanged, upstream
//     of the transport;
//   - at-most-once: coord.a2a_dispatch — the marker is written by the
//     dispatcher wrapper AROUND the transport, so BOTH paths terminate at
//     the identical idempotency marker and a re-drive reattaches regardless
//     of which leg executed. This file never touches it.
//
// Rollout is per-team (D9.3): MeshTeamFlag gates the leg, and an empty
// registry result falls back to the legacy path — a flagged team only gets
// mesh when a gateway is actually registered and heartbeating, so flag-on
// with no pool is a no-op, and rollback is a flag flip (or gateway
// deregistration) with in-flight runs re-driving safely.
//
// At P3.7 the selection mechanism — the flag/set, the (team, role) endpoint
// lookup seam, the LRU first-pick and the saturation refusal match — is the
// AgentMesh module's mesh.Route/mesh.IsSaturation (lifted verbatim at P3.4);
// the re-export aliases below keep the host names. What stays k8squad-side on
// purpose: the KSQUAD_MESH_TEAMS env knob, the store binding
// (coord.MeshRegistry via AsMeshRegistry), the Run CR annotation/condition
// surfaces and the at-most-once/claim guards (D3.3).
package rundrive

import (
	"context"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	wire "github.com/K8squad/K8squad/pkg/a2a"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// MeshTeamFlag decides whether a team's Runs take the mesh route leg. The
// default is the env-comma-list set (KSQUAD_MESH_TEAMS, D9.3 per-team
// canary); tests bind a fake. The module's route leg consumes the same
// interface, so the host's flag implementations satisfy both.
type MeshTeamFlag = agentmesh.TeamFlag

// MeshTeamSet is the env-backed MeshTeamFlag: a set of team names parsed
// from a comma-separated list. An empty set (flag unset) means the mesh leg
// is off cluster-wide — every Run takes the legacy path byte-identically.
type MeshTeamSet = agentmesh.TeamSet

// NewMeshTeamSet parses "team-a,team-b" (whitespace-tolerant, empties
// dropped).
func NewMeshTeamSet(csv string) MeshTeamSet {
	return agentmesh.NewTeamSet(csv)
}

// MeshTeamSetFromEnv reads the flag from KSQUAD_MESH_TEAMS.
func MeshTeamSetFromEnv(getenv func(string) string) MeshTeamSet {
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewMeshTeamSet(getenv("KSQUAD_MESH_TEAMS"))
}

// MeshCSV splits a comma-separated env list (whitespace-tolerant, empties
// dropped) — the mesh env knobs' shared parsing (e.g. KSQUAD_MESH_SKILLS).
func MeshCSV(csv string) []string {
	var out []string
	for _, v := range strings.Split(csv, ",") {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// MeshEndpointLookup is the registry read seam the route leg consumes —
// the Supervisor-side half of the (team, role) lookup. The production
// binding is the host store view (AsMeshRegistry over coord.MeshRegistry);
// tests bind a fake. The module's route leg consumes the same interface.
type MeshEndpointLookup = agentmesh.EndpointLookup

// MeshRouteConfig assembles the mesh route leg. Nil (or nil Teams/Lookup)
// disables the leg entirely — the pre-S3 operator shape. The module's
// mesh.Route consumes it directly.
type MeshRouteConfig = agentmesh.RouteConfig

// annotationDispatchPath records on the Run CR which dispatch path executed
// it ("mesh"; absent ⇒ legacy claim). The kill-criteria run-end SLI reads
// this at the terminal edge so failure_rate{path=mesh} is sliced from
// AUTHORITATIVE run-state, not span presence (§13.5 #1,
// k8squad-dt-verify-tailsampling-confound). An annotation is additive — the
// Run-CR contract is frozen for P1 (X4), and this adds no field to spec or
// status.
const annotationDispatchPath = "ksquad.io/dispatch-path"

// ConditionQueuedAtSupervisor is the S4 legible-wait marker (ISI-5831,
// chapter §5.4 D9.1): True means the Run's dispatch was accepted by the mesh
// route leg but every (team, role) pool process is busy — the run sits in the
// Supervisor's re-drive saturation queue, dispatched nowhere until a process
// frees. It distinguishes "queued at the Supervisor" from "executing" so the
// zombie-Claiming sweep (ISI-5541/5542 class, ISI-5438 gate) does not
// mis-fire on a pod-less run that is legitimately waiting on pool capacity
// (a mesh run never boots a sandbox pod). The dispatcher stamps it on a 409
// saturation refusal and clears it on the first delivered dispatch — the same
// discipline as WaitingForEndpointSlot (ISI-5591).
const ConditionQueuedAtSupervisor = "QueuedAtSupervisor"

// isMeshSaturation reports whether a mesh-leg submit error is the saturation
// refusal (§5.3: every standing process busy, band exhausted). The gateway
// answers 409 with the gateway-local sentinel text (D3.3 guard #3 /
// ErrNoIdleProcess); the a2a HTTP transport folds status + body into the
// returned error, so the module's IsSaturation match — "409 Conflict" plus
// one of the sentinel texts — is the contract (matched on "409 Conflict",
// not bare "409": a port number in the folded URL must never read as the
// status).
func isMeshSaturation(err error) bool {
	return agentmesh.IsSaturation(err)
}

// markMeshQueued stamps or clears the QueuedAtSupervisor condition on the
// Run CR behind the task (best-effort, exactly like markMeshDispatch: the
// queue MECHANISM is the re-drive retry, the condition is the legible
// surface the zombie sweep exempts on). queued=true stamps
// Reason=MeshPoolSaturated; queued=false removes a stale marker so a
// delivered run reads as executing again.
func (d *operatorDispatch) markMeshQueued(ctx context.Context, a2aTaskID string, queued bool) {
	c := d.cfg.Client
	if c == nil {
		return
	}
	run, err := d.runByUID(ctx, cleanRunID(a2aTaskID))
	if err != nil {
		return // the dispatch already validated the Run exists; a race here is cosmetic
	}
	if queued {
		patched := run.DeepCopy()
		if meta.SetStatusCondition(&patched.Status.Conditions, metav1.Condition{
			Type:               ConditionQueuedAtSupervisor,
			Status:             metav1.ConditionTrue,
			ObservedGeneration: run.Generation,
			Reason:             "MeshPoolSaturated",
			Message: "Mesh route leg refused (pool saturated): the run is queued at the Supervisor " +
				"and re-drives when a standing process frees. Exempt from the pod-less zombie reap (D9.1).",
		}) {
			_ = c.Status().Patch(ctx, patched, client.MergeFrom(run))
		}
		return
	}
	if meta.FindStatusCondition(run.Status.Conditions, ConditionQueuedAtSupervisor) == nil {
		return
	}
	patched := run.DeepCopy()
	if meta.RemoveStatusCondition(&patched.Status.Conditions, ConditionQueuedAtSupervisor) {
		_ = c.Status().Patch(ctx, patched, client.MergeFrom(run))
	}
}

// meshRoute resolves the mesh task URL for a dispatch, or reports that the
// legacy path should run — the module's route leg (mesh.Route) over the host
// store binding. team/role ride the task identity buildTask already stamped
// (Identity.Squad = TeamRef, Identity.Name = the dispatch agent).
//
// Empty-registry fallback: a flagged team whose pool has no live registered
// endpoint takes the LEGACY path — the canary only routes when a gateway is
// actually serving, which keeps "flag on, mesh broken" from becoming "flag
// on, runs dead" (reversibility is the point of the strangler). A registry
// ERROR, in contrast, is loud: the marker has not been written yet, so the
// re-drive retries cleanly.
func (d *operatorDispatch) meshRoute(ctx context.Context, t wire.Task) (url string, useMesh bool, err error) {
	team, role := t.Identity.Squad, t.Identity.Name
	return agentmesh.Route(ctx, d.cfg.MeshRoute, team, role)
}

// markMeshDispatch stamps the Run CR's dispatch-path annotation so the
// run-end SLI can slice the canary (§13.5 #1). Best-effort by design: a
// failed annotation is an observability gap on ONE label, never a dispatch
// failure — the task already routed and at-most-once does not depend on it.
func (d *operatorDispatch) markMeshDispatch(ctx context.Context, a2aTaskID string) {
	c := d.cfg.Client
	if c == nil {
		return
	}
	run, err := d.runByUID(ctx, cleanRunID(a2aTaskID))
	if err != nil {
		return // the dispatch already validated the Run exists; a race here is cosmetic
	}
	if run.Annotations[annotationDispatchPath] == "mesh" {
		return // idempotent across re-drives/laps
	}
	patch := client.MergeFrom(run.DeepCopy())
	if run.Annotations == nil {
		run.Annotations = map[string]string{}
	}
	run.Annotations[annotationDispatchPath] = "mesh"
	_ = c.Patch(ctx, run, patch)
}
