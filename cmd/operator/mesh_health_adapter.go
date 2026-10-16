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

// mesh_health_adapter.go — the k8squad-side bindings for the AgentMesh P1.4
// gateway-health loop (ISI-6010, chapter §2.4, D6.1/D6.2): the generic
// mesh.SupervisorHealth loop speaks seams (SupervisorStore / PodRecreator /
// health func); these adapters bind them to the coord.mesh_endpoint registry,
// the KubeProvisioner, and the gateway's /ready surface. (The cphealth
// "gateway" dependency probe itself landed on main in ce1445f0 via the
// MeshIngress health endpoint — this file keeps only the supervisor loop's
// seams.)
package main

import (
	"context"
	"fmt"

	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
	kubepool "github.com/K8squad/K8squad/pkg/warmpool"
)

// meshHealthStore adapts *coord.MeshRegistry to mesh.SupervisorStore: the
// unfiltered endpoint listing (live + TTL-expired) and the §2.4 drain-by-pod.
type meshHealthStore struct {
	registry *coord.MeshRegistry
}

// Endpoints implements mesh.SupervisorStore.
func (s meshHealthStore) Endpoints(ctx context.Context) ([]mesh.EndpointHealth, error) {
	eps, err := s.registry.ListEndpoints(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mesh.EndpointHealth, 0, len(eps))
	for _, e := range eps {
		out = append(out, mesh.EndpointHealth{
			EndpointID: e.EndpointID,
			Pod:        e.Pod,
			Team:       e.Team,
			Role:       e.Role,
			Endpoint:   e.Endpoint,
			LastSeenAt: e.LastSeenAt,
		})
	}
	return out, nil
}

// DrainPod implements mesh.SupervisorStore.
func (s meshHealthStore) DrainPod(ctx context.Context, pod string) error {
	return s.registry.DeregisterPod(ctx, pod)
}

// meshPodRecreator adapts the KubeProvisioner plus the boot-time spec table to
// mesh.PodRecreator: a drained pod is rebuilt from the SAME MeshPoolSpec the
// operator booted it with (the spec carries the envelope via env, D2.3).
type meshPodRecreator struct {
	provisioner *kubepool.KubeProvisioner
	// specs maps pod name → the MeshPoolSpec the pool was booted with.
	specs map[string]kubepool.MeshPoolSpec
}

// RecreatePod implements mesh.PodRecreator.
func (r meshPodRecreator) RecreatePod(ctx context.Context, pod string) error {
	spec, ok := r.specs[pod]
	if !ok {
		return fmt.Errorf("meshPodRecreator: no pool spec for pod %q (booted before the spec table existed?)", pod)
	}
	return r.provisioner.RecreateMeshPool(ctx, spec)
}

// meshSupervisorLogSink is the health loop's TelemetrySink: drain/recreate
// outcomes go to the operator log (loud on failure, one line on success).
type meshSupervisorLogSink struct{}

// OnLifecycle implements mesh.TelemetrySink.
func (meshSupervisorLogSink) OnLifecycle(op, handleID, _ string, err error) {
	if err != nil {
		ctrl.Log.Error(err, "AgentMesh supervisor health", "op", op, "pod", handleID)
		return
	}
	ctrl.Log.Info("AgentMesh supervisor health", "op", op, "pod", handleID)
}
