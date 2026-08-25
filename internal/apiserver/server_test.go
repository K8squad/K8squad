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
)

// devToken is a static session token wired into a StaticSessionResolver for the gated-surface tests.
const devToken = "dev-token-abc"

func testServer(t *testing.T, ready ReadinessChecker) http.Handler {
	t.Helper()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil), // nil store: gated tests only exercise pre-store paths
		Ready:         ready,
	})
	return srv.Handler()
}

func withSession(req *http.Request, token string) *http.Request {
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: token})
	return req
}

// TestHealthz — liveness is unauthenticated and never depends on the store.
func TestHealthz(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthz: got %d, want 200", rec.Code)
	}
}

// TestReadyzGating — /readyz is 200 when the checker passes and 503 when it fails.
func TestReadyzGating(t *testing.T) {
	rec := httptest.NewRecorder()
	testServer(t, okReady{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz(ok): got %d, want 200", rec.Code)
	}
	rec = httptest.NewRecorder()
	testServer(t, failReady{}).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("readyz(fail): got %d, want 503", rec.Code)
	}
}

// TestGatedRoutesRejectUnauthenticated — every gated route answers 401 without a session cookie
// (deny-by-default), before any handler/store runs (nil store would panic if reached).
func TestGatedRoutesRejectUnauthenticated(t *testing.T) {
	proj := uuid.NewString()
	run := uuid.NewString()
	for _, path := range []string{
		"/api/projects/" + proj + "/discussion/threads",
		"/api/runs/" + run + "/stream",
		"/api/runs/" + run + "/build/tree",
		"/api/squad/overview",
	} {
		rec := httptest.NewRecorder()
		testServer(t, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("GET %s unauthenticated: got %d, want 401", path, rec.Code)
		}
	}
}

// TestBuildBrowserNotImplemented — the build-browser route authorizes then answers a documented
// 501 (NOT 404) naming the tracking issue, so the BFF distinguishes "pending" from "bug".
func TestBuildBrowserNotImplemented(t *testing.T) {
	run := uuid.NewString()
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/runs/"+run+"/build/tree", nil), devToken)
	testServer(t, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("build/tree authenticated: got %d, want 501", rec.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode 501 body: %v", err)
	}
	if body["tracking"] == "" {
		t.Errorf("501 body missing tracking pointer: %v", body)
	}
}

// TestSquadOverviewNotImplemented — same honest-501 contract for the squad-overview read model.
func TestSquadOverviewNotImplemented(t *testing.T) {
	rec := httptest.NewRecorder()
	req := withSession(httptest.NewRequest(http.MethodGet, "/api/squad/overview", nil), devToken)
	testServer(t, nil).ServeHTTP(rec, req)
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("squad/overview authenticated: got %d, want 501", rec.Code)
	}
}

type okReady struct{}

func (okReady) Ready(context.Context) error { return nil }

type failReady struct{}

func (failReady) Ready(context.Context) error { return context.DeadlineExceeded }

// TestSSEStreamDeliversPublishedEvent — an authenticated stream receives the subscribed comment
// and a subsequently Published event in SSE wire format, then closes on client disconnect.
func TestSSEStreamDeliversPublishedEvent(t *testing.T) {
	runID := uuid.NewString()
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
	})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/runs/"+runID+"/stream", nil)
	req.AddCookie(&http.Cookie{Name: SessionCookieName, Value: devToken})
	resp, err := http.DefaultClient.Do(req) //nolint:bodyclose // body is closed via the defer below; bodyclose can't track it through the goroutine read at L166.
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("stream status: got %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("content-type: got %q, want text/event-stream", ct)
	}

	// Wait for the subscriber to register, then publish.
	deadline := time.Now().Add(2 * time.Second)
	for srv.Hub().subscriberCount(runID) == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Hub().subscriberCount(runID) != 1 {
		t.Fatalf("subscriber not registered")
	}
	srv.Hub().Publish(runID, Event{ID: "1", Type: "progress", Data: "step-1"})

	// Read until we see the published event's data line (or time out).
	buf := make([]byte, 4096)
	got := ""
	readCtx, readCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer readCancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				got += string(buf[:n])
				if strings.Contains(got, "event: progress") && strings.Contains(got, "data: step-1") {
					return
}

// TestAuditLogGating — /api/audit/log is gated behind authentication and team-based access control
func TestAuditLogGating(t *testing.T) {
	// Test without authentication (should be 401)
	rec := httptest.NewRecorder()
	testServer(t, nil).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/audit/log", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized access: got %d, want 401", rec.Code)
	}

	// Test with authentication but no audit log reader (should be 501)
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Ready:         nil,
		// AuditLog is intentionally nil to test 501 response
	})

	rec = httptest.NewRecorder()
	withSession(httptest.NewRequest(http.MethodGet, "/api/audit/log", nil), devToken)
	srv.Handler().ServeHTTP(rec, withSession(httptest.NewRequest(http.MethodGet, "/api/audit/log", nil), devToken))
	if rec.Code != http.StatusNotImplemented {
		t.Fatalf("no audit log reader: got %d, want 501", rec.Code)
	}

	// Verify response structure
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if resp["error"] != "not implemented" {
		t.Errorf("expected error 'not implemented', got: %s", resp["error"])
	}
	if resp["detail"] == "" || !strings.Contains(resp["detail"], "audit log read model") {
		t.Errorf("expected detail mentioning audit log, got: %s", resp["detail"])
	}
	if resp["tracking"] != "ISI-2881: wire an AuditLogReader (database connection) to enable" {
		t.Errorf("expected tracking ISI-2881, got: %s", resp["tracking"])
	}
}

// TestAuditLogRouteStructure — verify the route is properly mounted
func TestAuditLogRouteStructure(t *testing.T) {
	// Create a mock audit log reader for testing
	mockReader := &MockAuditLogReader{
		Response: AuditResponse{
			Entries: []AuditEntry{},
			Total:   0,
			Offset:  0,
			Limit:   100,
		},
	}

	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: uuid.New()},
	}}

	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Ready:         nil,
		AuditLog:      mockReader,
	})

	// Test the route is accessible with proper authentication
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/audit/log", nil)
	req = withSession(req, devToken)

	srv.Handler().ServeHTTP(rec, req)

	// Should succeed with empty response
	if rec.Code != http.StatusOK {
		t.Fatalf("authenticated access failed: got %d, want 200", rec.Code)
	}

	// Verify response structure
	var response AuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	if response.Total != 0 {
		t.Errorf("expected total 0, got: %d", response.Total)
	}
	if response.Offset != 0 {
		t.Errorf("expected offset 0, got: %d", response.Offset)
	}
	if response.Limit != 100 {
		t.Errorf("expected limit 100, got: %d", response.Limit)
	}
}

// MockAuditLogReader for testing
type MockAuditLogReader struct {
	Response AuditResponse
	Error    error
}

func (m *MockAuditLogReader) QueryAuditLog(ctx context.Context, query AuditQuery, teamID string) (AuditResponse, error) {
	if m.Error != nil {
		return AuditResponse{}, m.Error
	}
	return m.Response, nil
}
			}
			if rerr != nil {
				return
			}
		}
	}()
	select {
	case <-done:
	case <-readCtx.Done():
	}
	if !strings.Contains(got, ": subscribed") {
		t.Errorf("stream missing subscribed preamble; got:\n%s", got)
	}
	if !strings.Contains(got, "event: progress") || !strings.Contains(got, "data: step-1") {
		t.Errorf("stream missing published event; got:\n%s", got)
	}
}
