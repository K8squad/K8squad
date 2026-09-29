package discussion

// Mention-search endpoint coverage (ISI-4926, plan §4.3): the GET /mentions route answers the
// composer's @-popover with agent + work-item suggestions. The unit lane pins the contract that
// makes the feature tenancy-safe WITHOUT a database:
//
//   - the FTS Query's RBAC scope is derived from the server-stamped AuthorContext (TeamID fenced,
//     AllTeams only for admins — ADR-039), never from request input;
//   - results are narrowed to the path's project;
//   - agents come from the roster seam, Team-scoped by construction, substring-matched;
//   - the deny paths: no auth ⇒ 401, missing/stopword q ⇒ 400, searcher failure ⇒ 502 — and the
//     graceful degrades: nil deps ⇒ empty 200, roster failure ⇒ ticket-only 200.
//
// The store is never touched by this route, so these tests ride a nil *Store through
// NewHandlerWithDeps exactly like the HTTP surface does.

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/pkg/search"
)

// fakeSearcher records the Query the handler derived and answers canned results.
type fakeSearcher struct {
	captured []search.Query
	results  []search.Result
	err      error
}

func (f *fakeSearcher) Search(_ context.Context, q search.Query) ([]search.Result, error) {
	f.captured = append(f.captured, q)
	return f.results, f.err
}

// fakeRoster is the OrgReader seam with canned agents. `agents` backs TeamAgents (the caller's own
// team); `projectAgents`/`allAgents` back the ISI-5107 admin scopes when set (they fall back to
// `agents` when nil so pre-5107 tests keep their single-source behaviour).
type fakeRoster struct {
	agents        []TeamAgent
	projectAgents []TeamAgent
	allAgents     []TeamAgent
	err           error
}

func (f *fakeRoster) TeamAgents(_ context.Context, _ uuid.UUID) ([]TeamAgent, error) {
	return f.agents, f.err
}

func (f *fakeRoster) ProjectAgents(_ context.Context, _ string) ([]TeamAgent, error) {
	if f.projectAgents != nil {
		return f.projectAgents, f.err
	}
	return f.agents, f.err
}

func (f *fakeRoster) AllAgents(_ context.Context) ([]TeamAgent, error) {
	if f.allAgents != nil {
		return f.allAgents, f.err
	}
	return f.agents, f.err
}

// mentionServer builds the discussion group with deps injected and no authz middleware — the
// unit lane stamps the AuthorContext directly, exactly like the sibling handler tests.
func mentionServer(t *testing.T, searcher search.Searcher, org OrgReader) (*Handler, *mux.Router) {
	t.Helper()
	h := NewHandlerWithDeps(nil, searcher, org)
	r := mux.NewRouter()
	h.Register(r.PathPrefix("/api/projects/{projectId}/discussion").Subrouter())
	return h, r
}

func TestSearchMentionsAgentsFirstProjectScoped(t *testing.T) {
	project := uuid.New()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	fs := &fakeSearcher{results: []search.Result{
		{Type: "work_item", ID: "11111111-1111-1111-1111-111111111111", ProjectID: project.String(), Title: "Fix the intake sweep", State: "todo", Rank: 0.9},
		{Type: "work_item", ID: "22222222-2222-2222-2222-222222222222", ProjectID: uuid.New().String(), Title: "Other project ticket", State: "todo", Rank: 0.8}, // different project — dropped
	}}
	roster := &fakeRoster{agents: []TeamAgent{
		{Name: "Robo-Coder", Status: "working"},
		{Name: "Reviewer", Status: "offline"},
	}}
	_, router := mentionServer(t, fs, roster)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q=rob", nil)
	req = req.WithContext(WithAuth(req.Context(), auth))
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got MentionSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Query != "rob" {
		t.Fatalf("query echo = %q, want rob", got.Query)
	}
	if len(got.Results) != 2 {
		t.Fatalf("results = %+v, want 2 (agent + same-project ticket)", got.Results)
	}
	// Agents lead the popover; the partial "rob" matched Robo-Coder (case-insensitive) but not Reviewer.
	if got.Results[0].Type != "agent" || got.Results[0].ID != "Robo-Coder" || got.Results[0].State != "working" {
		t.Fatalf("results[0] = %+v, want agent Robo-Coder (working)", got.Results[0])
	}
	if got.Results[1].Type != "work_item" || got.Results[1].ID != "11111111-1111-1111-1111-111111111111" {
		t.Fatalf("results[1] = %+v, want the same-project work item", got.Results[1])
	}

	// The tenancy fence rides the query: TeamID from the stamped context, AllTeams only for admins.
	if len(fs.captured) != 1 {
		t.Fatalf("searcher called %d times, want 1", len(fs.captured))
	}
	q := fs.captured[0]
	if q.TeamID != auth.TeamID.String() || q.AllTeams {
		t.Fatalf("search Query scope = {team %s, allTeams %v}, want {caller team, false}", q.TeamID, q.AllTeams)
	}
	if q.Text != "rob" || q.Limit != mentionWorkItemLimit {
		t.Fatalf("search Query = {text %q, limit %d}, want {rob, %d}", q.Text, q.Limit, mentionWorkItemLimit)
	}
}

