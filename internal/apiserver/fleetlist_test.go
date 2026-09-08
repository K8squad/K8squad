package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// --- object builders (reuse team()/overviewScheme() from overview_test.go) -----------------------

func teamWithMembers(ns, name, uid string, agents, projects []string) *ksquadv1.Team {
	t := team(ns, name, uid)
	for _, a := range agents {
		t.Spec.Agents = append(t.Spec.Agents, ksquadv1.ObjectRef{Name: a})
	}
	for _, p := range projects {
		t.Spec.Projects = append(t.Spec.Projects, ksquadv1.ObjectRef{Name: p})
	}
	return t
}

func agentObj(ns, name, uid, runtime, role, model string, skills ...string) *ksquadv1.Agent {
	a := &ksquadv1.Agent{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: ksquadv1.AgentSpec{
			RuntimeRef: ksquadv1.ObjectRef{Name: runtime},
			RoleRef:    ksquadv1.ObjectRef{Name: role},
			Model:      model,
		},
	}
	for _, s := range skills {
		a.Spec.SkillRefs = append(a.Spec.SkillRefs, ksquadv1.ObjectRef{Name: s})
	}
	return a
}

func skillObj(ns, name, uid string, source ksquadv1.SkillSourceType, perms ...string) *ksquadv1.Skill {
	return &ksquadv1.Skill{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: ksquadv1.SkillSpec{
			Source:      ksquadv1.SkillSource{Type: source},
			Permissions: perms,
		},
	}
}

func roleObj(ns, name, uid, prompt, hint string, defaultSkills ...string) *ksquadv1.Role {
	ro := &ksquadv1.Role{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: ksquadv1.RoleSpec{
			PromptRef:        ksquadv1.ObjectRef{Name: prompt},
			RuntimeClassHint: hint,
		},
	}
	for _, s := range defaultSkills {
		ro.Spec.DefaultSkills = append(ro.Spec.DefaultSkills, ksquadv1.ObjectRef{Name: s})
	}
	return ro
}

func newFleetReader(t *testing.T, objs ...client.Object) *ClientFleetListReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientFleetListReader(c)
}

// Two-squad fleet fixture: squad-a (team alpha) and squad-b (team beta), each with one agent,
// one skill and one role in its own namespace.
func twoSquadObjs(uidA, uidB string) []client.Object {
	return []client.Object{
		teamWithMembers("squad-a", "alpha", uidA, []string{"agent-a"}, []string{"web"}),
		teamWithMembers("squad-b", "beta", uidB, []string{"agent-b"}, []string{"api", "cli"}),
		agentObj("squad-a", "agent-a", "ag-a", "claude", "dev", "claude-opus", "sk-a"),
		agentObj("squad-b", "agent-b", "ag-b", "codex", "qa", "gpt-5"),
		skillObj("squad-a", "sk-a", "s-a", ksquadv1.SkillSourceInline, "fs:read"),
		skillObj("squad-b", "sk-b", "s-b", ksquadv1.SkillSourceGit, "net:egress", "fs:write"),
		roleObj("squad-a", "dev", "r-a", "dev-prompt", "gpu", "sk-a"),
		roleObj("squad-b", "qa", "r-b", "qa-prompt", ""),
	}
}

