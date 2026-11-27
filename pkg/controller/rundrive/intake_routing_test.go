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
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	api "github.com/K8squad/K8squad/api/v1alpha1"
)

// ---------------------------------------------------------------------------
// WS-D fixtures (ISI-7959, ISI-7946): a squad with a real org chart and a
// root manager Process.
// ---------------------------------------------------------------------------

// wsDefaultProcess is the root manager's BMAD-style pipeline:
// code (engineer) → review (reviewer) → test (tester).
var wsDefaultProcess = []api.ProcessStage{
	{Stage: "code", RoleRef: api.ObjectRef{Name: "engineer"}},
	{Stage: "review", RoleRef: api.ObjectRef{Name: "reviewer"}},
	{Stage: "test", RoleRef: api.ObjectRef{Name: "tester"}},
}

// wsSquad builds the WS-D world: Team "alpha" (uid teamUID, squad ns
// squadNS) with composition [reviewer, coder, tester, boss] — deliberately
// NOT led by the stage owner — whose Agents hold Roles reviewer/engineer/
// tester plus the root Role eng-manager (held by boss: admission requires
// an agent to hold the root Role, or the chart has no root at all), each
// non-root Role reporting to eng-manager, which carries the given Process
// (nil = a chart with a root Role but no Process). Because Agents[0] is
// reviewer while the code-stage owner is coder, any dispatch to coder
// proves stage-owner routing, never the blind Agents[0] default.
func wsSquad(teamUID, squadNS string, process []api.ProcessStage) []client.Object {
	team := &api.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "alpha", UID: types.UID(teamUID)},
		Spec: api.TeamSpec{
			NamespaceStrategy: "dedicated",
			// boss (the eng-manager Role's agent, the team's root) is LAST, so
			// Agents[0] stays reviewer — the not-Agents[0] proof below holds.
			Agents: []api.ObjectRef{{Name: "reviewer"}, {Name: "coder"}, {Name: "tester"}, {Name: "boss"}},
		},
	}
	team.Status.Namespace = squadNS
	mkAgent := func(name, roleName string) *api.Agent {
		return &api.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: squadNS},
			Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: roleName}},
		}
	}
	mkRole := func(name, reportsTo string) *api.Role {
		spec := api.RoleSpec{}
		if reportsTo != "" {
			spec.ReportsTo = &api.ObjectRef{Name: reportsTo}
		}
		return &api.Role{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: squadNS},
			Spec:       spec,
		}
	}
	objs := []client.Object{
		team,
		mkAgent("reviewer", "reviewer"),
		mkAgent("coder", "engineer"),
		mkAgent("tester", "tester"),
		mkAgent("boss", "eng-manager"),
		mkRole("engineer", "eng-manager"),
		mkRole("reviewer", "eng-manager"),
		mkRole("tester", "eng-manager"),
		&api.Role{
			ObjectMeta: metav1.ObjectMeta{Name: "eng-manager", Namespace: squadNS},
			Spec:       api.RoleSpec{Process: process},
		},
		&api.Project{ObjectMeta: metav1.ObjectMeta{Name: "proj", Namespace: squadNS}},
	}
	return objs
}

// WS-D core: a team-targeted item with NO requested agent is DRIVEN to the
// stage owner — ONE Run for the agent holding the Role that owns the root
// manager's FIRST process stage — never the blind Agents[0] (reviewer here)
// and never a fan-out.
func TestIntakeSweepDrivesToStageOwner(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := wsSquad(teamUID, squadNS, wsDefaultProcess)
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("stage-owner routing must mint exactly ONE Run, got %d; logs=%v", len(runs.Items), *logs)
	}
	run := runs.Items[0]
	if got := run.Spec.Agents; len(got) != 1 || got[0].Name != "coder" {
		t.Fatalf("item must be driven to the stage owner (engineer Role owns stage code; Agents[0] is reviewer): got %+v", got)
	}
	if run.Name != "intake-"+itemID {
		t.Fatalf("single-agent stage-owner Run keeps the legacy name: got %q", run.Name)
	}
	if run.Spec.WorkItemRef != itemID || run.Namespace != squadNS {
		t.Fatalf("run identity: workItemRef=%q ns=%q", run.Spec.WorkItemRef, run.Namespace)
	}
	found := false
	for _, l := range *logs {
		if strings.Contains(l, "driven to stage owner coder") && strings.Contains(l, "eng-manager") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected a stage-owner drive log naming the stage owner and root Role; logs=%v", *logs)
	}

	// Idempotency: the minted Run is live (phase unset suppresses intake) —
	// a re-sweep must not mint anything more.
	in.sweep(context.Background())
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("re-list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("re-sweep must not over-mint: got %d Runs", len(runs.Items))
	}
}

