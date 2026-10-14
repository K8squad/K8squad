package apiserver

// credentialtypes_test.go — WS-9 S1 (ISI-5811): the per-project credential-type
// intersection read model behind GET /api/projects/{projectId}/credential-types.
//
// Covers the acceptance:
//   - intersection: only types declared by an enabled gated skill/MCPServer AND
//     mapped by the binding registry are returned;
//   - fail-closed: an annotation naming an unknown type is dropped, never surfaced;
//   - consumedBy aggregates the Skill + MCPServer names declaring a type;
//   - gate-state: unsatisfied with no oracle; satisfied when the presence oracle
//     reports the type materialized;
//   - tenancy: a tenant reads only their own project (404 existence-hiding), an
//     admin reads a foreign-namespace project fleet-wide;
//   - no secret value / no secret name ever crosses the boundary;
//   - cluster-less nil service keeps the documented 501.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/toolcred"
)

// --- fixtures -----------------------------------------------------------------

// gatedSkill builds a Skill that declares a credential type via the S6
// annotation. credType "" ⇒ no annotation (an ungated skill).
func gatedSkill(ns, name, credType string) *ksquadv1.Skill {
	s := &ksquadv1.Skill{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec:       ksquadv1.SkillSpec{Source: ksquadv1.SkillSource{Type: ksquadv1.SkillSourceInline, Inline: "body"}},
	}
	if credType != "" {
		s.Annotations = map[string]string{toolcred.AnnotationCredentialType: credType}
	}
	return s
}

// gatedServer builds an MCPServer that declares a credential type.
func gatedServer(ns, name, credType string) *ksquadv1.MCPServer {
	m := &ksquadv1.MCPServer{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
	if credType != "" {
		m.Annotations = map[string]string{toolcred.AnnotationCredentialType: credType}
	}
	return m
}

// fakePresence is a static gate-state oracle for the satisfied-path test.
type fakePresence struct {
	present map[string]bool
	err     error
}

func (f fakePresence) PresentTypes(_ context.Context, _, _ string) (map[string]bool, error) {
	return f.present, f.err
}

// testCredTypesServer mounts the credential-types route for the non-admin
// devToken caller scoped to teamID, with an optional presence oracle.
func testCredTypesServer(t *testing.T, teamID uuid.UUID, admin bool, reader client.Reader, presence CredentialPresence) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID, IsAdmin: admin},
	}}
	srv := NewServer(Options{
		Authenticator:   NewCookieAuthenticator(resolver),
		Discussion:      discussion.NewHandler(nil),
		CredentialTypes: NewCredentialTypesService(reader, presence),
	})
	return srv.Handler()
}

func getCredTypes(t *testing.T, h http.Handler, token, projectID string) (*httptest.ResponseRecorder, string, *CredentialTypesResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/credential-types", nil), token))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		return rec, body, nil
	}
	var out CredentialTypesResponse
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("decode credential-types: %v (body %s)", err, body)
	}
	return rec, body, &out
}

func (r *CredentialTypesResponse) byType(typ string) *CredentialTypeEntry {
	for i := range r.Types {
		if r.Types[i].Type == typ {
			return &r.Types[i]
		}
	}
	return nil
}

// --- intersection + fail-closed ----------------------------------------------

// TestCredTypesIntersectionAndFailClosed — the project enables: a github skill,
// a dynatrace MCPServer, an ungated skill (no annotation), and a skill naming an
// UNKNOWN type. Only github + dynatrace survive (the intersection); the unknown
// type fails closed and the ungated skill contributes nothing.
func TestCredTypesIntersectionAndFailClosed(t *testing.T) {
	teamID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		settingsProject("squad-a", "web", "https://x/y", "", "", nil),
		gatedSkill("squad-a", "gh-review", "github"),
		gatedSkill("squad-a", "plain", ""),            // ungated → ignored
		gatedSkill("squad-a", "bogus", "not-a-type"),  // unknown → fail closed
		gatedServer("squad-a", "dt-mcp", "dynatrace"), // MCPServer declares a type
	)
	h := testCredTypesServer(t, teamID, false, reader, nil)

	rec, body, resp := getCredTypes(t, h, devToken, "web")
	if resp == nil {
		t.Fatalf("want 200, got %d (%s)", rec.Code, body)
	}
	if len(resp.Types) != 2 {
		t.Fatalf("want 2 intersection types (github, dynatrace), got %d: %+v", len(resp.Types), resp.Types)
	}
	if resp.byType("github") == nil || resp.byType("dynatrace") == nil {
		t.Fatalf("missing expected types: %+v", resp.Types)
	}
	if resp.byType("not-a-type") != nil {
		t.Fatal("unknown type leaked into the projection (fail-closed violated)")
	}
	// Binding metadata is carried through from the registry.
	gh := resp.byType("github")
	if gh.EnvVar != "GITHUB_TOKEN" || gh.Consumption != string(toolcred.ConsumptionToolchainEnv) || gh.DataKey != "token" {
		t.Errorf("github binding metadata: %+v", gh)
	}
	if !strings.EqualFold(gh.DisplayName, "GitHub") {
		t.Errorf("github displayName = %q", gh.DisplayName)
	}
	// consumedBy names the declaring CR.
	if len(gh.ConsumedBy) != 1 || gh.ConsumedBy[0] != "gh-review" {
		t.Errorf("github consumedBy = %v, want [gh-review]", gh.ConsumedBy)
	}
	// Deterministic wire order (sorted types): dynatrace before github.
	if resp.Types[0].Type != "dynatrace" || resp.Types[1].Type != "github" {
		t.Errorf("types not sorted: %q,%q", resp.Types[0].Type, resp.Types[1].Type)
	}
}