func TestSearchMentionsAdminScopesFleetWide(t *testing.T) {
	project := uuid.New()
	auth := AuthorContext{Principal: "user:root", TeamID: uuid.New(), IsAdmin: true}
	fs := &fakeSearcher{}
	_, router := mentionServer(t, fs, nil)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q=anything", nil)
	req = req.WithContext(WithAuth(req.Context(), auth))
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	if len(fs.captured) != 1 || !fs.captured[0].AllTeams {
		t.Fatalf("admin scope: captured %+v, want AllTeams=true", fs.captured)
	}
}

func TestSearchMentionsDenyPaths(t *testing.T) {
	project := uuid.New()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}

	cases := []struct {
		name    string
		query   string
		auth    *AuthorContext
		searchr *fakeSearcher
		want    int
	}{
		{name: "unauthenticated", query: "rob", auth: nil, searchr: &fakeSearcher{}, want: http.StatusUnauthorized},
		{name: "missing q", query: "", auth: &auth, searchr: &fakeSearcher{}, want: http.StatusBadRequest},
		{name: "stopword-only q parses empty", query: "the", auth: &auth, searchr: &fakeSearcher{err: search.ErrEmptyQuery}, want: http.StatusBadRequest},
		{name: "searcher failure", query: "rob", auth: &auth, searchr: &fakeSearcher{err: errors.New("db down")}, want: http.StatusBadGateway},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, router := mentionServer(t, tc.searchr, nil)
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q="+tc.query, nil)
			if tc.auth != nil {
				req = req.WithContext(WithAuth(req.Context(), *tc.auth))
			}
			router.ServeHTTP(rec, req)
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d; body %s", rec.Code, tc.want, rec.Body.String())
			}
		})
	}
}