const (
	fleetUIDA = "aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	fleetUIDB = "bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

// --- reader: admin fleet-wide vs tenant-scoped ---------------------------------------------------

func TestFleetTeamsAdminSpansEverySquad(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	list, err := r.Teams(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Teams(admin): %v", err)
	}
	if !list.Fleet {
		t.Fatalf("admin list must set Fleet=true: %+v", list)
	}
	if len(list.Teams) != 2 {
		t.Fatalf("admin teams: got %d, want 2", len(list.Teams))
	}
	// Deterministic (namespace, name): squad-a/alpha before squad-b/beta.
	if list.Teams[0].Name != "alpha" || list.Teams[1].Name != "beta" {
		t.Fatalf("team order: %+v", list.Teams)
	}
	if list.Teams[0].AgentCount != 1 || list.Teams[1].ProjectCount != 2 {
		t.Fatalf("membership counts wrong: %+v", list.Teams)
	}
}

func TestFleetTeamsTenantSeesOnlyOwn(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	list, err := r.Teams(context.Background(), fleetUIDA, false)
	if err != nil {
		t.Fatalf("Teams(tenant): %v", err)
	}
	if list.Fleet {
		t.Fatalf("tenant list must not set Fleet")
	}
	if len(list.Teams) != 1 || list.Teams[0].Name != "alpha" {
		t.Fatalf("tenant must see only own team: %+v", list.Teams)
	}
}

func TestFleetTeamsTenantUnknownUIDNotFound(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	if _, err := r.Teams(context.Background(), "no-such-uid", false); err == nil {
		t.Fatalf("tenant with unknown team UID must error ErrTeamNotFound")
	}
}

func TestFleetAgentsScoping(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)

	admin, err := r.Agents(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Agents(admin): %v", err)
	}
	if !admin.Fleet || len(admin.Agents) != 2 {
		t.Fatalf("admin agents: %+v", admin)
	}
	if admin.Agents[0].ID != "ag-a" || admin.Agents[0].Runtime != "claude" ||
		admin.Agents[0].Role != "dev" || admin.Agents[0].Model != "claude-opus" || admin.Agents[0].SkillCount != 1 {
		t.Fatalf("agent-a projection: %+v", admin.Agents[0])
	}

	tenant, err := r.Agents(context.Background(), fleetUIDB, false)
	if err != nil {
		t.Fatalf("Agents(tenant): %v", err)
	}
	if len(tenant.Agents) != 1 || tenant.Agents[0].Namespace != "squad-b" {
		t.Fatalf("tenant B must see only squad-b agents: %+v", tenant.Agents)
	}
}

func TestFleetSkillsScoping(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)

	admin, err := r.Skills(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Skills(admin): %v", err)
	}
	if len(admin.Skills) != 2 {
		t.Fatalf("admin skills: got %d, want 2", len(admin.Skills))
	}
	if admin.Skills[0].SourceType != string(ksquadv1.SkillSourceInline) ||
		len(admin.Skills[0].Permissions) != 1 {
		t.Fatalf("sk-a projection: %+v", admin.Skills[0])
	}

	tenant, err := r.Skills(context.Background(), fleetUIDA, false)
	if err != nil {
		t.Fatalf("Skills(tenant): %v", err)
	}
	if len(tenant.Skills) != 1 || tenant.Skills[0].Namespace != "squad-a" {
		t.Fatalf("tenant A must see only squad-a skills: %+v", tenant.Skills)
	}
}

// gitSkillObj builds a git-sourced Skill with full detail (source coordinates, MCP
// tool refs, permissions, and toolchain/sidecar requirements) so the single-skill
// view projection (ISI-3961 AC4) can be asserted end to end.
func gitSkillObj(ns, name, uid string) *ksquadv1.Skill {
	return &ksquadv1.Skill{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name, UID: types.UID(uid)},
		Spec: ksquadv1.SkillSpec{
			Source: ksquadv1.SkillSource{
				Type: ksquadv1.SkillSourceGit,
				Git: &ksquadv1.GitSkillSource{
					RepoRef: "github.com/acme/squad-skills",
					Ref:     "deadbeef",
					Path:    "skills/pg-migrate",
				},
			},
			McpToolRefs: []ksquadv1.ObjectRef{{Name: "pg"}, {Name: "fs"}},
			Permissions: []string{"net:egress", "fs:write"},
			Requires: ksquadv1.SkillRequires{
				Toolchains: []string{"go@1.23"},
				Sidecars:   []string{"dockerd"},
			},
		},
	}
}

func TestFleetSkillsListEnrichment(t *testing.T) {
	// The list carries owning Team UID and, for a git skill, the provenance coords
	// (ISI-3961 AC1) additively over the ISI-3963 envelope.
	objs := append(twoSquadObjs(fleetUIDA, fleetUIDB), gitSkillObj("squad-a", "pg-migrate", "s-git"))
	r := newFleetReader(t, objs...)
	list, err := r.Skills(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Skills(admin): %v", err)
	}
	var git *SkillListEntry
	for i := range list.Skills {
		if list.Skills[i].Name == "pg-migrate" {
			git = &list.Skills[i]
		}
	}
	if git == nil {
		t.Fatalf("pg-migrate not listed: %+v", list.Skills)
	}
	if git.TeamUID != fleetUIDA {
		t.Fatalf("owning team UID: got %q want %q", git.TeamUID, fleetUIDA)
	}
	if git.SourceType != string(ksquadv1.SkillSourceGit) ||
		git.SourceRepoRef != "github.com/acme/squad-skills" || git.SourceRef != "deadbeef" || git.SourcePath != "skills/pg-migrate" {
		t.Fatalf("git provenance not projected: %+v", git)
	}
}

