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

// credpause_status_test.go — the Story 7.4/7.6 (ISI-2898) status-projection
// tests: the credential pause family projects onto PhasePaused with a distinct
// legible Ready reason per hold, and — when the reconciler supplies the
// per-credential ledger snapshot — the Ready Message is the 7.6 operator string
// ("paused: <reason> (credential X, resumes ~T | on refresh)"), never a bare
// "failed: auth". Status stays downstream of the durable step (AC2): a stale
// PauseDetail on a non-paused step is ignored.
package run

import (
	"strings"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/pkg/reconcile"
)

func TestPauseDetailOperatorMessage(t *testing.T) {
	resumeAt := metav1.NewTime(time.Date(2026, 8, 24, 12, 5, 0, 0, time.UTC))

	// Timer-mode: names the credential and the resume horizon.
	rl := PauseDetail{CredentialID: "team-a/alice-oauth", Principal: "alice",
		Reason: "rate_limited", ResumeAt: &resumeAt}
	msg := rl.OperatorMessage()
	for _, want := range []string{"rate_limited", "team-a/alice-oauth", "resumes ~2026-08-24T12:05:00Z"} {
		if !strings.Contains(msg, want) {
			t.Errorf("timer-mode OperatorMessage %q missing %q", msg, want)
		}
	}

	// Refresh-mode: no timer, so the message says it clears on refresh.
	exp := PauseDetail{CredentialID: "team-a/alice-oauth", Reason: "credential_expired"}
	if msg := exp.OperatorMessage(); !strings.Contains(msg, "refresh") || strings.Contains(msg, "resumes ~") {
		t.Errorf("refresh-mode OperatorMessage %q must say 'on refresh' and carry no timer", msg)
	}

	// Falls back to Principal when the Secret id is unset (attribution still names a who).
	byPrincipal := PauseDetail{Principal: "svc-runner", Reason: "endpoint_unreachable"}
	if msg := byPrincipal.OperatorMessage(); !strings.Contains(msg, "svc-runner") {
		t.Errorf("OperatorMessage must fall back to Principal, got %q", msg)
	}
}

func TestProjectStatusWithPauseCredentialFamily(t *testing.T) {
	fixed := metav1.NewTime(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	cases := []struct {
		step       reconcile.Step
		wantReason string
	}{
		{reconcile.StepPausedRateLimited, reasonRateLimited},
		{reconcile.StepPausedCredentialExpired, reasonCredentialExpired},
		{reconcile.StepPausedCredentialRotated, reasonCredentialRotated},
		{reconcile.StepPausedEndpointUnreachable, reasonEndpointUnreach},
	}
	for _, tc := range cases {
		// Every credential pause step projects onto PhasePaused with its own reason.
		got := ProjectStatus(api.RunStatus{}, tc.step, 1, fixed)
		if got.Phase != api.RunPhasePaused {
			t.Errorf("step %q: Phase = %q, want Paused", tc.step, got.Phase)
		}
		cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
		if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != tc.wantReason {
			t.Errorf("step %q: Ready = %v, want False/%s", tc.step, cond, tc.wantReason)
		}
		if cond.Message == "" {
			t.Errorf("step %q: paused Ready message must never be empty", tc.step)
		}
	}
}

func TestProjectStatusWithPauseCarriesOperatorMessage(t *testing.T) {
	fixed := metav1.NewTime(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	resumeAt := metav1.NewTime(time.Date(2026, 8, 24, 12, 3, 0, 0, time.UTC))
	detail := &PauseDetail{CredentialID: "team-b/bob-key", Principal: "bob",
		Reason: "rate_limited", ResumeAt: &resumeAt}

	got := ProjectStatusWithPause(api.RunStatus{}, reconcile.StepPausedRateLimited, 2, fixed, detail)
	cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
	if cond == nil {
		t.Fatalf("Ready condition missing")
	}
	if cond.Message != detail.OperatorMessage() {
		t.Errorf("Ready.Message = %q, want the 7.6 operator string %q", cond.Message, detail.OperatorMessage())
	}
	if !strings.Contains(cond.Message, "team-b/bob-key") {
		t.Errorf("attributed message must name the credential, got %q", cond.Message)
	}
}

// TestProjectStatusWithPauseIgnoresDetailOnNonPausedStep proves status stays
// downstream of the durable step (AC2): a stale ledger snapshot handed alongside
// a Running step must NOT fabricate a pause the step does not command.
func TestProjectStatusWithPauseIgnoresDetailOnNonPausedStep(t *testing.T) {
	fixed := metav1.NewTime(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	detail := &PauseDetail{CredentialID: "team-c/eve", Reason: "rate_limited"}

	got := ProjectStatusWithPause(api.RunStatus{}, reconcile.StepRunning, 1, fixed, detail)
	if got.Phase != api.RunPhaseRunning {
		t.Errorf("Phase = %q, want Running (detail must not fabricate a pause)", got.Phase)
	}
	cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
	if cond == nil || cond.Reason != reasonReconciling {
		t.Errorf("Ready reason = %v, want Reconciling", cond)
	}
	if strings.Contains(cond.Message, "team-c/eve") {
		t.Errorf("non-paused step must not carry credential attribution, got %q", cond.Message)
	}
}

// TestProjectStatusWithPauseNilDetailFallsBack proves the generic per-step
// message is used when the reconciler had no ledger snapshot — the condition is
// legible even before the ledger read lands, and never empty.
func TestProjectStatusWithPauseNilDetailFallsBack(t *testing.T) {
	fixed := metav1.NewTime(time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC))
	for _, step := range []reconcile.Step{
		reconcile.StepPausedRateLimited, reconcile.StepPausedCredentialExpired,
		reconcile.StepPausedCredentialRotated, reconcile.StepPausedEndpointUnreachable,
	} {
		got := ProjectStatusWithPause(api.RunStatus{}, step, 1, fixed, nil)
		cond := meta.FindStatusCondition(got.Conditions, ConditionReady)
		if cond == nil || cond.Message == "" {
			t.Errorf("step %q: nil-detail pause must fall back to a non-empty generic message", step)
		}
	}
}
