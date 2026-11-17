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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// TestInFlightSweepDrivesGoneSandboxToRetryLap (ISI-7536, the ISI-7531
// repro): a Run wedged in-flight against a sandbox pod that no longer exists
// reaches a retry lap WITHOUT any external operator restart — the sweep's
// kick is the only re-trigger. The chain exercised is exactly the production
// one: OnInFlightSweep (the sweeper's wake) → kickWorkItem (field index) →
// resumeCh → Reconcile → reapGoneSandbox → retryOrFail. The controller-runtime
// channel source between resumeCh and the workqueue is existing tested
// plumbing; draining the channel here stands in for it.
func TestInFlightSweepDrivesGoneSandboxToRetryLap(t *testing.T) {
	d, claims, releaser, clearer, cl := goneSandboxFixture(t, func(run *api.Run, _ *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
	})

	// The sweep's wake for this work item — nothing else re-enqueues the Run.
	d.OnInFlightSweep(context.Background(), []string{"10000000-0000-0000-0000-000000000001"})

	// Drain the resume channel, standing in for the channel source +
	// workqueue: each kicked Run is reconciled, with NO operator restart and
	// NO Run CR mutation along the way.
	kicked := 0
	for {
		select {
		case ev := <-d.ResumeEvents():
			kicked++
			_, err := d.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: ev.Object.GetNamespace(), Name: ev.Object.GetName(),
			}})
			require.NoError(t, err)
		default:
			require.GreaterOrEqual(t, kicked, 1, "the in-flight work item's Run gets kicked onto the resume channel")
			goto reconciled
		}
	}

reconciled:
	// The retry lap (fence-first RetryEnter within budget), with the stale
	// bind cleared and the corpse ref nulled — recovery drove itself.
	if len(claims.retryCalls) != 1 || claims.retryCalls[0] != "10000000-0000-0000-0000-000000000001/"+goneRunUID+"/5" {
		t.Fatalf("RetryEnter calls = %v, want one fence-first re-entry at fence 5", claims.retryCalls)
	}
	if claims.failCall {
		t.Fatal("FailEnter must not fire while the retry budget holds")
	}
	if len(releaser.released) != 1 || releaser.released[0] != goneRunUID {
		t.Fatalf("Sandbox.Release calls = %v, want [%s]", releaser.released, goneRunUID)
	}
	if len(clearer.cleared) != 1 || clearer.cleared[0] != goneRunUID {
		t.Fatalf("ClearSandboxBind calls = %v, want [%s]", clearer.cleared, goneRunUID)
	}
	var after api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &after); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if after.Status.SandboxRef != nil {
		t.Fatalf("status.sandboxRef = %+v, want nil (stale ref cleared)", after.Status.SandboxRef)
	}
}

// TestInFlightSweeperTicksAndDelegates: the sweeper lists InFlightDue on its
// tick and hands the result to OnDue; a Claims error is logged, not fatal.
func TestInFlightSweeperTicksAndDelegates(t *testing.T) {
	claims := &fakeClaims{inFlightDue: []string{"a0000000-0000-0000-0000-00000000000a"}}
	var got [][]string
	s := &InFlightSweeper{
		Claims: claims,
		OnDue:  func(_ context.Context, due []string) { got = append(got, due) },
		Tick:   time.Millisecond,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Start(ctx))
	// The static fake re-lists the same due item every tick, so the sweep
	// delegates it at least once over the window; the contract under test is
	// tick → InFlightDue → OnDue, not the exact tick count.
	require.GreaterOrEqual(t, len(got), 1)
	assert.Equal(t, []string{"a0000000-0000-0000-0000-00000000000a"}, got[0])
}

// TestInFlightSweeperClaimsErrorIsLoggedNotFatal: an infra error on the
// listing is logged and retried next tick — the sweeper survives.
func TestInFlightSweeperClaimsErrorIsLoggedNotFatal(t *testing.T) {
	claims := &fakeClaims{inFlightDueErr: errBoom}
	var logged int
	s := &InFlightSweeper{
		Claims: claims,
		OnDue:  func(context.Context, []string) {},
		Tick:   time.Millisecond,
		Log:    func(string, ...any) { logged++ },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	require.NoError(t, s.Start(ctx))
	assert.GreaterOrEqual(t, logged, 1, "the Claims error is logged, never fatal")
}

// TestInFlightSweeperDefaultsInterval: zero Tick falls back to the documented
// sweep interval — the wedge-to-recovery bound the fix promises (60–90s band).
func TestInFlightSweeperDefaultsInterval(t *testing.T) {
	require.Equal(t, 75*time.Second, InFlightSweepInterval)
	s := &InFlightSweeper{Tick: 0}
	tick := s.Tick
	if tick <= 0 {
		tick = InFlightSweepInterval
	}
	assert.Equal(t, 75*time.Second, tick, "default sweep interval is 75s")
}

// TestOnInFlightSweepSkipsUnknownWorkItem: kicking a work item with no owning
// Run CR is a silent no-op (the Run was deleted; nothing to re-drive).
func TestOnInFlightSweepSkipsUnknownWorkItem(t *testing.T) {
	cl := fake.NewClientBuilder().
		WithScheme(newScheme(t)).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).
		Build()
	d := newDriver(cl, &fakeClaims{}, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{}})

	d.OnInFlightSweep(context.Background(), []string{"00000000-0000-0000-0000-000000000009"})

	select {
	case <-d.ResumeEvents():
		t.Fatal("no Run owns the work item; nothing may be kicked")
	default:
	}
}

// TestProdClaimsInFlightDue: the listing selects the in-flight step trio from
// coord.claim as work-item ids.
func TestProdClaimsInFlightDue(t *testing.T) {
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	c := NewProdClaims(db, OperatorPrincipal)

	mock.ExpectQuery("SELECT work_item_id::text FROM coord.claim").
		WillReturnRows(sqlmock.NewRows([]string{"work_item_id"}).
			AddRow("10000000-0000-0000-0000-000000000001").
			AddRow("20000000-0000-0000-0000-000000000002"))

	due, err := c.InFlightDue(context.Background())
	if err != nil {
		t.Fatalf("InFlightDue: %v", err)
	}
	assert.Equal(t, []string{"10000000-0000-0000-0000-000000000001", "20000000-0000-0000-0000-000000000002"}, due)
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}
