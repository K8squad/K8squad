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
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/controller/credential"
	"github.com/K8squad/K8squad/pkg/reconcile"
	"github.com/K8squad/K8squad/pkg/toolcred"
)

// gate_test.go — the WS-9 S5 go-live gate's driver-level wiring (ISI-5849):
// pre-dispatch admission over the Run's recorded CapabilityManifest against the
// project's materialized credential Secrets. These drive the real Driver.Reconcile
// seam with the package's fake Claims/Runner/Store doubles; the pure any-of +
// fail-closed oracle itself is exhaustively covered in pkg/toolcred/gate_test.go.

const gateUID = "22222222-2222-2222-2222-222222222222"

// gateRun is a Run whose recorded manifest grants the given skills — the audit
// truth the pre-dispatch gate reads without re-listing Skills.
func gateRun(workItem string, skills ...api.GrantedSkill) *api.Run {
	r := newTestRun(gateUID, workItem)
	r.Status.CapabilityManifest = &api.CapabilityManifest{Skills: skills}
	return r
}

func gatedSkill(name, credType string) api.GrantedSkill {
	return api.GrantedSkill{
		Namespace: "default", Name: name,
		SourceType: api.SkillSourceInline, CredentialType: credType,
	}
}

// managedCredSecret is a vault-materialized credential Secret: the project-scoped
// deterministic name + the managed-credential label the gate requires.
func managedCredSecret(ns, project, credType string) *corev1.Secret {
	name, _ := toolcred.MaterializedSecretName(project, credType)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: ns, Name: name,
			Labels: map[string]string{credential.LabelManagedCredential: "true"},
		},
	}
}

func gateClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := newScheme(t)
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add corev1 scheme: %v", err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).
		WithIndex(&api.Run{}, workItemField,
			func(obj client.Object) []string { return []string{obj.(*api.Run).Spec.WorkItemRef} }).
		Build()
}

// claimingState is a synthetic claim at the pre-dispatch edge over a todo lane,
// so the §6.2 acquire branch admits (acquireOK) and the gate then runs.
func claimingState() *fakeClaims {
	return &fakeClaims{
		found:        true,
		state:        ClaimState{Step: reconcile.StepClaimingSandbox, Fence: 1, ItemState: "todo"},
		acquireOK:    true,
		acquireFence: 1,
		gateHoldOK:   true,
	}
}

// pausedHeldState is a synthetic claim parked on the gate hold with custody
// retained (RunID == the driven Run + a live lease): the resume re-entry shape.
func pausedHeldState() *fakeClaims {
	future := time.Now().Add(time.Hour)
	return &fakeClaims{
		found:        true,
		state:        ClaimState{Step: reconcile.StepPausedCredentialRequired, Fence: 1, RunID: gateUID, ItemState: "in_progress", LeaseExpiresAt: &future},
		renewOK:      true,
		gateResumeOK: true,
		gateHoldOK:   true,
	}
}

// TestGateWithholdsUnsatisfiedGatedSkill: a Run granting a [sec]-gated skill
// whose declared type is not materialized is held on paused(credential_required)
// pre-dispatch — GateHold carries the missing type, nothing dispatches.
func TestGateWithholdsUnsatisfiedGatedSkill(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000001", gatedSkill("image-gen", "gemini"))
	cl := gateClient(t, run) // no credential Secret
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !claims.gateHoldCall {
		t.Fatalf("GateHold not called — unsatisfied gated skill must be withheld pre-dispatch")
	}
	if len(claims.gateHoldMissing) != 1 || claims.gateHoldMissing[0] != "gemini" {
		t.Fatalf("GateHold missing types = %v, want [gemini] (named so the operator knows what to add)", claims.gateHoldMissing)
	}
	if len(eff.dispatches) != 0 {
		t.Fatalf("dispatched %v while withheld — the gate must stop dispatch", eff.dispatches)
	}
	if rq == 0 {
		t.Fatalf("requeueAfter = 0, want a resume poll while held")
	}
}

// TestGateAdmitsSatisfiedGatedSkill: the declared type's project-scoped Secret
// exists, so the gate admits and the machine dispatches normally.
func TestGateAdmitsSatisfiedGatedSkill(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000002", gatedSkill("image-gen", "gemini"))
	cl := gateClient(t, run, managedCredSecret("default", "p", "gemini"))
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if claims.gateHoldCall {
		t.Fatalf("GateHold called though the credential type is materialized — satisfied skills run normally")
	}
	if len(eff.dispatches) == 0 {
		t.Fatalf("satisfied Run did not dispatch (dispatches=%v)", eff.dispatches)
	}
}

