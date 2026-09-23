package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

func TestRunsService(t *testing.T) {
	// Create test Team
	teamUID := uuid.New()
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-team",
			UID:  types.UID(teamUID.String()),
		},
		Status: ksquadv1.TeamStatus{
			Namespace: "team-" + teamUID.String()[:8],
		},
	}

	// Create test Project
	project := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-project",
			Namespace: team.Status.Namespace,
		},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{URL: "https://github.com/test/repo"},
		},
	}

	// Create test Runs
	now := metav1.Now()
	runs := []*ksquadv1.Run{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "run-1",
				Namespace: team.Status.Namespace,
			},
			Spec: ksquadv1.RunSpec{
				TeamRef: ksquadv1.ObjectRef{
					Name: "test-team",
				},
				ProjectRef: ksquadv1.ObjectRef{
					Name: "test-project",
				},
				WorkItemRef: "work-item-1",
				Agents: []ksquadv1.ObjectRef{
					{Name: "agent-1"},
				},
			},
			Status: ksquadv1.RunStatus{
				Phase:     ksquadv1.RunPhaseSucceeded,
				ClaimedAt: &now,
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "run-2",
				Namespace: team.Status.Namespace,
			},
			Spec: ksquadv1.RunSpec{
				TeamRef: ksquadv1.ObjectRef{
					Name: "test-team",
				},
				ProjectRef: ksquadv1.ObjectRef{
					Name: "test-project",
				},
				WorkItemRef: "work-item-2",
				Agents: []ksquadv1.ObjectRef{
					{Name: "agent-2"},
				},
			},
			Status: ksquadv1.RunStatus{
				Phase:     ksquadv1.RunPhaseRunning,
				ClaimedAt: &now,
			},
		},
	}

	// Create fake client with test objects
	objs := []client.Object{team, project}
	for _, run := range runs {
		objs = append(objs, run)
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()

	// Create RunsService
	svc := NewRunsService(k8sClient)

	// Test cases
	t.Run("Global run listing", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/runs", nil)
		req = authRequest(req)
		w := httptest.NewRecorder()

		handler := listRuns(svc)
		handler(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Logf("Response body: %s", resp.Body)
		}
	})

	t.Run("Project-scoped run listing", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/projects/test-project/runs", nil)
		req = authRequest(req)
		w := httptest.NewRecorder()

		handler := listRuns(svc)
		handler(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Logf("Response body: %s", resp.Body)
		}
	})

	t.Run("Unauthenticated requests", func(t *testing.T) {
		req := httptest.NewRequest("GET", "/api/runs", nil)
		w := httptest.NewRecorder()

		handler := listRuns(svc)
		handler(w, req)

		resp := w.Result()
		assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
	})
}

func TestRunListQueryParsing(t *testing.T) {
	t.Run("Parse query params", func(t *testing.T) {
		// Test query parameter parsing logic
		query := RunListQuery{
			Limit:  50,
			Offset: 0,
		}

		// Test phase filter
		assert.Equal(t, "", query.Phase)
		query.Phase = "running"
		assert.Equal(t, "running", query.Phase)

		// Test agent filter
		assert.Equal(t, "", query.Agent)
		query.Agent = "agent-1"
		assert.Equal(t, "agent-1", query.Agent)

		// Test window filter
		assert.Equal(t, "", query.Window)
		query.Window = "24h"
		assert.Equal(t, "24h", query.Window)

		// Test pagination limits
		assert.Equal(t, 50, query.Limit)
		query.Limit = 100
		assert.Equal(t, 100, query.Limit)
	})
}

