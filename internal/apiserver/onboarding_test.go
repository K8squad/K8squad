package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- object builders (onboarding-specific) -------------------------------------------------------

// presetAgent builds an Agent on one of the AD-3 preset Roles with a credential ref set.
func presetAgent(ns, name, roleRef string) *ksquadv1.Agent {
	return &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef:          ksquadv1.ObjectRef{Name: "rt"},
			RoleRef:             ksquadv1.ObjectRef{Name: roleRef},
			CredentialSecretRef: ksquadv1.SecretRef{Name: name + "-cred"},
		},
	}
}

// onboardingProject builds a Project whose spec.repo.auth is set (milestone ④).
func onboardingProject(ns, name string) *ksquadv1.Project {
	return &ksquadv1.Project{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: ksquadv1.ProjectSpec{Repo: ksquadv1.RepoSpec{
			URL:  "https://example.com/repo.git",
			Auth: &ksquadv1.RepoAuth{CredentialSecretRef: ksquadv1.SecretRef{Name: name + "-repo-cred"}},
		}},
	}
}

// fullSquad returns the objects of a tenant at 3/4: Team + the three preset Agents (each with a
// credential ref set). Add an onboardingProject to reach 4/4.
func fullSquad(ns, teamUID string) []client.Object {
	return []client.Object{
		team(ns, "squad-a", teamUID),
		presetAgent(ns, "boss", "role-boss"),
		presetAgent(ns, "impl", "role-implementer"),
		presetAgent(ns, "mgr", "role-manager"),
	}
}

func newOnboardingReader(t *testing.T, objs ...client.Object) *ClientOnboardingReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientOnboardingReader(c)
}

// --- reader: milestone derivation --------------------------------------------------------------

// TestOnboardingNoTeamZeroProgress — a first-run tenant (no Team CR) gets the honest zero
// projection: step 1, done 0, nextMilestone "team" — NOT a 404 (milestone ① IS the absence).
func TestOnboardingNoTeamZeroProgress(t *testing.T) {
	r := newOnboardingReader(t)
	p, err := r.Progress(context.Background(), "99999999-9999-9999-9999-999999999999")
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	want := OnboardingProgress{Step: 1, Done: 0, Total: 4, NextMilestone: OnboardingMilestoneTeam}
	if p != want {
		t.Fatalf("got %+v, want %+v", p, want)
	}
}

// TestOnboardingTeamOnly — a tenant with only a Team is at step 2 (agents next).
func TestOnboardingTeamOnly(t *testing.T) {
	const teamUID = "11111111-1111-1111-1111-111111111111"
	r := newOnboardingReader(t, team("squad-a", "alpha", teamUID))
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.Done != 1 || p.Step != 2 || p.NextMilestone != OnboardingMilestoneAgents || p.Dismissed {
		t.Fatalf("progress: %+v", p)
	}
}

// TestOnboardingAgentsNeedsAllPresets — two of the three preset Roles covered is not enough;
// a fourth Agent on a non-preset Role does not count toward ②.
func TestOnboardingAgentsNeedsAllPresets(t *testing.T) {
	const teamUID = "22222222-2222-2222-2222-222222222222"
	r := newOnboardingReader(t,
		team("squad-a", "alpha", teamUID),
		presetAgent("squad-a", "boss", "role-boss"),
		presetAgent("squad-a", "impl", "role-implementer"),
		presetAgent("squad-a", "extra", "role-custom"),
	)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.NextMilestone != OnboardingMilestoneAgents || p.Step != 2 {
		t.Fatalf("agents milestone must stay incomplete: %+v", p)
	}
}

// TestOnboardingModelsNeedsCredentialRef — preset squad where one Agent carries no credential
// ref: ② completes but ③ does not. (Secret-object existence is the admission webhook's job —
// the apiserver SA has no Secret RBAC by design, ISI-3546.)
func TestOnboardingModelsNeedsCredentialRef(t *testing.T) {
	const teamUID = "33333333-3333-3333-3333-333333333333"
	mgr := presetAgent("squad-a", "mgr", "role-manager")
	mgr.Spec.CredentialSecretRef = ksquadv1.SecretRef{} // ref never set
	objs := []client.Object{
		team("squad-a", "alpha", teamUID),
		presetAgent("squad-a", "boss", "role-boss"),
		presetAgent("squad-a", "impl", "role-implementer"),
		mgr,
	}
	r := newOnboardingReader(t, objs...)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.Done != 2 || p.NextMilestone != OnboardingMilestoneModels {
		t.Fatalf("models milestone must be incomplete: %+v", p)
	}
}

