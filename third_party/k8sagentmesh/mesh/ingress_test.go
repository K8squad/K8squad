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
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Test battery re-homed from the reference host's operator ingress tests
// (rundrive meshingress tests, ISI-5822 P1.1): the store is swapped for the
// in-memory fakeRegistry — the assertions are the same contract checks.

func newIngress(t *testing.T) (*Ingress, *fakeRegistry, *httptest.Server) {
	t.Helper()
	reg := newFakeRegistry()
	in, err := NewIngress(reg)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(in.Handler())
	t.Cleanup(srv.Close)
	return in, reg, srv
}

// A registration POST upserts every listed endpoint and prunes the pod's
// unlisted ids — one heartbeat beat, end to end.
func TestIngress_Register(t *testing.T) {
	_, reg, srv := newIngress(t)

	body := Registration{
		Pod: "pool-aaa", Team: "team-acme", Role: "coder",
		Processes: []RegisteredProcess{
			{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"},
			{EndpointID: "e1", Endpoint: "http://10.0.0.1:8082"},
		},
	}
	payload, _ := json.Marshal(body)
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/register = %d, want 200", resp.StatusCode)
	}
	for _, id := range []string{"e0", "e1"} {
		ep, ok := reg.get(id)
		if !ok {
			t.Fatalf("endpoint %s not registered", id)
		}
		if ep.Team != "team-acme" || ep.Role != "coder" || ep.Pod != "pool-aaa" {
			t.Fatalf("endpoint %s = %+v, wrong identity", id, ep)
		}
	}
	if len(reg.prunes) != 1 {
		t.Fatalf("prunes = %d, want 1 (the per-beat reconcile)", len(reg.prunes))
	}
	if got := reg.prunes[0]; got.pod != "pool-aaa" || len(got.keep) != 2 {
		t.Fatalf("prune = %+v, want the pod's full keep-set", got)
	}
}

// Malformed or identity-less registrations are rejected 400 — never a
// half-identified row the lookup could route to.
func TestIngress_RegisterValidation(t *testing.T) {
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
// the registry right away. The ingress must accept it (200) and prune the
// whole pod — the blanket-400 refusal broke the stop→prune contract.
func TestIngress_RegisterEmptySetPrunesPod(t *testing.T) {
	_, reg, srv := newIngress(t)

	payload, _ := json.Marshal(Registration{
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
	if len(reg.prunes) != 1 || reg.prunes[0].pod != "pool-aaa" || len(reg.prunes[0].keep) != 0 {
		t.Fatalf("prunes = %+v, want one whole-pod prune", reg.prunes)
	}
}

// A registry failure surfaces as 500, not a silent 200 — the gateway's next
// heartbeat retries and the sink reports the miss.
func TestIngress_RegisterStoreError(t *testing.T) {
	_, reg, srv := newIngress(t)
	reg.mu.Lock()
	reg.errRegister = context.DeadlineExceeded
	reg.mu.Unlock()

	payload, _ := json.Marshal(Registration{
		Pod: "pool-aaa", Team: "team-acme", Role: "coder",
		Processes: []RegisteredProcess{{EndpointID: "e0", Endpoint: "http://10.0.0.1:8081"}},
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
func TestIngress_Deregister(t *testing.T) {
	_, reg, srv := newIngress(t)

	resp, err := http.Post(srv.URL+"/deregister", "application/json",
		strings.NewReader(`{"pod":"pool-aaa"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/deregister = %d, want 200", resp.StatusCode)
	}
	if len(reg.deregPods) != 1 || reg.deregPods[0] != "pool-aaa" {
		t.Fatalf("deregisters = %v, want pool-aaa", reg.deregPods)
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
func TestNewIngress_RequiresRegistry(t *testing.T) {
	if _, err := NewIngress(nil); err == nil {
		t.Fatal("expected nil-registry error")
	}
}

// ServeIngress returns cleanly on context cancellation (the supervisor
// shutdown path) and mounts the handler under /mesh.
func TestServeIngress_Shutdown(t *testing.T) {
	reg := newFakeRegistry()
	in, err := NewIngress(reg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if err := ServeIngress(ctx, "127.0.0.1:0", in); err != nil {
		t.Fatalf("ServeIngress: %v", err)
	}
}

// The /health endpoint answers 200 when the registry is reachable (the
// host control-plane health probe's "gateway" dependency). A registry
// failure surfaces as 500.
func TestIngress_Health(t *testing.T) {
	_, _, srv := newIngress(t)
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/health = %d, want 200", resp.StatusCode)
	}
}

// A registry failure surfaces as 500 on /health.
func TestIngress_HealthRegistryDown(t *testing.T) {
	_, reg, srv := newIngress(t)
	reg.mu.Lock()
	reg.errLookup = context.DeadlineExceeded
	reg.mu.Unlock()

	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("/health = %d, want 500 on registry failure", resp.StatusCode)
	}
}
