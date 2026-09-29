package discussion

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// fakeTeamResolver is a test ProjectTeamResolver: it maps one projectID to one Team, and can be told to
// report the project's Team as unknown (ok=false) or to fail (err), so roomTeam's three branches are
// each exercised in isolation.
type fakeTeamResolver struct {
	projectID string
	team      uuid.UUID
	unknown   bool // resolve succeeds but no Team claims the namespace
	err       error
}

func (f fakeTeamResolver) ResolveProjectTeam(_ context.Context, projectID string) (uuid.UUID, bool, error) {
	if f.err != nil {
		return uuid.Nil, false, f.err
	}
	if f.unknown || projectID != f.projectID {
		return uuid.Nil, false, nil
	}
	return f.team, true, nil
}

// TestRoomTeamScope is the ISI-5198 core: the room's tenancy fence is the Project's OWNING Team, not the
// caller's session Team — with a graceful fall back to the caller's Team when no resolver is wired or
// the project's Team is unknown, and an existence-hiding deny for a non-admin reaching another squad's
// room.
func TestRoomTeamScope(t *testing.T) {
	const proj = "bmad-squad/bmad-demo"
	projectTeam := uuid.New() // 7191cc8c… in the ticket
	sessionTeam := uuid.New() // 53992f80… the fleet-admin/session Team
	memberTeam := projectTeam // a project member's own Team IS the project's Team

	cases := []struct {
		name     string
		resolver ProjectTeamResolver
		auth     AuthorContext
		wantTeam uuid.UUID
		wantOK   bool
	}{
		{
			name:     "no resolver falls back to caller team (dev/test unchanged)",
			resolver: nil,
			auth:     AuthorContext{Principal: "u", TeamID: sessionTeam},
			wantTeam: sessionTeam,
			wantOK:   true,
		},
		{
			name:     "admin scopes to the project team, not their session team",
			resolver: fakeTeamResolver{projectID: proj, team: projectTeam},
			auth:     AuthorContext{Principal: "admin", TeamID: sessionTeam, IsAdmin: true},
			wantTeam: projectTeam,
			wantOK:   true,
		},
		{
			name:     "project member scopes to the project team",
			resolver: fakeTeamResolver{projectID: proj, team: projectTeam},
			auth:     AuthorContext{Principal: "member", TeamID: memberTeam},
			wantTeam: projectTeam,
			wantOK:   true,
		},
		{
			name:     "run scoped to the project team is admitted (the ISI-5152 reply path)",
			resolver: fakeTeamResolver{projectID: proj, team: projectTeam},
			auth:     AuthorContext{Principal: "agent", TeamID: projectTeam, RunID: strptr("run-1")},
			wantTeam: projectTeam,
			wantOK:   true,
		},
		{
			name:     "non-admin from another squad is denied (existence-hiding)",
			resolver: fakeTeamResolver{projectID: proj, team: projectTeam},
			auth:     AuthorContext{Principal: "stranger", TeamID: sessionTeam},
			wantOK:   false,
		},
		{
			name:     "unknown project team falls back to caller team",
			resolver: fakeTeamResolver{projectID: proj, unknown: true},
			auth:     AuthorContext{Principal: "u", TeamID: sessionTeam},
			wantTeam: sessionTeam,
			wantOK:   true,
		},
		{
			name:     "resolver error falls back to caller team",
			resolver: fakeTeamResolver{err: errors.New("cache down")},
			auth:     AuthorContext{Principal: "u", TeamID: sessionTeam},
			wantTeam: sessionTeam,
			wantOK:   true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := NewHandler(nil)
			if tc.resolver != nil {
				h.SetProjectTeamResolver(tc.resolver)
			}
			team, ok := h.roomTeam(context.Background(), proj, tc.auth)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && team != tc.wantTeam {
				t.Errorf("team = %v, want %v", team, tc.wantTeam)
			}
		})
	}
}

// TestScopedAuthDeniesForeignRoomWith404 — a non-admin caller addressing another squad's room is
// answered 404 (existence-hiding, NFR-SEC5): indistinguishable from a wholly absent project, and the
// request never reaches the (nil) store, so no cross-team read is possible.
func TestScopedAuthDeniesForeignRoomWith404(t *testing.T) {
	const proj = "secret" // single-segment id: no path-encoding needed for the {projectId} route var
	h := NewHandler(nil)  // nil store: any store call would panic, proving the deny short-circuits first
	h.SetProjectTeamResolver(fakeTeamResolver{projectID: proj, team: uuid.New()})

	r := mux.NewRouter()
	// A stub authenticator standing in for a non-admin whose session Team ≠ the project's Team.
	h.Mount(r, stubAuth{team: uuid.New()})

	req := httptest.NewRequest(http.MethodGet, "/api/projects/"+proj+"/discussion/threads", nil)
	req.Header.Set("X-Test-Principal", "stranger")
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("GET foreign room: got %d, want 404 (existence-hiding)", rec.Code)
	}
}

func strptr(s string) *string { return &s }
