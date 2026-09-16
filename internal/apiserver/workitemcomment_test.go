package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/coord"
)

// fakeWorkItemCommenter records the last call and returns a canned result/error so
// the handler's auth, body-parsing and error-mapping can be exercised without a DB.
type fakeWorkItemCommenter struct {
	called    bool
	gotID     string
	gotTeam   string
	gotAuthor string
	gotBody   string
	result    coord.HumanCommentOutcome
	err       error
}

func (f *fakeWorkItemCommenter) AppendHumanComment(_ context.Context, id, teamID, principal, body string) (coord.HumanCommentOutcome, error) {
	f.called = true
	f.gotID, f.gotTeam, f.gotAuthor, f.gotBody = id, teamID, principal, body
	return f.result, f.err
}

// testCommentServer wires the comment route with a human (alice) + agent session. The
// route is keyed by item id (no {projectId}) so there is no requireProjectRole gate —
// tenancy is the store's Team fence and authorship the handler's human-only gate. A
// nil store keeps the route at its documented 501.
func testCommentServer(t *testing.T, teamID uuid.UUID, store WorkItemCommenter) http.Handler {
	t.Helper()
	agentID := "agent:builder"
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken:   {Principal: "user:alice", TeamID: teamID},
		agentToken: {Principal: "agent:builder", TeamID: teamID, AgentID: &agentID},
	}}
	srv := NewServer(Options{
		Authenticator:    NewCookieAuthenticator(resolver),
		Discussion:       discussion.NewHandler(nil),
		WorkItemComments: store,
	})
	return srv.Handler()
}

func postComment(id, body, token string) *http.Request {
	r := httptest.NewRequest(http.MethodPost, "/api/work-items/"+id+"/comments", strings.NewReader(body))
	if token != "" {
		r = withSession(r, token)
	}
	return r
}

// TestWorkItemCommentOK — a human posts a comment: 201, the store is called with the
// server-derived id/team/principal, and the response is the persisted comment shape
// (author = principal, NOT body text).
func TestWorkItemCommentOK(t *testing.T) {
	teamID := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	created := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	store := &fakeWorkItemCommenter{result: coord.HumanCommentOutcome{TaskComment: coord.TaskComment{Author: "user:alice", Body: "looks good", CreatedAt: created}}}
	h := testCommentServer(t, teamID, store)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", `{"body":"looks good"}`, devToken))
	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201 (body %s)", rec.Code, rec.Body.String())
	}
	if !store.called {
		t.Fatal("store not called")
	}
	if store.gotID != "wi-1" || store.gotBody != "looks good" {
		t.Fatalf("id/body not forwarded: %+v", store)
	}
	if store.gotTeam != teamID.String() || store.gotAuthor != "user:alice" {
		t.Fatalf("identity/tenancy not server-derived: %+v", store)
	}
	var got coord.TaskComment
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Author != "user:alice" || got.Body != "looks good" {
		t.Fatalf("response shape: %+v", got)
	}
}

// TestWorkItemCommentAuthorNotSpoofable — an "author" field in the body is an unknown
// field (DisallowUnknownFields) → 400, so a client can never author as someone else;
// the store is never touched.
func TestWorkItemCommentAuthorNotSpoofable(t *testing.T) {
	store := &fakeWorkItemCommenter{}
	h := testCommentServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", `{"body":"x","author":"user:mallory"}`, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("author spoof: got %d, want 400 (body %s)", rec.Code, rec.Body.String())
	}
	if store.called {
		t.Fatal("store must not run for a body with unknown fields")
	}
}

// TestWorkItemCommentAgentForbidden — an agent-authored session is refused 403 before
// the store is touched (agents comment via the run-token custody path).
func TestWorkItemCommentAgentForbidden(t *testing.T) {
	store := &fakeWorkItemCommenter{}
	h := testCommentServer(t, uuid.New(), store)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", `{"body":"x"}`, agentToken))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("agent comment: got %d, want 403", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run for a forbidden request")
	}
}

// TestWorkItemCommentEmptyBody — a missing/blank body is a 400 and never calls the store.
func TestWorkItemCommentEmptyBody(t *testing.T) {
	for _, body := range []string{`{}`, `{"body":""}`} {
		store := &fakeWorkItemCommenter{}
		h := testCommentServer(t, uuid.New(), store)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, postComment("wi-1", body, devToken))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q: got %d, want 400", body, rec.Code)
		}
		if store.called {
			t.Fatalf("body %q: store must not run without text", body)
		}
	}
}

// TestWorkItemCommentOversized — a body over the 64KiB cap fails closed as a 400
// (MaxBytesReader trips the decode), never reaching the store.
func TestWorkItemCommentOversized(t *testing.T) {
	store := &fakeWorkItemCommenter{}
	h := testCommentServer(t, uuid.New(), store)
	huge := `{"body":"` + strings.Repeat("a", 70<<10) + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", huge, devToken))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("oversized: got %d, want 400", rec.Code)
	}
	if store.called {
		t.Fatal("store must not run for an oversized body")
	}
}

// TestWorkItemCommentErrorMapping — the store's sentinels map to their HTTP status
// (cross-tenant / missing item → 404 existence-hiding; invalid → 400).
func TestWorkItemCommentErrorMapping(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want int
	}{
		{"notfound", coord.ErrWorkItemNotFound, http.StatusNotFound},
		{"invalid", coord.ErrInvalidWorkItem, http.StatusBadRequest},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := testCommentServer(t, uuid.New(), &fakeWorkItemCommenter{err: tc.err})
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, postComment("wi-1", `{"body":"x"}`, devToken))
			if rec.Code != tc.want {
				t.Fatalf("%s: got %d, want %d", tc.name, rec.Code, tc.want)
			}
		})
	}
}

// TestWorkItemCommentNilStore501 — no commenter wired ⇒ documented 501, not 404/panic.
func TestWorkItemCommentNilStore501(t *testing.T) {
	h := testCommentServer(t, uuid.New(), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", `{"body":"x"}`, devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("nil store: got %d, want 501", rec.Code)
	}
}

// TestWorkItemCommentUnauthenticated — no session ⇒ 401 at the choke point.
func TestWorkItemCommentUnauthenticated(t *testing.T) {
	h := testCommentServer(t, uuid.New(), &fakeWorkItemCommenter{})
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, postComment("wi-1", `{"body":"x"}`, ""))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("no session: got %d, want 401", rec.Code)
	}
}
