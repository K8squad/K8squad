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

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	api "github.com/K8squad/K8squad/api/v1alpha1"
	wire "github.com/K8squad/K8squad/pkg/a2a"
)

// modelRoleRun is the shared fixture for the Model-Per-Role dispatch tests
// (ISI-4430 S4): one Run naming one Agent. Callers vary the Agent/Role/default
// world around it.
func modelRoleRun(uid, agentName string) *api.Run {
	return &api.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-mpr", Namespace: "team-a", UID: types.UID(uid)},
		Spec: api.RunSpec{
			TeamRef:     api.ObjectRef{Name: "team-a"},
			ProjectRef:  api.ObjectRef{Name: "proj-1"},
			WorkItemRef: "77777777-8888-9999-aaaa-bbbbbbbbbbbb",
			Agents:      []api.ObjectRef{{Name: agentName}},
		},
	}
}

// TestDispatchRoleTierModelReachesShimAndRecordsProvenance is the S4 role-tier
// acceptance: an Agent with NO spec.model but a Role that sets one dispatches on
// the ROLE's model — it reaches the shim env (KSQUAD_MODEL), and Run provenance
// records tier=role.
func TestDispatchRoleTierModelReachesShimAndRecordsProvenance(t *testing.T) {
	const runUID = "cafe0000-1111-4444-8888-aaaaaaaaaaaa"
	run := modelRoleRun(runUID, "role-coder")
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "role-coder", Namespace: "team-a"},
		Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "senior"}}, // no Model → falls to role tier
	}
	role := &api.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "senior", Namespace: "team-a"},
		Spec:       api.RoleSpec{Model: "qwen3:role-model"},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, agent, role, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:     OperatorDispatchConfig{Client: cl, RuntimeType: "codex"},
		shimBin: "shim",
		source:  fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	// The effective (role-tier) model reaches the shim env.
	cmd, err := d.shimCommand(context.Background(), wire.Task{A2ATaskID: runUID})
	if err != nil {
		t.Fatalf("shimCommand: %v", err)
	}
	if got := envMap(t, cmd.Env)["KSQUAD_MODEL"]; got != "qwen3:role-model" {
		t.Errorf("KSQUAD_MODEL=%q, want the Role's model to reach the shim", got)
	}

	// buildTask stamps the winning tier into Run provenance AND onto the submit
	// payload so the shim can surface ksquad.model.tier on the run.start span
	// (ISI-4430 S5).
	tk, err := d.buildTask(context.Background(), runUID, runUID)
	if err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	if tk.ModelTier != "role" {
		t.Errorf("task.ModelTier = %q, want %q", tk.ModelTier, "role")
	}
	segs := modelSegmentsOf(t, cl, run)
	if len(segs) != 1 {
		t.Fatalf("got %d model segments, want 1", len(segs))
	}
	if segs[0].Tier != "role" || segs[0].Model != "qwen3:role-model" {
		t.Errorf("provenance segment = {tier:%q model:%q}, want {role qwen3:role-model}", segs[0].Tier, segs[0].Model)
	}

	// Idempotent: a re-drive (C1) must not append a duplicate open segment.
	if _, err := d.buildTask(context.Background(), runUID, runUID); err != nil {
		t.Fatalf("buildTask re-drive: %v", err)
	}
	if segs := modelSegmentsOf(t, cl, run); len(segs) != 1 {
		t.Fatalf("re-drive appended a duplicate segment: got %d, want 1", len(segs))
	}
}

// TestDispatchDefaultTierModelAndProvenance is the S4 default-tier acceptance:
// an Agent AND its Role both empty resolve to the system-default ModelConfig
// singleton; provenance records tier=default.
func TestDispatchDefaultTierModelAndProvenance(t *testing.T) {
	const runUID = "cafe0000-2222-4444-8888-bbbbbbbbbbbb"
	run := modelRoleRun(runUID, "bare-coder")
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "bare-coder", Namespace: "team-a"},
		Spec:       api.AgentSpec{}, // no Model, no RoleRef → default tier
	}
	def := &api.ModelConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "default", Namespace: "k8squad-system"},
		Spec:       api.ModelConfigSpec{Model: "claude-default"},
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, agent, def, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	if _, err := d.buildTask(context.Background(), runUID, runUID); err != nil {
		t.Fatalf("buildTask: %v", err)
	}
	segs := modelSegmentsOf(t, cl, run)
	if len(segs) != 1 {
		t.Fatalf("got %d model segments, want 1", len(segs))
	}
	if segs[0].Tier != "default" || segs[0].Model != "claude-default" {
		t.Errorf("provenance segment = {tier:%q model:%q}, want {default claude-default}", segs[0].Tier, segs[0].Model)
	}
}

// TestDispatchFailsClosedWhenNoTierSuppliesModel is the S4 D3 fail-closed
// acceptance: an Agent with no model, no role model, and NO system-default
// ModelConfig must ABORT the dispatch (ErrNoModel) rather than ship an empty
// model to a paid provider default.
func TestDispatchFailsClosedWhenNoTierSuppliesModel(t *testing.T) {
	const runUID = "cafe0000-3333-4444-8888-cccccccccccc"
	run := modelRoleRun(runUID, "no-model")
	agent := &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "no-model", Namespace: "team-a"},
		Spec:       api.AgentSpec{}, // nothing anywhere, no default CR seeded
	}
	cl := fake.NewClientBuilder().WithScheme(dispatchScheme(t)).
		WithObjects(run, agent, dispatchTeamObj()).
		WithStatusSubresource(&api.Run{}).Build()
	d := &operatorDispatch{
		cfg:    OperatorDispatchConfig{Client: cl},
		source: fakeDispatchSource{title: "t", body: "b", fence: "1"},
	}

	if _, err := d.buildTask(context.Background(), runUID, runUID); err == nil {
		t.Fatal("buildTask must fail closed when no tier supplies a model (ErrNoModel)")
	}
}

func modelSegmentsOf(t *testing.T, cl client.Client, run *api.Run) []api.ModelSegment {
	t.Helper()
	var got api.Run
	if err := cl.Get(context.Background(), client.ObjectKeyFromObject(run), &got); err != nil {
		t.Fatalf("get run: %v", err)
	}
	return got.Status.ModelSegments
}
