package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
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

// --- object builders (org-specific; overview_test.go owns team/project/run) ---------------------

func orgAgent(ns, name, uid, runtimeRef, roleRef, model string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef:          ksquadv1.ObjectRef{Name: runtimeRef},
			RoleRef:             ksquadv1.ObjectRef{Name: roleRef},
			CredentialSecretRef: ksquadv1.SecretRef{Name: name + "-cred"},
			Model:               model,
		},
	}
}

func agentRuntime(ns, name, typ string) *ksquadv1.AgentRuntime {
	return &ksquadv1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.AgentRuntimeSpec{Type: typ},
	}
}

func role(ns, name, uid string) *ksquadv1.Role {
	return &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec:       ksquadv1.RoleSpec{PromptRef: ksquadv1.ObjectRef{Name: name + "-prompt"}},
	}
}

// agentRun builds a Run selecting one Agent by name, with an optional claim time and Paused reason.
func agentRun(ns, name, agentName, workItem string, phase ksquadv1.RunPhase, claimed *time.Time, pausedReason string) *ksquadv1.Run {
	r := &ksquadv1.Run{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.RunSpec{
			WorkItemRef: workItem,
			Agents:      []ksquadv1.ObjectRef{{Name: agentName}},
		},
		Status: ksquadv1.RunStatus{Phase: phase},
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

func newOrgReader(t *testing.T, objs ...client.Object) *ClientOrgReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientOrgReader(c)
}

// TestOrgProjection — the happy path: Team→Agent→Role projected, runtime flavor resolved, agents
// sorted by name, role badges carrying the Role UID, statuses derived from each agent's Runs.
func TestOrgProjection(t *testing.T) {
	const teamUID = "11111111-1111-1111-1111-111111111111"
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	r := newOrgReader(t,
		team("squad-a", "alpha", teamUID),
		agentRuntime("squad-a", "rt-claude", ksquadv1.RuntimeTypeClaudeCode),
		role("squad-a", "dev", "role-dev-uid"),
		orgAgent("squad-a", "zeta", "ag-zeta", "rt-claude", "dev", "claude-x"),
		orgAgent("squad-a", "alpha-agent", "ag-alpha", "rt-claude", "dev", "claude-x"),
		agentRun("squad-a", "run-z", "zeta", "ISI-1", ksquadv1.RunPhaseRunning, &claimed, ""),
	)

	org, err := r.Org(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Org: %v", err)
	}
	if org.TeamID != teamUID || org.TeamName != "alpha" {
		t.Fatalf("team ref: %+v", org)
	}
	if len(org.Agents) != 2 {
		t.Fatalf("agents: got %d, want 2", len(org.Agents))
	}
	// Sorted by name: alpha-agent before zeta.
	if org.Agents[0].Name != "alpha-agent" || org.Agents[1].Name != "zeta" {
		t.Fatalf("agent order: %s, %s", org.Agents[0].Name, org.Agents[1].Name)
	}
	z := org.Agents[1]
	if z.ID != "ag-zeta" || z.RuntimeType != ksquadv1.RuntimeTypeClaudeCode {
		t.Fatalf("zeta node: %+v", z)
	}
	if len(z.Roles) != 1 || z.Roles[0].Name != "dev" || z.Roles[0].ID != "role-dev-uid" {
		t.Fatalf("zeta roles: %+v", z.Roles)
	}
	if z.Status != AgentStatusRunning || z.CurrentRunID == nil || *z.CurrentRunID != "run-z" {
		t.Fatalf("zeta status: %+v", z)
	}
	// alpha-agent has no runs ⇒ idle, no current run.
	a := org.Agents[0]
	if a.Status != AgentStatusIdle || a.CurrentRunID != nil {
		t.Fatalf("alpha-agent status: %+v", a)
	}
}

// TestOrgRuntimeFallback — an Agent whose AgentRuntime is not in the cache falls back to the ref
// name rather than rendering a blank runtime (legibility over a hole).
func TestOrgRuntimeFallback(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	r := newOrgReader(t,
		team("squad-a", "alpha", teamUID),
		orgAgent("squad-a", "solo", "ag-solo", "rt-missing", "", "m"),
	)
	org, err := r.Org(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Org: %v", err)
	}
	if org.Agents[0].RuntimeType != "rt-missing" {
		t.Fatalf("runtime fallback: %+v", org.Agents[0])
	}
	// No RoleRef ⇒ roles is an empty slice (never nil — the frontend maps over it).
	if org.Agents[0].Roles == nil {
		t.Fatalf("roles must be non-nil, got nil")
	}
}

// TestDeriveStatusBuckets — the four-value legibility mapping from Run phase + Paused reason.
func TestDeriveStatusBuckets(t *testing.T) {
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	later := claimed.Add(time.Hour)

	cases := []struct {
		name       string
		runs       []*ksquadv1.Run
		wantStatus string
		wantReason string
		wantRun    string
	}{
		{"no runs ⇒ idle", nil, AgentStatusIdle, "", ""},
		{
			"only terminal ⇒ idle",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhaseSucceeded, &claimed, "")},
			AgentStatusIdle, "", "",
		},
		{
			"running",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhaseRunning, &claimed, "")},
			AgentStatusRunning, "", "r1",
		},
		{
			"pending ⇒ running (in-flight)",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhasePending, nil, "")},
			AgentStatusRunning, "", "r1",
		},
		{
			"paused on credential",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhasePaused, &claimed, "CredentialExpired")},
			AgentStatusPaused, PausedReasonCredential, "r1",
		},
		{
			"paused on rate limit",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhasePaused, &claimed, "rate_limited")},
			AgentStatusPaused, PausedReasonRateLimit, "r1",
		},
		{
			"paused with no recognized reason ⇒ nil sub-reason",
			[]*ksquadv1.Run{agentRun("ns", "r1", "a", "", ksquadv1.RunPhasePaused, &claimed, "SomethingElse")},
			AgentStatusPaused, "", "r1",
		},
		{
			"freshest non-terminal wins",
			[]*ksquadv1.Run{
				agentRun("ns", "old", "a", "", ksquadv1.RunPhasePaused, &claimed, "rate_limited"),
				agentRun("ns", "new", "a", "", ksquadv1.RunPhaseRunning, &later, ""),
			},
			AgentStatusRunning, "", "new",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, reason, runID := deriveStatus(tc.runs)
			if status != tc.wantStatus {
				t.Fatalf("status: got %q, want %q", status, tc.wantStatus)
			}
			gotReason := ""
			if reason != nil {
				gotReason = *reason
			}
			if gotReason != tc.wantReason {
				t.Fatalf("reason: got %q, want %q", gotReason, tc.wantReason)
			}
			gotRun := ""
			if runID != nil {
				gotRun = *runID
			}
			if gotRun != tc.wantRun {
				t.Fatalf("runID: got %q, want %q", gotRun, tc.wantRun)
			}
		})
	}
}

