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
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"

	"github.com/K8squad/K8squad/pkg/coord"
)

// newSupervisor returns a MeshSupervisor bound to a mocked registry.
func newSupervisor(t *testing.T, opts ...MeshSupervisorOption) (*MeshSupervisor, sqlmock.Sqlmock) {
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
	svc, err := NewMeshSupervisor(registry, opts...)
	if err != nil {
		t.Fatal(err)
	}
	return svc, mock
}

// Construction guard: nil registry is rejected.
func TestNewMeshSupervisor_NilRegistry(t *testing.T) {
	if _, err := NewMeshSupervisor(nil); err == nil {
		t.Fatal("expected nil-registry error")
	}
}

// The supervisor loop ticks on its configured interval and calls Sweep on
// the registry (ISI-6010 P1.4b: TTL-expiry drain path).
func TestMeshSupervisor_SweepOnTick(t *testing.T) {
	svc, mock := newSupervisor(t, WithMeshSupervisorSweepInterval(10*time.Millisecond))
	// Two ticks: first at t=0 (instant), second after 10ms. Each must issue
	// a DELETE for TTL-expired rows.
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE last_seen_at \\< \\$1").
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE last_seen_at \\< \\$1").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// Sweep failures are surfaced (logged) but do not stop the loop.
func TestMeshSupervisor_SweepErrorContinues(t *testing.T) {
	svc, mock := newSupervisor(t, WithMeshSupervisorSweepInterval(10*time.Millisecond))
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE").
		WillReturnError(context.DeadlineExceeded)
	// Loop must continue: expect a second DELETE on the next tick.
	mock.ExpectExec("DELETE FROM coord.mesh_endpoint WHERE").
		WillReturnResult(sqlmock.NewResult(0, 0))

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// The supervisor stops cleanly on context cancellation.
func TestMeshSupervisor_Shutdown(t *testing.T) {
	svc, mock := newSupervisor(t, WithMeshSupervisorSweepInterval(time.Hour))
	// With a long interval, the first sweep won't happen before we cancel,
	// so we don't expect any DELETE. The loop exits cleanly on ctx.Done().

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	if err := svc.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	// No sweep should have occurred, so no expectations.
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatal(err)
	}
}

// WithMeshSupervisorClock allows tests to pin the clock so TTL computations
// are deterministic without real-time sleeps.
func TestWithMeshSupervisorClock(t *testing.T) {
	now := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	svc, _ := newSupervisor(t,
		WithMeshSupervisorClock(func() time.Time { return now }),
		WithMeshSupervisorSweepInterval(time.Millisecond),
	)
	if svc.now().Year() != 2024 {
		t.Fatal("clock override did not take effect")
	}
}
