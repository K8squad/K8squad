//go:build handoffmirror_integration

package handoffmirror

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // database/sql driver "pgx" for the coord source

	"github.com/K8squad/K8squad/pkg/coord"
)

// TestSQLSource_Integration tests the SQL source against a real Postgres database
// when DATABASE_URL is set. This test applies migrations and tests the actual
// database queries used by the production mirror.
func TestSQLSource_Integration(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	// Apply migrations (in production this would be handled by the migration system)
	// For integration test, we create the required tables directly
	ctx := context.Background()

	// Create coord.audit_log table (minimal schema for testing)
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS coord.audit_log (
			id BIGSERIAL PRIMARY KEY,
			work_item_id UUID NOT NULL,
			run_id UUID NOT NULL,
			principal TEXT NOT NULL,
			fence_token BIGINT,
			payload TEXT NOT NULL,
			event_type TEXT NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create audit_log table: %v", err)
	}

	// Create coord.artifact table (minimal schema for testing)
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS coord.artifact (
			id BIGSERIAL PRIMARY KEY,
			work_item_id UUID NOT NULL,
			run_id UUID NOT NULL,
			kind TEXT NOT NULL,
			uri TEXT NOT NULL,
			sha256 TEXT NOT NULL,
			created_at TIMESTAMP WITH TIME ZONE NOT NULL
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create artifact table: %v", err)
	}

	// Create coord.work_item table (minimal schema for testing)
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS coord.work_item (
			id UUID PRIMARY KEY,
			project_id UUID NOT NULL,
			team_id UUID
		)
	`)
	if err != nil {
		t.Fatalf("Failed to create work_item table: %v", err)
	}

	// Test data
	now := time.Now()
	baseTime := now.Add(-time.Hour)

	// Insert test work items
	_, err = db.ExecContext(ctx, `
		INSERT INTO coord.work_item (id, project_id, team_id) VALUES 
		('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000002', '00000000-0000-0000-0000-000000000003'),
		('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000002', NULL)
	`)
	if err != nil {
		t.Fatalf("Failed to insert work items: %v", err)
	}

	// Insert test audit logs
	_, err = db.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, principal, fence_token, payload, event_type, created_at) VALUES 
		('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000005', 'agent-a', 1, '{"did":["test"]}', 'artifact_registered', $1),
		('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000006', 'agent-b', 2, '{"did":["test-no-team"]}', 'artifact_registered', $2)
	`, coord.HandoffKind, coord.AuditHandoffURI+"1", baseTime, coord.HandoffKind, coord.AuditHandoffURI+"2", baseTime.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Failed to insert audit logs: %v", err)
	}

	// Insert test artifacts
	_, err = db.ExecContext(ctx, `
		INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256, created_at) VALUES 
		('00000000-0000-0000-0000-000000000001', '00000000-0000-0000-0000-000000000005', $3, $4, 'hash1', $1),
		('00000000-0000-0000-0000-000000000004', '00000000-0000-0000-0000-000000000006', $5, $6, 'hash2', $2)
	`, coord.HandoffKind, coord.AuditHandoffURI+"1", baseTime, coord.HandoffKind, coord.AuditHandoffURI+"2", baseTime.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("Failed to insert artifacts: %v", err)
	}

	// Test the SQLSource
	src := NewSQLSource(db)

	// Test AllForMemoryMirror method
	rows, err := src.AllForMemoryMirror(ctx, baseTime.Add(-time.Hour), 100)
	if err != nil {
		t.Fatalf("AllForMemoryMirror failed: %v", err)
	}

	// Should return 2 rows: one with team, one without
	if len(rows) != 2 {
		t.Fatalf("Expected 2 rows, got %d", len(rows))
	}

	// Verify the first row (has team)
	row1 := rows[0]
	if row1.WorkItemID != "00000000-0000-0000-0000-000000000001" {
		t.Errorf("Expected work_item_id 00000000-0000-0000-0000-000000000001, got %s", row1.WorkItemID)
	}
	if row1.TeamID != "00000000-0000-0000-0000-000000000003" {
		t.Errorf("Expected team_id 00000000-0000-0000-0000-000000000003, got %s", row1.TeamID)
	}
	if row1.Principal != "agent-a" {
		t.Errorf("Expected principal agent-a, got %s", row1.Principal)
	}
	if row1.Payload != `{"did":["test"]}` {
		t.Errorf("Expected payload {\"did\":[\"test\"]}, got %s", row1.Payload)
	}

	// Verify the second row (no team)
	row2 := rows[1]
	if row2.WorkItemID != "00000000-0000-0000-0000-000000000004" {
		t.Errorf("Expected work_item_id 00000000-0000-0000-0000-000000000004, got %s", row2.WorkItemID)
	}
	if row2.TeamID != "" {
		t.Errorf("Expected empty team_id, got %s", row2.TeamID)
	}
	if row2.Principal != "agent-b" {
		t.Errorf("Expected principal agent-b, got %s", row2.Principal)
	}
	if row2.Payload != `{"did":["test-no-team"]}` {
		t.Errorf("Expected payload {\"did\":[\"test-no-team\"]}, got %s", row2.Payload)
	}

	// Test with limit
	rows, err = src.AllForMemoryMirror(ctx, baseTime.Add(-time.Hour), 1)
	if err != nil {
		t.Fatalf("AllForMemoryMirror with limit failed: %v", err)
	}

	if len(rows) != 1 {
		t.Fatalf("Expected 1 row with limit=1, got %d", len(rows))
	}

	// Test with since (should only get newer rows)
	rows, err = src.AllForMemoryMirror(ctx, baseTime.Add(5*time.Minute), 100)
	if err != nil {
		t.Fatalf("AllForMemoryMirror with since failed: %v", err)
	}

	// Should only get the second row (without team)
	if len(rows) != 1 {
		t.Fatalf("Expected 1 row since baseTime+5min, got %d", len(rows))
	}

	if rows[0].WorkItemID != "00000000-0000-0000-0000-000000000004" {
		t.Errorf("Expected work_item_id 00000000-0000-0000-0000-000000000004, got %s", rows[0].WorkItemID)
	}
}

