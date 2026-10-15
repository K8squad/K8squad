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

// meshingress.go — the Supervisor-side registration ingress (chapter §2.5
// seam table, ISI-5822 P1.1): the HTTP surface gateways POST their endpoint
// sets to. One POST is both the initial registration and the heartbeat —
// the ingress upserts every listed endpoint (refreshing last_seen_at),
// prunes ids the pod no longer serves, and answers 200. On the gateway side
// the same Registration wire type builds the POST (pkg/mesh), so there is
// one contract, not two.
//
// Surface (mounted under /mesh on the operator's ingress listener):
//
//	POST /mesh/register    {pod, team, role, processes:[{endpoint_id,
//	                       endpoint, card}]} — upsert + prune + heartbeat.
//	POST /mesh/deregister  {pod} — drain every endpoint of a pod (clean
//	                       shutdown; the §2.4 unhealthy-gateway drain).
//
// The ingress holds NO state: it is a thin decoder over MeshRegistry, which
// is the authoritative store. A gateway that dies stops heartbeating and
// its endpoints TTL out even if this process never hears a deregister.
package rundrive

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// MeshIngress serves the gateway registration surface over a MeshRegistry.
type MeshIngress struct {
	registry *coord.MeshRegistry
	// now bounds request handling (telemetry/deadline hygiene only).
	now func() time.Time
}

// NewMeshIngress binds the ingress to the registry.
func NewMeshIngress(registry *coord.MeshRegistry) (*MeshIngress, error) {
	if registry == nil {
		return nil, fmt.Errorf("rundrive.NewMeshIngress: nil registry")
	}
	return &MeshIngress{registry: registry, now: time.Now}, nil
}

// Handler returns the ingress's HTTP handler (mounted under /mesh/ by
// ServeMeshIngress; the paths below are relative to that mount).
func (in *MeshIngress) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", in.handleRegister)
	mux.HandleFunc("POST /deregister", in.handleDeregister)
	return mux
}

// handleRegister upserts the posted endpoint set and prunes the pod's
// unlisted ids — one idempotent beat of the registration loop.
func (in *MeshIngress) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var reg mesh.Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&reg); err != nil {
		http.Error(w, fmt.Sprintf("decode registration: %v", err), http.StatusBadRequest)
		return
	}
	if reg.Pod == "" || reg.Team == "" || reg.Role == "" {
		http.Error(w, "registration requires pod, team and role", http.StatusBadRequest)
		return
	}
	// An EMPTY process set is legitimate: a gateway whose every slot stopped
	// (scale-to-zero end state, chapter §6) POSTs the empty set precisely so
	// PrunePod drops all of the pod's endpoints right away instead of waiting
	// out the registry TTL. The broken-boot guard (never held any process)
	// lives gateway-side in RegisterOnce; ingress-side an empty beat from an
	// unknown pod is a harmless no-op prune.
	if len(reg.Processes) == 0 {
		if err := in.registry.PrunePod(ctx, reg.Pod, nil); err != nil {
			http.Error(w, fmt.Sprintf("prune pod %s: %v", reg.Pod, err), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
		return
	}

	keep := make([]string, 0, len(reg.Processes))
	for _, p := range reg.Processes {
		if p.EndpointID == "" || p.Endpoint == "" {
			http.Error(w, "every process requires endpoint_id and endpoint", http.StatusBadRequest)
			return
		}
		card, err := json.Marshal(p.Card)
		if err != nil {
			http.Error(w, fmt.Sprintf("encode agent card: %v", err), http.StatusBadRequest)
			return
		}
		if err := in.registry.Register(ctx, coord.MeshEndpoint{
			EndpointID: p.EndpointID,
			Team:       reg.Team,
			Role:       reg.Role,
			Pod:        reg.Pod,
			Endpoint:   p.Endpoint,
			AgentCard:  card,
		}); err != nil {
			http.Error(w, fmt.Sprintf("register %s: %v", p.EndpointID, err), http.StatusInternalServerError)
			return
		}
		keep = append(keep, p.EndpointID)
	}
	if err := in.registry.PrunePod(ctx, reg.Pod, keep); err != nil {
		http.Error(w, fmt.Sprintf("prune pod %s: %v", reg.Pod, err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleDeregister drains a pod's endpoint set.
func (in *MeshIngress) handleDeregister(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var req struct {
		Pod string `json:"pod"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("decode deregister: %v", err), http.StatusBadRequest)
		return
	}
	if req.Pod == "" {
		http.Error(w, "deregister requires pod", http.StatusBadRequest)
		return
	}
	if err := in.registry.DeregisterPod(ctx, req.Pod); err != nil {
		http.Error(w, fmt.Sprintf("deregister %s: %v", req.Pod, err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// ServeMeshIngress runs the ingress HTTP server until ctx is done. addr is
// the operator's ingress listener (KSQUAD_SUPERVISOR_INGRESS_ADDR, default
// :8090); the gateway env points KSQUAD_SUPERVISOR_INGRESS_URL at
// http://<operator-svc>:8090/mesh.
func ServeMeshIngress(ctx context.Context, addr string, in *MeshIngress) error {
	mux := http.NewServeMux()
	mux.Handle("/mesh/", http.StripPrefix("/mesh", in.Handler()))
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe() }()
	select {
	case err := <-errCh:
		if err == http.ErrServerClosed {
			return nil
		}
		return fmt.Errorf("rundrive mesh ingress: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
