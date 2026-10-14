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
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeMeshLookup is the registry-read seam bound in tests.
type fakeMeshLookup struct {
	eps []coord.MeshEndpoint
	err error
}

func (f fakeMeshLookup) LookupTeamRole(context.Context, string, string) ([]coord.MeshEndpoint, error) {
	return f.eps, f.err
}

// meshRoute stays dark with no config, an unflagged team, or an empty
// registry (the reversibility fallback); routes to the first LRU endpoint
// for a flagged team; errors loudly on a registry failure.
func TestMeshRoute_Decisions(t *testing.T) {
	eps := []coord.MeshEndpoint{{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"}}
	task := wire.Task{A2ATaskID: "run-1", Identity: wire.AgentIdentity{Squad: "team-acme", Name: "coder"}}

	t.Run("nil config", func(t *testing.T) {
		d := &operatorDispatch{}
		if _, use, err := d.meshRoute(context.Background(), task); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("unflagged team", func(t *testing.T) {
		d := &operatorDispatch{cfg: OperatorDispatchConfig{MeshRoute: &MeshRouteConfig{
			Teams: MeshTeamSet{"other": {}}, Lookup: fakeMeshLookup{eps: eps},
		}}}
		if _, use, err := d.meshRoute(context.Background(), task); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy no-op", use, err)
		}
	})
	t.Run("empty registry falls back to legacy", func(t *testing.T) {
		d := &operatorDispatch{cfg: OperatorDispatchConfig{MeshRoute: &MeshRouteConfig{
			Teams: MeshTeamSet{"team-acme": {}}, Lookup: fakeMeshLookup{},
		}}}
		if _, use, err := d.meshRoute(context.Background(), task); use || err != nil {
			t.Fatalf("use=%v err=%v, want legacy fallback", use, err)
		}
	})
	t.Run("flagged team routes to first endpoint", func(t *testing.T) {
		d := &operatorDispatch{cfg: OperatorDispatchConfig{MeshRoute: &MeshRouteConfig{
			Teams: MeshTeamSet{"team-acme": {}}, Lookup: fakeMeshLookup{eps: eps},
		}}}
		url, use, err := d.meshRoute(context.Background(), task)
		if err != nil || !use {
			t.Fatalf("use=%v err=%v, want mesh route", use, err)
		}
		if url != "http://10.0.0.1:8081/task" {
			t.Fatalf("url = %q, want the endpoint /task surface", url)
		}
	})
	t.Run("registry error is loud", func(t *testing.T) {
		d := &operatorDispatch{cfg: OperatorDispatchConfig{MeshRoute: &MeshRouteConfig{
			Teams: MeshTeamSet{"team-acme": {}}, Lookup: fakeMeshLookup{err: context.Canceled},
		}}}
		if _, _, err := d.meshRoute(context.Background(), task); err == nil {
			t.Fatal("want error on lookup failure")
		}
	})
}

// MeshTeamSet parses the per-team canary list (D9.3).
func TestMeshTeamSet_Parsing(t *testing.T) {
	s := NewMeshTeamSet(" team-acme , , other ")
	if !s.Enabled("team-acme") || !s.Enabled("other") || s.Enabled("team-b") {
		t.Fatalf("set = %v", s)
	}
	if len(MeshTeamSetFromEnv(func(string) string { return "" })) != 0 {
		t.Fatal("unset env must yield an empty set (flag off cluster-wide)")
	}
}

// newMeshDispatchTest assembles an operatorDispatch whose mesh leg points at
// a live fake standing process (an httptest server speaking the /task NDJSON
// wire), plus the Run CR the annotation stamps.
func newMeshDispatchTest(t *testing.T, teams MeshTeamSet, lookup MeshEndpointLookup) (*operatorDispatch, *httptest.Server, *atomic.Int32) {
	t.Helper()
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/task" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusNotFound)
			return
		}
		var task wire.Task
		_ = json.NewDecoder(r.Body).Decode(&task)
		hits.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		// One status event then EOF: the task settles, the session terminal.
		_ = json.NewEncoder(w).Encode(wire.Event{Seq: 1, Type: wire.EventStatus, Payload: wire.StatusPayload{
			State: wire.TaskCompleted,
		}})
	}))
	t.Cleanup(srv.Close)

	const runUID = "dddddddd-0000-4444-8888-dddddddddddd"
	run := &v1alpha1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-mesh", Namespace: "team-acme", UID: types.UID(runUID)},
		Spec:       v1alpha1.RunSpec{TeamRef: v1alpha1.ObjectRef{Name: "team-acme"}},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).WithObjects(run).Build()

	d := &operatorDispatch{
		cfg: OperatorDispatchConfig{
			Client:    cl,
			MeshRoute: &MeshRouteConfig{Teams: teams, Lookup: lookup},
		},
		httpClient: srv.Client(),
	}
	return d, srv, &hits
}

