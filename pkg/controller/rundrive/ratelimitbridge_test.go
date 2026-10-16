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

	sqlmock "github.com/DATA-DOG/go-sqlmock"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
	"github.com/K8squad/K8squad/pkg/modelendpoint"
)

// fallbackFixture is the ISI-6043 canary shape (ISI-6054's acceptance world):
// a glm-4.6 primary over a zai endpoint whose credential rides a separate
// credentialSecretRef, and a deepseek-chat fallback with its OWN endpoint
// Secret (URL + apiToken). The cross-provider credential fix means the
// fallback must resolve with the deepseek token, never the zai one.
func fallbackFixture(t *testing.T, run *api.Run) (client.Client, *operatorDispatch) {
	t.Helper()
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "glm-coder", Namespace: "team-a"},
		Spec: api.AgentSpec{
			Model:               "glm-4.6",
			ModelEndpointRef:    &api.SecretRef{Name: "zai-endpoint"},
			CredentialSecretRef: api.SecretRef{Name: "zai-coding-credentials"},
			FallbackModel: &api.FallbackModel{
				Model:            "deepseek-chat",
				ModelEndpointRef: &api.SecretRef{Name: "deepseek-endpoint"},
			},
		},
	}
	glmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zai-endpoint", Namespace: "team-a"},
		Data:       map[string][]byte{"endpointURL": []byte("https://api.zai.com/v1")},
	}
	dsSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "deepseek-endpoint", Namespace: "team-a"},
		Data: map[string][]byte{
			"endpointURL": []byte("https://api.deepseek.com/v1"),
			"apiToken":    []byte("sk-deepseek"),
		},
	}
	zaiCred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zai-coding-credentials", Namespace: "team-a"},
		Data:       map[string][]byte{"apiToken": []byte("sk-zai")},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, agent, glmSecret, dsSecret, zaiCred, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}
	return cl, d
}

// TestSinkForStampsFallbackSwitchMarker is the 5.10/5.11 acceptance: a
// rate_limited event on the per-Run sink (the seam BOTH transport legs share)
// drives the bridge to a SwitchModel verdict, and the executor closes the
// Run's open provenance portion with reason rate_limited — the marker buildTask
// routes the next lap on. A repeat signal is an idempotent no-op.
func TestSinkForStampsFallbackSwitchMarker(t *testing.T) {
	const runUID = "aaaabbbb-cccc-dddd-eeee-ffff00001111"
	started := metav1.Now()
	run := modelRoleRun(runUID, "glm-coder")
	run.Status.ModelSegments = []api.ModelSegment{
		{Model: "glm-4.6", Tier: "agent", StartedAt: &started},
	}
	cl, d := fallbackFixture(t, run)

	sink := d.sinkFor(runUID)
	ev := wire.Event{
		Type:      wire.EventRateLimited,
		A2ATaskID: runUID,
		Payload:   wire.RateLimitedPayload{Model: "glm-4.6"},
	}
	if err := sink.Event(context.Background(), ev); err != nil {
		t.Fatalf("sink rejected rate-limited event: %v", err)
	}

	var got api.Run
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: run.Name}, &got); err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if len(got.Status.ModelSegments) != 1 {
		t.Fatalf("modelSegments = %+v, want the single glm portion", got.Status.ModelSegments)
	}
	seg := got.Status.ModelSegments[0]
	if seg.Model != "glm-4.6" || seg.EndedAt == nil || seg.Reason != modelendpoint.ReasonRateLimited {
		t.Fatalf("glm portion not closed by the switch: %+v", seg)
	}

	// Idempotency: a repeat signal (no open portion anymore) neither errors
	// nor rewrites the segment log.
	if err := sink.Event(context.Background(), ev); err != nil {
		t.Fatalf("repeat rate-limited signal must be a no-op: %v", err)
	}
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: run.Name}, &got); err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if len(got.Status.ModelSegments) != 1 {
		t.Fatalf("repeat signal rewrote the segment log: %+v", got.Status.ModelSegments)
	}
}