// TestCredTypesConsumedByAggregates — two CRs (a skill + an MCPServer) declaring
// the SAME type collapse to one row whose consumedBy lists both, sorted.
func TestCredTypesConsumedByAggregates(t *testing.T) {
	teamID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	reader := newDashboardClient(t,
		team("squad-b", "beta", teamID.String()),
		settingsProject("squad-b", "site", "https://x/y", "", "", nil),
		gatedSkill("squad-b", "pr-review", "github"),
		gatedServer("squad-b", "gh-mcp", "github"),
	)
	h := testCredTypesServer(t, teamID, false, reader, nil)

	_, _, resp := getCredTypes(t, h, devToken, "site")
	if resp == nil || len(resp.Types) != 1 {
		t.Fatalf("want 1 aggregated type, got %+v", resp)
	}
	got := resp.Types[0].ConsumedBy
	if len(got) != 2 || got[0] != "gh-mcp" || got[1] != "pr-review" {
		t.Fatalf("consumedBy = %v, want [gh-mcp pr-review] (sorted)", got)
	}
}

// --- gate-state ---------------------------------------------------------------

// TestCredTypesGateUnsatisfiedNoOracle — with no presence oracle every type is
// unsatisfied (honest default; never a fabricated green badge).
func TestCredTypesGateUnsatisfiedNoOracle(t *testing.T) {
	teamID := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	reader := newDashboardClient(t,
		team("squad-c", "gamma", teamID.String()),
		settingsProject("squad-c", "app", "https://x/y", "", "", nil),
		gatedSkill("squad-c", "gh", "github"),
	)
	h := testCredTypesServer(t, teamID, false, reader, nil)

	_, _, resp := getCredTypes(t, h, devToken, "app")
	if resp == nil || len(resp.Types) != 1 {
		t.Fatalf("want 1 type, got %+v", resp)
	}
	if resp.Types[0].GateState != CredentialGateUnsatisfied {
		t.Fatalf("gateState = %q, want unsatisfied", resp.Types[0].GateState)
	}
}

// TestCredTypesGateSatisfiedFromOracle — the presence oracle reports github
// materialized ⇒ that row is satisfied; a type the oracle omits stays unsatisfied.
func TestCredTypesGateSatisfiedFromOracle(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	reader := newDashboardClient(t,
		team("squad-d", "delta", teamID.String()),
		settingsProject("squad-d", "svc", "https://x/y", "", "", nil),
		gatedSkill("squad-d", "gh", "github"),
		gatedSkill("squad-d", "dt", "dynatrace"),
	)
	presence := fakePresence{present: map[string]bool{"github": true}}
	h := testCredTypesServer(t, teamID, false, reader, presence)

	_, _, resp := getCredTypes(t, h, devToken, "svc")
	if resp == nil {
		t.Fatal("want 200")
	}
	if gh := resp.byType("github"); gh == nil || gh.GateState != CredentialGateSatisfied {
		t.Fatalf("github gateState: %+v", gh)
	}
	if dt := resp.byType("dynatrace"); dt == nil || dt.GateState != CredentialGateUnsatisfied {
		t.Fatalf("dynatrace gateState: %+v", dt)
	}
}

