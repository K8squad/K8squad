/*
Copyright 2026 The K8sAgentMesh Authors.

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

package mesh

import (
	"context"
	"fmt"
	"strings"
)

// TeamFlag decides whether a team's dispatches take the mesh route leg
// (chapter §5.2 D3.2, strangler S3, per-team canary D9.3; ISI-5822 P1.1;
// lifted P3.4). Hosts bind their env/config; tests bind a fake.
//
// The dispatch transport becomes pluggable exactly here (§13.2 I4): spawn
// sandbox (legacy) ∥ route-to-endpoint (the mesh, a standing process looked
// up in the registry). What is KEPT, deliberately, host-side (§5.4 D3.3):
// custody and claim guards, the at-most-once dispatch marker, and every
// run-object stamping (annotations/conditions) — the host wraps Route in
// its dispatcher and marks the run object around the transport, so BOTH
// legs terminate at the host's identical idempotency marker.
type TeamFlag interface {
	// Enabled reports whether team's dispatch should route-to-endpoint.
	Enabled(team string) bool
}

// TeamSet is a set-backed TeamFlag: team names parsed from a
// comma-separated list. An empty set (flag unset) means the mesh leg is off
// cluster-wide — every dispatch takes the legacy path byte-identically.
type TeamSet map[string]struct{}

// NewTeamSet parses "team-a,team-b" (whitespace-tolerant, empties dropped).
func NewTeamSet(csv string) TeamSet {
	set := TeamSet{}
	for _, name := range strings.Split(csv, ",") {
		if name = strings.TrimSpace(name); name != "" {
			set[name] = struct{}{}
		}
	}
	return set
}

// Enabled implements TeamFlag.
func (s TeamSet) Enabled(team string) bool {
	_, ok := s[team]
	return ok
}

// EndpointLookup is the registry read seam the route leg consumes — the
// supervisor-side half of the (team, role) lookup. The production binding
// is the host's Registry; tests bind a fake.
type EndpointLookup interface {
	LookupTeamRole(ctx context.Context, team, role string) ([]Endpoint, error)
}

// RouteConfig assembles the mesh route leg. Nil (or nil Teams/Lookup)
// disables the leg entirely — the pre-mesh operator shape.
type RouteConfig struct {
	Teams  TeamFlag
	Lookup EndpointLookup
}

// Route resolves the mesh task URL for a (team, role) dispatch, or reports
// that the legacy path should run (useMesh=false, err=nil). Rollout is
// per-team (D9.3): the flag gates the leg, and an empty registry result
// falls back to the legacy path — a flagged team only gets mesh when a
// gateway is actually registered and heartbeating, so flag-on with no pool
// is a no-op, and rollback is a flag flip (or gateway deregistration) with
// in-flight runs re-driving safely.
//
// Empty-registry fallback: a flagged team whose pool has no live registered
// endpoint takes the LEGACY path — the canary only routes when a gateway is
// actually serving, which keeps "flag on, mesh broken" from becoming "flag
// on, runs dead" (reversibility is the point of the strangler). A registry
// ERROR, in contrast, is loud: the caller surfaces it before any marker is
// written, so a re-drive retries cleanly.
func Route(ctx context.Context, cfg *RouteConfig, team, role string) (url string, useMesh bool, err error) {
	if cfg == nil || cfg.Teams == nil || cfg.Lookup == nil {
		return "", false, nil
	}
	if team == "" || role == "" || !cfg.Teams.Enabled(team) {
		return "", false, nil
	}
	eps, err := cfg.Lookup.LookupTeamRole(ctx, team, role)
	if err != nil {
		return "", false, fmt.Errorf("mesh route lookup %s/%s: %w", team, role, err)
	}
	if len(eps) == 0 {
		return "", false, nil
	}
	// LookupTeamRole is LRU-ordered; the first live endpoint is the pick.
	// Saturation (bounded queue + scale signal) is handled by the caller:
	// a busy gateway answers 409 and the host's re-drive retries — see
	// IsSaturation.
	return strings.TrimRight(eps[0].Endpoint, "/") + "/task", true, nil
}

// IsSaturation reports whether a mesh-leg submit error is the gateway's
// saturation refusal (§5.3: every standing process busy, band exhausted).
// The gateway answers 409 with a gateway-local sentinel (D3.3 guard #3:
// ErrProcessBusy / ErrNoIdleProcess); an HTTP transport folds status + body
// into the returned error, so the exact status line and one of the sentinel
// texts must appear. (Matched on "409 Conflict", not bare "409" — a port
// number in the folded URL must never read as the status.)
func IsSaturation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	if !strings.Contains(msg, "409 Conflict") {
		return false
	}
	return strings.Contains(msg, ErrProcessBusy.Error()) ||
		strings.Contains(msg, ErrNoIdleProcess.Error())
}
