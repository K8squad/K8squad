package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- object builders (agent-activity-specific) --------------------------------------------------

// activityRun builds a Run selecting one Agent by name, in a given project, with an
// optional claim time, Paused reason, and LLM-interaction stream.
func activityRun(ns, name, agentName, projectName, workItem string, phase ksquadv1.RunPhase, claimed *time.Time, pausedReason string, interactions ...ksquadv1.LLMInteraction) *ksquadv1.Run {
	r := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(name + "-uid-0000")},
		Spec: ksquadv1.RunSpec{
			ProjectRef:  ksquadv1.ObjectRef{Name: projectName},
			WorkItemRef: workItem,
			Agents:      []ksquadv1.ObjectRef{{Name: agentName}},
		},
		Status: ksquadv1.RunStatus{Phase: phase, LLMInteractions: interactions},
	}
	if claimed != nil {
		r.Status.ClaimedAt = &metav1.Time{Time: *claimed}
	}
	if pausedReason != "" {
		r.Status.Conditions = []metav1.Condition{{
			Type:               "Paused",
			Status:             metav1.ConditionTrue,
			Reason:             pausedReason,
			LastTransitionTime: metav1.Time{Time: time.Date(2026, 8, 18, 10, 0, 0, 0, time.UTC)},
		}}
	}
	return r
}

func llm(id, typ, text string, ts time.Time) ksquadv1.LLMInteraction {
	in := ksquadv1.LLMInteraction{ID: id, Type: typ, Model: "claude", Timestamp: metav1.Time{Time: ts}}
	if typ == "response" || typ == "tool_response" {
		in.Response = []byte(text)
	} else {
		in.Request = []byte(text)
	}
	return in
}

func newActivityReader(t *testing.T, objs ...client.Object) *ClientAgentActivityReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientAgentActivityReader(c, nil) // nil db ⇒ snippet from LLMInteractions only
}

// --- pickCurrentOrLast unit ---------------------------------------------------------------------

func TestPickCurrentOrLast(t *testing.T) {
	early := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	late := time.Date(2026, 8, 18, 11, 0, 0, 0, time.UTC)

	t.Run("running beats a terminal run", func(t *testing.T) {
		running := activityRun("ns", "r-run", "a", "p", "wi", ksquadv1.RunPhaseRunning, &early, "")
		done := activityRun("ns", "r-done", "a", "p", "wi", ksquadv1.RunPhaseSucceeded, &late, "")
		got, state := pickCurrentOrLast([]*ksquadv1.Run{done, running})
		if state != AgentStatusRunning || got.Name != "r-run" {
			t.Fatalf("got (%s,%s), want (r-run,running)", got.Name, state)
		}
	})

	t.Run("paused current run is blocked", func(t *testing.T) {
		paused := activityRun("ns", "r-paused", "a", "p", "wi", ksquadv1.RunPhasePaused, &early, "RateLimited")
		got, state := pickCurrentOrLast([]*ksquadv1.Run{paused})
		if state != AgentStatusBlocked || got.Name != "r-paused" {
			t.Fatalf("got (%s,%s), want (r-paused,blocked)", got.Name, state)
		}
	})

	t.Run("only terminal runs ⇒ idle, last one kept", func(t *testing.T) {
		old := activityRun("ns", "r-old", "a", "p", "wi", ksquadv1.RunPhaseFailed, &early, "")
		recent := activityRun("ns", "r-recent", "a", "p", "wi", ksquadv1.RunPhaseSucceeded, &late, "")
		got, state := pickCurrentOrLast([]*ksquadv1.Run{old, recent})
		if state != AgentStatusIdle || got == nil || got.Name != "r-recent" {
			t.Fatalf("got (%v,%s), want (r-recent,idle)", got, state)
		}
	})

	t.Run("no runs ⇒ idle, nil run", func(t *testing.T) {
		got, state := pickCurrentOrLast(nil)
		if state != AgentStatusIdle || got != nil {
			t.Fatalf("got (%v,%s), want (nil,idle)", got, state)
		}
	})
}

// --- classification units -----------------------------------------------------------------------

func TestCleanAndClassify(t *testing.T) {
	cases := []struct {
		name, body, wantClean string
		wantNarration         bool
	}{
		{"plain narration", "[run abcd1234] Investigating the failing test", "Investigating the failing test", true},
		{"tool stream demoted", "[run abcd1234][tool:bash/result(ok)] exit 0", "exit 0", false},
		{"status line demoted", "[run abcd1234][status] phase=running", "phase=running", false},
		{"server tag demoted", "[run abcd1234][server:dispatch] ok", "ok", false},
		{"ticket comment is not narration", "a user comment with no run prefix", "a user comment with no run prefix", false},
		{"bare marker keeps remainder", "[run abcd1234][status]", "[status]", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := cleanActivityBody(c.body); got != c.wantClean {
				t.Fatalf("cleanActivityBody(%q) = %q, want %q", c.body, got, c.wantClean)
			}
			if got := isNarration(c.body); got != c.wantNarration {
				t.Fatalf("isNarration(%q) = %v, want %v", c.body, got, c.wantNarration)
			}
		})
	}
}

