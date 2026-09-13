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

	"github.com/DATA-DOG/go-sqlmock"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// ISI-4354: a Run whose spec.workItemRef does not parse as a work-item uuid
// (live: "verif-isi4334" — every ::uuid read 22P02'd) must hit a TERMINAL
// path: the InvalidWorkItemRef condition stamped on the status, no error, no
// requeue — never a controller-runtime backoff loop with no exit.

const badRefRunUID = "99999999-9999-9999-9999-999999999999"

// badRefFixture builds a driver over a Run with a malformed workItemRef. The
// fake claims carry a stateErr: if the driver EVER reached Claims.State the
// reconcile would error — proving the guard short-circuits before any
// coordination read.
func badRefFixture(t *testing.T, mutate func(*api.Run)) (*Driver, *fakeClaims, client.Client) {
	t.Helper()
	run := newTestRun(badRefRunUID, "verif-isi4334") // the live defect's exact shape
	if mutate != nil {
		mutate(run)
	}
	claims := &fakeClaims{found: true, stateErr: errBoom}
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithObjects(run).
		WithStatusSubresource(&api.Run{}).
		Build()
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{})
	return d, claims, cl
}

// The unparseable ref is abandoned, not driven: no error, no requeue, and the
// abandonment condition is stamped on the status where `kubectl describe`
// reads it.
func TestUnparseableWorkItemRefIsAbandonedWithCondition(t *testing.T) {
	d, _, cl := badRefFixture(t, nil)

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq != 0 {
		t.Fatalf("requeue = %v, want 0 (abandoned, never requeued)", rq)
	}
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	cond := findCondition(after.Status.Conditions, ConditionInvalidWorkItemRef)
	if cond == nil {
		t.Fatalf("conditions = %+v, want a %s condition", after.Status.Conditions, ConditionInvalidWorkItemRef)
	}
	if cond.Status != metav1.ConditionTrue || cond.Reason != "UnparseableWorkItemRef" {
		t.Fatalf("condition = %+v, want True/UnparseableWorkItemRef", cond)
	}
	if cond.Message == "" {
		t.Fatal("abandonment condition must carry an actionable message")
	}
}

// The abandonment is idempotent: a second pass neither errors, requeues, nor
// restamps the condition (lastTransitionTime stable — no status flapping).
func TestUnparseableWorkItemRefAbandonmentIsIdempotent(t *testing.T) {
	d, _, cl := badRefFixture(t, nil)

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("first drive: %v", err)
	}
	var first api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &first); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	stamped := findCondition(first.Status.Conditions, ConditionInvalidWorkItemRef)
	if stamped == nil {
		t.Fatal("first pass must stamp the condition")
	}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("second drive: %v", err)
	}
	if rq != 0 {
		t.Fatalf("requeue = %v, want 0 on the idempotent re-touch", rq)
	}
	var second api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &second); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	again := findCondition(second.Status.Conditions, ConditionInvalidWorkItemRef)
	if again == nil || !again.LastTransitionTime.Equal(&stamped.LastTransitionTime) {
		t.Fatalf("condition restamped: %+v -> %+v", stamped, again)
	}
}

// A pre-existing foreign condition (e.g. the projector's Ready) survives the
// abandonment patch: the driver merges, it never clobbers the status book.
func TestUnparseableWorkItemRefPatchPreservesOtherConditions(t *testing.T) {
	ready := metav1.Condition{
		Type:               "Ready",
		Status:             metav1.ConditionFalse,
		Reason:             "Reconciling",
		Message:            "Run is progressing toward completion",
		LastTransitionTime: metav1.Now(),
	}
	d, _, cl := badRefFixture(t, func(run *api.Run) {
		run.Status.Conditions = []metav1.Condition{ready}
	})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if findCondition(after.Status.Conditions, "Ready") == nil {
		t.Fatalf("conditions = %+v, want the pre-existing Ready to survive", after.Status.Conditions)
	}
	if findCondition(after.Status.Conditions, ConditionInvalidWorkItemRef) == nil {
		t.Fatalf("conditions = %+v, want the abandonment marker", after.Status.Conditions)
	}
}

// The prod binding agrees with the driver: State on an unparseable ref is
// found=false WITHOUT issuing a query (sqlmock fails the test on any
// unexpected statement) — the 22P02 loop cannot restart at this seam either.
func TestProdClaimsStateUnparseableRefSkipsDB(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := NewProdClaims(db, OperatorPrincipal)

	for _, ref := range []string{"verif-isi4334", "not-a-uuid", "also not a uuid", ""} {
		_, found, err := c.State(context.Background(), ref)
		if err != nil || found {
			t.Fatalf("State(%q) = found=%v err=%v, want found=false/nil", ref, found, err)
		}
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("no DB statement may run for unparseable refs: %v", err)
	}
}

func findCondition(conds []metav1.Condition, condType string) *metav1.Condition {
	for i := range conds {
		if conds[i].Type == condType {
			return &conds[i]
		}
	}
	return nil
}