// TestGateAdmitsAnyOfWhenOneProviderMaterialized: a multi-provider any-of skill
// ("gemini,fal.ai") admits once ONE declared type is materialized.
func TestGateAdmitsAnyOfWhenOneProviderMaterialized(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000003", gatedSkill("image-gen", "gemini,fal.ai"))
	cl := gateClient(t, run, managedCredSecret("default", "p", "fal.ai")) // only fal.ai
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if claims.gateHoldCall {
		t.Fatalf("GateHold called though one any-of provider (fal.ai) is materialized")
	}
	if len(eff.dispatches) == 0 {
		t.Fatalf("any-of satisfied Run did not dispatch (dispatches=%v)", eff.dispatches)
	}
}

// TestGateAdmitsUngatedRun: a Run with only ungated skills (no credential-type
// annotation) is never withheld.
func TestGateAdmitsUngatedRun(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000004", gatedSkill("plain", ""))
	cl := gateClient(t, run)
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if claims.gateHoldCall {
		t.Fatalf("GateHold called for an ungated Run — nothing to withhold")
	}
	if len(eff.dispatches) == 0 {
		t.Fatalf("ungated Run did not dispatch (dispatches=%v)", eff.dispatches)
	}
}

// TestGateFailsClosedOnUnknownType: a declared type the registry does not map
// can never be materialized, so the Run is withheld (fail closed) — a mis-typed
// annotation denies the capability, never admits it.
func TestGateFailsClosedOnUnknownType(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000005", gatedSkill("image-gen", "mystery-provider"))
	cl := gateClient(t, run)
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !claims.gateHoldCall {
		t.Fatalf("GateHold not called for an unknown declared type — must fail closed")
	}
	if len(claims.gateHoldMissing) != 1 || claims.gateHoldMissing[0] != "mystery-provider" {
		t.Fatalf("GateHold missing = %v, want [mystery-provider]", claims.gateHoldMissing)
	}
	if len(eff.dispatches) != 0 {
		t.Fatalf("dispatched %v for an unknown type — fail closed means no dispatch", eff.dispatches)
	}
}

// TestGateIgnoresUnlabeledSecret: a Secret colliding with the deterministic name
// but lacking the managed-credential label does NOT satisfy the gate (defence in
// depth — only the vault's materialized Secret counts).
func TestGateIgnoresUnlabeledSecret(t *testing.T) {
	name, _ := toolcred.MaterializedSecretName("p", "gemini")
	rogue := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: "default", Name: name}} // no label
	run := gateRun("10000000-0000-0000-0000-000000000006", gatedSkill("image-gen", "gemini"))
	cl := gateClient(t, run, rogue)
	claims := claimingState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !claims.gateHoldCall {
		t.Fatalf("GateHold not called — an unlabeled Secret must not satisfy the gate")
	}
}

// TestGateResumesWhenSecretMaterializes: a Run parked on paused(credential_required)
// whose declared type is now materialized is resumed (GateResume) back onto the
// happy path and dispatches — the Secret-watch resume path end to end.
func TestGateResumesWhenSecretMaterializes(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000007", gatedSkill("image-gen", "gemini"))
	cl := gateClient(t, run, managedCredSecret("default", "p", "gemini"))
	claims := pausedHeldState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if !claims.gateResumeCall {
		t.Fatalf("GateResume not called — a now-satisfied paused Run must resume")
	}
	if claims.gateHoldCall {
		t.Fatalf("GateHold called on a satisfied resume — should admit, not re-hold")
	}
	if len(eff.dispatches) == 0 {
		t.Fatalf("resumed Run did not dispatch (dispatches=%v)", eff.dispatches)
	}
}

// TestGateStaysHeldWhenStillUnsatisfied: a paused Run re-evaluated while its
// credential is still missing re-holds (idempotent) and never resumes/dispatches.
func TestGateStaysHeldWhenStillUnsatisfied(t *testing.T) {
	run := gateRun("10000000-0000-0000-0000-000000000008", gatedSkill("image-gen", "gemini"))
	cl := gateClient(t, run) // still no Secret
	claims := pausedHeldState()
	eff := &fakeMachineEffects{}
	store := &fakeMachineStore{step: reconcile.StepClaimingSandbox, fence: 1, advanceOK: true}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("drive: %v", err)
	}
	if claims.gateResumeCall {
		t.Fatalf("GateResume called while still unsatisfied — must stay held")
	}
	if !claims.gateHoldCall {
		t.Fatalf("GateHold not called on a still-unsatisfied paused Run")
	}
	if len(eff.dispatches) != 0 {
		t.Fatalf("dispatched %v while still withheld", eff.dispatches)
	}
}