// TestCredTypesOracleErrorIs502 — an oracle outage is a 502, never a silently
// empty "nothing satisfied".
func TestCredTypesOracleErrorIs502(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	reader := newDashboardClient(t,
		team("squad-e", "epsilon", teamID.String()),
		settingsProject("squad-e", "svc", "https://x/y", "", "", nil),
		gatedSkill("squad-e", "gh", "github"),
	)
	presence := fakePresence{err: context.DeadlineExceeded}
	h := testCredTypesServer(t, teamID, false, reader, presence)

	rec, _, _ := getCredTypes(t, h, devToken, "svc")
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502 on oracle error, got %d", rec.Code)
	}
}

// --- tenancy ------------------------------------------------------------------

// TestCredTypesCrossTenantHidden — a tenant may not read another squad's project;
// the miss is existence-hiding 404, exactly like settings/dashboard.
func TestCredTypesCrossTenantHidden(t *testing.T) {
	callerTeam := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	reader := newDashboardClient(t,
		team("squad-caller", "caller", callerTeam.String()),
		team("squad-other", "other", "77777777-7777-7777-7777-777777777777"),
		settingsProject("squad-other", "secret-proj", "https://x/y", "", "", nil),
		gatedSkill("squad-other", "gh", "github"),
	)
	h := testCredTypesServer(t, callerTeam, false, reader, nil)

	rec, _, _ := getCredTypes(t, h, devToken, "secret-proj")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("want 404 existence-hiding, got %d", rec.Code)
	}
}

// TestCredTypesAdminFleetWide — a global admin reads a project in a foreign
// namespace fleet-wide.
func TestCredTypesAdminFleetWide(t *testing.T) {
	adminTeam := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	reader := newDashboardClient(t,
		team("squad-x", "x", "99999999-9999-9999-9999-999999999999"),
		settingsProject("squad-x", "foreign", "https://x/y", "", "", nil),
		gatedSkill("squad-x", "gh", "github"),
	)
	h := testCredTypesServer(t, adminTeam, true, reader, nil)

	_, body, resp := getCredTypes(t, h, devToken, "foreign")
	if resp == nil {
		t.Fatalf("want 200 fleet-wide, body %s", body)
	}
	if resp.Project.Namespace != "squad-x" || resp.byType("github") == nil {
		t.Fatalf("admin fleet-wide projection: %+v", resp)
	}
}

// --- no-secret invariant + wire shape -----------------------------------------

// TestCredTypesNoSecretLeak — the body never carries a Secret value or the
// materialized Secret name (the name is derivable but is the write path's concern).
func TestCredTypesNoSecretLeak(t *testing.T) {
	teamID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	reader := newDashboardClient(t,
		team("squad-s", "s", teamID.String()),
		settingsProject("squad-s", "p", "https://x/y", "", "", nil),
		gatedSkill("squad-s", "gh", "github"),
	)
	h := testCredTypesServer(t, teamID, false, reader, nil)

	_, body, resp := getCredTypes(t, h, devToken, "p")
	if resp == nil {
		t.Fatal("want 200")
	}
	// The deterministic materialized name must NOT appear in the read body.
	name, _ := toolcred.MaterializedSecretName("p", "github")
	if strings.Contains(body, name) {
		t.Fatalf("materialized Secret name %q leaked into the read body", name)
	}
	if strings.Contains(strings.ToLower(body), "secret://") {
		t.Fatal("a secret ref leaked into the read body")
	}
}

// TestCredTypesEmptyWhenNoGatedSkills — a project with only ungated skills yields
// a non-nil empty types array ([] never null).
func TestCredTypesEmptyWhenNoGatedSkills(t *testing.T) {
	teamID := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	reader := newDashboardClient(t,
		team("squad-n", "n", teamID.String()),
		settingsProject("squad-n", "p", "https://x/y", "", "", nil),
		gatedSkill("squad-n", "plain", ""),
	)
	h := testCredTypesServer(t, teamID, false, reader, nil)

	_, body, resp := getCredTypes(t, h, devToken, "p")
	if resp == nil || len(resp.Types) != 0 {
		t.Fatalf("want empty types, got %+v", resp)
	}
	if !strings.Contains(body, `"types":[]`) {
		t.Fatalf("types must marshal to [] not null: %s", body)
	}
}

// --- 501 nil service ----------------------------------------------------------

// TestCredTypesNilServiceIs501 — a cluster-less dev run (nil service) keeps the
// documented 501.
func TestCredTypesNilServiceIs501(t *testing.T) {
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// CredentialTypes left nil.
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/projects/p/credential-types", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("want 501 for nil service, got %d", rec.Code)
	}
}
