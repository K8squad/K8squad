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
package rundrive

import (
	"context"
	"fmt"
	"os"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/pkg/coord"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// MeshTeamFlag decides whether a team's Runs take the mesh route leg. The
// default is the env-comma-list set (KSQUAD_MESH_TEAMS, D9.3 per-team
// canary); tests bind a fake.
type MeshTeamFlag interface {
	// Enabled reports whether team's dispatch should route-to-endpoint.
	Enabled(team string) bool
}

// MeshTeamSet is the env-backed MeshTeamFlag: a set of team names parsed
// from a comma-separated list. An empty set (flag unset) means the mesh leg
// is off cluster-wide — every Run takes the legacy path byte-identically.
type MeshTeamSet map[string]struct{}

// NewMeshTeamSet parses "team-a,team-b" (whitespace-tolerant, empties
// dropped).
func NewMeshTeamSet(csv string) MeshTeamSet {
	set := MeshTeamSet{}
	for _, name := range strings.Split(csv, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

// Enabled implements MeshTeamFlag.
func (s MeshTeamSet) Enabled(team string) bool {
	_, ok := s[team]
	return ok
}

// MeshTeamSetFromEnv reads the flag from KSQUAD_MESH_TEAMS.
func MeshTeamSetFromEnv(getenv func(string) string) MeshTeamSet {
	if getenv == nil {
		getenv = os.Getenv
	}
	return NewMeshTeamSet(getenv("KSQUAD_MESH_TEAMS"))
}

// MeshEndpointLookup is the registry read seam the route leg consumes —
// the Supervisor-side half of the (team, role) lookup. The production
// binding is coord.MeshRegistry; tests bind a fake.
type MeshEndpointLookup interface {
	LookupTeamRole(ctx context.Context, team, role string) ([]coord.MeshEndpoint, error)
}

// MeshRouteConfig assembles the mesh route leg. Nil (or nil Teams/Lookup)
// disables the leg entirely — the pre-S3 operator shape.
type MeshRouteConfig struct {
	Teams  MeshTeamFlag
	Lookup MeshEndpointLookup
}

// annotationDispatchPath records on the Run CR which dispatch path executed
// it ("mesh"; absent ⇒ legacy claim). The kill-criteria run-end SLI reads
// this at the terminal edge so failure_rate{path=mesh} is sliced from
// AUTHORITATIVE run-state, not span presence (§13.5 #1,
// k8squad-dt-verify-tailsampling-confound). An annotation is additive — the
// Run-CR contract is frozen for P1 (X4), and this adds no field to spec or
// status.
const annotationDispatchPath = "ksquad.io/dispatch-path"

// meshRoute resolves the mesh task URL for a dispatch, or reports that the
// legacy path should run. team/role ride the task identity buildTask already
// stamped (Identity.Squad = TeamRef, Identity.Name = the dispatch agent).
//
// Empty-registry fallback: a flagged team whose pool has no live registered
// endpoint takes the LEGACY path — the canary only routes when a gateway is
// actually serving, which keeps "flag on, mesh broken" from becoming "flag
// on, runs dead" (reversibility is the point of the strangler). A registry
// ERROR, in contrast, is loud: the marker has not been written yet, so the
// re-drive retries cleanly.
func (d *operatorDispatch) meshRoute(ctx context.Context, t wire.Task) (url string, useMesh bool, err error) {
	mr := d.cfg.MeshRoute
	if mr == nil || mr.Teams == nil || mr.Lookup == nil {
		return "", false, nil
	}
	team, role := t.Identity.Squad, t.Identity.Name
	if team == "" || role == "" || !mr.Teams.Enabled(team) {
		return "", false, nil
	}
	eps, err := mr.Lookup.LookupTeamRole(ctx, team, role)
	if err != nil {
		return "", false, fmt.Errorf("rundrive: mesh lookup %s/%s: %w", team, role, err)
	}
	if len(eps) == 0 {
		return "", false, nil
	}
	// LookupTeamRole is LRU-ordered; the first live endpoint is the pick.
	// Saturation handling (bounded queue + scale signal) is P1.3; a busy
	// gateway answers 409 and the re-drive retries.
	return strings.TrimRight(eps[0].Endpoint, "/") + "/task", true, nil
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
