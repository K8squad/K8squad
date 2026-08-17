package discussion

// Unit coverage for the provenance/HTTP helper seam that the §7.5 handlers lean on but that the
// DB-backed handler tests never exercise directly (they need Postgres and are build-tag gated to the
// integration lane). These are pure functions — auth-context round-trip, path/query parsing, the
// store-error → HTTP-status mapping (AC5: tenancy misses are 404-not-403), and the AuthorContext
// NullString derivation (agent-vs-human is DERIVED, never a body flag) — so they belong in the default
// unit lane. This also pays down the ISI-2714 coverage ratchet on the highest-debt authored package.

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

func TestWithAuthRoundTrip(t *testing.T) {
	agent := "agent-7"
	run := "run-42"
	want := AuthorContext{Principal: "user:alice", TeamID: uuid.New(), AgentID: &agent, RunID: &run}

	ctx := WithAuth(context.Background(), want)
	got, ok := AuthFromContext(ctx)
	if !ok {
		t.Fatal("AuthFromContext: expected ok on a context populated by WithAuth")
	}
	if got.Principal != want.Principal || got.TeamID != want.TeamID {
		t.Fatalf("AuthFromContext = %+v, want %+v", got, want)
	}

	// A bare context carries no identity — the deny-by-default source of truth.
	if _, ok := AuthFromContext(context.Background()); ok {
		t.Fatal("AuthFromContext: expected !ok on an empty context")
	}
}

func TestPathUUID(t *testing.T) {
	id := uuid.New()
	r := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/", nil), map[string]string{
		"projectId": id.String(),
		"threadId":  "not-a-uuid",
	})

	if got, ok := pathUUID(r, "projectId"); !ok || got != id {
		t.Fatalf("pathUUID(projectId) = %v,%v; want %v,true", got, ok, id)
	}
	if _, ok := pathUUID(r, "threadId"); ok {
		t.Fatal("pathUUID(threadId): expected !ok for a non-UUID path var")
	}
	if _, ok := pathUUID(r, "absent"); ok {
		t.Fatal("pathUUID(absent): expected !ok for a missing path var")
	}
}

func TestQueryInt(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/?limit=25&bad=xyz", nil)
	if got := queryInt(r, "limit", 50); got != 25 {
		t.Fatalf("queryInt(limit) = %d, want 25", got)
	}
	if got := queryInt(r, "bad", 50); got != 50 {
		t.Fatalf("queryInt(bad): non-numeric must fall back to default; got %d, want 50", got)
	}
	if got := queryInt(r, "absent", 7); got != 7 {
		t.Fatalf("queryInt(absent): missing must fall back to default; got %d, want 7", got)
	}
}

func TestRequireAuth(t *testing.T) {
	// Authenticated: the AuthorContext is returned and nothing is written.
	authed := WithAuth(context.Background(), AuthorContext{Principal: "user:bob", TeamID: uuid.New()})
	rec := httptest.NewRecorder()
	got, ok := requireAuth(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(authed))
	if !ok || got.Principal != "user:bob" {
		t.Fatalf("requireAuth(authed) = %+v,%v; want principal user:bob, true", got, ok)
	}

	// Unauthenticated (empty principal) and no-context both answer 401.
	for name, ctx := range map[string]context.Context{
		"empty-principal": WithAuth(context.Background(), AuthorContext{Principal: ""}),
		"no-context":      context.Background(),
	} {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			if _, ok := requireAuth(rec, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx)); ok {
				t.Fatal("requireAuth: expected !ok")
			}
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("requireAuth: status = %d, want 401", rec.Code)
			}
		})
	}
}

func TestWriteStoreErr(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{ErrThreadNotFound, http.StatusNotFound},
		{ErrMessageNotFound, http.StatusNotFound},
		{ErrEmptyBody, http.StatusBadRequest},
		{ErrEmptyTitle, http.StatusBadRequest},
		{ErrNotAuthor, http.StatusForbidden},
		{ErrAlreadyRetracted, http.StatusConflict},
		{sql.ErrConnDone, http.StatusInternalServerError}, // an unmapped error defaults to 500
	}
	for _, c := range cases {
		rec := httptest.NewRecorder()
		writeStoreErr(rec, c.err)
		if rec.Code != c.want {
			t.Errorf("writeStoreErr(%v): status = %d, want %d", c.err, rec.Code, c.want)
		}
	}
}

func TestAuthorContextNullStringDerivation(t *testing.T) {
	// Human post: no agent, no run → both NULL (agent-vs-human is derived from these being set).
	human := AuthorContext{Principal: "user:carol", TeamID: uuid.New()}
	if human.agentID().Valid {
		t.Error("human.agentID(): expected NULL for a human-authored post")
	}
	if human.runID().Valid {
		t.Error("human.runID(): expected NULL for a console post")
	}

	// Agent post from within a Run: both set.
	agent, run := "agent-9", "run-1"
	fromRun := AuthorContext{Principal: "agent:svc", TeamID: uuid.New(), AgentID: &agent, RunID: &run}
	if got := fromRun.agentID(); !got.Valid || got.String != agent {
		t.Errorf("fromRun.agentID() = %+v, want valid %q", got, agent)
	}
	if got := fromRun.runID(); !got.Valid || got.String != run {
		t.Errorf("fromRun.runID() = %+v, want valid %q", got, run)
	}
}