// TestAgentDetailScope — an Agent resolves by UID within the caller's Team; a foreign-team Agent
// UID is existence-hidden as ErrAgentNotFound (never surfaced as another team's structure).
func TestAgentDetailScope(t *testing.T) {
	const uidA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	const uidB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	r := newOrgReader(t,
		team("squad-a", "alpha", uidA),
		team("squad-b", "beta", uidB),
		orgAgent("squad-a", "a-agent", "ag-a", "rt", "", "m"),
		orgAgent("squad-b", "b-agent", "ag-b", "rt", "", "m"),
	)

	got, err := r.Agent(context.Background(), uidA, "ag-a")
	if err != nil {
		t.Fatalf("Agent(own): %v", err)
	}
	if got.ID != "ag-a" || got.Name != "a-agent" {
		t.Fatalf("agent: %+v", got)
	}
	// Team A caller asking for Team B's agent UID ⇒ not found (existence-hiding).
	if _, err := r.Agent(context.Background(), uidA, "ag-b"); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("foreign agent: got %v, want ErrAgentNotFound", err)
	}
}

// TestAgentRuns — the Run history is most-recent-first, paginated, with paused sub-reason,
// work-item ref and a server-computed duration for a completed Run.
func TestAgentRuns(t *testing.T) {
	const teamUID = "33333333-3333-3333-3333-333333333333"
	start := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	older := start.Add(-time.Hour)
	r := newOrgReader(t,
		team("squad-a", "alpha", teamUID),
		orgAgent("squad-a", "solo", "ag-solo", "rt", "", "m"),
		agentRun("squad-a", "run-new", "solo", "ISI-2", ksquadv1.RunPhaseRunning, &start, ""),
		agentRun("squad-a", "run-old", "solo", "ISI-1", ksquadv1.RunPhaseSucceeded, &older, ""),
	)

	runs, err := r.AgentRuns(context.Background(), teamUID, "ag-solo", 0, 0)
	if err != nil {
		t.Fatalf("AgentRuns: %v", err)
	}
	if len(runs) != 2 {
		t.Fatalf("runs: got %d, want 2", len(runs))
	}
	// Most-recent-first: run-new (later claim) before run-old.
	if runs[0].ID != "run-new" || runs[1].ID != "run-old" {
		t.Fatalf("run order: %s, %s", runs[0].ID, runs[1].ID)
	}
	if runs[0].WorkItemRef != "ISI-2" || runs[0].StartedAt == nil {
		t.Fatalf("run-new: %+v", runs[0])
	}
	// A running run has no endedAt but a live-so-far duration.
	if runs[0].EndedAt != nil {
		t.Fatalf("run-new must have nil endedAt: %+v", runs[0].EndedAt)
	}

	// limit=1 returns only the freshest; offset=1 skips it.
	one, _ := r.AgentRuns(context.Background(), teamUID, "ag-solo", 1, 0)
	if len(one) != 1 || one[0].ID != "run-new" {
		t.Fatalf("limit=1: %+v", one)
	}
	off, _ := r.AgentRuns(context.Background(), teamUID, "ag-solo", 1, 1)
	if len(off) != 1 || off[0].ID != "run-old" {
		t.Fatalf("offset=1: %+v", off)
	}

	// An unknown agent UID is existence-hidden.
	if _, err := r.AgentRuns(context.Background(), teamUID, "nope", 0, 0); !errors.Is(err, ErrAgentNotFound) {
		t.Fatalf("unknown agent: got %v, want ErrAgentNotFound", err)
	}
}

