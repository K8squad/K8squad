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
	"fmt"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// egressGuardScheme is the test scheme for the guard: the API types plus
// networking (the guard lists NetworkPolicies).
func egressGuardScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatalf("add api scheme: %v", err)
	}
	if err := networkingv1.AddToScheme(s); err != nil {
		t.Fatalf("add networking scheme: %v", err)
	}
	return s
}

// egressPolicedNS builds a namespace carrying an egress NetworkPolicy (the
// team default-deny posture), or not, and returns a fake client seeded with
// it plus the Run under test.
func egressGuardClient(t *testing.T, policed bool) (client.Client, *api.Run) {
	t.Helper()
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-a", UID: types.UID("11111111-1111-1111-1111-111111111111")},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "t"},
			ProjectRef:  api.ObjectRef{Name: "p"},
			WorkItemRef: "10000000-0000-0000-0000-000000000001",
			Agents:      []api.ObjectRef{{Name: "sam"}},
		},
	}
	b := fake.NewClientBuilder().WithScheme(egressGuardScheme(t)).WithObjects(run)
	if policed {
		b = b.WithObjects(&networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{Name: "ksquad-default-deny", Namespace: "team-a"},
			Spec: networkingv1.NetworkPolicySpec{
				PodSelector: metav1.LabelSelector{},
				PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
			},
		})
	}
	return b.Build(), run
}

func guardDispatch(t *testing.T, policed bool, env map[string]string, baseURL string) error {
	t.Helper()
	cl, run := egressGuardClient(t, policed)
	d := &operatorDispatch{cfg: OperatorDispatchConfig{
		Client: cl,
		SandboxProxyEnv: func() map[string]string {
			if env == nil {
				return map[string]string{}
			}
			return env
		},
	}}
	return d.guardDispatchEgress(context.Background(), run, baseURL)
}

// TestGuardDispatchEgressTripsOnlyOnHarmfulTriple is the ISI-7705 guard matrix:
// fail ONLY when external FQDN + no effective proxy for the host + egress-
// policed namespace; every healthy config passes.
func TestGuardDispatchEgressTripsOnlyOnHarmfulTriple(t *testing.T) {
	proxyEnv := map[string]string{
		"KSQUAD_SANDBOX_HTTPS_PROXY": "http://egress-proxy.team-a.svc.cluster.local:8080",
	}
	cases := []struct {
		name    string
		baseURL string
		policed bool
		env     map[string]string
		wantErr bool
	}{
		// The harmful triple (ISI-7686): external FQDN, proxy-less, policed.
		{"external fqdn + no proxy + policed => blocked", "https://api.deepseek.com/v1", true, nil, true},
		{"external fqdn + blank proxy + policed => blocked", "https://api.deepseek.com/v1", true,
			map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "  "}, true},
		{"external fqdn + NO_PROXY=* + policed => blocked (bypass all)", "https://api.deepseek.com/v1", true,
			map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://proxy:8080", "KSQUAD_SANDBOX_NO_PROXY": "*"}, true},
		{"external fqdn + NO_PROXY covers host + policed => blocked", "https://api.deepseek.com/v1", true,
			map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://proxy:8080", "KSQUAD_SANDBOX_NO_PROXY": "api.deepseek.com"}, true},
		{"external fqdn + NO_PROXY suffix covers host + policed => blocked", "https://api.deepseek.com/v1", true,
			map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://proxy:8080", "KSQUAD_SANDBOX_NO_PROXY": ".deepseek.com"}, true},
		// Healthy configs — never trip.
		{"external fqdn + proxy + policed => ok", "https://api.deepseek.com/v1", true, proxyEnv, false},
		{"external fqdn + proxy + NO_PROXY other host + policed => ok", "https://api.deepseek.com/v1", true,
			map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://proxy:8080", "KSQUAD_SANDBOX_NO_PROXY": "internal.svc.cluster.local"}, false},
		{"external fqdn + no proxy + NO netpol (dev cluster) => ok", "https://api.deepseek.com/v1", false, nil, false},
		{"lan ollama ip + no proxy + policed => ok (supported proxy-less)", "http://10.0.0.185:11434", true, nil, false},
		{"in-cluster svc + no proxy + policed => ok", "http://ollama.team-a.svc.cluster.local:11434", true, nil, false},
		{"empty endpoint (provider default) => ok", "", true, nil, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := guardDispatch(t, c.policed, c.env, c.baseURL)
			if c.wantErr {
				if !IsEgressBlocked(err) {
					t.Fatalf("want EgressBlockedError, got %v", err)
				}
				var eb *EgressBlockedError
				if !asEgressBlocked(err, &eb) {
					t.Fatalf("error does not unwrap to *EgressBlockedError: %T", err)
				}
				if eb.Host == "" || eb.Namespace != "team-a" || eb.Endpoint != c.baseURL {
					t.Fatalf("evidence fields wrong: %+v", eb)
				}
				return
			}
			if err != nil {
				t.Fatalf("want pass-through, got %v", err)
			}
		})
	}
}

// asEgressBlocked mirrors errors.As for the typed cause.
func asEgressBlocked(err error, target **EgressBlockedError) bool {
	for err != nil {
		if eb, ok := err.(*EgressBlockedError); ok {
			*target = eb
			return true
		}
		u, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}

// TestIsEgressBlockedSurvivesWrapChain: the sentinel must classify through the
// a2a submit → effects wrapping back to the driver (the live wrap shape).
func TestIsEgressBlockedSurvivesWrapChain(t *testing.T) {
	cause := &EgressBlockedError{Endpoint: "https://api.deepseek.com/v1", Host: "api.deepseek.com", Namespace: "team-a"}
	wrapped := fmt.Errorf("a2a: build task for run %s: %w", "run-1",
		fmt.Errorf("coord.ProdEffects.Dispatch: submit: %w", cause))
	if !IsEgressBlocked(wrapped) {
		t.Fatal("IsEgressBlocked must see through the wrap chain")
	}
	if IsEgressBlocked(fmt.Errorf("some transient: %w", errEndpointSlotBusy)) {
		t.Fatal("unrelated errors must NOT classify as egress-blocked")
	}
	if IsEgressBlocked(nil) {
		t.Fatal("nil must not classify")
	}
}

// TestProxyCoversHost pins the effective-proxy semantics shared with the
// ISI-7691 boot guard (whitespace-only is not proxied; NO_PROXY=* is a full
// bypass) plus the per-host NO_PROXY coverage the boot guard could not do.
func TestProxyCoversHost(t *testing.T) {
	cases := []struct {
		env      map[string]string
		host     string
		covered  bool
	}{
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080"}, "api.deepseek.com", true},
		{map[string]string{"KSQUAD_SANDBOX_HTTP_PROXY": "http://p:8080"}, "api.deepseek.com", true},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": " "}, "api.deepseek.com", false},
		{map[string]string{}, "api.deepseek.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "NO_PROXY": "*"}, "api.deepseek.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "api.deepseek.com"}, "api.deepseek.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "example.com"}, "api.example.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": ".example.com"}, "api.example.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "API.EXAMPLE.COM:443"}, "api.example.com", false},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "other.com, localhost"}, "api.deepseek.com", true},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "example.com"}, "notexample.com", true},
		{map[string]string{"KSQUAD_SANDBOX_HTTPS_PROXY": "http://p:8080", "KSQUAD_SANDBOX_NO_PROXY": "ample.com"}, "example.com", true}, // suffix must be dot-bounded
	}
	for _, c := range cases {
		if got := proxyCoversHost(c.env, c.host); got != c.covered {
			t.Errorf("proxyCoversHost(%v, %q) = %v, want %v", c.env, c.host, got, c.covered)
		}
	}
}

