package discussion

// ISI-5617 routing guard: the literal …/party-sessions/active must keep resolving to the live read, not
// be captured by the new …/party-sessions/{sessionId} get-by-id route. gorilla/mux matches in
// registration order, so this pins that the active route is registered FIRST — a reorder that regresses
// it (routing "active" into {sessionId}, which then 400s on the non-UUID) goes RED here, with no DB.

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gorilla/mux"
)

func TestPartySessionRouteMatching(t *testing.T) {
	r := mux.NewRouter()
	NewHandler(nil).Register(r) // nil store: we only inspect which route matches, never invoke a handler

	cases := []struct {
		name     string
		path     string
		wantTmpl string
	}{
		{"active is not swallowed by {sessionId}", "/threads/t1/party-sessions/active", "/threads/{threadId}/party-sessions/active"},
		{"a concrete id routes to get-by-id", "/threads/t1/party-sessions/abc", "/threads/{threadId}/party-sessions/{sessionId}"},
		{"collection routes to the list read", "/threads/t1/party-sessions", "/threads/{threadId}/party-sessions"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, c.path, nil)
			var m mux.RouteMatch
			if !r.Match(req, &m) {
				t.Fatalf("no route matched %s", c.path)
			}
			tmpl, err := m.Route.GetPathTemplate()
			if err != nil {
				t.Fatalf("path template: %v", err)
			}
			if tmpl != c.wantTmpl {
				t.Fatalf("GET %s matched %q, want %q", c.path, tmpl, c.wantTmpl)
			}
		})
	}
}
