package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/apiserver"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/auth"
)

// fakeBootstrapStore records the reconcile's decisions. Create/UpdatePassword are
// the two write paths reconcileBootstrapAdmin must choose between.
type fakeBootstrapStore struct {
	user       *auth.User // seeded existing user, or nil for a fresh install
	created    *auth.User
	updatedID  uuid.UUID
	updatedPwd string
}

func (f *fakeBootstrapStore) ByUsername(_ context.Context, name string) (*auth.User, error) {
	if f.user != nil && f.user.Username == name {
		cp := *f.user
		return &cp, nil
	}
	return nil, auth.ErrNotFound
}

func (f *fakeBootstrapStore) Create(_ context.Context, u *auth.User) error {
	u.ID = uuid.New()
	u.Principal = "user:" + u.Username
	f.created = u
	return nil
}

func (f *fakeBootstrapStore) UpdatePassword(_ context.Context, id uuid.UUID, hash string) error {
	f.updatedID = id
	f.updatedPwd = hash
	return nil
}

func TestReconcileBootstrapAdmin(t *testing.T) {
	ctx := context.Background()
	cfg := apiserver.Config{
		BootstrapAdminUsername: "admin",
		BootstrapAdminPassword: "correct horse battery staple",
	}

	t.Run("fresh install creates the admin", func(t *testing.T) {
		s := &fakeBootstrapStore{}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created == nil {
			t.Fatal("expected the admin to be created on a fresh install")
		}
		if s.created.GlobalRole != auth.RoleAdmin {
			t.Fatalf("created user role = %q, want admin", s.created.GlobalRole)
		}
		if s.updatedPwd != "" {
			t.Fatal("did not expect an UpdatePassword on a fresh install")
		}
	})

	t.Run("matching password is a no-op", func(t *testing.T) {
		hash, err := auth.HashPassword(cfg.BootstrapAdminPassword)
		if err != nil {
			t.Fatal(err)
		}
		s := &fakeBootstrapStore{user: &auth.User{
			ID: uuid.New(), Username: "admin", PasswordHash: hash, GlobalRole: auth.RoleAdmin,
		}}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created != nil {
			t.Fatal("did not expect a Create when the admin already exists")
		}
		if s.updatedPwd != "" {
			t.Fatal("did not expect an UpdatePassword when the stored hash already matches")
		}
	})

	t.Run("diverged password is reconciled (the ISI-4232 upgrade case)", func(t *testing.T) {
		staleHash, err := auth.HashPassword("some other password from a prior install")
		if err != nil {
			t.Fatal(err)
		}
		existing := &auth.User{
			ID: uuid.New(), Username: "admin", PasswordHash: staleHash, GlobalRole: auth.RoleAdmin,
		}
		s := &fakeBootstrapStore{user: existing}
		reconcileBootstrapAdmin(ctx, s, cfg)
		if s.created != nil {
			t.Fatal("did not expect a Create when the admin already exists")
		}
		if s.updatedID != existing.ID {
			t.Fatalf("UpdatePassword id = %v, want %v", s.updatedID, existing.ID)
		}
		// The reconciled hash must verify against the configured password so the
		// documented login works again.
		if err := auth.VerifyPassword(cfg.BootstrapAdminPassword, s.updatedPwd); err != nil {
			t.Fatalf("reconciled hash does not verify against the configured password: %v", err)
		}
	})
}

// stubOrgReader satisfies apiserver.OrgReader with just enough to drive the mention roster seam.
// nsAgents (keyed by namespace) and fleet back the ISI-5107 project/fleet scopes; both fall back to
// `agents` when unset so the pre-5107 delegation test keeps its single-source behaviour.
type stubOrgReader struct {
	agents   []apiserver.OrgAgent
	nsAgents map[string][]apiserver.OrgAgent
	fleet    []apiserver.OrgAgent
}

func (s stubOrgReader) Org(_ context.Context, teamUID string) (apiserver.TeamOrg, error) {
	return apiserver.TeamOrg{TeamID: teamUID, Agents: s.agents}, nil
}
func (stubOrgReader) Agent(context.Context, string, string, bool) (apiserver.OrgAgent, error) {
	return apiserver.OrgAgent{}, nil
}
func (stubOrgReader) AgentRuns(context.Context, string, string, int, int, bool) ([]apiserver.RunSummary, error) {
	return nil, nil
}
func (stubOrgReader) AgentStatuses(context.Context, string) ([]apiserver.AgentStatusDelta, error) {
	return nil, nil
}
func (s stubOrgReader) NamespaceAgents(_ context.Context, namespace string) ([]apiserver.OrgAgent, error) {
	if s.nsAgents != nil {
		return s.nsAgents[namespace], nil
	}
	return s.agents, nil
}
func (s stubOrgReader) AllAgents(context.Context) ([]apiserver.OrgAgent, error) {
	if s.fleet != nil {
		return s.fleet, nil
	}
	return s.agents, nil
}

// TestRosterForMentionsNilOrg is the ISI-4926 regression: on the cache-less boot shape org is a nil
// apiserver.OrgReader, and wrapping it in a non-nil mentionRoster would pass the handler's
// `h.org != nil` guard and then panic inside TeamAgents. rosterForMentions must collapse a nil org
// to a nil discussion.OrgReader so the /mentions route degrades to ticket-only instead.
func TestRosterForMentionsNilOrg(t *testing.T) {
	if got := rosterForMentions(nil); got != nil {
		t.Fatalf("rosterForMentions(nil) = %#v, want nil discussion.OrgReader", got)
	}
}