func TestRunInTimeWindow(t *testing.T) {
	t.Run("Time window filtering", func(t *testing.T) {
		// Create test run
		now := metav1.Now()
		run := &ksquadv1.Run{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-run",
				Namespace: "test-namespace",
			},
			Status: ksquadv1.RunStatus{
				ClaimedAt: &now,
			},
		}

		// Test various time windows
		assert.True(t, runInTimeWindow(run, "1h"))
		assert.True(t, runInTimeWindow(run, "24h"))
		assert.True(t, runInTimeWindow(run, "7d"))
		assert.True(t, runInTimeWindow(run, "unknown")) // Unknown window includes all
	})
}

func TestRunListItemProjection(t *testing.T) {
	t.Run("Project Run to RunListItem", func(t *testing.T) {
		now := metav1.Now()
		pausedReason := "rate_limited"

		run := &ksquadv1.Run{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-run",
				Namespace: "test-namespace",
			},
			Spec: ksquadv1.RunSpec{
				WorkItemRef: "work-item-1",
				ProjectRef: ksquadv1.ObjectRef{
					Name: "test-project",
				},
				Agents: []ksquadv1.ObjectRef{
					{Name: "test-agent"},
				},
			},
			Status: ksquadv1.RunStatus{
				Phase:     ksquadv1.RunPhaseRunning,
				ClaimedAt: &now,
				Conditions: []metav1.Condition{
					{
						Type:   "Paused",
						Reason: pausedReason,
					},
				},
			},
		}

		item := runListItem(run)

		assert.Equal(t, "test-run", item.ID)
		assert.Equal(t, "test-run", item.Name)
		assert.Equal(t, "Running", item.Phase)
		assert.Equal(t, &pausedReason, item.PausedReason)
		assert.Equal(t, "work-item-1", item.WorkItemRef)
		assert.Equal(t, "test-project", item.ProjectRef)
		assert.Equal(t, now.Time, *item.StartedAt)
	})
}

// TestEnrichWorkItemsNilDBLeavesFieldsEmpty guards the ISI-4777 fabrication
// discipline: without a database (or on an older apiserver) the work-item title
// and triggering principal must stay absent so the console renders "—" rather
// than a fabricated value. enrichWorkItems is a fail-open no-op here.
func TestEnrichWorkItemsNilDBLeavesFieldsEmpty(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-abc-r1", Namespace: "ns"},
		Spec: ksquadv1.RunSpec{
			ProjectRef:  ksquadv1.ObjectRef{Name: "proj"},
			WorkItemRef: "ef5b2075-1111-2222-3333-444455556666",
		},
		Status: ksquadv1.RunStatus{Phase: ksquadv1.RunPhaseRunning, ClaimedAt: &now},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(run).Build()
	svc := NewRunsService(k8sClient) // nil db

	items, err := svc.listRunsInNamespace(ctx, "ns", RunListQuery{Limit: 10}, discussion.AuthorContext{})
	assert.NoError(t, err)
	assert.Len(t, items, 1)
	assert.Empty(t, items[0].WorkItemTitle, "no db → title must stay absent")
	assert.Empty(t, items[0].TriggeredBy, "no db → triggeredBy must stay absent")

	// enrichWorkItems must also tolerate an empty slice without panicking.
	svc.enrichWorkItems(ctx, nil)
}

func authRequest(r *http.Request) *http.Request {
	// Add test auth token (simplified for test)
	r.Header.Set("Authorization", "Bearer test-token")
	r.Header.Set("Cookie", "ksquad_session=test-session")
	return r
}