// TestOnboardingFailedTestConnectionBlocks — a RECORDED test-connection failure (AD-7 Team
// annotation) un-completes ③ even though every credential Secret resolves.
func TestOnboardingFailedTestConnectionBlocks(t *testing.T) {
	const teamUID = "44444444-4444-4444-4444-444444444444"
	tm := team("squad-a", "alpha", teamUID)
	SetTestConnectionFlag(tm, "impl", false)
	objs := append(fullSquad("squad-a", teamUID), onboardingProject("squad-a", "proj"))
	objs[0] = tm
	r := newOnboardingReader(t, objs...)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.NextMilestone != OnboardingMilestoneModels || p.Done != 3 {
		t.Fatalf("recorded failure must block ③: %+v", p)
	}
}

// TestOnboardingPassedTestConnectionOK — a recorded pass keeps ③ complete.
func TestOnboardingPassedTestConnectionOK(t *testing.T) {
	const teamUID = "55555555-5555-5555-5555-555555555555"
	tm := team("squad-a", "alpha", teamUID)
	SetTestConnectionFlag(tm, "boss", true)
	objs := append(fullSquad("squad-a", teamUID), onboardingProject("squad-a", "proj"))
	objs[0] = tm
	r := newOnboardingReader(t, objs...)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.Done != 4 || p.Step != 4 || p.NextMilestone != "" {
		t.Fatalf("4/4 expected: %+v", p)
	}
}

// TestOnboardingComplete — full squad + Project with repo.auth: 4/4, no next milestone.
func TestOnboardingComplete(t *testing.T) {
	const teamUID = "66666666-6666-6666-6666-666666666666"
	objs := append(fullSquad("squad-a", teamUID), onboardingProject("squad-a", "proj"))
	r := newOnboardingReader(t, objs...)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p != (OnboardingProgress{Step: 4, Done: 4, Total: 4}) {
		t.Fatalf("got %+v", p)
	}
}

// TestOnboardingProjectNeedsRepoAuth — a Project WITHOUT spec.repo.auth does not complete ④;
// step stays 4 with done=3 (team+agents+models complete).
func TestOnboardingProjectNeedsRepoAuth(t *testing.T) {
	const teamUID = "77777777-7777-7777-7777-777777777777"
	objs := append(fullSquad("squad-a", teamUID), project("squad-a", "proj", "https://example.com/r.git"))
	r := newOnboardingReader(t, objs...)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.Done != 3 || p.Step != 4 || p.NextMilestone != OnboardingMilestoneProject {
		t.Fatalf("project without repo.auth must not complete ④: %+v", p)
	}
}

// TestOnboardingDismissedFlagSurfaced — the dismissal annotation rides the payload but NEVER
// changes the derived counts (AC3: derived progress is authoritative over dismissal).
func TestOnboardingDismissedFlagSurfaced(t *testing.T) {
	const teamUID = "88888888-8888-8888-8888-888888888888"
	tm := team("squad-a", "alpha", teamUID)
	SetOnboardingDismissed(tm, true)
	r := newOnboardingReader(t, tm)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if !p.Dismissed || p.Done != 1 || p.NextMilestone != OnboardingMilestoneAgents {
		t.Fatalf("dismissed must surface without changing derivation: %+v", p)
	}
}

// TestOnboardingOutOfOrderMilestones — a tenant with a Project but no Agents has done=2 while
// step=2 (the FIRST incomplete milestone drives nextMilestone, not the done count).
func TestOnboardingOutOfOrderMilestones(t *testing.T) {
	const teamUID = "99999999-0000-0000-0000-000000000000"
	r := newOnboardingReader(t,
		team("squad-a", "alpha", teamUID),
		onboardingProject("squad-a", "proj"),
	)
	p, err := r.Progress(context.Background(), teamUID)
	if err != nil {
		t.Fatalf("Progress: %v", err)
	}
	if p.Done != 2 || p.Step != 2 || p.NextMilestone != OnboardingMilestoneAgents {
		t.Fatalf("out-of-order completion: %+v", p)
	}
}

