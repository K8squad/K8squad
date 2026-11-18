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

	"k8s.io/apimachinery/pkg/types"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/coord"
)

// ISI-7548 regression: during API-stall windows the a2a settle marker can
// land between the top-of-pass settledOK read and the death-path commit, and
// the gone-sandbox path then terminal-fails a lap whose agent task already
// settled SUCCEEDED (live evidence 2026-11-17: one run per verification
// batch). Both death seams must defer to the success finalize instead.

// flipSettle reports an unsettled lap on the first read and a durably
// succeeded lap on every later read — the stall-window ordering where the
// settle commits mid-pass.
type flipSettle struct{ calls int }

func (f *flipSettle) RunFollowOutcome(context.Context, string) (bool, bool, string, error) {
	f.calls++
	if f.calls == 1 {
		return true, false, "", nil
	}
	return true, true, coord.SettleOutcomeSucceeded, nil
}

// The machine-drive bind effect hits the gone pod AFTER the follow settled
// succeeded: no FailEnter, no RetryEnter — the stale bind is cleared and the
// pass requeues so the follow gate finalizes success. On unfixed main this
// fixture terminal-fails (the budget is spent), which is the bug.
func TestBindGoneAfterSucceededSettleDefersToSuccess(t *testing.T) {
	d, claims, _, clearer := bindGoneFixture(t, func(run *api.Run, fc *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
		fc.laps = 1 // budget spent: unfixed main FailEnters here
	})
	d.Settle = &fakeFollowSettle{dispatched: true, settled: true, outcome: coord.SettleOutcomeSucceeded}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq <= 0 {
		t.Fatalf("requeue = %v, want > 0 (defer to success finalize)", rq)
	}
	if claims.failCall {
		t.Fatal("FailEnter must not fire for a lap that durably settled succeeded")
	}
	if len(claims.retryCalls) != 0 {
		t.Fatalf("RetryEnter calls = %v, want none — the lap already finished", claims.retryCalls)
	}
	if len(clearer.cleared) != 1 {
		t.Fatalf("ClearSandboxBind calls = %d, want 1 (stale bind cleared)", len(clearer.cleared))
	}
}

// The 4310 gone-pod probe fires on a pass whose FIRST settle read predated
// the settle commit (the flip): the retryOrFail last-chance guard re-reads
// the marker, sees the succeeded settle, and defers instead of spending the
// spent budget on FailEnter.
func TestGonePodDeathPathDefersWhenSettleLandsMidPass(t *testing.T) {
	d, claims, _, clearer := bindGoneFixture(t, func(run *api.Run, fc *fakeClaims) {
		run.Spec.RetryPolicy = &api.RetryPolicy{MaxRetries: ptrInt32(1)}
		fc.laps = 1 // budget spent: unfixed main FailEnters here
		run.Status.SandboxRef = &api.ObjectRef{Namespace: "default", Name: "sandbox-gone-1"}
	})
	d.Settle = &flipSettle{}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if rq <= 0 {
		t.Fatalf("requeue = %v, want > 0 (defer to success finalize)", rq)
	}
	if claims.failCall {
		t.Fatal("FailEnter must not fire when the settle landed before the death commit")
	}
	if len(claims.retryCalls) != 0 {
		t.Fatalf("RetryEnter calls = %v, want none — the lap already finished", claims.retryCalls)
	}
	if len(clearer.cleared) != 1 {
		t.Fatalf("ClearSandboxBind calls = %d, want 1 (stale bind cleared)", len(clearer.cleared))
	}
}