// TestAgentStatuses — the light poll projection keys deltas by Agent UID and derives status.
func TestAgentStatuses(t *testing.T) {
	const teamUID = "44444444-4444-4444-4444-444444444444"
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	r := newOrgReader(t,
		team("squad-a", "alpha", teamUID),
		orgAgent("squad-a", "busy", "ag-busy", "rt", "", "m"),
		orgAgent("squad-a", "free", "ag-free", "rt", "", "m"),
		agentRun("squad-a", "run-b", "busy", "ISI-1", ksquadv1.RunPhaseRunning, &claimed, ""),
	)
	deltas, err := r.AgentStatuses(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("AgentStatuses: %v", err)
	}
	byID := map[string]AgentStatusDelta{}
	for _, d := range deltas {
		byID[d.AgentID] = d
	}
	if byID["ag-busy"].Status != AgentStatusRunning || byID["ag-busy"].CurrentRunID == nil {
		t.Fatalf("busy: %+v", byID["ag-busy"])
	}
	if byID["ag-free"].Status != AgentStatusIdle {
		t.Fatalf("free: %+v", byID["ag-free"])
	}
}

// TestStatusFrame — the hand-built SSE JSON line is single-line and omits absent optionals.
func TestStatusFrame(t *testing.T) {
	reason := PausedReasonRateLimit
	runID := "run-1"
	full := statusFrame(AgentStatusDelta{AgentID: "a", Status: "paused", PausedReason: &reason, CurrentRunID: &runID})
	want := `{"agentId":"a","status":"paused","pausedReason":"rate_limited","currentRunId":"run-1"}`
	if full != want {
		t.Fatalf("frame: got %s, want %s", full, want)
	}
	// A valid JSON object with only the required fields when optionals are nil.
	bare := statusFrame(AgentStatusDelta{AgentID: "a", Status: "idle"})
	var m map[string]any
	if err := json.Unmarshal([]byte(bare), &m); err != nil {
		t.Fatalf("bare frame not valid JSON: %v (%s)", err, bare)
	}
	if _, ok := m["pausedReason"]; ok {
		t.Fatalf("bare frame must omit pausedReason: %s", bare)
	}
	if strings.ContainsAny(full, "\n\r") {
		t.Fatalf("frame must be single-line")
	}
}

// --- handler / server wiring ---------------------------------------------------------------------

func testOrgServer(t *testing.T, teamID uuid.UUID, reader OrgReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Org:           reader,
	})
	return srv.Handler()
}