func TestSearchMentionsDegradedPaths(t *testing.T) {
	project := uuid.New()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}

	t.Run("nil deps answer an empty array, not null", func(t *testing.T) {
		_, router := mentionServer(t, nil, nil)

		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q=rob", nil)
		req = req.WithContext(WithAuth(req.Context(), auth))
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		if got := rec.Body.String(); !json.Valid([]byte(got)) || got == "" {
			t.Fatalf("body not valid JSON: %s", got)
		}
		var resp struct {
			Results []MentionSuggestion `json:"results"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if resp.Results == nil {
			t.Fatal("results = null, want [] (the composer renders an empty state without a nil guard)")
		}
	})

	t.Run("roster failure degrades to ticket-only", func(t *testing.T) {
		fs := &fakeSearcher{results: []search.Result{
			{Type: "work_item", ID: "11111111-1111-1111-1111-111111111111", ProjectID: project.String(), Title: "Ticket", State: "todo"},
		}}
		_, router := mentionServer(t, fs, &fakeRoster{err: errors.New("cache down")})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q=ticket", nil)
		req = req.WithContext(WithAuth(req.Context(), auth))
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 (roster is a projection, not the fence)", rec.Code)
		}
		var got MentionSearchResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if len(got.Results) != 1 || got.Results[0].Type != "work_item" {
			t.Fatalf("results = %+v, want the ticket-only degrade", got.Results)
		}
	})
}

// fakeProjectUIDResolver maps a room's {projectId} slug to a Project CR UID, mirroring the apiserver
// adapter over the shared project-ref resolver. ok=false when the slug is not in uids (unknown /
// uncomposed) so the handler falls back to the raw-path narrow.
type fakeProjectUIDResolver struct {
	uids map[string]string // slug → Project UID
	err  error
}

func (f *fakeProjectUIDResolver) ResolveProjectUID(_ context.Context, projectID string) (string, bool, error) {
	if f.err != nil {
		return "", false, f.err
	}
	uid, ok := f.uids[projectID]
	return uid, ok, nil
}

// TestSearchMentionsSlugKeyedProjectNarrow is the ISI-5213 regression: in production the {projectId}
// route carries a "namespace/name" slug, but the search read model returns each hit keyed by the
// Project CR UID (w.project_id::text). Before the fix the handler compared slug != UID and dropped
// EVERY row — the picker returned nothing for any query. With the slug→UID resolver wired, the
// in-project hit survives and a foreign-project hit is still dropped. It also pins that the picker
// derives a prefix (while-typing) search query.
func TestSearchMentionsSlugKeyedProjectNarrow(t *testing.T) {
	const slug = "bmad-squad/intake"
	projectUID := uuid.New().String()
	otherUID := uuid.New().String()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}

	fs := &fakeSearcher{results: []search.Result{
		{Type: "work_item", ID: "11111111-1111-1111-1111-111111111111", ProjectID: projectUID, Title: "Fix the intake sweep", State: "todo", Rank: 0.9},
		{Type: "work_item", ID: "22222222-2222-2222-2222-222222222222", ProjectID: otherUID, Title: "Other project ticket", State: "todo", Rank: 0.8}, // foreign project — dropped
	}}
	h, _ := mentionServer(t, fs, &fakeRoster{})
	h.SetProjectUIDResolver(&fakeProjectUIDResolver{uids: map[string]string{slug: projectUID}})

	rec := httptest.NewRecorder()
	// Set the {projectId} var directly (like TestPathUUID) to a "namespace/name" slug — the value the
	// production router recovers after UseEncodedPath()+unescape — without the unit router collapsing
	// the embedded '/' into two path segments.
	req := httptest.NewRequest(http.MethodGet, "/api/projects/x/discussion/mentions?q=intake", nil)
	req = mux.SetURLVars(req, map[string]string{"projectId": slug})
	req = req.WithContext(WithAuth(req.Context(), auth))
	h.searchMentions(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got MentionSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v, want exactly the in-project ticket (foreign UID dropped)", got.Results)
	}
	if got.Results[0].ID != "11111111-1111-1111-1111-111111111111" || got.Results[0].ProjectID != projectUID {
		t.Fatalf("results[0] = %+v, want the slug-keyed project's ticket", got.Results[0])
	}
	if len(fs.captured) != 1 || !fs.captured[0].Prefix {
		t.Fatalf("derived query = %+v, want a single prefix (while-typing) search", fs.captured)
	}
}

// TestSearchMentionsNarrowFallsBackWithoutResolver pins the degrade contract: with no slug→UID
// resolver wired (a DB-less dev host), the picker narrows against the raw {projectId} path value
// exactly as before — a UID-keyed dev project still matches when addressed by its UID.
func TestSearchMentionsNarrowFallsBackWithoutResolver(t *testing.T) {
	projectUID := uuid.New().String()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	fs := &fakeSearcher{results: []search.Result{
		{Type: "work_item", ID: "11111111-1111-1111-1111-111111111111", ProjectID: projectUID, Title: "Ticket", State: "todo"},
	}}
	_, router := mentionServer(t, fs, &fakeRoster{}) // no SetProjectUIDResolver

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+projectUID+"/discussion/mentions?q=ticket", nil)
	req = req.WithContext(WithAuth(req.Context(), auth))
	router.ServeHTTP(rec, req)

	var got MentionSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 1 {
		t.Fatalf("results = %+v, want the raw-path narrow to still match a UID-addressed project", got.Results)
	}
}

func TestSearchMentionsAgentCap(t *testing.T) {
	project := uuid.New()
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	agents := make([]TeamAgent, 0, 10)
	for i := 0; i < 10; i++ {
		agents = append(agents, TeamAgent{Name: "rob-agent-" + string(rune('a'+i)), Status: "online"})
	}
	_, router := mentionServer(t, nil, &fakeRoster{agents: agents})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+project.String()+"/discussion/mentions?q=rob", nil)
	req = req.WithContext(WithAuth(req.Context(), auth))
	router.ServeHTTP(rec, req)

	var got MentionSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != mentionAgentLimit {
		t.Fatalf("agent results = %d, want capped at %d", len(got.Results), mentionAgentLimit)
	}
}
