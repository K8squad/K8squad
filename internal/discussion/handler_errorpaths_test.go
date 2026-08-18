package discussion

// DB-free error-branch coverage for the §7.5 handlers. Each handler validates its path vars, auth, and
// request body BEFORE it reaches the store, so those branches are exercisable with a nil store in the
// default unit lane — the store-backed happy paths remain in the Postgres integration lane. This pays
// down the ISI-2714 ratchet on internal/discussion (the highest-debt authored package) by covering the
// 400/401 guard rails the DB-backed tests skip.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/gorilla/mux"
)

// TestHandlerBadPathVars covers the invalid-UUID (400) guard on every handler that parses a path var.
func TestHandlerBadPathVars(t *testing.T) {
	h := NewHandler(nil) // nil store: none of these cases reach it
	good := uuid.New().String()
	bad := "not-a-uuid"

	cases := []struct {
		name    string
		call    func(w http.ResponseWriter, r *http.Request)
		vars    map[string]string
		wantMsg string
	}{
		{"listThreads/badProject", h.listThreads, map[string]string{"projectId": bad}, "invalid projectId"},
		{"openThread/badProject", h.openThread, map[string]string{"projectId": bad}, "invalid projectId"},
		{"getThread/badProject", h.getThread, map[string]string{"projectId": bad}, "invalid projectId"},
		{"getThread/badThread", h.getThread, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"postMessage/badProject", h.postMessage, map[string]string{"projectId": bad}, "invalid projectId"},
		{"postMessage/badThread", h.postMessage, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"retractMessage/badProject", h.retractMessage, map[string]string{"projectId": bad}, "invalid projectId"},
		{"retractMessage/badThread", h.retractMessage, map[string]string{"projectId": good, "threadId": bad}, "invalid threadId"},
		{"retractMessage/badMessage", h.retractMessage, map[string]string{"projectId": good, "threadId": good, "messageId": bad}, "invalid messageId"},
		{"memoryIndex/badProject", h.memoryIndex, map[string]string{"projectId": bad}, "invalid projectId"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/", nil), c.vars)
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

// TestHandlerUnauthenticated covers requireAuth's 401 defence-in-depth reached via a bare handler (no
// BFFAuthz), i.e. valid path vars but no authenticated context.
func TestHandlerUnauthenticated(t *testing.T) {
	h := NewHandler(nil)
	good := uuid.New().String()

	cases := []struct {
		name string
		call func(w http.ResponseWriter, r *http.Request)
		vars map[string]string
	}{
		{"listThreads", h.listThreads, map[string]string{"projectId": good}},
		{"openThread", h.openThread, map[string]string{"projectId": good}},
		{"getThread", h.getThread, map[string]string{"projectId": good, "threadId": good}},
		{"postMessage", h.postMessage, map[string]string{"projectId": good, "threadId": good}},
		{"retractMessage", h.retractMessage, map[string]string{"projectId": good, "threadId": good, "messageId": good}},
		{"memoryIndex", h.memoryIndex, map[string]string{"projectId": good}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			req := mux.SetURLVars(httptest.NewRequest(http.MethodGet, "/", nil), c.vars) // no WithAuth
			rec := httptest.NewRecorder()
			c.call(rec, req)
			if rec.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", rec.Code)
			}
		})
	}
}

// TestHandlerBadBody covers the invalid-JSON (400) branch on the two write handlers and the invalid
// parentId (400) branch on postMessage — all reached after auth but before any store call.
func TestHandlerBadBody(t *testing.T) {
	h := NewHandler(nil)
	good := uuid.New().String()
	// build on r.Context() so the mux URL vars set by SetURLVars survive (WithAuth must not wipe them).
	authed := func(r *http.Request) *http.Request {
		return r.WithContext(WithAuth(r.Context(), AuthorContext{Principal: "user:alice", TeamID: uuid.New()}))
	}

	t.Run("openThread/badJSON", func(t *testing.T) {
		req := authed(mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json")),
			map[string]string{"projectId": good}))
		rec := httptest.NewRecorder()
		h.openThread(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON body") {
			t.Fatalf("status = %d body = %q, want 400 invalid JSON body", rec.Code, rec.Body.String())
		}
	})

	t.Run("postMessage/badJSON", func(t *testing.T) {
		req := authed(mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{not json")),
			map[string]string{"projectId": good, "threadId": good}))
		rec := httptest.NewRecorder()
		h.postMessage(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid JSON body") {
			t.Fatalf("status = %d body = %q, want 400 invalid JSON body", rec.Code, rec.Body.String())
		}
	})

	t.Run("postMessage/badParentId", func(t *testing.T) {
		body := `{"body":"hi","parentId":"not-a-uuid"}`
		req := authed(mux.SetURLVars(httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body)),
			map[string]string{"projectId": good, "threadId": good}))
		rec := httptest.NewRecorder()
		h.postMessage(rec, req)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "invalid parentId") {
			t.Fatalf("status = %d body = %q, want 400 invalid parentId", rec.Code, rec.Body.String())
		}
	})
}
