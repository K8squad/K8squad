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

// Package mesh is k8squad's host-side re-export seam over the generic
// AgentMesh mesh engine (github.com/K8sAgentMesh/K8sAgentMesh/mesh, lifted in
// roadmap P3 — AgentMesh chapter §1A, leak L1 of the ISI-5827 extraction
// spec). Every type below is a type alias, so cmd/shim, rundrive, meshobs
// and the operator keep compiling against the same names while the mechanism
// — the gateway process manager, router, lifecycle state machine,
// child-process substrate, worktree discipline, supervisor health loop — is
// the module's, one implementation instead of two. The full test battery
// lives in the module; this package keeps the host seam pins (seam_test.go).
//
// What stays k8squad-side on purpose (adapters.go, the L1 fix):
//
//   - NewOpenCodeAdapter / NewCodexAdapter — host constructors with the
//     pre-P3.3 signatures. They build the module's RuntimeSpec from the
//     k8squad sources of truth (pkg/shim/runtimes, internal/protocol.Pinned,
//     the ksquad.a2a/v1 SchemaVersion stamp and the KSQUAD_RUNTIME_TYPE
//     launch-env contract) and delegate to the module constructors, so the
//     capability contract the mesh announces stays exactly the one the shim
//     serves — one source of truth per host, never two.
//
//   - The CRD shapes, coord.* + ksquad.* bindings and board/SCM integrations
//     stay host #1 behind the seam (ISI-5827 extraction spec §1).
package mesh

