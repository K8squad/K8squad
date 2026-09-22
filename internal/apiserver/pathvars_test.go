package apiserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

// TestDecodePathVar — the reversal is an identity for plain values (UUIDs,
// numbers, DNS labels) and unescapes a percent-encoded composite; a malformed
// escape degrades to the raw value rather than dropping the var.
func TestDecodePathVar(t *testing.T) {
	cases := map[string]string{
		"bmad-squad%2Fbmad-demo-project":       "bmad-squad/bmad-demo-project",         // the reported Project composite
		"web":                                  "web",                                  // bare name — no escapes, identity
		"550e8400-e29b-41d4-a716-446655440000": "550e8400-e29b-41d4-a716-446655440000", // UUID
		"42":                                   "42",                                   // issue number
		"a%2":                                  "a%2",                                  // malformed escape → raw fallback, not dropped
		"ns%2Fp%2Fq":                           "ns/p/q",                               // defensive: multiple encoded slashes
	}
	for in, want := range cases {
		if got := decodePathVar(in); got != want {
			t.Errorf("decodePathVar(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestComposeProjectIDRoutesThroughEncodedSlash is the ISI-4795 regression guard:
// a Project addressed by its canonical "namespace/name" composite id, percent-
// encoded to one path segment ("squad-a%2Fweb"), must ROUTE to the project handler
// and resolve — not 404 at the router because mux collapsed "%2F" into an extra
// path segment. This exercises the real NewServer wiring (UseEncodedPath()+
// SkipClean(true)) plus the handler-side decodePathVar, end to end. The bare-name
// form must keep working too (no regression for the SquadOverview/FleetOverview
// links that already used it).
func TestComposeProjectIDRoutesThroughEncodedSlash(t *testing.T) {
	teamID := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	reader := newDashboardClient(t,
		team("squad-a", "alpha", teamID.String()),
		project("squad-a", "web", "https://github.com/acme/web"),
	)
	h := testDashboardServer(t, teamID, reader,
		&fakeTicketSource{}, &fakePRSource{}, &fakeMetricsSource{})

	for _, projectID := range []string{
		"web",           // bare name (canonical accepted form today)
		"squad-a%2Fweb", // "namespace/name" composite, single-encoded — the reported 404
	} {
		rec := httptest.NewRecorder()
		req := withSession(httptest.NewRequest(http.MethodGet, "/api/projects/"+projectID+"/dashboard", nil), devToken)
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET /api/projects/%s/dashboard = %d, want 200 (body %s)", projectID, rec.Code, rec.Body.String())
		}
	}
}