func TestCollapseWhitespace(t *testing.T) {
	if got := collapseWhitespace("  line one\n  line two\t\tx "); got != "line one line two x" {
		t.Fatalf("collapseWhitespace = %q", got)
	}
}

// --- reader projection (tenant, nil db) ---------------------------------------------------------

// TestAgentActivityTenantProjection — a tenant caller gets their squad's agents
// with the right state buckets, counts, current-or-last run selection, a
// server-classified snippet from the newest LLM response, and an in-console ticket
// deep-link.
func TestAgentActivityTenantProjection(t *testing.T) {
	const teamUID = "11111111-1111-1111-1111-111111111111"
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)

	// amelia: running, two LLM interactions (prompt then response) — snippet = response.
	ameliaRun := activityRun("squad-a", "run-amelia", "amelia", "web", "wi-amelia", ksquadv1.RunPhaseRunning, &t0, "",
		llm("i1", "prompt", "do the thing", t0),
		llm("i2", "response", "Reading the handler to find the bug", t1),
	)
	// winston: paused ⇒ blocked.
	winstonRun := activityRun("squad-a", "run-winston", "winston", "web", "wi-winston", ksquadv1.RunPhasePaused, &t0, "RateLimited")
	// cade: only a terminal run ⇒ idle but still shows the last run.
	cadeRun := activityRun("squad-a", "run-cade", "cade", "web", "wi-cade", ksquadv1.RunPhaseSucceeded, &t0, "")

	r := newActivityReader(t,
		team("squad-a", "alpha", teamUID),
		orgAgent("squad-a", "amelia", "ag-amelia", "rt", "backend", "claude"),
		orgAgent("squad-a", "winston", "ag-winston", "rt", "architect", "claude"),
		orgAgent("squad-a", "cade", "ag-cade", "rt", "", "claude"),
		ameliaRun, winstonRun, cadeRun,
	)

	list, err := r.AgentActivity(context.Background(), teamUID, false)
	if err != nil {
		t.Fatalf("AgentActivity: %v", err)
	}
	if list.Fleet {
		t.Fatalf("tenant projection must not be fleet-flagged")
	}
	if list.Counts != (ActivityCounts{Running: 1, Idle: 1, Blocked: 1}) {
		t.Fatalf("counts: %+v", list.Counts)
	}
	if len(list.Agents) != 3 {
		t.Fatalf("agents: %d", len(list.Agents))
	}
	by := map[string]AgentActivity{}
	for _, a := range list.Agents {
		by[a.Name] = a
	}

	amelia := by["amelia"]
	if amelia.State != AgentStatusRunning || amelia.Role != "backend" {
		t.Fatalf("amelia: %+v", amelia)
	}
	if amelia.CurrentOrLastRun == nil {
		t.Fatalf("amelia has no run")
	}
	if got := amelia.CurrentOrLastRun.LastThinkingSnippet; got != "Reading the handler to find the bug" {
		t.Fatalf("amelia snippet = %q (want the newest LLM response, not the prompt)", got)
	}
	if got := amelia.CurrentOrLastRun.WorkItemURL; got != "/projects/squad-a%2Fweb/issues/wi-amelia" {
		t.Fatalf("amelia workItemUrl = %q", got)
	}
	if amelia.CurrentOrLastRun.DiscussionURL != "" {
		t.Fatalf("amelia discussionUrl must be empty for a board run, got %q", amelia.CurrentOrLastRun.DiscussionURL)
	}
	if amelia.CurrentOrLastRun.LastActivityAt == nil || !amelia.CurrentOrLastRun.LastActivityAt.Equal(t1) {
		t.Fatalf("amelia lastActivityAt = %v, want %v", amelia.CurrentOrLastRun.LastActivityAt, t1)
	}

	if by["winston"].State != AgentStatusBlocked {
		t.Fatalf("winston state = %s", by["winston"].State)
	}
	cade := by["cade"]
	if cade.State != AgentStatusIdle || cade.CurrentOrLastRun == nil || cade.CurrentOrLastRun.RunID != "run-cade" {
		t.Fatalf("cade: %+v", cade)
	}
}

// TestAgentActivitySnippetExcludesToolStream — a tool_call is the newest
// interaction but is never surfaced as thinking: the snippet stays the older
// response while lastActivityAt advances to the tool call (freshness).
func TestAgentActivitySnippetExcludesToolStream(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	t1 := t0.Add(time.Minute)
	t2 := t1.Add(time.Minute)

	run := activityRun("squad-a", "run-x", "amelia", "web", "wi-x", ksquadv1.RunPhaseRunning, &t0, "",
		llm("i1", "response", "Planning the fix", t1),
		llm("i2", "tool_call", "bash: go test ./...", t2),
	)
	r := newActivityReader(t,
		team("squad-a", "alpha", teamUID),
		orgAgent("squad-a", "amelia", "ag-amelia", "rt", "backend", "claude"),
		run,
	)
	list, err := r.AgentActivity(context.Background(), teamUID, false)
	if err != nil {
		t.Fatalf("AgentActivity: %v", err)
	}
	ar := list.Agents[0].CurrentOrLastRun
	if ar == nil {
		t.Fatalf("no run")
	}
	if ar.LastThinkingSnippet != "Planning the fix" {
		t.Fatalf("snippet = %q, want the response (tool_call must not surface)", ar.LastThinkingSnippet)
	}
	if ar.LastActivityAt == nil || !ar.LastActivityAt.Equal(t2) {
		t.Fatalf("lastActivityAt = %v, want the tool call at %v", ar.LastActivityAt, t2)
	}
}