// TestRunsForManagedSecret: the Secret-watch mapper enqueues every Run in the
// Secret's namespace (and only that namespace) so each one's pre-dispatch gate
// re-evaluates on a credential landing.
func TestRunsForManagedSecret(t *testing.T) {
	inNS := gateRun("10000000-0000-0000-0000-000000000009", gatedSkill("image-gen", "gemini"))
	other := newTestRun("33333333-3333-3333-3333-333333333333", "10000000-0000-0000-0000-00000000000a")
	other.Name = "run-2"
	other.Namespace = "other-ns"
	cl := gateClient(t, inNS, other)
	d := newDriver(cl, claimingState(), &fakePauses{}, &fakeRunner{})

	sec := managedCredSecret("default", "p", "gemini")
	reqs := d.runsForManagedSecret(context.Background(), sec)
	if len(reqs) != 1 || reqs[0].Name != "run-1" || reqs[0].Namespace != "default" {
		t.Fatalf("mapper requests = %v, want exactly the default-ns Run (run-1)", reqs)
	}
}

// ---------------------------------------------------------------------------
// ISI-7499 C4: per-agent non-preemptive dispatch queue (AgentGate)
// ---------------------------------------------------------------------------

// agentGateTestBase is the deterministic clock base for the queue tests
// (the 2026-11-16T16:20Z party-batch mint second).
var agentGateTestBase = time.Date(2026, 11, 16, 16, 20, 40, 0, time.UTC)

// TestAgentGateSingleFlightNonPreemptive: one in-flight run per (team,
// agent); a contender is refused and names the holder; a later board-class
// arrival NEVER evicts the incumbent (non-preemption, D3); the holder's
// re-acquire is an idempotent re-grant (sticky admission, D4); after the
// terminal release the BOARD waiter is admitted ahead of the earlier-queued
// discussion waiter (class beats FIFO, D1).
func TestAgentGateSingleFlightNonPreemptive(t *testing.T) {
	g := NewAgentGate()
	k1 := DispatchPriorityKey{Class: DispatchClassBoard, Since: agentGateTestBase, RunID: "run-1"}
	k2 := DispatchPriorityKey{Class: DispatchClassDiscussion, Since: agentGateTestBase.Add(time.Second), RunID: "run-2"}
	k3 := DispatchPriorityKey{Class: DispatchClassBoard, Since: agentGateTestBase.Add(2 * time.Second), RunID: "run-3"}

	if _, ok := g.Acquire("t", "sam", "run-1", k1); !ok {
		t.Fatalf("the agent's first run must be admitted")
	}
	if h, ok := g.Acquire("t", "sam", "run-2", k2); ok || h != "run-1" {
		t.Fatalf("second run for the agent must be refused naming the holder; got holder=%q ok=%v", h, ok)
	}
	if h, ok := g.Acquire("t", "sam", "run-3", k3); ok || h != "run-1" {
		t.Fatalf("higher-priority arrival must queue, not preempt; got holder=%q ok=%v", h, ok)
	}
	if got := g.Holder("t", "sam"); got != "run-1" {
		t.Fatalf("holder demoted to %q — admission must be sticky until terminal", got)
	}
	// Sticky re-grant: the holder re-acquiring (re-drive / retry lap /
	// follow re-attach) is a no-op grant.
	if h, ok := g.Acquire("t", "sam", "run-1", k1); !ok || h != "run-1" {
		t.Fatalf("holder re-acquire must be an idempotent grant; got holder=%q ok=%v", h, ok)
	}
	// Other teams and other agents are independent axes.
	if _, ok := g.Acquire("t2", "sam", "run-9", DispatchPriorityKey{Since: agentGateTestBase, RunID: "run-9"}); !ok {
		t.Fatalf("same agent name in another team must not contend")
	}
	if _, ok := g.Acquire("t", "kai", "run-10", DispatchPriorityKey{Since: agentGateTestBase, RunID: "run-10"}); !ok {
		t.Fatalf("another agent in the same team must not contend")
	}

	// Terminal release: the freed slot goes to the best-queued run. run-2
	// (discussion, queued first) polls first and must be told the slot is
	// reserved for run-3 (board) — class outranks FIFO. run-3's poll grants.
	g.Release("run-1")
	if h, ok := g.Acquire("t", "sam", "run-2", k2); ok || h != "run-3" {
		t.Fatalf("discussion waiter must not jump the board waiter; got holder=%q ok=%v", h, ok)
	}
	if h, ok := g.Acquire("t", "sam", "run-3", k3); !ok || h != "run-3" {
		t.Fatalf("board waiter must be admitted after the terminal release; got holder=%q ok=%v", h, ok)
	}
}