// TestBuildTaskRoutesSwitchedRunToFallback is the mid-Run fallback acceptance:
// a Run whose provenance carries the rate_limited switch marker dispatches on
// the FALLBACK endpoint — deepseek URL, deepseek model, and the fallback's OWN
// token (the cross-provider credential fix) — and stamps the new open
// portion. The same Run keeps its identity and checkout: only the route moved.
func TestBuildTaskRoutesSwitchedRunToFallback(t *testing.T) {
	const runUID = "aaaabbbb-cccc-dddd-eeee-ffff00002222"
	started := metav1.Now()
	ended := metav1.Now()
	run := modelRoleRun(runUID, "glm-coder")
	run.Status.ModelSegments = []api.ModelSegment{
		{Model: "glm-4.6", Tier: "agent", StartedAt: &started, EndedAt: &ended, Reason: modelendpoint.ReasonRateLimited},
	}
	cl, d := fallbackFixture(t, run)

	task, err := d.buildTask(context.Background(), runUID, runUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.ModelRoute.Endpoint != "https://api.deepseek.com/v1" {
		t.Errorf("ModelRoute.Endpoint = %q, want the fallback endpoint", task.ModelRoute.Endpoint)
	}
	if task.ModelRoute.Model != "deepseek-chat" {
		t.Errorf("ModelRoute.Model = %q, want the fallback model", task.ModelRoute.Model)
	}
	if task.ModelRoute.Token != "sk-deepseek" {
		t.Errorf("ModelRoute.Token = %q, want the fallback endpoint's OWN token, never the primary credential", task.ModelRoute.Token)
	}

	var got api.Run
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: run.Name}, &got); err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if len(got.Status.ModelSegments) != 2 {
		t.Fatalf("modelSegments = %+v, want the closed glm portion plus the open deepseek portion", got.Status.ModelSegments)
	}
	open := got.Status.ModelSegments[1]
	if open.Model != "deepseek-chat" || open.EndedAt != nil || open.Tier != "agent" || open.SecretName != "deepseek-endpoint" {
		t.Fatalf("fallback portion not opened with provenance: %+v", open)
	}
}

// TestBuildTaskUnswitchedRunStaysOnPrimary: without the switch marker the
// dispatch is byte-identical to the pre-5.11 behavior — the primary endpoint.
func TestBuildTaskUnswitchedRunStaysOnPrimary(t *testing.T) {
	const runUID = "aaaabbbb-cccc-dddd-eeee-ffff00003333"
	started := metav1.Now()
	run := modelRoleRun(runUID, "glm-coder")
	run.Status.ModelSegments = []api.ModelSegment{
		{Model: "glm-4.6", Tier: "agent", StartedAt: &started}, // open, no rate_limited marker
	}
	_, d := fallbackFixture(t, run)

	task, err := d.buildTask(context.Background(), runUID, runUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if task.ModelRoute.Endpoint != "https://api.zai.com/v1" {
		t.Errorf("ModelRoute.Endpoint = %q, want the primary endpoint (no switch marker)", task.ModelRoute.Endpoint)
	}
	if task.ModelRoute.Model != "glm-4.6" {
		t.Errorf("ModelRoute.Model = %q, want the primary model", task.ModelRoute.Model)
	}
}

// TestSinkForPauseRunWithoutDurableStores: the PauseRun verdict (no fallback
// configured) on a hand-built dispatcher with no durable pause surfaces is a
// logged no-op, never a panic and never a canceled task — the ledger-only lane
// degrades honestly.
func TestSinkForPauseRunWithoutDurableStores(t *testing.T) {
	const runUID = "aaaabbbb-cccc-dddd-eeee-ffff00004444"
	run := modelRoleRun(runUID, "glm-coder")
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "glm-coder", Namespace: "team-a"},
		Spec: api.AgentSpec{
			Model:               "glm-4.6",
			ModelEndpointRef:    &api.SecretRef{Name: "zai-endpoint"},
			CredentialSecretRef: api.SecretRef{Name: "zai-coding-credentials"},
			// NO FallbackModel: the 5.11 verdict is PauseRun.
		},
	}
	glmSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zai-endpoint", Namespace: "team-a"},
		Data:       map[string][]byte{"endpointURL": []byte("https://api.zai.com/v1")},
	}
	zaiCred := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "zai-coding-credentials", Namespace: "team-a"},
		Data:       map[string][]byte{"apiToken": []byte("sk-zai")},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, agent, glmSecret, zaiCred, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{cfg: OperatorDispatchConfig{Client: cl}}

	sink := d.sinkFor(runUID)
	err := sink.Event(context.Background(), wire.Event{
		Type: wire.EventRateLimited, A2ATaskID: runUID,
		Payload: wire.RateLimitedPayload{Model: "glm-4.6", RetryAfter: "120"},
	})
	if err != nil {
		t.Fatalf("PauseRun on the ledger-only lane must be a no-op, got %v", err)
	}
	var got api.Run
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: "team-a", Name: run.Name}, &got); err != nil {
		t.Fatalf("reload run: %v", err)
	}
	if len(got.Status.ModelSegments) != 0 {
		t.Fatalf("PauseRun verdict must not stamp a switch marker: %+v", got.Status.ModelSegments)
	}
}