// TestAgentActivityFleetAdmin — an admin spans every squad; a run living in a
// squad's execution namespace still deep-links through the squad's HOME namespace.
func TestAgentActivityFleetAdmin(t *testing.T) {
	tm := &ksquadv1.Team{
		ObjectMeta: metav1.ObjectMeta{Namespace: "squad-a", Name: "alpha", UID: types.UID("team-a")},
		Status:     ksquadv1.TeamStatus{Namespace: "squad-a-exec"},
	}
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	// Agent in the HOME ns; its run in the EXECUTION ns.
	execRun := activityRun("squad-a-exec", "run-a", "amelia", "web", "wi-a", ksquadv1.RunPhaseRunning, &t0, "",
		llm("i1", "response", "working", t0))

	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(
		tm,
		orgAgent("squad-a", "amelia", "ag-amelia", "rt", "backend", "claude"),
		execRun,
	).Build()
	r := NewClientAgentActivityReader(c, nil)

	list, err := r.AgentActivity(context.Background(), "", true) // admin ⇒ teamUID irrelevant
	if err != nil {
		t.Fatalf("AgentActivity(admin): %v", err)
	}
	if !list.Fleet {
		t.Fatalf("admin projection must be fleet-flagged")
	}
	if len(list.Agents) != 1 || list.Agents[0].CurrentOrLastRun == nil {
		t.Fatalf("agents: %+v", list.Agents)
	}
	// The run lives in squad-a-exec but the deep-link uses the HOME ns squad-a.
	if got := list.Agents[0].CurrentOrLastRun.WorkItemURL; got != "/projects/squad-a%2Fweb/issues/wi-a" {
		t.Fatalf("admin workItemUrl = %q (exec ns must be mapped to home)", got)
	}
}

// TestAgentActivityTenantNotFound — an authenticated tenant whose UID matches no
// Team gets ErrTeamNotFound (the handler maps it to an existence-hiding 404).
func TestAgentActivityTenantNotFound(t *testing.T) {
	r := newActivityReader(t, team("squad-a", "alpha", "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"))
	_, err := r.AgentActivity(context.Background(), "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", false)
	if err != ErrTeamNotFound {
		t.Fatalf("err = %v, want ErrTeamNotFound", err)
	}
}

// --- handler / server wiring --------------------------------------------------------------------

func testActivityServer(t *testing.T, teamID uuid.UUID, admin bool, reader AgentActivityReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID, IsAdmin: admin},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		AgentActivity: reader,
	})
	return srv.Handler()
}

func TestAgentActivityHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	t0 := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	reader := newActivityReader(t,
		team("squad-a", "alpha", teamID.String()),
		orgAgent("squad-a", "amelia", "ag-amelia", "rt", "backend", "claude"),
		activityRun("squad-a", "run-a", "amelia", "web", "wi-a", ksquadv1.RunPhaseRunning, &t0, "",
			llm("i1", "response", "on it", t0)),
	)
	h := testActivityServer(t, teamID, false, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/activity", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("activity: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var list AgentActivityList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if list.Counts.Running != 1 || len(list.Agents) != 1 || list.Agents[0].Name != "amelia" {
		t.Fatalf("body: %+v", list)
	}
}

// TestAgentActivityHandlerNotShadowedByName — the literal /activity route wins over
// the /api/squad/agents/{name} capture (registration-order precedence).
func TestAgentActivityHandlerNotShadowedByName(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	reader := newActivityReader(t, team("squad-a", "alpha", teamID.String()))
	h := testActivityServer(t, teamID, false, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/activity", nil), devToken))
	// 200 (activity handler), not a 404/501 from the {name} detail route (which has
	// no FleetList reader wired here).
	if rec.Code != http.StatusOK {
		t.Fatalf("activity route shadowed: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
}

func TestAgentActivityHandlerTeamNotFound(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	reader := newActivityReader(t, team("squad-a", "alpha", "66666666-6666-6666-6666-666666666666"))
	h := testActivityServer(t, teamID, false, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/activity", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("activity(no team): got %d, want 404", rec.Code)
	}
}

func TestAgentActivityHandlerUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reader := newActivityReader(t, team("squad-a", "alpha", teamID.String()))
	h := testActivityServer(t, teamID, false, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/squad/agents/activity", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("activity(no session): got %d, want 401", rec.Code)
	}
}

func TestAgentActivityNilReaderStill501(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	h := testActivityServer(t, teamID, false, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/agents/activity", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("activity(nil reader): got %d, want 501", rec.Code)
	}
}
