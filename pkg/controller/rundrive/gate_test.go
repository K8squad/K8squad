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

	corev1 "k8s.io/api/core/v1"
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
