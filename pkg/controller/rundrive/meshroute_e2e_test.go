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

package rundrive

// meshroute_e2e_test.go — the P1.1 thin-thread PROOF (ISI-5822): one run
// routed end-to-end through the real mesh stack, hermetic:
//
//	gateway (pkg/mesh + real opencode adapter + LocalHost)
//	  → spawns REAL child processes (this test binary re-exec'd as the
//	    child supervisor, the established Go helper pattern)
//	  → registers them at the supervisor ingress (MeshIngress over the
//	    real MeshRegistry store)
//	  → the rundrive route leg looks (team, role) up and POSTs the task
//	  → the child executes, streams NDJSON back through the gateway, and
//	    echoes the KSQUAD_RUN_PATH env it booted with — proving the S0/S3
//	    run.path=mesh stamp reached the process that actually executed.
//
// What this deliberately does NOT re-prove: at-most-once (coord.a2a_dispatch
// chaos suites own it; the route leg never touches the marker) and the flag-off
// path (meshroute_test.go).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
)

// TestMeshChildSupervisorHelper is the re-exec'd standing-process helper: a
// minimal `shim supervisor` stand-in serving the :addr POST /task NDJSON
// contract. It skips in the parent's own run; the mesh LocalHost execs the
// test binary with -test.run pinned here and GO_WANT_MESH_CHILD=1 (which
// rides os.Environ() into the child).
func TestMeshChildSupervisorHelper(t *testing.T) {
	if os.Getenv("GO_WANT_MESH_CHILD") != "1" {
		t.Skip("helper process")
	}
	addr := os.Getenv("KSQUAD_SUPERVISOR_ADDR")
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok\n"))
	})
	mux.HandleFunc("POST /task", func(w http.ResponseWriter, r *http.Request) {
		var task wire.Task
		_ = json.NewDecoder(r.Body).Decode(&task)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		// Echo the dispatch-path env the child booted with (S0/S3 stamp)
		// in the terminal status reason — the parent asserts on it.
		_ = json.NewEncoder(w).Encode(wire.Event{
			Seq: 1, Type: wire.EventStatus,
			Payload: wire.StatusPayload{
				State:  wire.TaskCompleted,
				Reason: "runpath=" + os.Getenv("KSQUAD_RUN_PATH"),
			},
		})
	})
	srv := &http.Server{Addr: addr, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	// Serve until the gateway's Stop SIGTERMs the process group.
	_ = srv.ListenAndServe()
}