// TestDispatchPriorityKeyOrder pins the D1 total order: class first, then
// numeric priority, then FIFO wait start, then runID.
func TestDispatchPriorityKeyOrder(t *testing.T) {
	base := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	boardLate := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 9, Since: base.Add(time.Hour), RunID: "zzz"}
	discEarly := DispatchPriorityKey{Class: DispatchClassDiscussion, Priority: 0, Since: base, RunID: "aaa"}
	if !boardLate.before(discEarly) {
		t.Fatalf("board class must outrank discussion regardless of priority and wait time")
	}
	hi := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 0, Since: base.Add(time.Minute), RunID: "b"}
	lo := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 1, Since: base, RunID: "a"}
	if !hi.before(lo) {
		t.Fatalf("within a class, smaller priority value must rank first (C2 seam)")
	}
	fifoA := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 0, Since: base, RunID: "b"}
	fifoB := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 0, Since: base.Add(time.Second), RunID: "a"}
	if !fifoA.before(fifoB) {
		t.Fatalf("within a class and priority, earlier wait start must rank first")
	}
	tieA := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 0, Since: base, RunID: "aaa"}
	tieB := DispatchPriorityKey{Class: DispatchClassBoard, Priority: 0, Since: base, RunID: "bbb"}
	if !tieA.before(tieB) || tieB.before(tieA) {
		t.Fatalf("exact ties must break deterministically on runID")
	}
}

// TestDispatchClassForRun: the class derives from the Run's
// api.LabelWorkItemSource label, failing closed toward board.
func TestDispatchClassForRun(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000009")
	if got := dispatchClassForRun(run); got != DispatchClassBoard {
		t.Fatalf("no source label = board default; got %d", got)
	}
	run.Labels = map[string]string{api.LabelWorkItemSource: "board"}
	if got := dispatchClassForRun(run); got != DispatchClassBoard {
		t.Fatalf("board label = board; got %d", got)
	}
	run.Labels = map[string]string{api.LabelWorkItemSource: workItemSourceDiscussion}
	if got := dispatchClassForRun(run); got != DispatchClassDiscussion {
		t.Fatalf("discussion label = discussion; got %d", got)
	}
}

// TestAgentGateWaiterTTLEvictsSilentWaiter: a waiter that stops re-polling
// past the TTL loses its queue place (the dead-front-of-queue wedge guard).
func TestAgentGateWaiterTTLEvictsSilentWaiter(t *testing.T) {
	g := NewAgentGate()
	clk := agentGateTestBase
	g.now = func() time.Time { return clk }
	if _, ok := g.Acquire("t", "sam", "run-1", DispatchPriorityKey{Since: clk, RunID: "run-1"}); !ok {
		t.Fatalf("holder must be admitted")
	}
	if _, ok := g.Acquire("t", "sam", "run-2", DispatchPriorityKey{Since: clk.Add(time.Second), RunID: "run-2"}); ok {
		t.Fatalf("waiter must be refused while the agent is busy")
	}
	// run-2 never re-polls; its entry ages past the TTL.
	clk = clk.Add(2 * agentWaiterTTL)
	g.Release("run-1")
	// A fresh arrival is admitted immediately: the silent waiter no longer
	// holds the front of the queue.
	if h, ok := g.Acquire("t", "sam", "run-3", DispatchPriorityKey{Since: clk, RunID: "run-3"}); !ok || h != "run-3" {
		t.Fatalf("stale waiter must have been evicted; got holder=%q ok=%v", h, ok)
	}
}