// TestSQLSource_LimitRespected tests that the SQLSource respects the limit parameter
// to properly test batch behavior and watermark handling.
func TestSQLSource_LimitRespected(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set - skipping integration test")
	}

	db, err := sql.Open("pgx", os.Getenv("DATABASE_URL"))
	if err != nil {
		t.Fatalf("Failed to connect to database: %v", err)
	}
	defer db.Close()

	ctx := context.Background()

	// Clean up any existing test data
	db.ExecContext(ctx, "TRUNCATE coord.audit_log, coord.artifact, coord.work_item CASCADE")

	// Create test data with more rows than default limit
	now := time.Now()
	baseTime := now.Add(-time.Hour)

	// Insert multiple work items at the SAME deterministic ids the audit/artifact
	// rows reference (the JOIN in AllForMemoryMirror must find them).
	for i := 1; i <= 5; i++ {
		workItemId := fmt.Sprintf("00000000-0000-0000-0000-%012d", i)
		runId := fmt.Sprintf("00000000-0000-0000-0000-%012d", i+10)
		_, err = db.ExecContext(ctx, `
			INSERT INTO coord.work_item (id, project_id, team_id) VALUES
			($1, gen_random_uuid(), gen_random_uuid())
		`, workItemId)
		if err != nil {
			t.Fatalf("Failed to insert work item %d: %v", i, err)
		}
		// Insert audit log
		_, err = db.ExecContext(ctx, `
			INSERT INTO coord.audit_log (work_item_id, run_id, principal, fence_token, payload, event_type, created_at) 
		VALUES ($1, $2, 'agent-a', 1, '{"test":true}', 'artifact_registered', $3)
		`, workItemId, runId, baseTime.Add(time.Duration(i)*10*time.Minute))
		if err != nil {
			t.Fatalf("Failed to insert audit log %d: %v", i, err)
		}

		// Insert artifact
		_, err = db.ExecContext(ctx, `
			INSERT INTO coord.artifact (work_item_id, run_id, kind, uri, sha256, created_at) 
			VALUES ($1, $2, $3, $4, 'hash', $5)
		`, workItemId, runId, coord.HandoffKind, fmt.Sprintf("%s%d", coord.AuditHandoffURI, i), baseTime.Add(time.Duration(i)*10*time.Minute))
		if err != nil {
			t.Fatalf("Failed to insert artifact %d: %v", i, err)
		}
	}

	// Test with limit = 3
	src := NewSQLSource(db)
	rows, err := src.AllForMemoryMirror(ctx, baseTime.Add(-time.Hour), 3)
	if err != nil {
		t.Fatalf("AllForMemoryMirror with limit=3 failed: %v", err)
	}

	if len(rows) != 3 {
		t.Fatalf("Expected 3 rows with limit=3, got %d", len(rows))
	}

	// Test with limit = 0 (should use default)
	rows, err = src.AllForMemoryMirror(ctx, baseTime.Add(-time.Hour), 0)
	if err != nil {
		t.Fatalf("AllForMemoryMirror with limit=0 failed: %v", err)
	}

	// Should get default (100) or all remaining, whichever is smaller
	expected := 5 // total remaining
	if expected > 100 {
		expected = 100
	}
	if len(rows) != expected {
		t.Fatalf("Expected %d rows with limit=0, got %d", expected, len(rows))
	}

	// Test with limit = 1000 (should cap at 500)
	rows, err = src.AllForMemoryMirror(ctx, baseTime.Add(-time.Hour), 1000)
	if err != nil {
		t.Fatalf("AllForMemoryMirror with limit=1000 failed: %v", err)
	}

	expected = 5 // total remaining
	if expected > 500 {
		expected = 500
	}
	if len(rows) != expected {
		t.Fatalf("Expected %d rows with limit=1000, got %d", expected, len(rows))
	}
}