// TestDiscussionRosterAndMentionAdminCrossSquad is the ISI-5107 end-to-end regression: it wires the
// REAL apiserver org read model (a fake k8s cache with two squads) through the mentionRoster adapter
// into the discussion handler, and proves an admin viewing ANOTHER squad's project resolves that
// squad's agents in BOTH the right-rail roster and the @-mention popover — while a non-admin stays
// fenced to their own team (existence-hiding). This is the exact bug Henrik reported (empty roster,
// unresolved @john) with john = a Product Manager agent living in a different squad namespace.
func TestDiscussionRosterAndMentionAdminCrossSquad(t *testing.T) {
	const (
		teamAUID = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
		teamBUID = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
	)
	// Two co-tenant squads (home namespace == exec namespace), each a Team + one agent. `john` lives
	// in squad-b, the OTHER squad from squad-a — the cross-team case the fence used to swallow.
	scheme := runtime.NewScheme()
	if err := ksquadv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	mkTeam := func(name, uid string) *ksquadv1.Team {
		return &ksquadv1.Team{
			ObjectMeta: metav1.ObjectMeta{Namespace: name, Name: name, UID: types.UID(uid)},
			Status:     ksquadv1.TeamStatus{Namespace: name},
		}
	}
	mkAgent := func(ns, name string) *ksquadv1.Agent {
		return &ksquadv1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID("ag-" + name)}}
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		mkTeam("squad-a", teamAUID), mkTeam("squad-b", teamBUID),
		mkAgent("squad-a", "alice"), mkAgent("squad-b", "john"),
	).Build()

	roster := rosterForMentions(apiserver.NewClientOrgReader(c))
	if roster == nil {
		t.Fatal("rosterForMentions(org) = nil, want a wired seam")
	}
	h := discussion.NewHandlerWithDeps(nil, nil, roster)
	router := mux.NewRouter()
	router.UseEncodedPath() // production captures "namespace%2Fname" as one {projectId} segment
	h.Register(router.PathPrefix("/api/projects/{projectId}/discussion").Subrouter())

	// squad-b's project, addressed by its "namespace/name" slug (percent-encoded on the wire).
	const projectB = "squad-b%2Ftodo-demo"
	admin := discussion.AuthorContext{Principal: "user:root", IsAdmin: true}
	memberA := discussion.AuthorContext{Principal: "user:alice", TeamID: uuid.MustParse(teamAUID)}

	get := func(path string, a discussion.AuthorContext) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req = req.WithContext(discussion.WithAuth(req.Context(), a))
		router.ServeHTTP(rec, req)
		return rec
	}
	rosterNames := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("roster status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		var got []discussion.RosterAgent
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode roster: %v", err)
		}
		names := make([]string, 0, len(got))
		for _, a := range got {
			names = append(names, a.Name)
		}
		return names
	}
	mentionNames := func(rec *httptest.ResponseRecorder) []string {
		t.Helper()
		if rec.Code != http.StatusOK {
			t.Fatalf("mentions status = %d, want 200; body %s", rec.Code, rec.Body.String())
		}
		var got discussion.MentionSearchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode mentions: %v", err)
		}
		names := make([]string, 0, len(got.Results))
		for _, r := range got.Results {
			if r.Type == "agent" {
				names = append(names, r.ID)
			}
		}
		return names
	}
	has := func(names []string, want string) bool {
		for _, n := range names {
			if n == want {
				return true
			}
		}
		return false
	}

	// (1) Admin roster on squad-b's project → squad-b's agent (john), NOT squad-a's alice.
	if names := rosterNames(get("/api/projects/"+projectB+"/discussion/roster", admin)); !has(names, "john") || has(names, "alice") {
		t.Fatalf("admin squad-b roster = %v, want [john] (the project's own squad, not squad-a)", names)
	}
	// (2) Admin @-mention resolves fleet-wide: @john (in squad-b) resolves…
	if names := mentionNames(get("/api/projects/"+projectB+"/discussion/mentions?q=john", admin)); !has(names, "john") {
		t.Fatalf("admin @john = %v, want it to resolve the cross-squad PM agent", names)
	}
	// …and so does @alice (squad-a) — a global admin can pull an agent from ANY squad.
	if names := mentionNames(get("/api/projects/"+projectB+"/discussion/mentions?q=alice", admin)); !has(names, "alice") {
		t.Fatalf("admin @alice = %v, want the fleet-wide reach ADR-039 grants", names)
	}
	// (3) Existence-hiding: a squad-a member viewing squad-b's project sees only their OWN team's
	//     agents — never squad-b's john — in both roster and mention.
	if names := rosterNames(get("/api/projects/"+projectB+"/discussion/roster", memberA)); has(names, "john") || !has(names, "alice") {
		t.Fatalf("non-admin squad-b roster = %v, want fenced to squad-a [alice]", names)
	}
	if names := mentionNames(get("/api/projects/"+projectB+"/discussion/mentions?q=john", memberA)); has(names, "john") {
		t.Fatalf("non-admin @john = %v, want empty (existence-hiding — never another team's agent)", names)
	}
}

func TestRosterForMentionsDelegates(t *testing.T) {
	org := stubOrgReader{agents: []apiserver.OrgAgent{
		{ID: "a-1", Name: "Robo-Coder", Status: "working"},
	}}
	roster := rosterForMentions(org)
	if roster == nil {
		t.Fatal("rosterForMentions(org) = nil, want a wired seam")
	}
	agents, err := roster.TeamAgents(context.Background(), uuid.New())
	if err != nil {
		t.Fatalf("TeamAgents: %v", err)
	}
	if len(agents) != 1 || agents[0].Name != "Robo-Coder" || agents[0].Status != "working" {
		t.Fatalf("TeamAgents = %+v, want the delegated Robo-Coder (working)", agents)
	}
}
