package apiserver

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/K8squad/K8squad/internal/discussion"
)

func newTeamsReader(t *testing.T, objs ...client.Object) *ClientTeamsReader {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(overviewScheme(t)).WithObjects(objs...).Build()
	return NewClientTeamsReader(c)
}

const (
	teamAUID = "11111111-1111-1111-1111-111111111111"
	teamBUID = "22222222-2222-2222-2222-222222222222"
	teamCUID = "33333333-3333-3333-3333-333333333333"
)

// TestTeamsAdminFleetWide — an admin sees every Team, sorted by (namespace, name),
// with Fleet=true (AC1).
func TestTeamsAdminFleetWide(t *testing.T) {
	r := newTeamsReader(t,
		team("squad-b", "bravo", teamBUID),
		team("squad-a", "alpha", teamAUID),
		team("squad-a", "zulu", teamCUID),
	)

	list, err := r.Teams(context.Background(), "", true)
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	if !list.Fleet {
		t.Fatalf("admin list must set Fleet=true")
	}
	if len(list.Teams) != 3 {
		t.Fatalf("teams: got %d, want 3", len(list.Teams))
	}
	// Sorted by (namespace, name): squad-a/alpha, squad-a/zulu, squad-b/bravo.
	want := []struct{ ns, name, uid string }{
		{"squad-a", "alpha", teamAUID},
		{"squad-a", "zulu", teamCUID},
		{"squad-b", "bravo", teamBUID},
	}
	for i, w := range want {
		got := list.Teams[i]
		if got.Namespace != w.ns || got.Name != w.name || got.UID != w.uid {
			t.Fatalf("team[%d]: got %+v, want %+v", i, got, w)
		}
	}
}

// TestTeamsTenantOwnOnly — a non-admin sees ONLY their own Team; no other Team's
// name/namespace/uid appears anywhere (AC2 existence-hiding). The returned uid is
// the object UID, byte-identical to what /api/teams/{uid}/org resolves on (AC3).
func TestTeamsTenantOwnOnly(t *testing.T) {
	r := newTeamsReader(t,
		team("squad-a", "alpha", teamAUID),
		team("squad-b", "bravo", teamBUID),
		team("squad-c", "charlie", teamCUID),
	)

	list, err := r.Teams(context.Background(), teamBUID, false)
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	if list.Fleet {
		t.Fatalf("tenant list must not set Fleet")
	}
	if len(list.Teams) != 1 {
		t.Fatalf("tenant must see exactly 1 team, got %d", len(list.Teams))
	}
	only := list.Teams[0]
	if only.Name != "bravo" || only.Namespace != "squad-b" || only.UID != teamBUID {
		t.Fatalf("tenant team: %+v", only)
	}
	// Existence-hiding: no trace of the other two Teams in the payload.
	blob, _ := json.Marshal(list)
	for _, leak := range []string{"alpha", "squad-a", teamAUID, "charlie", "squad-c", teamCUID} {
		if strings.Contains(string(blob), leak) {
			t.Fatalf("payload leaked foreign team detail %q: %s", leak, blob)
		}
	}
}

// TestTeamsTenantDanglingEmpty — a non-admin whose UID matches no Team CR (a
// dangling binding) gets an empty list + nil error, never an error that would
// betray whether other Teams exist (AC2 edge).
func TestTeamsTenantDanglingEmpty(t *testing.T) {
	r := newTeamsReader(t,
		team("squad-a", "alpha", teamAUID),
		team("squad-b", "bravo", teamBUID),
	)

	list, err := r.Teams(context.Background(), "99999999-9999-9999-9999-999999999999", false)
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	if list.Fleet {
		t.Fatalf("tenant list must not set Fleet")
	}
	if len(list.Teams) != 0 {
		t.Fatalf("dangling tenant must see 0 teams, got %d", len(list.Teams))
	}
}