func TestFleetSkillViewAdminAnySquad(t *testing.T) {
	r := newFleetReader(t, append(twoSquadObjs(fleetUIDA, fleetUIDB), gitSkillObj("squad-a", "pg-migrate", "s-git"))...)
	v, err := r.Skill(context.Background(), "", "pg-migrate", true)
	if err != nil {
		t.Fatalf("Skill(admin): %v", err)
	}
	if v.Name != "pg-migrate" || v.Namespace != "squad-a" || v.TeamUID != fleetUIDA {
		t.Fatalf("identity: %+v", v)
	}
	if v.Source.Type != "git" || v.Source.RepoRef != "github.com/acme/squad-skills" || v.Source.Ref != "deadbeef" || v.Source.Path != "skills/pg-migrate" {
		t.Fatalf("source projection: %+v", v.Source)
	}
	if len(v.McpToolRefs) != 2 || len(v.Permissions) != 2 {
		t.Fatalf("refs/permissions: %+v", v)
	}
	if len(v.Requires.Toolchains) != 1 || v.Requires.Toolchains[0] != "go@1.23" || len(v.Requires.Sidecars) != 1 {
		t.Fatalf("requires projection: %+v", v.Requires)
	}
}

func TestFleetSkillViewTenantOwnOnly(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	// Tenant A sees its own skill.
	if _, err := r.Skill(context.Background(), fleetUIDA, "sk-a", false); err != nil {
		t.Fatalf("tenant A own skill: %v", err)
	}
	// Tenant A must NOT see squad-b's skill — existence-hiding, not a 403.
	if _, err := r.Skill(context.Background(), fleetUIDA, "sk-b", false); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("foreign skill: got %v, want ErrSkillNotFound", err)
	}
}

func TestFleetSkillViewNotFound(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	if _, err := r.Skill(context.Background(), "", "no-such", true); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("absent skill (admin): got %v, want ErrSkillNotFound", err)
	}
	// A tenant whose UID resolves to no Team is existence-hiding too.
	if _, err := r.Skill(context.Background(), "unknown-uid", "sk-a", false); !errors.Is(err, ErrSkillNotFound) {
		t.Fatalf("unresolved tenant: got %v, want ErrSkillNotFound", err)
	}
}

func TestFleetSkillViewHandler(t *testing.T) {
	tenantID := uuid.MustParse(fleetUIDA)
	reader := newFleetReader(t, append(twoSquadObjs(fleetUIDA, fleetUIDB), gitSkillObj("squad-a", "pg-migrate", "s-git"))...)
	h := testFleetServer(t, tenantID, false, reader)

	// 200 own skill.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/skills/pg-migrate", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("own skill: got %d want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var v SkillView
	if err := json.Unmarshal(rec.Body.Bytes(), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Name != "pg-migrate" || v.Source.Ref != "deadbeef" {
		t.Fatalf("view body: %+v", v)
	}

	// 404 foreign skill (squad-b) — existence-hiding.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/skills/sk-b", nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign skill: got %d want 404", rec.Code)
	}
}

func TestFleetSkillViewHandlerUnauthenticated(t *testing.T) {
	adminID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	reader := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	h := testFleetServer(t, adminID, true, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/squad/skills/sk-a", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

func TestFleetSkillViewNilReaderStill501(t *testing.T) {
	adminID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	h := testFleetServer(t, adminID, true, nil)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/skills/sk-a", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil reader: got %d, want 501", rec.Code)
	}
}

func TestFleetRolesScoping(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)

	admin, err := r.Roles(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Roles(admin): %v", err)
	}
	if len(admin.Roles) != 2 {
		t.Fatalf("admin roles: got %d, want 2", len(admin.Roles))
	}
	if admin.Roles[0].Prompt != "dev-prompt" || admin.Roles[0].RuntimeClassHint != "gpu" ||
		len(admin.Roles[0].DefaultSkills) != 1 {
		t.Fatalf("dev role projection: %+v", admin.Roles[0])
	}

	tenant, err := r.Roles(context.Background(), fleetUIDB, false)
	if err != nil {
		t.Fatalf("Roles(tenant): %v", err)
	}
	if len(tenant.Roles) != 1 || tenant.Roles[0].Name != "qa" {
		t.Fatalf("tenant B must see only squad-b roles: %+v", tenant.Roles)
	}
	// DefaultSkills must be [] never nil on the wire.
	if tenant.Roles[0].DefaultSkills == nil {
		t.Fatalf("DefaultSkills must be non-nil (empty slice)")
	}
}