// TestPauseRateLimitedMovesLiveStepToPark: the guarded step move — a live claim
// (running) moves into paused(rate_limited) with audit + outbox; a terminal
// claim (succeeded) is never resurrected into a pause.
func TestPauseRateLimitedMovesLiveStepToPark(t *testing.T) {
	const item = "11111111-2222-3333-4444-555555555555"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	// Live claim: the guarded UPDATE lands, then the audit/outbox inserts.
	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WithArgs(item).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectQuery(`SELECT run_id::text FROM coord.a2a_dispatch`).
		WillReturnRows(sqlmock.NewRows([]string{"run_id"}).AddRow(""))
	mock.ExpectExec(`INSERT INTO coord.audit_log`).
		WithArgs(item, sqlmock.AnyArg(), OperatorPrincipal).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectExec(`INSERT INTO coord.outbox`).
		WithArgs(item, sqlmock.AnyArg()).
		WillReturnResult(sqlmock.NewResult(0, 1))
	mock.ExpectCommit()

	c := NewProdClaims(db, OperatorPrincipal)
	moved, err := c.PauseRateLimited(context.Background(), item)
	if err != nil {
		t.Fatalf("PauseRateLimited: %v", err)
	}
	if !moved {
		t.Fatal("live claim must move into paused(rate_limited)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// TestPauseRateLimitedLeavesTerminalClaim: a terminal (or pre-dispatch) claim
// matches no pausable step — the guarded UPDATE is a no-op and no audit/outbox
// rows are written.
func TestPauseRateLimitedLeavesTerminalClaim(t *testing.T) {
	const item = "11111111-2222-3333-4444-555555555555"
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock: %v", err)
	}
	defer func() { _ = db.Close() }()

	mock.ExpectBegin()
	mock.ExpectExec(`UPDATE coord.claim`).
		WithArgs(item).
		WillReturnResult(sqlmock.NewResult(0, 0))
	mock.ExpectRollback()

	c := NewProdClaims(db, OperatorPrincipal)
	moved, err := c.PauseRateLimited(context.Background(), item)
	if err != nil {
		t.Fatalf("PauseRateLimited: %v", err)
	}
	if moved {
		t.Fatal("terminal claim must not move into paused(rate_limited)")
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet sql expectations: %v", err)
	}
}

// Compile-time: modelRoleRun's UID stays a types.UID — the fixtures above rely
// on it.
var _ types.UID