// TestTeamsTenantEmptyUIDEmpty — an empty caller UID (no resolved Team) yields an
// empty list, not an error and not the fleet (defence in depth for AC2).
func TestTeamsTenantEmptyUIDEmpty(t *testing.T) {
	r := newTeamsReader(t, team("squad-a", "alpha", teamAUID))
	list, err := r.Teams(context.Background(), "", false)
	if err != nil {
		t.Fatalf("Teams: %v", err)
	}
	if len(list.Teams) != 0 {
		t.Fatalf("empty-uid tenant must see 0 teams, got %d", len(list.Teams))
	}
}

// TestTeamsHandlerUnauthenticated — the handler answers 401 when no principal is
// stamped, even though BFFAuthz already guards this (AC6, defence in depth).
func TestTeamsHandlerUnauthenticated(t *testing.T) {
	s := &Server{}
	h := s.teams(stubTeamsReader{})
	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil) // no auth in context
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status: got %d, want 401", rec.Code)
	}
}

// TestTeamsHandlerOK — with an admin principal stamped, the handler returns 200
// and the reader's list.
func TestTeamsHandlerOK(t *testing.T) {
	s := &Server{}
	reader := newTeamsReader(t, team("squad-a", "alpha", teamAUID))
	h := s.teams(reader)

	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{
		Principal: "admin@example.com",
		TeamID:    uuid.Nil,
		IsAdmin:   true,
	}))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status: got %d, want 200", rec.Code)
	}
	var got TeamsList
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.Fleet || len(got.Teams) != 1 || got.Teams[0].UID != teamAUID {
		t.Fatalf("body: %+v", got)
	}
}

// TestTeamsHandlerReaderError — a reader failure surfaces as 502 (AC: bad gateway,
// never a fabricated empty list).
func TestTeamsHandlerReaderError(t *testing.T) {
	s := &Server{}
	h := s.teams(stubTeamsReader{err: errors.New("cache down")})
	req := httptest.NewRequest(http.MethodGet, "/api/teams", nil)
	req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{
		Principal: "u@example.com", TeamID: uuid.New(),
	}))
	rec := httptest.NewRecorder()
	h(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status: got %d, want 502", rec.Code)
	}
}

// testTeamsServer builds a full server router with a Teams reader wired and an
// admin session bound to devToken, so a GET /api/teams exercises the real mux
// dispatch (including coexistence with the POST /api/teams compose route).
func testTeamsServer(t *testing.T, reader TeamsReader) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:admin", TeamID: uuid.New(), IsAdmin: true},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Teams:         reader,
	})
	return srv.Handler()
}

// TestTeamsRouteWiring — GET /api/teams dispatches to the read model, and the
// distinct POST /api/teams compose route still resolves (proving the two sibling
// subrouters on the same path coexist via mux method fall-through, not a
// collision). With no ComposeService wired POST answers the documented 501 — the
// point is it is NOT swallowed by the GET handler (which would 200/405).
func TestTeamsRouteWiring(t *testing.T) {
	h := testTeamsServer(t, newTeamsReader(t, team("squad-a", "alpha", teamAUID)))

	// GET → 200 fleet list.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/teams", nil), devToken))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /api/teams: got %d, want 200", rec.Code)
	}
	var list TeamsList
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !list.Fleet || len(list.Teams) != 1 || list.Teams[0].UID != teamAUID {
		t.Fatalf("GET body: %+v", list)
	}

	// POST → reaches the compose collection (nil ComposeService ⇒ 501), NOT the
	// GET handler. A 200/405 here would mean the GET subrouter shadowed the POST.
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodPost, "/api/teams", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("POST /api/teams: got %d, want 501 (compose collection, not the GET handler)", rec.Code)
	}
}

// TestTeamsNilReader501 — with no read model wired the route keeps the documented
// 501 (AC4), exactly like the sibling read models.
func TestTeamsNilReader501(t *testing.T) {
	h := testTeamsServer(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/teams", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("GET /api/teams (nil reader): got %d, want 501", rec.Code)
	}
}

type stubTeamsReader struct {
	list TeamsList
	err  error
}

func (s stubTeamsReader) Teams(context.Context, string, bool) (TeamsList, error) {
	return s.list, s.err
}
