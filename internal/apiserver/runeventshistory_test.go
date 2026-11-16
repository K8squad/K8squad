package apiserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/mux"

	"github.com/K8squad/K8squad/internal/discussion"
	"github.com/K8squad/K8squad/pkg/events"
)

// ISI-7484 WS-3: GET /api/runs/{runId}/events — the durable Activity-history
// hydration read. These tests pin the contract the console hydration relies on:
// ascending rows keyed by the SAME outbox id the SSE id: line carries, resolver
// scoping identical to the stream route, and an honest 404/400 surface.

const (
	testRunUID  = "11111111-2222-3333-4444-555555555555"
	otherRunUID = "99999999-8888-7777-6666-555555555555"
)

func historyRequest(t *testing.T, target string, resolve runEventUIDResolver, admin bool) *httptest.ResponseRecorder {
	t.Helper()
	reader := &fakeRunReader{}
	ts := time.Date(2026, 11, 16, 8, 0, 0, 0, time.UTC)
	reader.add(
		events.RunEvent{ID: 11, RunID: testRunUID, EventType: "thinking", Payload: []byte(`{"author":"sam","body":"step one"}`), OccurredAt: ts},
		events.RunEvent{ID: 12, RunID: testRunUID, EventType: "ended", Payload: []byte(`{"agent":"sam"}`), OccurredAt: ts.Add(time.Second)},
		events.RunEvent{ID: 13, RunID: otherRunUID, EventType: "thinking", Payload: []byte(`{"author":"other"}`), OccurredAt: ts},
	)
	return historyRequestReader(t, target, resolve, admin, reader)
}

func historyRequestReader(t *testing.T, target string, resolve runEventUIDResolver, admin bool, reader events.RunEventReader) *httptest.ResponseRecorder {
	t.Helper()
	router := mux.NewRouter()
	router.Path("/api/runs/{runId}/events").Methods(http.MethodGet).HandlerFunc(runEventsHandler(reader, resolve))
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req = req.WithContext(discussion.WithAuth(req.Context(), discussion.AuthorContext{
		Principal: "user:alice",
		IsAdmin:   admin,
	}))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func okResolve(context.Context, string) (string, error) { return testRunUID, nil }

func missResolve(context.Context, string) (string, error) {
	return "", context.DeadlineExceeded // any resolver miss: scope-hide
}

func decodeHistory(t *testing.T, rec *httptest.ResponseRecorder) RunEventHistoryResponse {
	t.Helper()
	var resp RunEventHistoryResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return resp
}

func TestRunEventsHandlerHydratesAscendingTail(t *testing.T) {
	rec := historyRequest(t, "/api/runs/intake-r1/events", okResolve, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	resp := decodeHistory(t, rec)
	if resp.RunID != testRunUID {
		t.Fatalf("runId = %s, want the resolved UID", resp.RunID)
	}
	if resp.Count != 2 || len(resp.Events) != 2 {
		t.Fatalf("count = %d events = %d, want 2 (only this run's rows)", resp.Count, len(resp.Events))
	}
	if resp.Events[0].ID != "11" || resp.Events[0].Type != "thinking" {
		t.Fatalf("first row = %+v, want id 11 type thinking", resp.Events[0])
	}
	if string(resp.Events[0].Data) != `{"author":"sam","body":"step one"}` {
		t.Fatalf("data passthrough broken: %s", resp.Events[0].Data)
	}
	if resp.Events[0].At.IsZero() {
		t.Fatal("occurred_at missing from hydration rows — console ordering needs it")
	}
	if resp.LastID != "12" {
		t.Fatalf("lastId = %s, want 12", resp.LastID)
	}
}

func TestRunEventsHandlerForwardsAfterIDAndLimit(t *testing.T) {
	rec := historyRequest(t, "/api/runs/intake-r1/events?afterId=11&limit=1", okResolve, false)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	resp := decodeHistory(t, rec)
	if resp.Count != 1 || resp.Events[0].ID != "12" {
		t.Fatalf("afterId/limit not honored: %+v", resp.Events)
	}
}

func testStatus(t *testing.T, target string, resolve runEventUIDResolver, admin bool, want int) {
	t.Helper()
	rec := historyRequest(t, target, resolve, admin)
	if rec.Code != want {
		t.Fatalf("GET %s status = %d, want %d (%s)", target, rec.Code, want, rec.Body.String())
	}
}

func TestRunEventsHandlerAuthAndValidation(t *testing.T) {
	// Unauthenticated (no AuthorContext in the request).
	router := mux.NewRouter()
	router.Path("/api/runs/{runId}/events").Methods(http.MethodGet).HandlerFunc(runEventsHandler(&fakeRunReader{}, okResolve))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/runs/intake-r1/events", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d, want 401", rec.Code)
	}

	testStatus(t, "/api/runs/bad%0Aid/events", okResolve, true, http.StatusBadRequest) // CR/LF injection charset
	testStatus(t, "/api/runs/intake-r1/events?afterId=zzz", okResolve, true, http.StatusBadRequest)
	testStatus(t, "/api/runs/intake-r1/events?limit=0", okResolve, true, http.StatusBadRequest)
}

func TestRunEventsHandlerResolverScoping(t *testing.T) {
	// Resolver miss + a NAME on the wire: existence-hiding 404 (tenant AND admin —
	// a name the cache cannot resolve addresses nothing).
	testStatus(t, "/api/runs/intake-r1/events", missResolve, true, http.StatusNotFound)

	// Resolver miss + a raw UID: journal-native fallback is admin-only. Tenants
	// keep the honest 404 (the resolver IS their tenancy check); admins may read
	// history for a run whose CR is already gone.
	uidPath := "/api/runs/" + testRunUID + "/events"
	testStatus(t, uidPath, missResolve, false, http.StatusNotFound)
	rec := historyRequest(t, uidPath, missResolve, true)
	if rec.Code != http.StatusOK {
		t.Fatalf("admin uid fallback status = %d, want 200 (%s)", rec.Code, rec.Body.String())
	}
	if resp := decodeHistory(t, rec); resp.Count != 2 {
		t.Fatalf("admin uid fallback hydrated %d rows, want 2", resp.Count)
	}
}

func TestRunEventsHandlerEmptyTailIsAnEmptyArray(t *testing.T) {
	rec := historyRequestReader(t, "/api/runs/intake-r1/events", okResolve, false, &fakeRunReader{})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"events":[]`) {
		t.Fatalf("empty tail must serialize as an empty array, got %s", body)
	}
	resp := decodeHistory(t, rec)
	if resp.Count != 0 || resp.LastID != "" {
		t.Fatalf("empty tail: count = %d lastId = %q, want 0/\"\"", resp.Count, resp.LastID)
	}
}
