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

package sandbox

import (
	"context"
	"testing"

	nodev1 "k8s.io/api/node/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestResolveRuntimeClassPrecedence (AC1): Run policy > Role hint > Pool
// class > gVisor default.
func TestResolveRuntimeClassPrecedence(t *testing.T) {
	cases := []struct {
		name string
		sel  Selection
		want string
	}{
		{"all set", Selection{RunPolicy: "kata", RoleHint: "gvisor", PoolClass: "gvisor"}, "kata"},
		{"role wins over pool", Selection{RoleHint: "kata", PoolClass: "gvisor"}, "kata"},
		{"pool only", Selection{PoolClass: "kata"}, "kata"},
		{"default", Selection{}, DefaultRuntimeClass},
	}
	for _, tc := range cases {
		if got := ResolveRuntimeClass(tc.sel); got != tc.want {
			t.Errorf("%s: ResolveRuntimeClass = %q, want %q", tc.name, got, tc.want)
		}
	}
	if DefaultRuntimeClass != ClassGVisor {
		t.Errorf("§9.1 ratified default must be gvisor, got %q", DefaultRuntimeClass)
	}
}

// TestAdmitRuntimeClassRejectsUntrusted (AC2 — the AD-3 crux): runc, the
// empty/node-default class, and any non-approved class are rejected for
// untrusted code; gVisor/Kata pass; the explicit trustedDev escape is the
// only opt-out.
func TestAdmitRuntimeClassRejectsUntrusted(t *testing.T) {
	for _, class := range []string{ClassGVisor, ClassKata} {
		if err := AdmitRuntimeClass(class, false); err != nil {
			t.Errorf("approved class %q rejected: %v", class, err)
		}
	}
	for _, class := range []string{ClassRunc, "", "sysbox", "some-weak-runtime"} {
		if err := AdmitRuntimeClass(class, false); err == nil {
			t.Errorf("class %q admitted for untrusted code (AC2 violation)", class)
		} else if !IsPolicyError(err) {
			t.Errorf("class %q: error is not a PolicyError", class)
		}
	}
	// trustedDev escape admits runc (audited, non-default opt-out).
	if err := AdmitRuntimeClass(ClassRunc, true); err != nil {
		t.Errorf("trustedDev escape rejected runc: %v", err)
	}
	// trustedDev + empty selection = node default = runc-equivalent: still
	// admitted on the escape.
	if err := AdmitRuntimeClass("", true); err != nil {
		t.Errorf("trustedDev escape rejected the node-default runtime: %v", err)
	}
	// The escape admits runc ONLY (Cursor review): an operator-added weak
	// isolation runtime is rejected exactly like it would be without the
	// flag, so the allowlist keeps meaning something.
	for _, class := range []string{"sysbox", "runsc-weak", "some-weak-runtime"} {
		if err := AdmitRuntimeClass(class, true); err == nil {
			t.Errorf("trustedDev escape admitted non-runc class %q — the allowlist must stay meaningful", class)
		} else if !IsPolicyError(err) {
			t.Errorf("class %q with trustedDev: error is not a PolicyError", class)
		}
	}
}

func trustedRun() *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "r-1",
			Namespace:   "ksquad-team-alpha-38152234",
			UID:         types.UID("uid-r-1"),
			Annotations: map[string]string{TrustedDevAnnotation: "true"},
		},
	}
}

func untrustedRun() *api.Run {
	r := trustedRun()
	r.Name = "r-2"
	r.UID = types.UID("uid-r-2")
	r.Annotations = nil
	return r
}

func fakeClientWithRuntimeClasses(t *testing.T, classes ...string) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := nodev1.AddToScheme(s); err != nil {
		t.Fatalf("add node scheme: %v", err)
	}
	objs := []client.Object{}
	for _, name := range classes {
		objs = append(objs, &nodev1.RuntimeClass{ObjectMeta: metav1.ObjectMeta{Name: name}})
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

// TestSelectRuntimeClassFailClosedOnMissing (AC4): a resolved class that does
// not exist on the cluster fails the Run closed — never a downgrade to runc.
func TestSelectRuntimeClassFailClosedOnMissing(t *testing.T) {
	c := fakeClientWithRuntimeClasses(t, ClassGVisor) // kata NOT installed

	if _, err := SelectRuntimeClass(context.Background(), c, untrustedRun(), Selection{}); err != nil {
		t.Errorf("gvisor installed but selection failed: %v", err)
	}

	_, err := SelectRuntimeClass(context.Background(), c, untrustedRun(), Selection{RunPolicy: ClassKata})
	if err == nil {
		t.Fatalf("missing kata admitted (AC4 violation: silent downgrade path)")
	}
	if !IsRuntimeClassUnavailable(err) {
		t.Errorf("missing-class error is not RuntimeClassUnavailable: %v", err)
	}

	// trustedDev + explicit runc policy pins the concrete node-default name
	// and still requires the class to exist (fail-closed even on the escape).
	_, err = SelectRuntimeClass(context.Background(), c, trustedRun(), Selection{RunPolicy: ClassRunc})
	if !IsRuntimeClassUnavailable(err) {
		t.Errorf("trustedDev runc with no installed runc class should be unavailable, got %v", err)
	}

	// The precedence default is gvisor for an empty selection — an empty
	// selection can never silently mean the node-default runtime.
	if class, err := SelectRuntimeClass(context.Background(), c, trustedRun(), Selection{}); err != nil || class != ClassGVisor {
		t.Errorf("empty selection = (%q, %v), want (gvisor, nil)", class, err)
	}
}
