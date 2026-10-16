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
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Ingress is the Supervisor-side registration ingress (chapter §2.5 seam
// table, ISI-5822 P1.1; lifted from the reference host's operator ingress
// at P3.4): the HTTP surface gateways POST their endpoint sets to. One
// POST is both the initial registration and the heartbeat — the ingress
// upserts every listed endpoint (refreshing last_seen_at), prunes ids the
// pod no longer serves, and answers 200. On the gateway side the same
// Registration wire type builds the POST, so there is one contract, not
// two.
//
// Surface (mounted under /mesh by ServeIngress; the paths below are
// relative to that mount):
//
//	POST /mesh/register    {pod, team, role, processes:[{EndpointID,
//	                       Endpoint, Card}]} — upsert + prune + heartbeat.
//	POST /mesh/deregister  {pod} — drain every endpoint of a pod (clean
//	                       shutdown; the §2.4 unhealthy-gateway drain).
//	GET  /mesh/health      — liveness for the ingress process itself.
//
// The ingress holds NO state: it is a thin decoder over a Registry, which is
// the authoritative store. A gateway that dies stops heartbeating and its
// endpoints TTL out even if this process never hears a deregister.
type Ingress struct {
	registry Registry
}

// NewIngress binds the ingress to the registry.
func NewIngress(registry Registry) (*Ingress, error) {
	if registry == nil {
		return nil, fmt.Errorf("mesh.NewIngress: nil registry")
	}
	return &Ingress{registry: registry}, nil
}

// Handler returns the ingress's HTTP handler (mounted under /mesh/ by
// ServeIngress; the paths below are relative to that mount).
func (in *Ingress) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", in.handleHealth)
	mux.HandleFunc("POST /register", in.handleRegister)
	mux.HandleFunc("POST /deregister", in.handleDeregister)
	return mux
}

// handleHealth returns 200 if the Ingress is serving — the "gateway"
// dependency of the host's control-plane health probe. This is a liveness
// check for the Ingress process itself; per-gateway pod health is tracked
// via the registry TTL in LookupTeamRole and does not gate this probe (zero
// registered gateways during boot must not flapper the supervisor health).
func (in *Ingress) handleHealth(w http.ResponseWriter, r *http.Request) {
	// The Ingress is healthy if it can reach its registry. A lightweight
	// connectivity check suffices: if the store answers, the service is
	// alive. We deliberately do NOT require live gateways here — that
	// would create a chicken-and-egg problem on boot (the supervisor
	// reports unhealthy before any gateway registers). Per-gateway
	// liveness is surfaced via the registry's last_seen_at TTL filter in
	// LookupTeamRole.
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	// A lookup with a synthetic pair: an error means the registry (store)
	// is unreachable; an empty answer means reachable-and-healthy.
	_, err := in.registry.LookupTeamRole(ctx, "__health_check__", "__probe__")
	if err != nil {
		http.Error(w, fmt.Sprintf("registry unreachable: %v", err), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok\n"))
}

// handleRegister upserts the posted endpoint set and prunes the pod's
// unlisted ids — one idempotent beat of the registration loop.
func (in *Ingress) handleRegister(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	var reg Registration
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&reg); err != nil {
		http.Error(w, fmt.Sprintf("decode registration: %v", err), http.StatusBadRequest)
		return
	}
	if reg.Pod == "" || reg.Team == "" || reg.Role == "" {
		http.Error(w, "registration requires pod, team and role", http.StatusBadRequest)
		return
	}
	// An EMPTY process set is legitimate: a gateway whose every slot
	// stopped (scale-to-zero end state, chapter §6) POSTs the empty set
	// precisely so PrunePod drops all of the pod's endpoints right away
	// instead of waiting out the registry TTL. The broken-boot guard
	// (never held any process) lives gateway-side in RegisterOnce;
	// ingress-side an empty beat from an unknown pod is a harmless no-op
	// prune.
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
		if err := in.registry.Register(ctx, Endpoint{
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
func (in *Ingress) handleDeregister(w http.ResponseWriter, r *http.Request) {
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

// ServeIngress runs the ingress HTTP server until ctx is done. addr is the
// host's supervisor ingress listener; the gateway env points the gateway's
// RegisterURL at http://<supervisor>/mesh.
func ServeIngress(ctx context.Context, addr string, in *Ingress) error {
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
		return fmt.Errorf("mesh ingress: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	}
}
