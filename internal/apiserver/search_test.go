package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/search"
)

// fakeSearcher records the scoped Query the handler built and returns canned
// results/err, so the handler's auth, param-parsing, RBAC-scope derivation, and
// error mapping can be exercised without a DB.
type fakeSearcher struct {
	called  bool
	gotQ    search.Query
	results []search.Result
	err     error
}

func (f *fakeSearcher) Search(_ context.Context, q search.Query) ([]search.Result, error) {
	f.called = true
	f.gotQ = q
	return f.results, f.err
}

const searchAdminToken = "dev-token-admin"

func testSearchServer(t *testing.T, teamID uuid.UUID, store search.Searcher) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:         {Principal: "user:alice", TeamID: teamID},
		searchAdminToken: {Principal: "user:root", TeamID: teamID, IsAdmin: true},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Search:        store,
	})
	return srv.Handler()
}

func getSearch(query, token string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/search"+query, nil)
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// A human session searches: 200, the store is called Team-fenced (AllTeams=false,
// TeamID = the caller's Team), and the envelope echoes the query + results.
func TestSearchHandlerScopedToTeam(t *testing.T) {
	teamID := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	store := &fakeSearcher{results: []search.Result{{Type: "work_item", ID: "wi-1", Title: "Fix checkout"}}}
	h := testSearchServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getSearch("?q=checkout&limit=5", devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.called {
		t.Fatal("store was not called")
	}
	if store.gotQ.AllTeams {
		t.Fatal("a non-admin caller must be Team-fenced (AllTeams must be false)")
	}
	if store.gotQ.TeamID != teamID.String() {
		t.Fatalf("scope TeamID = %q, want %q", store.gotQ.TeamID, teamID.String())
	}
	if store.gotQ.Text != "checkout" || store.gotQ.Limit != 5 {
		t.Fatalf("unexpected query: %+v", store.gotQ)
	}
	var resp SearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Query != "checkout" || len(resp.Results) != 1 || resp.Results[0].ID != "wi-1" {
		t.Fatalf("unexpected response: %+v", resp)
	}
}

// An admin session searches fleet-wide: the store is called with AllTeams=true.
func TestSearchHandlerAdminIsFleetWide(t *testing.T) {
	teamID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	store := &fakeSearcher{}
	h := testSearchServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getSearch("?q=refund", searchAdminToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.gotQ.AllTeams {
		t.Fatal("an admin caller must search fleet-wide (AllTeams must be true)")
	}
}

// A blank/absent q is a 400 and the store is never called.
func TestSearchHandlerEmptyQuery(t *testing.T) {
	for _, q := range []string{"", "?q="} {
		store := &fakeSearcher{}
		h := testSearchServer(t, uuid.New(), store)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, getSearch(q, devToken))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: got %d, want 400", q, rec.Code)
		}
		if store.called {
			t.Fatalf("query %q: store must not be called for an empty query", q)
		}
	}
}

// An unauthenticated request is rejected before the store.
func TestSearchHandlerUnauthenticated(t *testing.T) {
	store := &fakeSearcher{}
	h := testSearchServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getSearch("?q=x", ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("got %d, want 401", rec.Code)
	}
	if store.called {
		t.Fatal("store must not be called without a session")
	}
}

// A store error maps to 502 (the read model is unavailable), not a 500 leak.
func TestSearchHandlerStoreErrorIs502(t *testing.T) {
	store := &fakeSearcher{err: context.DeadlineExceeded}
	h := testSearchServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getSearch("?q=x", devToken))
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("got %d, want 502", rec.Code)
	}
}

// ErrEmptyQuery bubbling up from the store (stopwords-only text) maps to 400.
func TestSearchHandlerStoreEmptyQueryIs400(t *testing.T) {
	store := &fakeSearcher{err: search.ErrEmptyQuery}
	h := testSearchServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, getSearch("?q=the", devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("got %d, want 400", rec.Code)
	}
}

// With no Search wired, the route answers a documented 501 (DB-less host shape).
func TestSearchRouteNotImplementedWhenUnwired(t *testing.T) {
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		// Search intentionally nil
	})
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, getSearch("?q=x", devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("got %d, want 501", rec.Code)
	}
}

// limit is clamped into 1..searchMaxLimit with a default for garbage/out-of-range.
func TestSearchHandlerLimitClamp(t *testing.T) {
	cases := []struct {
		q    string
		want int
	}{
		{q: "?q=x", want: searchDefaultLimit},
		{q: "?q=x&limit=0", want: searchDefaultLimit},
		{q: "?q=x&limit=abc", want: searchDefaultLimit},
		{q: "?q=x&limit=999", want: searchMaxLimit},
		{q: "?q=x&limit=7", want: 7},
	}
	for _, c := range cases {
		store := &fakeSearcher{}
		h := testSearchServer(t, uuid.New(), store)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, getSearch(c.q, devToken))
		if store.gotQ.Limit != c.want {
			t.Fatalf("query %q: limit=%d, want %d", c.q, store.gotQ.Limit, c.want)
		}
	}
}