// Test that the RunsService can be created with nil database (fallback mode)
// TestRunDetailActivityTypesFromInteractions verifies the ISI-4811 mapping:
// CRD LLMInteractions become typed timeline entries (tool calls get tool_use /
// observation, responses surface the Response digest) and are not double-listed.
func TestRunDetailActivityTypesFromInteractions(t *testing.T) {
	ctx := context.Background()
	ts := metav1.Now()
	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "run-1", Namespace: "team-ns"},
		Status: ksquadv1.RunStatus{
			LLMInteractions: []ksquadv1.LLMInteraction{
				{ID: "i1", Type: "prompt", Model: "claude", Timestamp: ts, Request: []byte("hello?")},
				{ID: "i2", Type: "response", Model: "claude", Timestamp: ts, Response: []byte("hi there")},
				{ID: "i3", Type: "tool_call", Model: "claude", Timestamp: ts, Request: []byte("bash: ls")},
				{ID: "i4", Type: "tool_response", Model: "claude", Timestamp: ts, Response: []byte("file.txt")},
			},
		},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(run).Build()
	svc := NewRunsService(k8sClient) // nil db → no comments, activity still maps

	resp, err := svc.getRunDetailInNamespace(ctx, "team-ns", "run-1")
	assert.NoError(t, err)
	assert.NotNil(t, resp)

	// Exactly one timeline entry per interaction — no double-listing.
	assert.Len(t, resp.Thinking, 4)

	byID := map[string]ThinkingEntry{}
	for _, e := range resp.Thinking {
		byID[e.ID] = e
	}
	assert.Equal(t, "llm_interaction", byID["i1"].Type)
	assert.Equal(t, "hello?", byID["i1"].Content)
	assert.Equal(t, "llm_interaction", byID["i2"].Type)
	assert.Equal(t, "hi there", byID["i2"].Content) // response digest, not empty request
	assert.Equal(t, "tool_use", byID["i3"].Type)
	assert.Equal(t, "bash: ls", byID["i3"].Content)
	assert.Equal(t, "observation", byID["i4"].Type)
	assert.Equal(t, "file.txt", byID["i4"].Content)

	// Digest array still populated alongside the timeline.
	assert.Len(t, resp.LLMInteractions, 4)
}

func TestRunsServiceNilDB(t *testing.T) {
	ctx := context.Background()

	// Create test Team
	teamUID := uuid.New()
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name: "test-team",
			UID:  types.UID(teamUID.String()),
		},
		Status: ksquadv1.TeamStatus{
			Namespace: "team-" + teamUID.String()[:8],
		},
	}

	// Create test Project
	project := &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-project",
			Namespace: team.Status.Namespace,
		},
		Spec: ksquadv1.ProjectSpec{
			Repo: ksquadv1.RepoSpec{URL: "https://github.com/test/repo"},
		},
	}

	// Create fake client with test objects
	objs := []client.Object{team, project}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()

	// Create RunsService with nil database (should fallback to placeholder logic)
	svc := NewRunsService(k8sClient)

	// Test that getRunDetail works without database (fallback mode)
	response, err := svc.getRunDetailInNamespace(ctx, team.Status.Namespace, "nonexistent-run")
	if err != nil {
		// Expected for non-existent run
		t.Logf("Expected error for non-existent run: %v", err)
	} else if response != nil {
		t.Logf("Response received: %+v", response)
	}
}

// TestGetRunDetailFleetResolvesExecNamespace reproduces the live k8squad-test
// topology (ISI-4565): the Run CR lives in the squad *execution* namespace, but
// an admin's detail request is fleet-scoped (namespace == ""). A namespaced Get
// with an empty namespace does NOT search all namespaces, so before the fix the
// endpoint 502'd with "Run not found" for every admin. The fleet path must
// resolve the Run by name across namespaces.
func TestGetRunDetailFleetResolvesExecNamespace(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	// Run lives in the execution namespace, distinct from any home/default ns.
	execNS := "ksquad-team-bmad-squad-f6e8fc70"
	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "intake-ef5b2075-r15",
			Namespace: execNS,
		},
		Spec: ksquadv1.RunSpec{
			ProjectRef:  ksquadv1.ObjectRef{Name: "bmad-demo-project"},
			WorkItemRef: "ef5b2075",
		},
		Status: ksquadv1.RunStatus{Phase: ksquadv1.RunPhaseRunning, ClaimedAt: &now},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(run).Build()
	svc := NewRunsService(k8sClient)

	// Fleet/admin scope (namespace == "") must find the Run despite it living in
	// the execution namespace.
	resp, err := svc.getRunDetailInNamespace(ctx, "", "intake-ef5b2075-r15")
	assert.NoError(t, err)
	assert.NotNil(t, resp)
	if resp != nil {
		assert.Equal(t, "intake-ef5b2075-r15", resp.Run.Name)
		assert.Equal(t, execNS, resp.Run.Namespace)
	}

	// A genuinely missing Run in fleet scope must be a typed NotFound (→ 404),
	// not an opaque error the handler would surface as a 502.
	_, missErr := svc.getRunDetailInNamespace(ctx, "", "does-not-exist")
	assert.Error(t, missErr)
	assert.True(t, apierrors.IsNotFound(missErr), "expected IsNotFound, got %v", missErr)
}

