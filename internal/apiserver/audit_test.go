package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// TestAuditLogIntegration — integration test for complete audit log flow
func TestAuditLogIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	// Create a test database
	db, err := sql.Open("postgres", "host=localhost port=5432 user=ksquad password=ksquad dbname=ksquad_test sslmode=disable")
	if err != nil {
		t.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	// Create test team
	teamID := uuid.New()
	
	// Create test work item
	workItemID := uuid.New()
	_, err = db.ExecContext(context.Background(), `
		INSERT INTO coord.work_item (id, project_id, team_id, title, state, created_by, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`, workItemID, uuid.New(), teamID, "Test Work Item", "todo", "user:alice", time.Now())
	if err != nil {
		t.Fatalf("failed to create work item: %v", err)
	}

	// Create test audit log entries
	testEvents := []struct {
		eventType string
		principal string
		payload   map[string]any
	}{
		{"claim_acquired", "user:alice", map[string]any{"step": "enter"}},
		{"reconcile_advanced", "user:alice", map[string]any{"step": "step-1"}},
		{"state_transition", "user:alice", map[string]any{"from": "todo", "to": "in_progress"}},
	}

	for _, event := range testEvents {
		payload, _ := json.Marshal(event.payload)
		_, err = db.ExecContext(context.Background(), `
			INSERT INTO coord.audit_log (work_item_id, event_type, principal, payload, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, workItemID, event.eventType, event.principal, payload, time.Now())
		if err != nil {
			t.Fatalf("failed to create audit log entry: %v", err)
		}
	}

	// Create audit log reader
	auditReader := NewDBAuditLogReader(db)

	// Create test server with audit log
	resolver := &StaticSessionResolver{Sessions: map[string]discussion.AuthorContext{
		devToken: {Principal: "user:alice", TeamID: teamID},
	}}
	
	srv := NewServer(Options{
		Authenticator: NewCookieAuthenticator(resolver),
		Discussion:    discussion.NewHandler(nil),
		Ready:         nil,
		AuditLog:      auditReader,
	})

	// Test basic query
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/audit/log", nil)
	req = withSession(req, devToken)
	
	srv.Handler().ServeHTTP(rec, req)
	
	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: got %d, want 200", rec.Code)
	}

	var response AuditResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}

	// Verify response structure
	if response.Total != 3 {
		t.Errorf("expected total 3 entries, got: %d", response.Total)
	}
	if len(response.Entries) != 3 {
		t.Errorf("expected 3 entries, got: %d", len(response.Entries))
	}

	// Test filtering by event type
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/audit/log?eventType=claim_acquired", nil)
	req = withSession(req, devToken)
	
	srv.Handler().ServeHTTP(rec, req)
	
	if rec.Code != http.StatusOK {
		t.Fatalf("filtering failed: got %d, want 200", rec.Code)
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse filtered response: %v", err)
	}

	if len(response.Entries) != 1 {
		t.Errorf("expected 1 entry for 'claim_acquired', got: %d", len(response.Entries))
	}
	if response.Entries[0].EventType != "claim_acquired" {
		t.Errorf("expected 'claim_acquired', got: %s", response.Entries[0].EventType)
	}

	// Test pagination
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/audit/log?limit=1", nil)
	req = withSession(req, devToken)
	
	srv.Handler().ServeHTTP(rec, req)
	
	if rec.Code != http.StatusOK {
		t.Fatalf("pagination failed: got %d, want 200", rec.Code)
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse paginated response: %v", err)
	}

	if len(response.Entries) != 1 {
		t.Errorf("expected 1 entry with limit=1, got: %d", len(response.Entries))
	}
	if response.Limit != 1 {
		t.Errorf("expected limit=1 in response, got: %d", response.Limit)
	}

	// Test work item filtering
	rec = httptest.NewRecorder()
	req = httptest.NewRequest(http.MethodGet, "/api/audit/log?workItemId="+workItemID.String(), nil)
	req = withSession(req, devToken)
	
	srv.Handler().ServeHTTP(rec, req)
	
	if rec.Code != http.StatusOK {
		t.Fatalf("work item filtering failed: got %d, want 200", rec.Code)
	}

	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatalf("failed to parse work item filtered response: %v", err)
	}

	if response.Total != 3 {
		t.Errorf("expected 3 entries for work item, got: %d", response.Total)
	}
}

// BenchmarkAuditLogQuery — performance test for audit log queries
func BenchmarkAuditLogQuery(b *testing.B) {
	// Setup similar to integration test but with synthetic data
	db, err := sql.Open("postgres", "host=localhost port=5432 user=ksquad password=ksquad dbname=ksquad_test sslmode=disable")
	if err != nil {
		b.Fatalf("failed to open database: %v", err)
	}
	defer db.Close()

	teamID := uuid.New()
	workItemID := uuid.New()

	// Insert test data
	for i := 0; i < 1000; i++ {
		payload, _ := json.Marshal(map[string]any{"iteration": i})
		_, err = db.ExecContext(context.Background(), `
			INSERT INTO coord.audit_log (work_item_id, event_type, principal, payload, created_at)
			VALUES ($1, $2, $3, $4, $5)
		`, workItemID, "test_event", "user:alice", payload, time.Now())
		if err != nil {
			b.Fatalf("failed to create audit log entry: %v", err)
		}
	}

	auditReader := NewDBAuditLogReader(db)
	query := AuditQuery{
		Limit:  100,
		Offset: 0,
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := auditReader.QueryAuditLog(context.Background(), query, teamID)
			if err != nil {
				b.Fatalf("query failed: %v", err)
			}
		}
	})
}