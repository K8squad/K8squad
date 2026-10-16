// Package k8sagentmesh is the root of the K8sAgentMesh module — the
// runtime-agnostic AgentMesh seam lifted out of its reference host k8squad
// (roadmap P3, AgentMesh chapter §1A/§9.3, approved conf 83139335).
//
// The module owns every [generic] surface of the AgentMesh architecture:
//
//   - mesh    — the RuntimeAdapter 7-op contract (Spawn/Register/Route/
//     Hibernate/Wake/Stop/Recreate), the per-pod Gateway process manager,
//     the router, the lifecycle state machine, the child-process substrate
//     and the runtime adapters (opencode #1, codex #2); plus the
//     Supervisor-side registration seams: the mesh Registry store interface
//     (upsert/heartbeat/prune/deregister/lookup/sweep/list), the
//     registration Ingress (the gateway POST surface), the TTL-prune
//     Sweeper loop, the health-sweep Supervisor loop and the
//     route-to-endpoint selection leg.
//   - a2a     — the A2A wire types (Task/Event/Status/AgentCard/...).
//
// Hosts (adopters) keep their own control planes and plug in behind these
// seams: k8squad is host #1 — its role-based scheduling becomes a POLICY on
// the mesh scheduling interface; its CRD shapes, coordination guards and
// telemetry spine stay host-side.
//
// This module must stay extraction-clean: no package here may import
// github.com/K8squad/K8squad/* (enforced by hack/extraction-clean.sh in CI).
package k8sagentmesh
