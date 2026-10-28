package discussion

// ISI-6748 — restore internal/discussion coverage re-pinned DOWN in the ISI-6539 baseline refresh
// (PR #948): climb from the lowered floor back to the old 44.9 floor. No production-code changes —
// these are DB-free unit tests for the error/branch paths the DB-backed integration lane never
// exercises in the default `go test` run (covgate measures the untagged unit lane only):
//
//   - the remaining §7.5 handlers' path-var / auth / body guard rails, reached with a nil store exactly
//     like handler_errorpaths_test.go (store-backed happy paths stay in the Postgres integration lane);
//   - the pure party/decision helpers (flatten, staleness rotation, null-jsonb, advancer principal +
//     dispatch guards) that no unit test reached.
//
// Pattern reference: PR #958 (pkg/controller/run/coverage_restore_test.go).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// TestHandlerBadPathVarsMore extends handler_errorpaths_test.go's 400 guard coverage to the
// proposal / decision-request / party-start / reset handlers it did not reach.
func TestHandlerBadPathVarsMore(t *testing.T) {
	h := NewHandler(nil) // nil store: none of these cases reach it
	good := uuid.New().String()
	bad := "not-a-uuid"

	cases := []struct {
		name    string
		call    func(w http.ResponseWriter, r *http.Request)
		vars    map[string]string
		wantMsg string
	}{
		{"resetThread/badProject", h.resetThread, map[string]string{"projectId": ""}, "invalid projectId"},
		{"resetThread/badThread", h.resetThread, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"postProposal/badProject", h.postProposal, map[string]string{"projectId": ""}, "invalid projectId"},
		{"postProposal/badThread", h.postProposal, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"listProposals/badProject", h.listProposals, map[string]string{"projectId": ""}, "invalid projectId"},
		{"listProposals/badThread", h.listProposals, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"postDecisionRequest/badProject", h.postDecisionRequest, map[string]string{"projectId": ""}, "invalid projectId"},
		{"postDecisionRequest/badThread", h.postDecisionRequest, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"listDecisionRequests/badProject", h.listDecisionRequests, map[string]string{"projectId": ""}, "invalid projectId"},
		{"listDecisionRequests/badThread", h.listDecisionRequests, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"startPartySession/badProject", h.startPartySession, map[string]string{"projectId": ""}, "invalid projectId"},
		{"startPartySession/badThread", h.startPartySession, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"getActivePartySession/badProject", h.getActivePartySession, map[string]string{"projectId": ""}, "invalid projectId"},
		{"getActivePartySession/badThread", h.getActivePartySession, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", nil), c.vars)
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400", rec.Code)
			}
			if !strings.Contains(rec.Body.String(), c.wantMsg) {
				t.Fatalf("body = %q, want to contain %q", rec.Body.String(), c.wantMsg)
			}
		})
	}
}

// TestHandlerUnauthenticatedMore covers requireAuth's 401 guard on the handlers not already in
// handler_errorpaths_test.go — valid path vars, no authenticated context, so scopedAuth fails closed
// before any store call.
func TestHandlerUnauthenticatedMore(t *testing.T) {
	h := NewHandler(nil)
	good := uuid.New().String()

	cases := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		vars map[string]string
	}{
		{"resetThread", h.resetThread, map[string]string{"projectId": good, "threadId": good}},
		{"postProposal", h.postProposal, map[string]string{"projectId": good, "threadId": good}},
		{"listProposals", h.listProposals, map[string]string{"projectId": good, "threadId": good}},
		{"postDecisionRequest", h.postDecisionRequest, map[string]string{"projectId": good, "threadId": good}},
		{"listDecisionRequests", h.listDecisionRequests, map[string]string{"projectId": good, "threadId": good}},
		{"startPartySession", h.startPartySession, map[string]string{"projectId": good, "threadId": good}},
		{"getActivePartySession", h.getActivePartySession, map[string]string{"projectId": good, "threadId": good}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", nil), c.vars) // no WithAuth
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestHandlerBadBodyMore covers the invalid-JSON (400) branch on the write handlers not already in
// handler_errorpaths_test.go, plus the invalid-parentId (400) branch on postDecisionRequest — all
// reached after auth (nil teamResolver ⇒ roomTeam falls back to the caller Team without a store call)
// but before any store call.
func TestHandlerBadBodyMore(t *testing.T) {
	h := NewHandler(nil)
	good := uuid.New().String()
	authed := func(r *http.Request) *http.Request {
		return r.WithContext(WithAuth(r.Context(), AuthorContext{Principal: "user:alice", TeamID: uuid.New()}))
	}

	badJSON := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
	}{
		{"postProposal", h.postProposal},
		{"postDecisionRequest", h.postDecisionRequest},
		{"startPartySession", h.startPartySession},
	}
	for _, c := range badJSON {
		t.Run(c.name+"/badJSON", func(t *testing.T) {
			req := authed(mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json")),
				map[string]string{"projectId": good, "threadId": good}))
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON body") {
				t.Fatalf("status = %d body = %q, want 400 invalid JSON body", rec.Code, rec.Body.String())
			}
		})
	}

	t.Run("postDecisionRequest/badParentId", func(t *testing.T) {
		body := `{"body":"decide?","parentId":"not-a-uuid"}`
		req := authed(mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)),
			map[string]string{"projectId": good, "threadId": good}))
		rec := httptest.NewRecorder()
		h.postDecisionRequest(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid parentId") {
			t.Fatalf("status = %d body = %q, want 400 invalid parentId", rec.Code, rec.Body.String())
		}
	})
}

