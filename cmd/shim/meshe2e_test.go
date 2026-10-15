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

// meshe2e_test.go — the AgentMesh P1.1 thin-thread route-through proof
// (chapter §5.2, ISI-5822 slice 4): one run routed end-to-end over the REAL
// components — the mesh.Gateway (registration + /task routing), the
// rundrive.MeshIngress register surface, and the coord.MeshRegistry lookup —
// with the task POSTed through the SAME a2a.HTTPTransport client the
// operator's route leg uses (dispatch.go's sandboxTransport mesh branch).
//
// Fakes are exactly where the chapter draws the seams: the RuntimeAdapter is
// scripted (opencode is adapter #1 behind the seam, D8.1) and Postgres is
// sqlmock (the registry's SQL is pinned by pkg/coord's own tests). Everything
// between — the registration wire, the ingress decode, the registry rows, the
// lookup ordering, the advertise rewrite, the /task NDJSON contract — is the
// production code path.
//
// What this pins that the slice tests could not: the COMPOSITION. The
// gateway's Registration JSON is decoded by the real ingress; the advertised
// endpoint the registry stores is the GATEWAY's routable URL (AdvertiseURL —
// children bind 127.0.0.1 in-pod only); the URL the route leg would resolve
// (eps[0].Endpoint + "/task") actually serves the gateway's /task contract;
// and the a2a.HTTPTransport round-trips the gateway's NDJSON stream.
package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	clienta2a "github.com/K8squad/K8squad/internal/a2a"
	"github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/controller/rundrive"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// debugSink surfaces registration-loop failures the gateway swallows by
// design (the next heartbeat retries) straight into the test log.
type debugSink struct{ t *testing.T }

func (s debugSink) OnLifecycle(op, handleID, endpoint string, err error) {
	if err != nil {
		s.t.Logf("gateway lifecycle %s: %v", op, err)
	}
}

// TestMeshThinThread_RouteThrough walks §5.2's dispatch sequence minus the
// board custody legs (coord.claim / coord.a2a_dispatch are unchanged by P1.1
// and pinned by their own tests + the FR-B3 contract):
//
//	gateway Start → RegisterOnce → ingress /register → registry upsert+prune
//	→ LookupTeamRole(team, role) → HTTPTransport POST <endpoint>/task
//	→ gateway routes to the standing process → NDJSON events settle the session.
func TestMeshThinThread_RouteThrough(t *testing.T) {
	const (
		team = "team-acme"
		role = "coder"
		pod  = "mesh-team-acme-coder"
	)
	ctx := context.Background()

	// The authoritative registry store (sqlmock stands in for Postgres).
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	registry, err := coord.NewMeshRegistry(db)
	if err != nil {
		t.Fatal(err)
	}

	// The Supervisor-side register ingress: the real handler surface the
	// gateway heartbeats at. (Mounted at the root — ServeMeshIngress's
	// /mesh/ prefixing has its own coverage; StripPrefix around a
	// method-pattern mux breaks Go's ServeMux matching, so keep it bare.)
	in, err := rundrive.NewMeshIngress(registry)
	if err != nil {
		t.Fatal(err)
	}
	ingress := httptest.NewServer(in.Handler())
	t.Cleanup(ingress.Close)

	// Pre-bind the gateway's :8080-equivalent listener so the ADVERTISED url
	// (what the registry will serve the route leg) is known before Start.
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gwURL := "http://" + lis.Addr().String()

	// The immediate registration beat Start fires: one upsert per process
	// with the ADVERTISED url (not the child's 127.0.0.1), then the prune.
	mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
		WithArgs("fake-0", team, role, pod, gwURL, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint").
		WithArgs(pod, pq.Array([]string{"fake-0"})).
		WillReturnResult(sqlmock.NewResult(0, 0))

	fa := &fakeGwAdapter{autoSettle: true}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter:     fa,
		Envelope:    mesh.Envelope{RuntimeType: "opencode"},
		Min:         1,
		Pod:         pod,
		Team:        team,
		Role:        role,
		RegisterURL: ingress.URL,
		// §5.2: the route target is gateway(target.pod) — the child's
		// loopback endpoint is unreachable from the operator side.
		AdvertiseURL: gwURL,
		Sink:         debugSink{t},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := gw.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = gw.Stop(ctx) })

	// The gateway's kube-contract HTTP surface on the pre-bound listener —
	// the same mux runGateway serves.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("GET /ready", handleGatewayReady(gw))
	mux.HandleFunc("GET /endpoints", handleGatewayEndpoints(gw))
	mux.HandleFunc("POST /task", handleGatewayTask(gw))
	gwSrv := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = gwSrv.Serve(lis) }()
	t.Cleanup(func() { _ = gwSrv.Close() })

	// The route leg's read: the registry returns the advertised endpoint.
	// (Ordered-expectation note: this SELECT is queued AFTER the register
	// beat's INSERT+prune above — sqlmock pins the sequence.)
	mock.ExpectQuery("FROM coord.mesh_endpoint").
		WithArgs(team, role, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{
			"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at",
		}).AddRow("fake-0", team, role, pod, gwURL, []byte(`{}`),
			time.Now().Add(-time.Hour), time.Now().Add(-time.Second)))
	eps, err := registry.LookupTeamRole(ctx, team, role)
	if err != nil {
		t.Fatalf("LookupTeamRole: %v", err)
	}
	if len(eps) != 1 {
		t.Fatalf("registry endpoints = %d, want 1", len(eps))
	}
	if eps[0].Endpoint != gwURL {
		t.Fatalf("registry endpoint = %q, want the ADVERTISED gateway url %q (a child loopback url is unroutable from the operator)", eps[0].Endpoint, gwURL)
	}

	// Submit through the SAME transport the route leg binds (dispatch.go:
	// a2a.HTTPTransport{URL: eps[0].Endpoint + "/task"}) — proving the
	// gateway's /task wire answers the operator's client contract.
	taskURL := eps[0].Endpoint + "/task"
	ht := &clienta2a.HTTPTransport{
		URL: func(context.Context, a2a.Task) (string, error) { return taskURL, nil },
	}
	const taskID = "dddddddd-0000-4444-8888-dddddddddddd"
	sess, err := ht.Submit(ctx, a2a.Task{
		A2ATaskID: taskID,
		Identity:  a2a.AgentIdentity{Squad: team, Name: role},
	})
	if err != nil {
		t.Fatalf("route-leg Submit: %v", err)
	}
	events := 0
	for range sess.Events() {
		events++
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if events == 0 {
		t.Fatal("routed task streamed zero events — the /task NDJSON contract did not round-trip")
	}

	// The standing process executed exactly once (guard #3's flip side:
	// one route, one execution — at-most-once stays in coord.a2a_dispatch,
	// unchanged and out of scope here by design).
	fa.mu.Lock()
	routes := len(fa.streams)
	fa.mu.Unlock()
	if routes != 1 {
		t.Fatalf("standing-process routes = %d, want 1", routes)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