// TestMeshThinThreadE2E routes one run end-to-end: gateway children →
// ingress registration → registry lookup → route leg POST → child executes.
func TestMeshThinThreadE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("e2e composition in short mode")
	}
	const (
		team = "team-acme"
		role = "coder"
	)
	const runUID = "eeeeeeee-0000-4444-8888-eeeeeeeeeeee"

	// The child re-exec marker must ride os.Environ() into LocalHost children.
	t.Setenv("GO_WANT_MESH_CHILD", "1")

	// --- Coordinator side: the registry store over sqlmock -----------------
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	registry, err := coord.NewMeshRegistry(db)
	if err != nil {
		t.Fatal(err)
	}

	// --- The supervisor ingress (real handler, counted) --------------------
	ingress, err := NewMeshIngress(registry)
	if err != nil {
		t.Fatal(err)
	}
	var registrations atomic.Int32
	ingMux := http.NewServeMux()
	ingMux.Handle("/mesh/", http.StripPrefix("/mesh", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/register" {
			registrations.Add(1)
		}
		ingress.Handler().ServeHTTP(w, r)
	})))
	ingSrv := httptest.NewServer(ingMux)
	defer ingSrv.Close()

	// --- The gateway: real adapter + LocalHost over re-exec'd children -----
	host := &mesh.LocalHost{
		BinPath:  os.Args[0],
		Args:     []string{"-test.run=TestMeshChildSupervisorHelper"},
		BasePort: 18200,
	}
	adapter, err := mesh.NewOpenCodeAdapter(host)
	if err != nil {
		t.Fatal(err)
	}
	gw, err := mesh.NewGateway(mesh.GatewayConfig{
		Adapter: adapter,
		Envelope: mesh.Envelope{
			RuntimeType: "opencode",
			Identity:    wire.AgentIdentity{Name: role, Squad: team, Project: "proj"},
			Skills:      []string{"go"},
			// Mirrors cmd/shim buildGateway's host-side stamp: the dispatch
			// path label is k8squad policy layered onto the generic envelope.
			Env: map[string]string{"KSQUAD_RUN_PATH": "mesh"},
		},
		Min:              1,
		Pod:              "mesh-team-acme-coder",
		Team:             team,
		Role:             role,
		RegisterURL:      ingSrv.URL + "/mesh",
		RegisterInterval: time.Hour, // the initial beat only; the lookup side is driven below
	})
	if err != nil {
		t.Fatal(err)
	}

	// The registration beat hits the registry: 1 upsert + 1 prune.
	childEP := "http://127.0.0.1:18200"
	mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
		WithArgs(sqlmock.AnyArg(), team, role, "mesh-team-acme-coder", childEP, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint").
		WithArgs("mesh-team-acme-coder", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 0))

	ctx := context.Background()
	if err := gw.Start(ctx); err != nil {
		t.Fatalf("gateway start: %v", err)
	}
	defer func() { _ = gw.Stop(ctx) }()

	// Wait for the child supervisor to actually serve (spawn race).
	up := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(childEP + "/health")
		if err == nil {
			_ = resp.Body.Close()
			up = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !up {
		t.Fatal("child supervisor never came up")
	}
	if n := registrations.Load(); n < 1 {
		t.Fatalf("ingress registrations = %d, want >= 1", n)
	}

	// --- The dispatch side: the route leg reads the registry ---------------
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-mesh", Namespace: team, UID: types.UID(runUID)},
		Spec:       api.RunSpec{TeamRef: api.ObjectRef{Name: team}},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run).Build()

	now := time.Now().UTC()
	mock.ExpectQuery("FROM coord.mesh_endpoint").
		WithArgs(team, role, sqlmock.AnyArg()).
		WillReturnRows(sqlmock.NewRows([]string{"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at"}).
			AddRow("opencode-0-1", team, role, "mesh-team-acme-coder", childEP, []byte(`{}`), now, now))

	d := &operatorDispatch{
		cfg: OperatorDispatchConfig{
			Client: cl,
			MeshRoute: &MeshRouteConfig{
				Teams:  MeshTeamSet{team: {}},
				Lookup: AsMeshRegistry(registry),
			},
		},
		httpClient: newDispatchHTTPClient(time.Second * 30),
	}

	task := wire.Task{
		A2ATaskID: runUID,
		Identity:  wire.AgentIdentity{Squad: team, Name: role},
	}
	sess, err := sandboxTransport{d}.Submit(ctx, task)
	if err != nil {
		t.Fatalf("mesh Submit: %v", err)
	}
	var reason string
	for ev := range sess.Events() {
		if ev.Type != wire.EventStatus {
			continue
		}
		if sp, ok := ev.Payload.(map[string]any); ok {
			if r, ok := sp["reason"].(string); ok {
				reason = r
			}
		}
	}
	_ = sess.Close()
	if !strings.Contains(reason, "runpath=mesh") {
		t.Fatalf("child echoed %q, want the KSQUAD_RUN_PATH=mesh stamp it booted with", reason)
	}

	// The run-end SLI's authoritative source was stamped too.
	var after api.Run
	if err := cl.Get(ctx, client.ObjectKey{Namespace: team, Name: "run-mesh"}, &after); err != nil {
		t.Fatal(err)
	}
	if got := after.Annotations[annotationDispatchPath]; got != "mesh" {
		t.Fatalf("dispatch-path annotation = %q, want mesh", got)
	}

	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