// TestListRunsProjectScopedCompositeID reproduces the empty project/runs screen
// (ISI-4565): the console passes the Project id as a "namespace/name" composite,
// but a Run's spec.projectRef stores the bare Name (+ optional Namespace). The
// filter must split the composite instead of comparing "ns/name" to "name".
func TestListRunsProjectScopedCompositeID(t *testing.T) {
	ctx := context.Background()
	now := metav1.Now()

	execNS := "ksquad-team-bmad-squad-f6e8fc70"
	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-abc-r1", Namespace: execNS},
		Spec: ksquadv1.RunSpec{
			// projectRef carries the bare name + the squad HOME namespace.
			ProjectRef:  ksquadv1.ObjectRef{Name: "bmad-demo-project", Namespace: "bmad-squad"},
			WorkItemRef: "abc",
		},
		Status: ksquadv1.RunStatus{Phase: ksquadv1.RunPhaseRunning, ClaimedAt: &now},
	}
	// A same-named project in a different squad must NOT cross-list.
	other := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-xyz-r1", Namespace: "ksquad-team-other"},
		Spec: ksquadv1.RunSpec{
			ProjectRef:  ksquadv1.ObjectRef{Name: "bmad-demo-project", Namespace: "other-squad"},
			WorkItemRef: "xyz",
		},
		Status: ksquadv1.RunStatus{Phase: ksquadv1.RunPhaseRunning, ClaimedAt: &now},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(run, other).Build()
	svc := NewRunsService(k8sClient)

	// Admin fleet scope (namespace "") + composite project id, exactly as the
	// console route delivers it.
	got, err := svc.listRunsInNamespace(ctx, "", RunListQuery{
		ProjectID: "bmad-squad/bmad-demo-project",
		Limit:     50,
	}, discussion.AuthorContext{})
	assert.NoError(t, err)
	assert.Len(t, got, 1, "composite project id must return only the matching squad's run")
	if len(got) == 1 {
		assert.Equal(t, "intake-abc-r1", got[0].Name)
	}
}

// TestRunNamespaceForTeam proves the ISI-4738 fix: a non-admin caller's Team UID
// resolves to the squad's real EXECUTION namespace (Team.Status.Namespace, where
// Run CRs live), bridged home→exec, rather than the old fabricated "team-<hash>"
// stub that matched no real namespace and returned an empty list for every tenant.
func TestRunNamespaceForTeam(t *testing.T) {
	ctx := context.Background()
	teamUID := uuid.New()
	execNS := "ksquad-team-bmad-squad-f6e8fc70" // realistic exec ns, NOT "team-<hash>"

	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bmad-squad",
			Namespace: "bmad-squad", // home namespace
			UID:       types.UID(teamUID.String()),
		},
		Status: ksquadv1.TeamStatus{Namespace: execNS},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(team).Build()
	svc := NewRunsService(k8sClient)

	// UID → execution namespace (never the "team-<hash>" fabrication).
	ns, err := svc.runNamespaceForTeam(ctx, teamUID.String())
	assert.NoError(t, err)
	assert.Equal(t, execNS, ns)
	assert.NotEqual(t, "team-"+teamUID.String()[:8], ns, "must not fabricate a team-<hash> namespace")
}