// --- annotation helpers -------------------------------------------------------------------------

// TestOnboardingAnnotationHelpers — set/clear/read round-trips on the Team CR annotations.
func TestOnboardingAnnotationHelpers(t *testing.T) {
	tm := team("squad-a", "alpha", "uid")
	if OnboardingDismissed(tm) {
		t.Fatalf("dismissed must default false")
	}
	SetOnboardingDismissed(tm, true)
	if !OnboardingDismissed(tm) {
		t.Fatalf("dismissed must read back true")
	}
	SetOnboardingDismissed(tm, false)
	if OnboardingDismissed(tm) {
		t.Fatalf("clearing must remove the annotation (absent beats stale false)")
	}
	if _, ok := tm.Annotations[OnboardingDismissedAnnotation]; ok {
		t.Fatalf("cleared key must be deleted, got %v", tm.Annotations)
	}

	if rec, _ := TestConnectionFlag(tm, "boss"); rec {
		t.Fatalf("test-connection must default unrecorded")
	}
	SetTestConnectionFlag(tm, "boss", false)
	if rec, passed := TestConnectionFlag(tm, "boss"); !rec || passed {
		t.Fatalf("recorded failure must read (true,false)")
	}
	SetTestConnectionFlag(tm, "boss", true)
	if rec, passed := TestConnectionFlag(tm, "boss"); !rec || !passed {
		t.Fatalf("recorded pass must read (true,true)")
	}
}

// --- handler / server wiring ---------------------------------------------------------------------

func testOnboardingServer(t *testing.T, teamID uuid.UUID, reader OnboardingReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Onboarding:    reader,
	})
	return srv.Handler()
}

// TestOnboardingHandlerOK — a session serves 200 + its derived projection.
func TestOnboardingHandlerOK(t *testing.T) {
	teamID := uuid.MustParse("aaaa1111-1111-1111-1111-111111111111")
	r := newOnboardingReader(t, team("squad-a", "alpha", teamID.String()))
	h := testOnboardingServer(t, teamID, r)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/onboarding/progress", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var p OnboardingProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p.Total != 4 || p.Done != 1 || p.NextMilestone != OnboardingMilestoneAgents {
		t.Fatalf("body: %+v", p)
	}
}

// TestOnboardingHandlerFirstRun — a session whose Team CR does not exist yet gets the honest
// zero projection with 200 (NOT 404 — the endpoint's job is to describe that absence).
func TestOnboardingHandlerFirstRun(t *testing.T) {
	teamID := uuid.MustParse("bbbb1111-1111-1111-1111-111111111111")
	h := testOnboardingServer(t, teamID, newOnboardingReader(t))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/onboarding/progress", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("first-run: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var p OnboardingProgress
	if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if p != (OnboardingProgress{Step: 1, Done: 0, Total: 4, NextMilestone: OnboardingMilestoneTeam}) {
		t.Fatalf("body: %+v", p)
	}
}

// TestOnboardingHandlerUnauthenticated — no session ⇒ 401 at the choke point. The route carries
// no {teamId} path param, so cross-tenant reads are structurally impossible (AC4).
func TestOnboardingHandlerUnauthenticated(t *testing.T) {
	teamID := uuid.MustParse("cccc1111-1111-1111-1111-111111111111")
	h := testOnboardingServer(t, teamID, newOnboardingReader(t))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/onboarding/progress", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

// TestOnboardingNilReaderStill501 — with no read model wired the route keeps the documented 501.
func TestOnboardingNilReaderStill501(t *testing.T) {
	teamID := uuid.MustParse("dddd1111-1111-1111-1111-111111111111")
	h := testOnboardingServer(t, teamID, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/onboarding/progress", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil reader: got %d, want 501 (body %s)", rec.Code, rec.Body.String())
	}
}