// TestAgentGateReclaimInflightNeverOnAMaybe: the sweep backstop evicts only
// PROVEN-dead holders; a live holder and a checker error both keep the slot.
func TestAgentGateReclaimInflightNeverOnAMaybe(t *testing.T) {
	g := NewAgentGate()
	if _, ok := g.Acquire("t", "sam", "run-1", DispatchPriorityKey{RunID: "run-1"}); !ok {
		t.Fatalf("holder must be admitted")
	}
	if ev := g.ReclaimInflight(func(string) (bool, error) { return true, nil }); len(ev) != 0 {
		t.Fatalf("live holder must keep its slot; evicted=%v", ev)
	}
	if ev := g.ReclaimInflight(func(string) (bool, error) { return false, errors.New("maybe") }); len(ev) != 0 {
		t.Fatalf("checker error must keep the slot (never reclaim on a maybe); evicted=%v", ev)
	}
	if ev := g.ReclaimInflight(func(string) (bool, error) { return false, nil }); len(ev) != 1 || ev[0].RunID != "run-1" {
		t.Fatalf("dead holder must be evicted; evicted=%v", ev)
	}
	if got := g.Holder("t", "sam"); got != "" {
		t.Fatalf("slot must be free after reclaim; got %q", got)
	}
}

// TestAgentBusyParksPreDispatch (driver wiring): at the pending edge a run
// whose agent already has a run in flight parks pod-less on the
// WaitingForAgentSlot condition — nothing dispatches, no error surfaces, the
// requeue rides the wait cadence.
func TestAgentBusyParksPreDispatch(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000011")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})
	d.AgentGate = NewAgentGate()
	if _, ok := d.AgentGate.Acquire("t", "sam", "other-run", DispatchPriorityKey{RunID: "other-run"}); !ok {
		t.Fatalf("fixture: incumbent run must hold the agent slot")
	}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("busy agent must not surface an error: %v", err)
	}
	if rq != agentSlotWaitDelay {
		t.Fatalf("requeue = %v, want agentSlotWaitDelay (%v)", rq, agentSlotWaitDelay)
	}
	if len(eff.dispatches) != 0 || eff.binds != 0 {
		t.Fatalf("parked run must not bind or dispatch (binds=%d dispatches=%v)", eff.binds, eff.dispatches)
	}
	var got api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &got); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAgentSlotWait)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Fatalf("WaitingForAgentSlot condition not stamped: %+v", got.Status.Conditions)
	}
	if cond.Reason != "AgentBusy" || !strings.Contains(cond.Message, "other-run") {
		t.Fatalf("condition must name the busy reason and the in-flight run: %+v", cond)
	}
}

// TestAgentSlotAdmitsAfterTerminalRelease (driver wiring): once the
// incumbent goes terminal the parked run acquires the slot, drives, and its
// stale wait condition clears — admission only on terminal exit, then the
// roster reads working, not queued.
func TestAgentSlotAdmitsAfterTerminalRelease(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000012")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepPending, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepPending, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})
	d.AgentGate = NewAgentGate()
	d.AgentGate.Acquire("t", "sam", "other-run", DispatchPriorityKey{RunID: "other-run"})

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("park lap: %v", err)
	}
	d.AgentGate.Release("other-run") // the incumbent went terminal
	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("admit lap: %v", err)
	}
	if len(eff.dispatches) == 0 {
		t.Fatalf("admitted run must dispatch (dispatches=%v)", eff.dispatches)
	}
	var got api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &got); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	if cond := meta.FindStatusCondition(got.Status.Conditions, ConditionAgentSlotWait); cond != nil {
		t.Fatalf("stale agent-wait condition must clear once dispatched: %+v", cond)
	}
}

// TestQueueWaiterNeverTerminalizes (the C1 regression, ISI-7499 C4/D5): a
// never-dispatched run parked in a dispatch queue whose lease lapsed (the
// exact death-path shape that terminal-failed the 16:20Z party waiters ≤1s
// after mint) must re-park on the wait cadence — no FailEnter, no retry lap,
// no sandbox teardown. Queue waits burn no retry budget for any class.
func TestQueueWaiterNeverTerminalizes(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000013")
	run.Status.Conditions = []metav1.Condition{{
		Type:               ConditionEndpointSlotWait,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: run.Generation,
		Reason:             "EndpointBusy",
		Message:            "Waiting for a slot on the BYO model endpoint.",
		LastTransitionTime: metav1.Now(),
	}}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	expired := agentGateTestBase.Add(-time.Minute)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepDispatching, Fence: 2, Holder: gateUID, RunID: gateUID,
		LeaseExpiresAt: &expired, ItemState: "in_progress"},
		laps: 5, failOK: true, acquireOK: true, acquireFence: 2}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{step: reconcile.StepDispatching, fence: 2, advanceOK: true}, effects: &fakeMachineEffects{}})
	d.Settle = &fakeFollowSettle{dispatched: false} // never reached an agent
	sandbox := &fakeReleaser{}
	d.Sandbox = sandbox

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("queued waiter must not surface an error: %v", err)
	}
	if rq != endpointSlotWaitDelay {
		t.Fatalf("requeue = %v, want the wait cadence (%v)", rq, endpointSlotWaitDelay)
	}
	if claims.failCall || len(claims.retryCalls) != 0 {
		t.Fatalf("queue wait must burn no budget: fail=%v retry=%v", claims.failCall, claims.retryCalls)
	}
	if len(sandbox.released) != 0 {
		t.Fatalf("queued waiter must not tear down a sandbox: %v", sandbox.released)
	}
}