// --- reader: team detail existence-hiding --------------------------------------------------------

func TestFleetTeamDetailAdminAnyTeam(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	// Admin (empty home tenancy) reads squad-b's team by UID.
	d, err := r.Team(context.Background(), "", fleetUIDB, true)
	if err != nil {
		t.Fatalf("Team(admin, B): %v", err)
	}
	if d.Name != "beta" || d.Namespace != "squad-b" || d.ProjectCount != 2 {
		t.Fatalf("team detail: %+v", d)
	}
	if len(d.Projects) != 2 || d.Projects[0] != "api" || d.Projects[1] != "cli" {
		t.Fatalf("projects (sorted): %+v", d.Projects)
	}
}

func TestFleetTeamDetailTenantForeignHidden(t *testing.T) {
	r := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	// Tenant A asking for team B's UID is existence-hiding ErrTeamNotFound (never a 403 leak).
	if _, err := r.Team(context.Background(), fleetUIDA, fleetUIDB, false); err == nil {
		t.Fatalf("tenant reading foreign team must be ErrTeamNotFound")
	}
	// Tenant A reading their own team succeeds.
	d, err := r.Team(context.Background(), fleetUIDA, fleetUIDA, false)
	if err != nil {
		t.Fatalf("Team(tenant, own): %v", err)
	}
	if d.Name != "alpha" {
		t.Fatalf("own team detail: %+v", d)
	}
}

// --- handler wiring ------------------------------------------------------------------------------

func testFleetServer(t *testing.T, teamID uuid.UUID, admin bool, reader FleetListReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID, IsAdmin: admin},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		FleetList:     reader,
	})
	return srv.Handler()
}

func TestFleetHandlersOK(t *testing.T) {
	adminID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	reader := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	h := testFleetServer(t, adminID, true, reader)

	for _, tc := range []struct {
		path  string
		count func([]byte) int
	}{
		{"/api/squad/teams", func(b []byte) int {
			var l FleetTeamList
			_ = json.Unmarshal(b, &l)
			return len(l.Teams)
		}},
		{"/api/squad/agents", func(b []byte) int {
			var l FleetAgentList
			_ = json.Unmarshal(b, &l)
			return len(l.Agents)
		}},
		{"/api/squad/skills", func(b []byte) int {
			var l FleetSkillList
			_ = json.Unmarshal(b, &l)
			return len(l.Skills)
		}},
		{"/api/squad/roles", func(b []byte) int {
			var l FleetRoleList
			_ = json.Unmarshal(b, &l)
			return len(l.Roles)
		}},
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, tc.path, nil), devToken))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: got %d, want 200 (body %s)", tc.path, rec.Code, rec.Body.String())
		}
		if got := tc.count(rec.Body.Bytes()); got != 2 {
			t.Fatalf("%s: admin must see 2 fleet rows, got %d", tc.path, got)
		}
	}
}

func TestFleetTeamDetailHandlerOK(t *testing.T) {
	adminID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	reader := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	h := testFleetServer(t, adminID, true, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/teams/"+fleetUIDA, nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("team detail: got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	var d TeamDetail
	if err := json.Unmarshal(rec.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if d.Name != "alpha" || d.UID != fleetUIDA {
		t.Fatalf("team detail body: %+v", d)
	}
}

func TestFleetTeamDetailHandlerNotFound(t *testing.T) {
	tenantID := uuid.MustParse(fleetUIDA)
	reader := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	h := testFleetServer(t, tenantID, false, reader)

	// Tenant A asking for team B ⇒ existence-hiding 404.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/squad/teams/"+fleetUIDB, nil), devToken))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("foreign team detail: got %d, want 404", rec.Code)
	}
}

func TestFleetHandlersUnauthenticated(t *testing.T) {
	adminID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	reader := newFleetReader(t, twoSquadObjs(fleetUIDA, fleetUIDB)...)
	h := testFleetServer(t, adminID, true, reader)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/squad/agents", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}

func TestFleetHandlersNilReaderStill501(t *testing.T) {
	adminID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	h := testFleetServer(t, adminID, true, nil)

	for _, path := range []string{
		"/api/squad/teams", "/api/squad/teams/" + fleetUIDA,
		"/api/squad/agents", "/api/squad/skills", "/api/squad/roles",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, path, nil), devToken))
		if rec.Code != http.StatusNotImplemented {
			t.Fatalf("%s (nil reader): got %d, want 501", path, rec.Code)
		}
	}
}