// The human stays the top of the chain (plan rev 3): an explicit
// requested_agent still on the team beats the stage owner — a pre-run pick
// is a decision, not blindness.
func TestIntakeSweepRequestedAgentBeatsStageOwner(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := wsSquad(teamUID, squadNS, wsDefaultProcess)
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj", RequestedAgent: "reviewer"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("requested agent must stay a single Run, got %d", len(runs.Items))
	}
	if got := runs.Items[0].Spec.Agents; len(got) != 1 || got[0].Name != "reviewer" {
		t.Fatalf("the human's requested agent must beat the stage owner: got %+v want [reviewer]", got)
	}
}

// A STALE requested agent (off-team) is driven to the stage owner — not the
// blind Agents[0] fallback (reviewer is Agents[0] here; coder is the stage
// owner) — and the log says both.
func TestIntakeSweepStaleRequestedAgentDrivesToStageOwner(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := wsSquad(teamUID, squadNS, wsDefaultProcess)
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj", RequestedAgent: "ghost"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("stale requested agent must stay a single Run, got %d; logs=%v", len(runs.Items), *logs)
	}
	if got := runs.Items[0].Spec.Agents; len(got) != 1 || got[0].Name != "coder" {
		t.Fatalf("stale requested agent must be driven to the stage owner, not Agents[0]: got %+v want [coder]", got)
	}
	staleLog, driveLog := false, false
	for _, l := range *logs {
		if strings.Contains(l, "no longer in team") && strings.Contains(l, "ghost") {
			staleLog = true
		}
		if strings.Contains(l, "driving to stage owner coder") {
			driveLog = true
		}
	}
	if !staleLog || !driveLog {
		t.Fatalf("expected both the stale-request and drive logs; logs=%v", *logs)
	}
}

// Config gates the stronger drive: a root Role with NO Process keeps the
// legacy selection bit-for-bit — the ISI-4927 fan-out mints one Run per
// composition agent, agent-scoped names and all.
func TestIntakeSweepProcesslessRootKeepsFanOut(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := wsSquad(teamUID, squadNS, nil) // chart with a root, no Process
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 4 {
		t.Fatalf("a process-less team must keep the legacy fan-out (one Run per composition agent): got %d Runs; logs=%v", len(runs.Items), *logs)
	}
	degraded := false
	for _, l := range *logs {
		if strings.Contains(l, "defines no Process") {
			degraded = true
		}
	}
	if !degraded {
		t.Fatalf("expected the honest-degraded routing log; logs=%v", *logs)
	}
}

// Determinism: several composition agents hold the stage-owner Role — the
// FIRST composition entry holding it is driven (the same order the legacy
// fan-out mints in), never a random pick.
func TestIntakeSweepStageOwnerDeterministicAmongSameRole(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	objs := wsSquad(teamUID, squadNS, wsDefaultProcess)
	// echo also holds the engineer Role, AFTER coder in the composition.
	objs = append(objs, &api.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "echo", Namespace: squadNS},
		Spec:       api.AgentSpec{RoleRef: api.ObjectRef{Name: "engineer"}},
	})
	objs[0].(*api.Team).Spec.Agents = append(objs[0].(*api.Team).Spec.Agents, api.ObjectRef{Name: "echo"})
	in, cl, _ := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 1 {
		t.Fatalf("still exactly ONE Run, got %d", len(runs.Items))
	}
	if got := runs.Items[0].Spec.Agents; len(got) != 1 || got[0].Name != "coder" {
		t.Fatalf("the FIRST composition agent holding the stage Role is driven: got %+v want [coder]", got)
	}
}

// Defensive degradation (admission owns the real guarantee): a root Process
// stage naming a Role no composition agent holds — the live world drifted
// past admission — resolves no stage owner, logs the reason, and the legacy
// fan-out dispatches instead of a wrong dispatch.
func TestIntakeSweepUnresolvedStageRoleFallsBackToLegacy(t *testing.T) {
	const (
		itemID  = "11111111-1111-1111-1111-111111111111"
		teamUID = "22222222-2222-2222-2222-222222222222"
		squadNS = "squad-alpha"
	)
	process := []api.ProcessStage{
		{Stage: "code", RoleRef: api.ObjectRef{Name: "ghost-role"}},
		{Stage: "review", RoleRef: api.ObjectRef{Name: "reviewer"}},
	}
	objs := wsSquad(teamUID, squadNS, process)
	in, cl, logs := newIntake(t, &fakeIntakeSource{items: []IntakeItem{
		{ID: itemID, TeamID: teamUID, ProjectID: "proj"},
	}}, objs...)

	in.sweep(context.Background())

	var runs api.RunList
	if err := cl.List(context.Background(), &runs); err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs.Items) != 4 {
		t.Fatalf("an unresolvable stage Role must fall back to the legacy fan-out (one Run per composition agent): got %d Runs; logs=%v", len(runs.Items), *logs)
	}
	found := false
	for _, l := range *logs {
		if strings.Contains(l, "ghost-role") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected the honest-degraded log naming the unheld Role; logs=%v", *logs)
	}
}
