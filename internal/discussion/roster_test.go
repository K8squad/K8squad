package discussion

// Roster + admin-mention scoping (ISI-5107): the fix for Henrik's empty right-rail and unresolved
// @john. The unit lane pins the seam contract WITHOUT a database or k8s cache — the fakeRoster's three
// sources (own team / project namespace / fleet) stand in for the apiserver org projection:
//
//   - GET /roster resolves the agents dispatchable into THIS project: admin ⇒ the project's namespace
//     (ProjectAgents), non-admin ⇒ their own team (TeamAgents, existence-hiding preserved);
//   - GET /mentions?q= for an admin resolves fleet-wide (AllAgents — ADR-039), so @john in another
//     squad resolves; a non-admin stays fenced to their own team;
//   - the roster degrades to an empty array (never null, never 5xx) on a read failure or nil seam.

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
)

func rosterNamesFrom(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("roster status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got []RosterAgent
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	names := make([]string, 0, len(got))
	for _, a := range got {
		names = append(names, a.Name)
	}
	return names
}

func TestRosterAdminUsesProjectNamespace(t *testing.T) {
	// Admin viewing squad-b's project: ProjectAgents (the project's namespace) backs the rail, NOT
	// TeamAgents (which would be the admin's — empty — home team). john is the other squad's agent.
	roster := &fakeRoster{
		agents:        []TeamAgent{{Name: "should-not-appear"}}, // TeamAgents source — must be unused
		projectAgents: []TeamAgent{{Name: "john", Status: "working"}},
	}
	_, router := mentionServer(t, nil, roster)
	admin := AuthorContext{Principal: "user:root", IsAdmin: true}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/roster", nil)
	req = req.WithContext(WithAuth(req.Context(), admin))
	router.ServeHTTP(rec, req)

	if names := rosterNamesFrom(t, rec); len(names) != 1 || names[0] != "john" {
		t.Fatalf("admin roster = %v, want [john] (project namespace, not the caller's team)", names)
	}
}

func TestRosterNonAdminFencedToOwnTeam(t *testing.T) {
	// A non-admin never reaches ProjectAgents/AllAgents — the rail is their own team only, so a
	// project in another squad can never leak that squad's agents (existence-hiding).
	roster := &fakeRoster{
		agents:        []TeamAgent{{Name: "alice", Status: "idle"}},
		projectAgents: []TeamAgent{{Name: "john"}}, // present, but the non-admin path must not read it
	}
	_, router := mentionServer(t, nil, roster)
	member := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/roster", nil)
	req = req.WithContext(WithAuth(req.Context(), member))
	router.ServeHTTP(rec, req)

	names := rosterNamesFrom(t, rec)
	if len(names) != 1 || names[0] != "alice" {
		t.Fatalf("non-admin roster = %v, want fenced to own team [alice]", names)
	}
}

func TestRosterDegradesToEmptyArray(t *testing.T) {
	t.Run("nil seam ⇒ [] not null", func(t *testing.T) {
		_, router := mentionServer(t, nil, nil)
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/roster", nil)
		req = req.WithContext(WithAuth(req.Context(), AuthorContext{Principal: "user:alice", TeamID: uuid.New()}))
		router.ServeHTTP(rec, req)
		var got []RosterAgent
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &got) != nil || got == nil {
			t.Fatalf("nil-seam roster: code=%d body=%s (want 200 and a [] array)", rec.Code, rec.Body.String())
		}
	})
	t.Run("read failure ⇒ [] not 5xx", func(t *testing.T) {
		_, router := mentionServer(t, nil, &fakeRoster{err: errors.New("cache down")})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/roster", nil)
		req = req.WithContext(WithAuth(req.Context(), AuthorContext{Principal: "user:alice", TeamID: uuid.New()}))
		router.ServeHTTP(rec, req)
		if names := rosterNamesFrom(t, rec); len(names) != 0 {
			t.Fatalf("roster on read failure = %v, want [] (a projection, not the fence)", names)
		}
	})
	t.Run("unauthenticated ⇒ 401", func(t *testing.T) {
		_, router := mentionServer(t, nil, &fakeRoster{})
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/roster", nil)
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("status = %d, want 401", rec.Code)
		}
	})
}

func TestSearchMentionsAdminResolvesFleetWideAgents(t *testing.T) {
	// The @-mention admin path reads AllAgents (fleet-wide), so an agent in another squad resolves —
	// the direct fix for "@john does not resolve." A non-admin reads TeamAgents (own team) instead.
	roster := &fakeRoster{
		agents:    []TeamAgent{{Name: "alice", Status: "idle"}},                    // own-team source
		allAgents: []TeamAgent{{Name: "john", Status: "working"}, {Name: "alice"}}, // fleet source
	}
	_, router := mentionServer(t, nil, roster)
	admin := AuthorContext{Principal: "user:root", IsAdmin: true}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/projects/squad-b/discussion/mentions?q=john", nil)
	req = req.WithContext(WithAuth(req.Context(), admin))
	router.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body.String())
	}
	var got MentionSearchResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(got.Results) != 1 || got.Results[0].Type != "agent" || got.Results[0].ID != "john" {
		t.Fatalf("admin @john = %+v, want the fleet-wide john agent", got.Results)
	}
}