func meshTestTask(runUID string) wire.Task {
	return wire.Task{
		A2ATaskID: runUID,
		Identity:  wire.AgentIdentity{Squad: "team-acme", Name: "coder"},
	}
}

// Submit routes a flagged team's task to the registered endpoint AND stamps
// the Run's dispatch-path annotation — the run-end canary slice's source.
func TestSubmit_MeshRoutesAndStampsRun(t *testing.T) {
	eps := []coord.MeshEndpoint{{EndpointID: "e0", Endpoint: ""}} // endpoint filled below
	d, srv, hits := newMeshDispatchTest(t, MeshTeamSet{"team-acme": {}}, nil)
	eps[0].Endpoint = srv.URL
	d.cfg.MeshRoute.Lookup = fakeMeshLookup{eps: eps}

	st := sandboxTransport{d}
	sess, err := st.Submit(context.Background(), meshTestTask("dddddddd-0000-4444-8888-dddddddddddd"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	// Drain the session to settlement.
	for range sess.Events() {
	}
	_ = sess.Close()
	if n := hits.Load(); n != 1 {
		t.Fatalf("mesh endpoint hits = %d, want 1", n)
	}

	var run v1alpha1.Run
	if err := d.cfg.Client.Get(context.Background(), client.ObjectKey{Namespace: "team-acme", Name: "run-mesh"}, &run); err != nil {
		t.Fatal(err)
	}
	if got := run.Annotations[annotationDispatchPath]; got != "mesh" {
		t.Fatalf("dispatch-path annotation = %q, want mesh", got)
	}
}

// Submit with the flag off never contacts the registry-backed endpoint: the
// legacy path (no sandboxRef → stdio topology attempt) runs instead, and no
// annotation is stamped. This is the D9.3 flag-off byte-identical guarantee
// at the transport seam.
func TestSubmit_FlagOffSkipsMesh(t *testing.T) {
	d, srv, hits := newMeshDispatchTest(t, MeshTeamSet{"other": {}}, fakeMeshLookup{})

	// Point the lookup at the server anyway: a flag-off dispatch must not
	// reach it even though an endpoint exists.
	d.cfg.MeshRoute.Lookup = fakeMeshLookup{eps: []coord.MeshEndpoint{{EndpointID: "e0", Endpoint: srv.URL}}}

	st := sandboxTransport{d}
	// The legacy leg with no bound sandbox spawns `shim run` via stdio —
	// shortcut: with no shimBin configured the stdio transport fails fast,
	// which is enough to prove the mesh leg was skipped (no hit, no error
	// from the mesh side). Assert by hitting count only.
	task := meshTestTask("dddddddd-0000-4444-8888-dddddddddddd")
	_, _ = st.Submit(context.Background(), task)
	if n := hits.Load(); n != 0 {
		t.Fatalf("mesh endpoint hits = %d, want 0 with the flag off", n)
	}
	var run v1alpha1.Run
	if err := d.cfg.Client.Get(context.Background(), client.ObjectKey{Namespace: "team-acme", Name: "run-mesh"}, &run); err != nil {
		t.Fatal(err)
	}
	if _, ok := run.Annotations[annotationDispatchPath]; ok {
		t.Fatal("flag-off dispatch must not stamp the mesh annotation")
	}
}
