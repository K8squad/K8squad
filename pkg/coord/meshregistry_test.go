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

package coord_test

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/lib/pq"

	"github.com/K8squad/K8squad/pkg/coord"
)

var meshCard = []byte(`{"agent":{"name":"coder","squad":"team-acme"}}`)

func newMeshRegistry(t *testing.T) (*coord.MeshRegistry, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	r, err := coord.NewMeshRegistry(db)
	if err != nil {
		t.Fatal(err)
	}
	return r, mock
}

// Register upserts on endpoint_id: the INSERT ... ON CONFLICT (endpoint_id)
// DO UPDATE shape, with the full column set and the jsonb cast.
func TestMeshRegistry_RegisterUpserts(t *testing.T) {
	r, mock := newMeshRegistry(t)
	mock.ExpectExec("INSERT INTO coord.mesh_endpoint").
		WithArgs("opencode-0-1", "team-acme", "coder", "pool-aaa", "http://10.0.0.1:8081", string(meshCard)).
		WillReturnResult(sqlmock.NewResult(0, 1))

	err := r.Register(context.Background(), coord.MeshEndpoint{
		EndpointID: "opencode-0-1", Team: "team-acme", Role: "coder",
		Pod: "pool-aaa", Endpoint: "http://10.0.0.1:8081", AgentCard: meshCard,
	})
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Register is fail-closed on the identity set: no endpoint_id / team / role /
// pod / endpoint / card means no row, never a half-identified registration
// the lookup could route to.
func TestMeshRegistry_RegisterRequiresIdentity(t *testing.T) {
	r, _ := newMeshRegistry(t)
	cases := []coord.MeshEndpoint{
		{Team: "t", Role: "r", Pod: "p", Endpoint: "http://x", AgentCard: meshCard},
		{EndpointID: "id", Role: "r", Pod: "p", Endpoint: "http://x", AgentCard: meshCard},
		{EndpointID: "id", Team: "t", Pod: "p", Endpoint: "http://x", AgentCard: meshCard},
		{EndpointID: "id", Team: "t", Role: "r", Endpoint: "http://x", AgentCard: meshCard},
		{EndpointID: "id", Team: "t", Role: "r", Pod: "p", AgentCard: meshCard},
		{EndpointID: "id", Team: "t", Role: "r", Pod: "p", Endpoint: "http://x"},
	}
	for i, ep := range cases {
		if err := r.Register(context.Background(), ep); err == nil {
			t.Fatalf("case %d: expected error, got nil", i)
		}
	}
}

// HeartbeatPod refreshes last_seen_at scoped to the pod's own endpoint ids.
func TestMeshRegistry_HeartbeatPod(t *testing.T) {
	r, mock := newMeshRegistry(t)
	mock.ExpectExec("UPDATE coord.mesh_endpoint SET last_seen_at = now()").
		WithArgs("pool-aaa", pq.Array([]string{"opencode-0-1", "opencode-0-2"})).
		WillReturnResult(sqlmock.NewResult(0, 2))

	n, err := r.HeartbeatPod(context.Background(), "pool-aaa", []string{"opencode-0-1", "opencode-0-2"})
	if err != nil {
		t.Fatalf("HeartbeatPod: %v", err)
	}
	if n != 2 {
		t.Fatalf("refreshed = %d, want 2", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// DeregisterPod deletes by pod (the drain-by-pod path).
func TestMeshRegistry_DeregisterPod(t *testing.T) {
	r, mock := newMeshRegistry(t)
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE pod = \\$1").
		WithArgs("pool-aaa").
		WillReturnResult(sqlmock.NewResult(0, 2))
	if err := r.DeregisterPod(context.Background(), "pool-aaa"); err != nil {
		t.Fatalf("DeregisterPod: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// LookupTeamRole filters on (team, role, TTL window) and orders
// least-recently-seen first — the §5.3 LRU tie-break.
func TestMeshRegistry_LookupTeamRole(t *testing.T) {
	pinned := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	r, err := coord.NewMeshRegistry(db, coord.WithMeshClock(func() time.Time { return pinned }))
	if err != nil {
		t.Fatal(err)
	}
	ttl := coord.MeshEndpointTTL
	mock.ExpectQuery("FROM coord.mesh_endpoint").
		WithArgs("team-acme", "coder", pinned.Add(-ttl)).
		WillReturnRows(sqlmock.NewRows([]string{"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at"}).
			AddRow("opencode-0-2", "team-acme", "coder", "pool-bbb", "http://10.0.0.2:8081", meshCard, pinned.Add(-time.Hour), pinned.Add(-40*time.Second)).
			AddRow("opencode-0-1", "team-acme", "coder", "pool-aaa", "http://10.0.0.1:8081", meshCard, pinned.Add(-2*time.Hour), pinned.Add(-5*time.Second)))

	eps, err := r.LookupTeamRole(context.Background(), "team-acme", "coder")
	if err != nil {
		t.Fatalf("LookupTeamRole: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("endpoints = %d, want 2", len(eps))
	}
	if eps[0].EndpointID != "opencode-0-2" {
		t.Fatalf("first = %s, want the least-recently-seen endpoint first", eps[0].EndpointID)
	}
	if string(eps[1].AgentCard) != string(meshCard) {
		t.Fatalf("agent card round-trip mismatch: %s", eps[1].AgentCard)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Sweep deletes rows older than the TTL.
func TestMeshRegistry_Sweep(t *testing.T) {
	r, mock := newMeshRegistry(t)
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE last_seen_at < \\$1").
		WithArgs(sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 3))
	n, err := r.Sweep(context.Background())
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if n != 3 {
		t.Fatalf("swept = %d, want 3", n)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// ListEndpoints returns ALL rows — live and TTL-expired alike — ordered by
// (pod, endpoint_id): the supervisor health loop's unfiltered view (P1.4,
// ISI-6010). LookupTeamRole would hide exactly the expired rows the loop
// drains, so the loop must NOT read through it.
func TestMeshRegistry_ListEndpoints(t *testing.T) {
	r, mock := newMeshRegistry(t)
	pinned := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	mock.ExpectQuery("FROM coord.mesh_endpoint").
		WillReturnRows(sqlmock.NewRows([]string{"endpoint_id", "team", "role", "pod", "endpoint", "agent_card", "registered_at", "last_seen_at"}).
			AddRow("ep-a1", "team-acme", "coder", "pool-aaa", "http://10.0.0.1:8080", meshCard, pinned.Add(-time.Hour), pinned.Add(-5*time.Minute)). // expired
			AddRow("ep-b1", "team-acme", "coder", "pool-bbb", "http://10.0.0.2:8080", meshCard, pinned.Add(-time.Hour), pinned.Add(-10*time.Second)))  // live

	eps, err := r.ListEndpoints(context.Background())
	if err != nil {
		t.Fatalf("ListEndpoints: %v", err)
	}
	if len(eps) != 2 {
		t.Fatalf("endpoints = %d, want 2 (expired rows included)", len(eps))
	}
	if eps[0].EndpointID != "ep-a1" || eps[1].EndpointID != "ep-b1" {
		t.Fatalf("order = %s,%s, want pod/endpoint_id order", eps[0].EndpointID, eps[1].EndpointID)
	}
	if eps[0].LastSeenAt.After(eps[1].LastSeenAt) {
		t.Fatal("the expired row must keep its stale last_seen_at — the loop applies the TTL itself")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}
