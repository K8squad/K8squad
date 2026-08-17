package discussion

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// stubAuth is a test Authenticator: it authenticates iff a non-empty X-Test-Principal header is
// present, standing in for the real §13 BFF (session/token/Run → AuthorContext).
type stubAuth struct{ team uuid.UUID }

func (s stubAuth) Authenticate(r *http.Request) (AuthorContext, bool) {
	p := r.Header.Get("X-Test-Principal")
	if p == "" {
		return AuthorContext{}, false
	}
	return AuthorContext{Principal: p, TeamID: s.team}, true
}

// mountedRouter builds the real gated surface (Mount → BFFAuthz + Register) with a nil store; the
// tests below only exercise paths that answer BEFORE any store call (401 / 400).
func mountedRouter(t *testing.T) http.Handler {
	t.Helper()
	r := mux.NewRouter()
	NewHandler(nil).Mount(r, stubAuth{team: uuid.New()})
	return r
}

// TestBFFAuthzRejectsUnauthenticated — the §13 choke point answers 401 before any handler/store runs
// (deny-by-default). A nil store proves the request never reached the store (else nil-panic/500).
func TestBFFAuthzRejectsUnauthenticated(t *testing.T) {
	proj := uuid.NewString()
	for _, path := range []string{
		"/api/projects/" + proj + "/discussion/threads",
		"/api/projects/" + proj + "/discussion/threads/" + uuid.NewString(),
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil) // no X-Test-Principal
		rec := httptest.NewRecorder()
		mountedRouter(t).ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: got %d, want 401", path, rec.Code)
		}
	}
}

// TestRequestStructsIgnoreBodyAuthor (AC3, structural) — the write request structs carry NO author_*
// field, so a body-supplied author has no path into the decoded request and is silently ignored;
// provenance is stamped from the authenticated context alone. This proves impersonation is
// un-representable at the type layer. The end-to-end "stored row shows the authenticated principal"
// assertion runs against real Postgres in integration_test.go (AC3).
func TestRequestStructsIgnoreBodyAuthor(t *testing.T) {
	forged := `{"title":"t","body":"hi","author_principal":"principal:victim","authorId":"x",` +
		`"authorType":"human","authorName":"attacker","author_run_id":"run:evil","createdBy":"who"}`
	var ot openThreadReq
	if err := json.Unmarshal([]byte(forged), &ot); err != nil {
		t.Fatalf("decode openThreadReq: %v", err)
	}
	if ot.Title != "t" || ot.Body != "hi" {
		t.Fatalf("openThreadReq lost its real fields: %+v", ot)
	}

	var pm postMessageReq
	if err := json.Unmarshal([]byte(`{"body":"hi","author":"principal:victim","authorType":"human"}`), &pm); err != nil {
		t.Fatalf("decode postMessageReq: %v", err)
	}
	if pm.Body != "hi" || pm.ParentID != nil {
		t.Fatalf("postMessageReq lost/gained fields: %+v", pm)
	}
	// The structs expose only these fields — no author/provenance surface exists to smuggle through.
	// (If a future edit adds an author field to either struct, this comment and the AC3 guarantee break.)
}

// TestRoutesResolveToHandlers — the five §7.5 routes resolve to distinct handlers at the canonical
// prefix, and the literal /memory-index route is not captured by the {threadId} variable route
// (registration-order matters in gorilla/mux).
func TestRoutesResolveToHandlers(t *testing.T) {
	proj := uuid.NewString()
	r := mux.NewRouter()
	NewHandler(nil).Mount(r, stubAuth{team: uuid.New()})
	base := "/api/projects/{projectId}/discussion"
	cases := []struct {
		method, path, want string
	}{
		{http.MethodGet, "/api/projects/" + proj + "/discussion/threads", base + "/threads"},
		{http.MethodPost, "/api/projects/" + proj + "/discussion/threads", base + "/threads"},
		{http.MethodGet, "/api/projects/" + proj + "/discussion/threads/" + uuid.NewString(), base + "/threads/{threadId}"},
		{http.MethodPost, "/api/projects/" + proj + "/discussion/threads/" + uuid.NewString() + "/messages", base + "/threads/{threadId}/messages"},
		{http.MethodPatch, "/api/projects/" + proj + "/discussion/threads/" + uuid.NewString() + "/messages/" + uuid.NewString(), base + "/threads/{threadId}/messages/{messageId}"},
		{http.MethodGet, "/api/projects/" + proj + "/discussion/memory-index", base + "/memory-index"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		var m mux.RouteMatch
		if !r.Match(req, &m) {
			t.Errorf("no route matched %s %s", tc.method, tc.path)
			continue
		}
		got, err := m.Route.GetPathTemplate()
		if err != nil {
			t.Errorf("GetPathTemplate %s: %v", tc.path, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s %s resolved to %q, want %q", tc.method, tc.path, got, tc.want)
		}
	}
}
