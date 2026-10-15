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

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/mesh"
)

func newIngress(t *testing.T) (*MeshIngress, sqlmock.Sqlmock, *httptest.Server) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	registry, err := coord.NewMeshRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	in, err := NewMeshIngress(registry)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in.Handler())
	t.Cleanup(srv.Close)
	return in, mock, srv
}

// A registration POST upserts every listed endpoint and prunes the pod's
// unlisted ids — one heartbeat beat, end to end.
func TestMeshIngress_Register(t *testing.T) {
	_, mock, srv := newIngress(t)

	reg := mesh.Registration{
		Pod: "pool-aaa", Team: "team-acme", Role: "coder",
		Processes: []mesh.RegisteredProcess{
			{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"},
			{EndpointID: "e1", Endpoint: "http://10.0.0.1:8082"},
		},
	}
	for _, p := range reg.Processes {
		mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
			WithArgs(p.EndpointID, "team-acme", "coder", "pool-aaa", p.Endpoint, sqlmock.AnyArg()).
			WillReturnResult(sqlmock.NewResult(0, 1))
	}
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint").
		WithArgs("pool-aaa", pq.Array([]string{"e0", "e1"})).
		WillReturnResult(sqlmock.NewResult(0, 0))

	payload, _ := json.Marshal(reg)
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/register = %d, want 200", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Malformed or identity-less registrations are rejected 400 — never a
// half-identified row the lookup could route to.
func TestMeshIngress_RegisterValidation(t *testing.T) {
	_, _, srv := newIngress(t)
	cases := []string{
		`{}`,
		`{"pod":"pool-aaa","team":"t","role":"r","processes":[{"endpoint_id":"","endpoint":"http://x"}]}`,
		`not json`,
	}
	for i, body := range cases {
		resp, err := http.Post(srv.URL+"/register", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("case %d = %d, want 400", i, resp.StatusCode)
		}
	}
}

// An empty process set is the scale-to-zero prune (chapter §6): a gateway
// whose every slot stopped POSTs the empty set so the pod's endpoints leave
// coord.mesh_endpoint right away. The ingress must accept it (200) and prune
// the whole pod — P1.1's blanket 400 broke the P1.2 stop→prune contract
// (ISI-5978 review RED-1).
func TestMeshIngress_RegisterEmptySetPrunesPod(t *testing.T) {
	_, mock, srv := newIngress(t)
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint").
		WithArgs("pool-aaa", sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 2))

	payload, _ := json.Marshal(mesh.Registration{
		Pod: "pool-aaa", Team: "team-acme", Role: "coder",
	})
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty-set /register = %d, want 200 (scale-to-zero prune)", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A registry failure surfaces as 500, not a silent 200 — the gateway's next
// heartbeat retries and the sink reports the miss.
func TestMeshIngress_RegisterStoreError(t *testing.T) {
	_, mock, srv := newIngress(t)
	mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
		WithArgs(sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg(), sqlmock.AnyArg()).
		WillReturnError(context.DeadlineExceeded)

	payload, _ := json.Marshal(mesh.Registration{
		Pod: "pool-aaa", Team: "team-acme", Role: "coder",
		Processes: []mesh.RegisteredProcess{{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"}},
	})
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/register = %d, want 500 on store failure", resp.StatusCode)
	}
}

// Deregister drains a pod.
func TestMeshIngress_Deregister(t *testing.T) {
	_, mock, srv := newIngress(t)
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE pod = \\$1").
		WithArgs("pool-aaa").
		WillReturnResult(sqlmock.NewResult(0, 2))

	resp, err := http.Post(srv.URL+"/deregister", "application/json",
		strings.NewReader(`{"pod":"pool-aaa"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/deregister = %d, want 200", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}

	// A pod-less deregister is a 400.
	resp2, err := http.Post(srv.URL+"/deregister", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusBadRequest {
		t.Fatalf("pod-less /deregister = %d, want 400", resp2.StatusCode)
	}
}

// The ingress binds no state: constructing one without a registry fails
// closed.
func TestNewMeshIngress_RequiresRegistry(t *testing.T) {
	if _, err := NewMeshIngress(nil); err == nil {
		t.Fatal("expected nil-registry error")
	}
}

// ServeMeshIngress returns cleanly on context cancellation (operator
// shutdown path).
func TestServeMeshIngress_Shutdown(t *testing.T) {
	in, _, _ := newIngress(t)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := ServeMeshIngress(ctx, "127.0.0.1:0", in); err != nil {
		t.Fatalf("ServeMeshIngress: %v", err)
	}
}

// The /health endpoint answers 200 when the registry is reachable (ISI-6010
// P1.4b gateway dependency probe). A registry failure surfaces as 500.
func TestMeshIngress_Health(t *testing.T) {
	_, mock, srv := newIngress(t)
	// The health check issues a LookupTeamRole with sentinel values; it must
	// succeed even with empty results (registry reachable = healthy).
	mock.ExpectQuery("SELECT endpoint_id").
		WillReturnRows(sqlmock.NewRows([]string{"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at"}))

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d, want 200", resp.StatusCode)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// A registry failure surfaces as 500 on /health.
func TestMeshIngress_HealthRegistryDown(t *testing.T) {
	_, mock, srv := newIngress(t)
	mock.ExpectQuery("SELECT endpoint_id").
		WillReturnError(context.DeadlineExceeded)

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/health = %d, want 500 on registry failure", resp.StatusCode)
	}
}
