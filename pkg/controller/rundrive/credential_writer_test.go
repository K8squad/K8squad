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

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/orgops"
	"github.com/K8squad/K8squad/pkg/taskio"
)

func credWriterScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := api.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func testMinter(t *testing.T) *taskio.Minter {
	t.Helper()
	m, err := taskio.NewMinter([]byte("0123456789abcdef0123456789abcdef"), 0)
	if err != nil {
		t.Fatalf("minter: %v", err)
	}
	return m
}

func sandboxPod(name, ns, uid string) *corev1.Pod {
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:      name,
		Namespace: ns,
		UID:       types.UID(uid),
		Labels:    map[string]string{"app": "k8squad-sandbox", "sandbox": name},
	}}
}

// The Bind-path writer mints the run-scoped credential and writes it to the
// per-sandbox Secret with the SAME scopes/principal the shim path would mint
// (scope parity), places it in the pod's namespace, and owns it by the pod.
func TestSecretCredentialWriter_WritesScopedSecret(t *testing.T) {
	const ns = "bmad-squad"
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: types.UID("run-uid-1")},
		Spec:       api.RunSpec{WorkItemRef: "wi-1", Agents: []api.ObjectRef{{Name: "pm-agent"}}},
	}
	seed := []client.Object{
		roleWithReportsTo("ceo", ns, ""),
		roleWithReportsTo("product-manager", ns, "ceo"),
		roleWithReportsTo("coder", ns, "product-manager"), // pm has a subordinate → manager → org:write
		agentWithRole("pm-agent", ns, "product-manager"),
		run,
		sandboxPod("sbx-1", ns, "pod-uid-1"),
	}
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).WithObjects(seed...).Build()

	minter := testMinter(t)
	w := NewSecretCredentialWriter(cl, minter, "http://coord.ksquad-system.svc:8080")
	if w == nil {
		t.Fatal("writer nil with client+minter+url")
	}
	if err := w.WriteRunCredential(context.Background(), "run-uid-1", "sbx-1"); err != nil {
		t.Fatalf("write: %v", err)
	}

	var sec corev1.Secret
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "sbx-1"}, &sec); err != nil {
		t.Fatalf("secret not written: %v", err)
	}
	// Content: the four task-io keys, in the pod's namespace, named == pod.
	if got := string(sec.Data[taskio.EnvCoordURL]); got != "http://coord.ksquad-system.svc:8080" {
		t.Errorf("KSQUAD_COORD_URL = %q", got)
	}
	if got := string(sec.Data[taskio.EnvWorkItemID]); got != "wi-1" {
		t.Errorf("WORK_ITEM_ID = %q, want wi-1", got)
	}
	if got := string(sec.Data[taskio.EnvRunID]); got != "run-uid-1" {
		t.Errorf("RUN_ID = %q, want run-uid-1", got)
	}
	// Owned by the pod for auto-GC.
	if len(sec.OwnerReferences) != 1 || sec.OwnerReferences[0].Kind != "Pod" ||
		string(sec.OwnerReferences[0].UID) != "pod-uid-1" {
		t.Errorf("OwnerReferences = %+v, want the sandbox pod", sec.OwnerReferences)
	}

	// Scope parity: the token verifies with the SAME (run, item, principal,
	// scopes) the shim path derives — a product-manager Role gets org:write.
	tok, err := minter.Verify(string(sec.Data[taskio.EnvCoordToken]))
	if err != nil {
		t.Fatalf("verify token: %v", err)
	}
	if tok.RunID != "run-uid-1" || tok.WorkItemID != "wi-1" || tok.Principal != "pm-agent" {
		t.Errorf("token binding = %+v, want run-uid-1/wi-1/pm-agent", tok)
	}
	if !tok.HasScope(orgops.ScopeOrgWrite) {
		t.Errorf("token missing %s (scope parity with shim path broken); scopes=%v", orgops.ScopeOrgWrite, tok.Scopes)
	}
}

// A re-bind create-or-updates the same Secret without error (idempotent).
func TestSecretCredentialWriter_Idempotent(t *testing.T) {
	const ns = "bmad-squad"
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: types.UID("run-uid-2")},
		Spec:       api.RunSpec{WorkItemRef: "wi-2", Agents: []api.ObjectRef{{Name: "coder-agent"}}},
	}
	seed := []client.Object{
		roleWithReportsTo("coder", ns, ""),
		agentWithRole("coder-agent", ns, "coder"),
		run,
		sandboxPod("sbx-2", ns, "pod-uid-2"),
	}
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).WithObjects(seed...).Build()
	w := NewSecretCredentialWriter(cl, testMinter(t), "http://coord/")

	for i := 0; i < 2; i++ {
		if err := w.WriteRunCredential(context.Background(), "run-uid-2", "sbx-2"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}
	var secs corev1.SecretList
	if err := cl.List(context.Background(), &secs, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if n := len(secs.Items); n != 1 {
		t.Fatalf("re-bind produced %d Secrets, want exactly 1 (idempotent)", n)
	}
}

// No work item ⇒ no Secret (fail-safe: an absent token makes the coord API
// refuse, never fail-open), same as the shim path's skip.
func TestSecretCredentialWriter_NoWorkItemSkips(t *testing.T) {
	const ns = "bmad-squad"
	run := &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "r1", Namespace: ns, UID: types.UID("run-uid-3")},
		Spec:       api.RunSpec{Agents: []api.ObjectRef{{Name: "a"}}}, // no WorkItemRef
	}
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).
		WithObjects(run, sandboxPod("sbx-3", ns, "pod-uid-3")).Build()
	w := NewSecretCredentialWriter(cl, testMinter(t), "http://coord/")

	if err := w.WriteRunCredential(context.Background(), "run-uid-3", "sbx-3"); err != nil {
		t.Fatalf("write: %v", err)
	}
	var secs corev1.SecretList
	if err := cl.List(context.Background(), &secs, client.InNamespace(ns)); err != nil {
		t.Fatal(err)
	}
	if len(secs.Items) != 0 {
		t.Fatalf("wrote a Secret for a work-item-less Run: %d", len(secs.Items))
	}
}

// Nil minter or empty URL ⇒ nil writer (credential-off), like the shim pairing.
func TestNewSecretCredentialWriter_OffWhenUnconfigured(t *testing.T) {
	cl := fake.NewClientBuilder().WithScheme(credWriterScheme(t)).Build()
	if NewSecretCredentialWriter(cl, nil, "http://coord/") != nil {
		t.Error("nil minter should yield nil writer")
	}
	if NewSecretCredentialWriter(cl, testMinter(t), "") != nil {
		t.Error("empty coord URL should yield nil writer")
	}
}