// TestDispatchedRunKeepsDeathSemantics: the queue-wait guard is scoped to
// runs that never dispatched — a run WITH an a2a settlement keeps the
// pre-C4 death path (retryOrFail decides by budget as before).
func TestDispatchedRunKeepsDeathSemantics(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000014")
	run.Status.Conditions = []metav1.Condition{{
		Type:               ConditionAgentSlotWait,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: run.Generation,
		Reason:             "AgentBusy",
		Message:            "stale marker from a pre-dispatch park",
		LastTransitionTime: metav1.Now(),
	}}
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	expired := agentGateTestBase.Add(-time.Minute)
	claims := &fakeClaims{found: true, state: ClaimState{
		Step: reconcile.StepDispatching, Fence: 2, Holder: gateUID, RunID: gateUID,
		LeaseExpiresAt: &expired, ItemState: "in_progress"},
		laps: 5, failOK: true, acquireOK: true, acquireFence: 2}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: &fakeMachineStore{step: reconcile.StepDispatching, fence: 2, advanceOK: true}, effects: &fakeMachineEffects{}})
	d.Settle = &fakeFollowSettle{dispatched: true, settled: true, outcome: "follow_error"} // it DID reach an agent

	if _, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"}); err != nil {
		t.Fatalf("death drive: %v", err)
	}
	if !claims.failCall {
		t.Fatalf("dispatched run outside its budget must still terminal-fail (stale wait marker must not loop it)")
	}
}

// TestMeshSaturationParksQuietly (D5): a saturated-Supervisor refusal riding
// the effects error converts to the quiet Supervisor wait — no reconcile
// error, the QueuedAtSupervisor marker stamps, the wait cadence requeues.
func TestMeshSaturationParksQuietly(t *testing.T) {
	run := newTestRun(gateUID, "10000000-0000-0000-0000-000000000015")
	cl := fake.NewClientBuilder().WithScheme(newScheme(t)).WithObjects(run).
		WithStatusSubresource(&api.Run{}).Build()
	claims := &fakeClaims{found: true, state: ClaimState{Step: reconcile.StepDispatching, Fence: 1, ItemState: "todo"},
		acquireOK: true, acquireFence: 1}
	store := &fakeMachineStore{step: reconcile.StepDispatching, fence: 1, advanceOK: true}
	eff := &fakeMachineEffects{err: fmt.Errorf("coord.ProdEffects.Dispatch: submit: mesh gateway refused: %w; cause: 409 Conflict: no idle process", errMeshPoolSaturated)}
	d := newDriver(cl, claims, &fakePauses{}, &fakeRunner{store: store, effects: eff})
	d.Settle = &fakeFollowSettle{}

	rq, err := runOnce(t, d, types.NamespacedName{Namespace: "default", Name: "run-1"})
	if err != nil {
		t.Fatalf("mesh saturation must not surface an error: %v", err)
	}
	if rq != endpointSlotWaitDelay {
		t.Fatalf("requeue = %v, want the wait cadence (%v)", rq, endpointSlotWaitDelay)
	}
	if store.advances != 0 {
		t.Fatalf("the machine must not advance past a saturated pool (%d advances)", store.advances)
	}
	var got api.Run
	if err := cl.Get(context.Background(), types.NamespacedName{Namespace: "default", Name: "run-1"}, &got); err != nil {
		t.Fatalf("re-read run: %v", err)
	}
	cond := meta.FindStatusCondition(got.Status.Conditions, ConditionQueuedAtSupervisor)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "MeshPoolSaturated" {
		t.Fatalf("QueuedAtSupervisor condition not stamped: %+v", got.Status.Conditions)
	}
}
