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

// meshsupervisor.go — the k8squad binding of the TTL-prune supervisor loop
// (mechanism lifted to the AgentMesh module as mesh.Sweeper at P3.4). The
// host keeps the store binding (coord.MeshRegistry via AsMeshRegistry), the
// host default cadence (the registry's MeshEndpointTTL / 3) and the
// manager.Runnable wiring. Per-pod gateway liveness probing and explicit
// DeregisterPod on gateway-down is the SupervisorHealth leg
// (cmd/operator/mesh_health_adapter.go over the module's health loop).
package rundrive

import (
	"context"
	"fmt"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"

	agentmesh "github.com/K8sAgentMesh/K8sAgentMesh/mesh"
)

// MeshSupervisorOption configures a MeshSupervisor.
type MeshSupervisorOption func(*MeshSupervisor)

// WithMeshSupervisorClock overrides the supervisor's clock (tests).
func WithMeshSupervisorClock(now func() time.Time) MeshSupervisorOption {
	return func(s *MeshSupervisor) { s.now = now }
}

// WithMeshSupervisorSweepInterval overrides the sweep cadence.
func WithMeshSupervisorSweepInterval(d time.Duration) MeshSupervisorOption {
	return func(s *MeshSupervisor) {
		if d > 0 {
			s.sweepInterval = d
		}
	}
}

// MeshSupervisor monitors and maintains the mesh endpoint registry.
// It is a controller-runtime manager.Runnable that can be added to the operator
// with mgr.Add(s).
type MeshSupervisor struct {
	registry      *coord.MeshRegistry
	sweepInterval time.Duration
	now           func() time.Time
}

// NewMeshSupervisor creates a supervisor bound to the registry.
// Sweep runs at sweepInterval (default: the registry's MeshEndpointTTL / 3,
// i.e. ~30s when TTL is 90s) to keep TTL-expired rows from accumulating.
func NewMeshSupervisor(registry *coord.MeshRegistry, opts ...MeshSupervisorOption) (*MeshSupervisor, error) {
	if registry == nil {
		return nil, fmt.Errorf("rundrive.NewMeshSupervisor: nil registry")
	}
	s := &MeshSupervisor{
		registry:      registry,
		sweepInterval: coord.MeshEndpointTTL / 3,
		now:           time.Now,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s, nil
}

// Start implements manager.Runnable: runs the module's sweep loop until ctx
// is cancelled. Each tick it Sweeps the registry to delete TTL-expired rows —
// the authoritative clean-up path that guarantees coord.mesh_endpoint never
// grows unbounded (NFR-OBS3 cardinality firewall). Per-pod gateway liveness
// probing and explicit DeregisterPod on gateway-down is a follow-up
// enhancement; the current loop handles the TTL-expiry half of the ISI-6010
// requirement (D6.2: drain on TTL-expiry via Sweep).
func (s *MeshSupervisor) Start(ctx context.Context) error {
	sweeper, err := agentmesh.NewSweeper(
		AsMeshRegistry(s.registry),
		agentmesh.WithSweeperInterval(s.sweepInterval),
		agentmesh.WithSweeperClock(s.now),
	)
	if err != nil {
		return err
	}
	return sweeper.Start(ctx)
}