import (
	"context"
	"net/http"

	a2amesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// Sentinel errors, re-exported by value so errors.Is keeps working across
// the seam (the gateway answers them; cmd/shim and rundrive match on them).
var (
	ErrProcessBusy   = a2amesh.ErrProcessBusy
	ErrNoIdleProcess = a2amesh.ErrNoIdleProcess
)

// Core contract and engine types (module mesh.go, gateway.go, router.go).
type (
	// RuntimeAdapter is the 7-op child-process contract (Spawn/Register/
	// Route/Hibernate/Wake/Stop/Recreate).
	RuntimeAdapter = a2amesh.RuntimeAdapter
	// Envelope is the pre-loaded role envelope a standing process serves.
	Envelope = a2amesh.Envelope
	// Handle identifies one standing process slot.
	Handle = a2amesh.Handle
	// Stream is the routed-task result stream.
	Stream = a2amesh.Stream
	// LaunchSpec is the process launch contract.
	LaunchSpec = a2amesh.LaunchSpec
	// PlatformHost is the substrate the processes run on.
	PlatformHost = a2amesh.PlatformHost
	// Instance is one launched process on the substrate.
	Instance = a2amesh.Instance
	// TelemetrySink receives gateway lifecycle transitions.
	TelemetrySink = a2amesh.TelemetrySink
	// NopSink is the zero-op sink.
	NopSink = a2amesh.NopSink
	// ChildProcessAdapter is the shared standing-child-process base of the
	// runtime adapters.
	ChildProcessAdapter = a2amesh.ChildProcessAdapter
	// Option configures a ChildProcessAdapter.
	Option = a2amesh.Option
	// Router submits a task to an endpoint over HTTP.
	Router = a2amesh.Router
	// RuntimeSpec is the injected runtime profile a host supplies to a
	// concrete adapter (leak L1). k8squad builds it from its own
	// declarative runtime registry — see adapters.go.
	RuntimeSpec = a2amesh.RuntimeSpec
	// ProcessStateSnapshot is the processes.active gauge input closure.
	ProcessStateSnapshot = a2amesh.ProcessStateSnapshot
)

// Gateway types (gateway.go).
type (
	// Gateway is the per-pod PID1 process manager.
	Gateway = a2amesh.Gateway
	// GatewayConfig assembles the gateway.
	GatewayConfig = a2amesh.GatewayConfig
	// ProcState is the standing-process state vocabulary.
	ProcState = a2amesh.ProcState
	// ProcessInfo is the state snapshot row of one slot.
	ProcessInfo = a2amesh.ProcessInfo
)

// Lifecycle types (lifecycle.go).
type (
	// LifecycleConfig carries the lifecycle state machine's knobs.
	LifecycleConfig = a2amesh.LifecycleConfig
	// LifecycleObserver is the gateway's lifecycle telemetry seam: the
	// route/spawn/wake event contract meshobs implements on the OTel spine.
	LifecycleObserver = a2amesh.LifecycleObserver
	// RouteEvent reports a routed task's selection.
	RouteEvent = a2amesh.RouteEvent
	// RouteSelection is the route selection vocabulary.
	RouteSelection = a2amesh.RouteSelection
	// SpawnEvent reports a process spawn.
	SpawnEvent = a2amesh.SpawnEvent
	// SpawnSource is the spawn source vocabulary.
	SpawnSource = a2amesh.SpawnSource
	// WakeEvent reports a hibernated process wake (S4).
	WakeEvent = a2amesh.WakeEvent
	// SupervisorHealth is the endpoint-health sweep loop.
	SupervisorHealth = a2amesh.SupervisorHealth
	// SupervisorHealthConfig assembles the health loop.
	SupervisorHealthConfig = a2amesh.SupervisorHealthConfig
	// SupervisorStore is the endpoint store the health loop consumes.
	SupervisorStore = a2amesh.SupervisorStore
	// EndpointHealth is one endpoint's health-loop row.
	EndpointHealth = a2amesh.EndpointHealth
	// PodRecreator drains and recreates a downed gateway pod.
	PodRecreator = a2amesh.PodRecreator
)

// Registration wire (gateway.go) — one contract, not two: the gateway builds
// the POST body and the supervisor-side ingress decodes the same type.
type (
	// Registration is the gateway→supervisor announce/heartbeat wire.
	Registration = a2amesh.Registration
	// RegisteredProcess is one endpoint set row of a Registration.
	RegisteredProcess = a2amesh.RegisteredProcess
)

// LocalHost (localhost.go) and worktree write discipline (worktree.go,
// ADR-0024).
type (
	// LocalHost is the local-substrate PlatformHost.
	LocalHost = a2amesh.LocalHost
	// SourceSpec describes the branch/worktree a task's source lands in.
	SourceSpec = a2amesh.SourceSpec
)

// Process states (lifecycle.go).
const (
	StateActive     = a2amesh.StateActive
	StateIdle       = a2amesh.StateIdle
	StateHibernated = a2amesh.StateHibernated
	StateStopped    = a2amesh.StateStopped
)

// Lifecycle defaults (lifecycle.go).
const (
	DefaultCheckInterval   = a2amesh.DefaultCheckInterval
	DefaultIdleToHibernate = a2amesh.DefaultIdleToHibernate
	DefaultHibernateToStop = a2amesh.DefaultHibernateToStop
)

// Route selections (lifecycle.go).
const (
	SelectionIdle     = a2amesh.SelectionIdle
	SelectionOverflow = a2amesh.SelectionOverflow
	SelectionWake     = a2amesh.SelectionWake
)

// Spawn sources (lifecycle.go).
const (
	SpawnSourceEnvelope = a2amesh.SpawnSourceEnvelope
	SpawnSourceCold     = a2amesh.SpawnSourceCold
)

// Gateway + worktree defaults (gateway.go, worktree.go).
const (
	DefaultBaseRef        = a2amesh.DefaultBaseRef
	DefaultGrowIdleShrink = a2amesh.DefaultGrowIdleShrink
)

// NewGateway builds the per-pod process manager (module gateway.go).
func NewGateway(cfg GatewayConfig) (*Gateway, error) {
	return a2amesh.NewGateway(cfg)
}

// NewHTTPRouter builds the HTTP task router.
func NewHTTPRouter(client *http.Client) Router {
	return a2amesh.NewHTTPRouter(client)
}

// WithRouter injects a Router into a child-process adapter.
func WithRouter(r Router) Option {
	return a2amesh.WithRouter(r)
}

// NewSupervisorHealth builds the endpoint-health sweep loop.
func NewSupervisorHealth(cfg SupervisorHealthConfig) (*SupervisorHealth, error) {
	return a2amesh.NewSupervisorHealth(cfg)
}

// ProbeReady GETs <baseURL>/health until it answers 200 within its budget.
func ProbeReady(ctx context.Context, client *http.Client, baseURL string) bool {
	return a2amesh.ProbeReady(ctx, client, baseURL)
}

// BranchName is the worktree branch naming rule (worktree.go, ADR-0024).
func BranchName(role, workItemID string) string {
	return a2amesh.BranchName(role, workItemID)
}

// WorktreeDirName is the worktree directory naming rule.
func WorktreeDirName(role, workItemID string) string {
	return a2amesh.WorktreeDirName(role, workItemID)
}

// EnsureWorktree creates/updates the task worktree per SourceSpec.
func EnsureWorktree(ctx context.Context, s SourceSpec) error {
	return a2amesh.EnsureWorktree(ctx, s)
}

// SnapshotProcessStates builds the processes.active gauge input from the
// gateway's live slot snapshot (the canonical builder lifted from this
// composition root's closure at P3.6).
func SnapshotProcessStates(procs []ProcessInfo) map[ProcState]int64 {
	return a2amesh.SnapshotProcessStates(procs)
}
