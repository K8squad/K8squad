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

	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/pkg/coord"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

// countingClaims mirrors ProdClaims.LapsUsed's ISI-7763 semantics for the
// driver fakes: the lap ledger counts ACCEPTED retry enters (one
// retry_lap_entered audit row per accepted RetryEnter), so LapsUsed reports
// the number of recorded RetryEnter calls. The canned fakeClaims.laps field
// cannot model a counter that ADVANCES as laps are entered — which is exactly
// how the pre-ISI-7763 '#lap'-marker counter wedged at zero forever.
type countingClaims struct {
	fakeClaims
}

func (c *countingClaims) LapsUsed(context.Context, string) (int, error) {
	return len(c.retryCalls), nil
}

// TestRetryStormBudgetExhaustsTerminalizes (ISI-7763): a follow_error-settled
// in-flight run re-enters the §5.3 death path on EVERY drive pass
// (followSettledNonSuccess, ISI-7524); an infra-class death gets the
// defaultInfraRetries budget. When the lap ledger counts accepted retry
// enters, the budget exhausts and the run terminalizes via FailEnter instead
// of retrying forever. Live regression on 2bcb0abc (2026-11-24): laps were
// counted from '#lap'-suffixed coord.a2a_dispatch rows that were never
// created (the retry lap id itself derived from that zero count), so
// laps < budget held forever — a ~17 laps/sec retry storm that starved
// queued runs behind never-released per-agent slots and BYO endpoint
// permits (6k+ laps per run).
func TestRetryStormBudgetExhaustsTerminalizes(t *testing.T) {
	uid := "11111111-1111-1111-1111-111111111111"
	run := newTestRun(uid, "10000000-0000-0000-0000-000000000001")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).Build()
	claims := &countingClaims{fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepRunning, Fence: 7, Holder: "ksquad-operator", RunID: uid,
		LeaseExpiresAt: leaseAgo(time.Minute), ItemState: "in_progress"},
		retryOK: true, retryNewFence: 8, failOK: true}}
	store := &fakeMachineStore{step: reconcile.StepRunning, fence: 7, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: &fakeMachineEffects{}})
	d.Sandbox = &fakeReleaser{}
	d.Settle = &fakeFollowSettle{dispatched: true, settled: true, outcome: coord.SettleOutcomeFollowError}

	// Passes 1..budget: within the infra budget — one RetryEnter per pass,
	// each requeued on backoff.
	for i := 1; i <= defaultInfraRetries; i++ {
		rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
		if err != nil {
			t.Fatalf("lap %d: %v", i, err)
		}
		if rq == 0 {
			t.Fatalf("lap %d: expected a backoff requeue", i)
		}
		if len(claims.retryCalls) != i || claims.failCall {
			t.Fatalf("lap %d: retry enters = %d (want %d), fail = %v", i, len(claims.retryCalls), i, claims.failCall)
		}
	}
	// Next pass: the ledger (budget accepted enters) has reached the infra
	// budget — terminalize, never another lap.
	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("budget pass: %v", err)
	}
	if len(claims.retryCalls) != defaultInfraRetries {
		t.Fatalf("retry enters = %d, want %d (defaultInfraRetries)", len(claims.retryCalls), defaultInfraRetries)
	}
	if !claims.failCall {
		t.Fatal("budget exhausted but the terminal FailEnter was not taken")
	}
}
