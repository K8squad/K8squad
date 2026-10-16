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

// meshingress.go — the k8squad binding of the registration Ingress (chapter
// §2.5 seam table, ISI-5822 P1.1; mechanism lifted to the AgentMesh module at
// P3.4): the HTTP surface gateways POST their endpoint sets to is the
// module's mesh.Ingress, a stateless decoder over the mesh.Registry store
// contract. The host keeps what §1A of the AgentMesh chapter draws as host
// territory: the authoritative store (coord.MeshRegistry, the Postgres
// coord.mesh_endpoint table), the operator wiring and the listener.
//
// Surface (mounted under /mesh on the operator's ingress listener; the
// gateway env points KSQUAD_SUPERVISOR_INGRESS_URL at it):
//
//	POST /mesh/register    {pod, team, role, processes:[{endpoint_id,
//	                       endpoint, card}]} — upsert + prune + heartbeat.
//	POST /mesh/deregister  {pod} — drain every endpoint of a pod (clean
//	                       shutdown; the §2.4 unhealthy-gateway drain).
//	GET  /mesh/health      — liveness for the ingress process itself.
//
// On the gateway side the same Registration wire type builds the POST
// (pkg/mesh re-exports it from the module), so there is one contract, not
// two. A gateway that dies stops heartbeating and its endpoints TTL out even
// if this process never hears a deregister.
package rundrive

import (
	"context"
	"fmt"
	"net/http"

	"github.com/K8squad/K8squad/pkg/coord"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// meshRegistryAdapter binds the host store to the module's mesh.Registry
// contract (P3.4's seam; the module owns the interface, every store stays
// host-side). The row shapes are field-identical (coord.MeshEndpoint and
// mesh.Endpoint are both the endpoint_id/team/role/pod/endpoint/agent_card/
// registered_at/last_seen_at row), so the adapter is a pure pass-through —
// zero behavior change by construction.
type meshRegistryAdapter struct {
	store *coord.MeshRegistry
}

// AsMeshRegistry returns the module's Registry view of the host's
// coord.mesh_endpoint store — the one binding used by the ingress, the sweep
// supervisor and the route leg, so all three read the same authoritative
// rows.
func AsMeshRegistry(registry *coord.MeshRegistry) agentmesh.Registry {
	if registry == nil {
		return nil
	}
	return meshRegistryAdapter{store: registry}
}

// Register upserts one endpoint (the module ingress's per-beat upsert).
func (a meshRegistryAdapter) Register(ctx context.Context, ep agentmesh.Endpoint) error {
	return a.store.Register(ctx, meshEndpointFromModule(ep))
}

// HeartbeatPod refreshes last_seen_at for every endpoint a pod's gateway
// registered.
func (a meshRegistryAdapter) HeartbeatPod(ctx context.Context, pod string, endpointIDs []string) (int64, error) {
	return a.store.HeartbeatPod(ctx, pod, endpointIDs)
}

// PrunePod drops every endpoint of pod EXCEPT the listed ids.
func (a meshRegistryAdapter) PrunePod(ctx context.Context, pod string, keepIDs []string) error {
	return a.store.PrunePod(ctx, pod, keepIDs)
}

// DeregisterPod drops every endpoint a pod's gateway registered.
func (a meshRegistryAdapter) DeregisterPod(ctx context.Context, pod string) error {
	return a.store.DeregisterPod(ctx, pod)
}

// LookupTeamRole returns the LIVE endpoints serving (team, role), ordered
// least-recently-seen first.
func (a meshRegistryAdapter) LookupTeamRole(ctx context.Context, team, role string) ([]agentmesh.Endpoint, error) {
	eps, err := a.store.LookupTeamRole(ctx, team, role)
	if err != nil {
		return nil, err
	}
	out := make([]agentmesh.Endpoint, len(eps))
	for i, ep := range eps {
		out[i] = meshEndpointToModule(ep)
	}
	return out, nil
}

// Sweep deletes TTL-expired rows.
func (a meshRegistryAdapter) Sweep(ctx context.Context) (int64, error) {
	return a.store.Sweep(ctx)
}

// ListEndpoints returns EVERY registered endpoint, live and TTL-expired
// alike — the health loop's unfiltered input.
func (a meshRegistryAdapter) ListEndpoints(ctx context.Context) ([]agentmesh.Endpoint, error) {
	eps, err := a.store.ListEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]agentmesh.Endpoint, len(eps))
	for i, ep := range eps {
		out[i] = meshEndpointToModule(ep)
	}
	return out, nil
}

func meshEndpointToModule(ep coord.MeshEndpoint) agentmesh.Endpoint {
	return agentmesh.Endpoint{
		EndpointID:   ep.EndpointID,
		Team:         ep.Team,
		Role:         ep.Role,
		Pod:          ep.Pod,
		Endpoint:     ep.Endpoint,
		AgentCard:    ep.AgentCard,
		RegisteredAt: ep.RegisteredAt,
		LastSeenAt:   ep.LastSeenAt,
	}
}

func meshEndpointFromModule(ep agentmesh.Endpoint) coord.MeshEndpoint {
	return coord.MeshEndpoint{
		EndpointID:   ep.EndpointID,
		Team:         ep.Team,
		Role:         ep.Role,
		Pod:          ep.Pod,
		Endpoint:     ep.Endpoint,
		AgentCard:    ep.AgentCard,
		RegisteredAt: ep.RegisteredAt,
		LastSeenAt:   ep.LastSeenAt,
	}
}

// MeshIngress serves the gateway registration surface: the module's stateless
// Ingress decoder over the host's coord.mesh_endpoint store.
type MeshIngress struct {
	ingress *agentmesh.Ingress
}

// NewMeshIngress binds the ingress to the registry.
func NewMeshIngress(registry *coord.MeshRegistry) (*MeshIngress, error) {
	if registry == nil {
		return nil, fmt.Errorf("rundrive.NewMeshIngress: nil registry")
	}
	in, err := agentmesh.NewIngress(AsMeshRegistry(registry))
	if err != nil {
		return nil, err
	}
	return &MeshIngress{ingress: in}, nil
}

// Handler returns the ingress's HTTP handler (mounted under /mesh/ by
// ServeMeshIngress; the paths below are relative to that mount).
func (in *MeshIngress) Handler() http.Handler {
	return in.ingress.Handler()
}

// ServeMeshIngress runs the ingress HTTP server until ctx is done. addr is
// the operator's ingress listener (KSQUAD_SUPERVISOR_INGRESS_ADDR, default
// :8090); the gateway env points KSQUAD_SUPERVISOR_INGRESS_URL at
// http://<operator-svc>:8090/mesh.
func ServeMeshIngress(ctx context.Context, addr string, in *MeshIngress) error {
	if in == nil {
		return fmt.Errorf("rundrive.ServeMeshIngress: nil ingress")
	}
	return agentmesh.ServeIngress(ctx, addr, in.ingress)
}