// TestFlattenMessages covers the breadth-first unwrap of GetThread's nested Replies tree.
func TestFlattenMessages(t *testing.T) {
	if got := flattenMessages(nil); got != nil {
		t.Fatalf("flattenMessages(nil) = %v, want nil", got)
	}
	a, b, c, d := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	tree := []Message{
		{ID: a, Replies: []Message{
			{ID: c},
			{ID: d},
		}},
		{ID: b},
	}
	got := flattenMessages(tree)
	if len(got) != 4 {
		t.Fatalf("flattenMessages len = %d, want 4", len(got))
	}
	// Breadth-first: roots before their replies.
	order := []uuid.UUID{got[0].ID, got[1].ID, got[2].ID, got[3].ID}
	want := []uuid.UUID{a, b, c, d}
	for i := range want {
		if order[i] != want[i] {
			t.Fatalf("flattenMessages order = %v, want %v", order, want)
		}
	}
}

// TestStaleness covers the NeverSpoke sentinel (most overdue) and the clamp-to-zero branch for a
// current/future LastRound, alongside the ordinary rounds-ago case.
func TestStaleness(t *testing.T) {
	if got := staleness(NeverSpoke, 3); got != 4 {
		t.Fatalf("staleness(NeverSpoke, 3) = %d, want 4 (strictly more overdue than any speaker)", got)
	}
	if got := staleness(5, 3); got != 0 {
		t.Fatalf("staleness(5, 3) = %d, want 0 (future LastRound clamps to least overdue)", got)
	}
	if got := staleness(1, 3); got != 2 {
		t.Fatalf("staleness(1, 3) = %d, want 2", got)
	}
}

// TestNullJSON covers the empty-jsonb → SQL NULL branch and the pass-through branch.
func TestNullJSON(t *testing.T) {
	if got := nullJSON(nil); got != nil {
		t.Fatalf("nullJSON(nil) = %v, want nil", got)
	}
	if got := nullJSON([]byte{}); got != nil {
		t.Fatalf("nullJSON(empty) = %v, want nil", got)
	}
	in := []byte(`{"k":1}`)
	got, ok := nullJSON(in).([]byte)
	if !ok || string(got) != string(in) {
		t.Fatalf("nullJSON(%q) = %v, want the bytes back", in, nullJSON(in))
	}
}

// TestAdvancerPrincipal covers the configured-principal and the default-fallback branches.
func TestAdvancerPrincipal(t *testing.T) {
	if got := (&PartyAdvancer{Principal: "ksquad-ci"}).advancerPrincipal(); got != "ksquad-ci" {
		t.Fatalf("advancerPrincipal(set) = %q, want ksquad-ci", got)
	}
	if got := (&PartyAdvancer{}).advancerPrincipal(); got != "ksquad-operator" {
		t.Fatalf("advancerPrincipal(unset) = %q, want ksquad-operator", got)
	}
}

// TestDispatchVoiceGuards covers dispatchVoice's three defensive early returns — all reached before any
// store read, so they need no Postgres.
func TestDispatchVoiceGuards(t *testing.T) {
	ctx := context.Background()
	mid := uuid.New()
	sess := &PartySession{ID: uuid.New(), ProjectID: "p", TeamID: uuid.New(), ThreadID: uuid.New(), Round: 1}

	// 1. no voice dispatcher wired → warn + return (no store access).
	(&PartyAdvancer{}).dispatchVoice(ctx, sess, PartyTickPlan{VoiceIndex: 0})

	// 2. dispatcher wired but the round's facilitator has not posted yet (CurrentRoundMessageID nil).
	a := &PartyAdvancer{VoiceDispatcher: &fakeDispatcher{}}
	a.dispatchVoice(ctx, sess, PartyTickPlan{VoiceIndex: 0})

	// 3. cursor out of range (roster changed under us) → return before GetThread.
	sess.CurrentRoundMessageID = &mid
	sess.RoundVoices = nil
	a.dispatchVoice(ctx, sess, PartyTickPlan{VoiceIndex: 0})

	// The fake must never have been called — every path returned before DispatchMention.
	if fd := a.VoiceDispatcher.(*fakeDispatcher); len(fd.calls) != 0 {
		t.Fatalf("dispatchVoice dispatched %d voices through the guards, want 0", len(fd.calls))
	}
}

// TestRouteBarePartyNilStore covers routeBarePartyToFacilitator's defensive nil-store / nil-message
// guard — the one branch reachable without a Postgres-backed OpenPartySessionForMessage.
func TestRouteBarePartyNilStore(t *testing.T) {
	h := NewHandler(nil) // store nil
	auth := AuthorContext{Principal: "user:alice", TeamID: uuid.New()}
	if h.routeBarePartyToFacilitator(context.Background(), "p", auth, &Message{ID: uuid.New()}, nil) {
		t.Fatal("routeBarePartyToFacilitator(nil store) = true, want false")
	}
	if h.routeBarePartyToFacilitator(context.Background(), "p", auth, nil, nil) {
		t.Fatal("routeBarePartyToFacilitator(nil msg) = true, want false")
	}
}

// TestNewStoreAndWiring covers the trivial constructor + post-construction wiring setters that the
// unit lane never otherwise touches.
func TestNewStoreAndWiring(t *testing.T) {
	if NewStore(nil) == nil {
		t.Fatal("NewStore(nil) = nil, want a non-nil *Store")
	}
	h := NewHandler(nil)
	h.SetRoomStreamPublisher(nil) // wiring setter — just exercise the assignment
}