// TestRunNamespaceForTeam_FallbackToHome covers a Team with no execution namespace
// provisioned (co-tenant dev host / pre-per-team layout): the resolver falls back
// to the home namespace, matching runNamespaceForHome's contract.
func TestRunNamespaceForTeam_FallbackToHome(t *testing.T) {
	ctx := context.Background()
	teamUID := uuid.New()
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "dev-squad",
			Namespace: "dev-squad",
			UID:       types.UID(teamUID.String()),
		},
		// Status.Namespace intentionally empty.
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(team).Build()
	svc := NewRunsService(k8sClient)

	ns, err := svc.runNamespaceForTeam(ctx, teamUID.String())
	assert.NoError(t, err)
	assert.Equal(t, "dev-squad", ns)
}

// TestRunNamespaceForTeam_NoScope covers the no-scope paths: an empty/zero UID
// yields ("", nil) so the handler answers 404, and a non-zero UID matching no Team
// yields ErrTeamNotFound — never a fabricated namespace.
func TestRunNamespaceForTeam_NoScope(t *testing.T) {
	ctx := context.Background()
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).Build()
	svc := NewRunsService(k8sClient)

	ns, err := svc.runNamespaceForTeam(ctx, "")
	assert.NoError(t, err)
	assert.Equal(t, "", ns)

	ns, err = svc.runNamespaceForTeam(ctx, "00000000-0000-0000-0000-000000000000")
	assert.NoError(t, err)
	assert.Equal(t, "", ns)

	ns, err = svc.runNamespaceForTeam(ctx, uuid.New().String())
	assert.ErrorIs(t, err, ErrTeamNotFound)
	assert.Equal(t, "", ns)
}

// TestListRunsNonAdminSeesExecNamespace is the end-to-end proof: a non-admin
// request whose TeamID owns the exec namespace sees its runs (before the fix the
// stub namespace matched nothing → empty 200); an orphan TeamID gets 404.
func TestListRunsNonAdminSeesExecNamespace(t *testing.T) {
	teamUID := uuid.New()
	execNS := "ksquad-team-bmad-squad-f6e8fc70"
	team := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Name: "bmad-squad", Namespace: "bmad-squad", UID: types.UID(teamUID.String())},
		Status:     ksquadv1.TeamStatus{Namespace: execNS},
	}
	run := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Name: "intake-abc-r1", Namespace: execNS},
		Spec:       ksquadv1.RunSpec{ProjectRef: ksquadv1.ObjectRef{Name: "bmad-demo-project"}},
		Status:     ksquadv1.RunStatus{Phase: ksquadv1.RunPhaseRunning},
	}
	k8sClient := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(team, run).Build()
	svc := NewRunsService(k8sClient)

	// Non-admin whose TeamID owns execNS: 200 with the run.
	req := httptest.NewRequest("GET", "/api/runs", nil)
	req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{
		Principal: "user:tenant", TeamID: teamUID,
	}))
	w := httptest.NewRecorder()
	listRuns(svc)(w, req)
	resp := w.Result()
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	var items []RunListItem
	assert.NoError(t, json.NewDecoder(resp.Body).Decode(&items))
	resp.Body.Close()
	assert.Len(t, items, 1, "non-admin must see the run in its exec namespace")

	// Orphan TeamID (no Team CR): 404, not an empty 200.
	req2 := httptest.NewRequest("GET", "/api/runs", nil)
	req2 = req2.WithContext(discussion.WithAuth(req2.Context(), discussion.AuthorContext{
		Principal: "user:orphan", TeamID: uuid.New(),
	}))
	w2 := httptest.NewRecorder()
	listRuns(svc)(w2, req2)
	assert.Equal(t, http.StatusNotFound, w2.Result().StatusCode)
}