// TestOrgHandlerOK — a session serves 200 + its Team org when the path teamId matches its scope.
func TestOrgHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	reader := newOrgReader(t,
		team("squad-a", "alpha", teamID.String()),
		orgAgent("squad-a", "solo", "ag-solo", "rt", "", "m"),
	)
	h := testOrgServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/teams/"+teamID.String()+"/org", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("org: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var org TeamOrg
	if err := json.Unmarshal(rec.Body.Bytes(), &org); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if org.TeamName != "alpha" || len(org.Agents) != 1 {
		t.Fatalf("body: %+v", org)
	}
}

// TestOrgHandlerForeignTeam404 — a caller asking for a Team UID that is not their session scope is
// existence-hidden (404), never served another team's org.
func TestOrgHandlerForeignTeam404(t *testing.T) {
	teamID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	reader := newOrgReader(t, team("squad-a", "alpha", teamID.String()))
	h := testOrgServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	other := "99999999-9999-9999-9999-999999999999"
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/teams/"+other+"/org", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign team: got %d, want 404", rec.Code)
	}
}

// TestAgentRunsHandlerArray — the runs route returns a bare JSON array (the client's contract).
func TestAgentRunsHandlerArray(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	reader := newOrgReader(t,
		team("squad-a", "alpha", teamID.String()),
		orgAgent("squad-a", "solo", "ag-solo", "rt", "", "m"),
		agentRun("squad-a", "run-1", "solo", "ISI-1", ksquadv1.RunPhaseRunning, &claimed, ""),
	)
	h := testOrgServer(t, teamID, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/agents/ag-solo/runs", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("agent-runs: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var runs []RunSummary
	if err := json.Unmarshal(rec.Body.Bytes(), &runs); err != nil {
		t.Fatalf("decode array: %v (body %s)", err, rec.Body.String())
	}
	if len(runs) != 1 || runs[0].ID != "run-1" {
		t.Fatalf("runs: %+v", runs)
	}
}

// TestOrgHandlerUnauthenticated — no session ⇒ 401 at the choke point.
func TestOrgHandlerUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	h := testOrgServer(t, teamID, newOrgReader(t, team("squad-a", "alpha", teamID.String())))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/teams/"+teamID.String()+"/org", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// TestOrgNilReaderStill501 — with no read model wired all four routes keep the documented 501.
func TestOrgNilReaderStill501(t *testing.T) {
	teamID := uuid.MustParse("aaaa0000-0000-0000-0000-000000000000")
	h := testOrgServer(t, teamID, nil)
	for _, path := range []string{
		"/api/teams/" + teamID.String() + "/org",
		"/api/teams/" + teamID.String() + "/status/stream",
		"/api/agents/ag-x",
		"/api/agents/ag-x/runs",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s (nil reader): got %d, want 501", path, rec.Code)
		}
	}
}

// TestStatusStreamInitialSnapshot — the SSE stream emits the current per-agent status on connect,
// so the diagram's live overlay reflects truth immediately, then closes on client disconnect.
func TestStatusStreamInitialSnapshot(t *testing.T) {
	old := orgStatusPollInterval
	orgStatusPollInterval = 10 * time.Millisecond
	defer func() { orgStatusPollInterval = old }()

	teamID := uuid.MustParse("bbbb0000-0000-0000-0000-000000000000")
	claimed := time.Date(2026, 8, 18, 9, 0, 0, 0, time.UTC)
	reader := newOrgReader(t,
		team("squad-a", "alpha", teamID.String()),
		orgAgent("squad-a", "busy", "ag-busy", "rt", "", "m"),
		agentRun("squad-a", "run-b", "busy", "ISI-1", ksquadv1.RunPhaseRunning, &claimed, ""),
	)
	h := testOrgServer(t, teamID, reader)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/teams/"+teamID.String()+"/status/stream", nil), devToken).WithContext(ctx)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("stream: got %d, want 200", rec.Code)
	}
	if !strings.Contains(body, `"agentId":"ag-busy"`) || !strings.Contains(body, `"status":"running"`) {
		t.Fatalf("stream missing initial snapshot for ag-busy: %q", body)
	}
	if !strings.Contains(body, `"currentRunId":"run-b"`) {
		t.Fatalf("stream missing currentRunId: %q", body)
	}
}