// TestDriveEgressBlockedParksAndStampsCondition (ISI-7705): a dispatch whose
// resolved endpoint trips the egress guard (external FQDN + proxy-less
// sandbox + egress policy) must be terminated at dispatch — NOT requeued into
// the 134s-hang loop — via FailClosedPark with the sandbox_egress_blocked
// reason, AND the SandboxEgressBlocked condition must land on the Run CR as
// the durable, non-null evidence.
func TestDriveEgressBlockedParksAndStampsCondition(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1, failParkOK: true}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	// The exact wrap the live path produces: buildTask's guard error → a2a
	// submit wrap → the dispatch effect's error back up to the effects seam.
	eff := &fakeMachineEffects{err: fmt.Errorf("coord.ProdEffects.Dispatch: submit: %w",
		fmt.Errorf("a2a: build task for run %s: %w", "11111111-1111-1111-1111-111111111111",
			&EgressBlockedError{Endpoint: "https://api.deepseek.com/v1", Host: "api.deepseek.com", Namespace: "default"}))}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("egress-blocked must be absorbed by the terminal, not surfaced: %v", err)
	}
	if !claims.failParkCall {
		t.Fatal("an egress-blocked dispatch must call FailClosedPark (park the item)")
	}
	if claims.failParkReason != "sandbox_egress_blocked" {
		t.Fatalf("park reason = %q, want sandbox_egress_blocked", claims.failParkReason)
	}
	if claims.failCall {
		t.Fatal("egress-blocked must use FailClosedPark, not the plain FailEnter")
	}
	var got api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &got); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionSandboxEgressBlocked)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("SandboxEgressBlocked condition not stamped on the Run: %+v", got.Status.Conditions)
	}
	if cond.Reason != "ProxylessExternalEndpoint" || cond.Message == "" {
		t.Fatalf("condition must carry the non-null cause: %+v", cond)
	}
}

// TestDriveEgressBlockedParkRaceAbsorbs: FailClosedPark ok=false (another path
// settled this generation first) is a clean absorb — no error, no requeue.
func TestDriveEgressBlockedParkRaceAbsorbs(t *testing.T) {
	run := newTestRun("11111111-1111-1111-1111-111111111111", "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1, failParkOK: false}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{err: fmt.Errorf("submit: %w", &EgressBlockedError{Host: "api.deepseek.com", Namespace: "default"})}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("a lost park race must absorb, not surface an error: %v", err)
	}
	if !claims.failParkCall {
		t.Fatal("FailClosedPark must still be attempted")
	}
}
