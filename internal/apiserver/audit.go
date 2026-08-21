package apiserver

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Audit log query API (story 2.6 / ISI-2881) — the HTTP query interface for
// coord.audit_log with filtering by work item/actor/time and RBAC-scoped access.
// ============================================================================

// AuditQuery defines the query parameters for filtering audit log entries
type AuditQuery struct {
	WorkItemID *string `json:"workItemId,omitempty"`
	Actor      *string `json:"actor,omitempty"`
	RunID      *string `json:"runId,omitempty"`
	EventType  *string `json:"eventType,omitempty"`
	StartTime  *time.Time `json:"startTime,omitempty"`
	EndTime    *time.Time `json:"endTime,omitempty"`
	Limit      int    `json:"limit"`
	Offset     int    `json:"offset"`
}

// AuditEntry represents a single audit log entry
type AuditEntry struct {
	ID                   int64                `json:"id"`
	WorkItemID           *string              `json:"workItemId,omitempty"`
	RunID                *string              `json:"runId,omitempty"`
	EventType            string               `json:"eventType"`
	Principal            string               `json:"principal"`
	InitiatedByUserID    *string              `json:"initiatedByUserId,omitempty"`
	FenceToken           *int64               `json:"fenceToken,omitempty"`
	FromState            *string              `json:"fromState,omitempty"`
	ToState              *string              `json:"toState,omitempty"`
	Payload              map[string]any       `json:"payload,omitempty"`
	CreatedAt            time.Time            `json:"createdAt"`
}

// AuditResponse represents the response for an audit log query
type AuditResponse struct {
	Entries []AuditEntry `json:"entries"`
	Total   int64        `json:"total"`
	Offset  int          `json:"offset"`
	Limit   int          `json:"limit"`
}

// ErrNoAuditAccess is returned when the caller has no access to audit logs
var ErrNoAuditAccess = errors.New("apiserver: no access to audit logs")

// AuditLogReader provides access to the audit log with RBAC-scoped queries
type AuditLogReader interface {
	QueryAuditLog(ctx context.Context, query AuditQuery, teamID uuid.UUID) (AuditResponse, error)
}

// DBAuditLogReader implements AuditLogReader using direct database access
type DBAuditLogReader struct {
	db *sql.DB
}

// NewDBAuditLogReader creates a new audit log reader with database access
func NewDBAuditLogReader(db *sql.DB) *DBAuditLogReader {
	return &DBAuditLogReader{db: db}
}

// QueryAuditLog queries the audit log with the given parameters and team-based access control
func (r *DBAuditLogReader) QueryAuditLog(ctx context.Context, query AuditQuery, teamID uuid.UUID) (AuditResponse, error) {
	// Build the base query with team-based access control
	baseQuery := `
		SELECT a.id, a.work_item_id, a.run_id, a.event_type, a.principal, 
		       a.initiated_by_user_id, a.fence_token, a.from_state, a.to_state,
		       a.payload, a.created_at
		FROM coord.audit_log a
		LEFT JOIN coord.work_item w ON a.work_item_id = w.id
		WHERE w.team_id = $1
	`
	
	// Build the query parameters starting with team_id
	params := []any{teamID}
	paramCount := 1
	
	// Add WHERE conditions for each filter
	if query.WorkItemID != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.work_item_id = $%d", paramCount)
		params = append(params, query.WorkItemID)
	}
	
	if query.Actor != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.principal = $%d", paramCount)
		params = append(params, *query.Actor)
	}
	
	if query.RunID != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.run_id = $%d", paramCount)
		params = append(params, query.RunID)
	}
	
	if query.EventType != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.event_type = $%d", paramCount)
		params = append(params, *query.EventType)
	}
	
	if query.StartTime != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.created_at >= $%d", paramCount)
		params = append(params, query.StartTime)
	}
	
	if query.EndTime != nil {
		paramCount++
		baseQuery += fmt.Sprintf(" AND a.created_at <= $%d", paramCount)
		params = append(params, query.EndTime)
	}
	
	// Add ordering by ID (monotonic sequence)
	baseQuery += ` ORDER BY a.id DESC`
	
	// Get the total count first
	var total int64
	countQuery := `SELECT COUNT(*) FROM coord.audit_log a LEFT JOIN coord.work_item w ON a.work_item_id = w.id WHERE w.team_id = $1`
	if err := r.db.QueryRowContext(ctx, countQuery, teamID).Scan(&total); err != nil {
		return AuditResponse{}, err
	}
	
	// Apply pagination
	if query.Limit <= 0 {
		query.Limit = 100 // Default limit
	}
	if query.Limit > 1000 {
		query.Limit = 1000 // Maximum limit
	}
	
	if query.Offset < 0 {
		query.Offset = 0
	}
	
	baseQuery += fmt.Sprintf(" LIMIT $%d OFFSET $%d", paramCount+1, paramCount+2)
	params = append(params, query.Limit, query.Offset)
	
	// Execute the query
	rows, err := r.db.QueryContext(ctx, baseQuery, params...)
	if err != nil {
		return AuditResponse{}, err
	}
	defer rows.Close()
	
	// Parse the results
	var entries []AuditEntry
	for rows.Next() {
		var entry AuditEntry
		var payloadStr []byte
		
		// Scan the row into our struct
		var workItemID, runID, initiatedByUserID sql.NullString
		var fenceToken sql.NullInt64
		
		err := rows.Scan(
			&entry.ID,
			&workItemID,
			&runID,
			&entry.EventType,
			&entry.Principal,
			&initiatedByUserID,
			&fenceToken,
			&entry.FromState,
			&entry.ToState,
			&payloadStr,
			&entry.CreatedAt,
		)
		
		// Handle nullable fields
		if workItemID.Valid {
			entry.WorkItemID = &workItemID.String
		}
		if runID.Valid {
			entry.RunID = &runID.String
		}
		if initiatedByUserID.Valid {
			entry.InitiatedByUserID = &initiatedByUserID.String
		}
		if fenceToken.Valid {
			ft := fenceToken.Int64
			entry.FenceToken = &ft
		}
		if err != nil {
			return AuditResponse{}, err
		}
		
		// Parse JSON payload if it exists
		if payloadStr != nil {
			if err := json.Unmarshal(payloadStr, &entry.Payload); err != nil {
				// If JSON parsing fails, store the raw string
				entry.Payload = map[string]any{"raw": string(payloadStr)}
			}
		}
		
		// Note: UUIDs are already converted from sql.NullString to strings above
		// No additional conversion needed since we're working with string types
		
		entries = append(entries, entry)
	}
	
	if err = rows.Err(); err != nil {
		return AuditResponse{}, err
	}
	
	return AuditResponse{
		Entries: entries,
		Total:   total,
		Offset:  query.Offset,
		Limit:   query.Limit,
	}, nil
}

// queryAuditLog is the handler behind GET /api/audit/log
func (s *Server) queryAuditLog(reader AuditLogReader) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}
		
		// Parse query parameters from the URL
		query := AuditQuery{
			Limit: 100, // Default limit
		}
		
		// Parse workItemId
		if workItemId := r.URL.Query().Get("workItemId"); workItemId != "" {
			query.WorkItemID = &workItemId
		}
		
		// Parse actor
		if actor := r.URL.Query().Get("actor"); actor != "" {
			query.Actor = &actor
		}
		
		// Parse runId
		if runId := r.URL.Query().Get("runId"); runId != "" {
			query.RunID = &runId
		}
		
		// Parse eventType
		if eventType := r.URL.Query().Get("eventType"); eventType != "" {
			query.EventType = &eventType
		}
		
		// Parse startTime
		if startTimeStr := r.URL.Query().Get("startTime"); startTimeStr != "" {
			if startTime, err := time.Parse(time.RFC3339, startTimeStr); err == nil {
				query.StartTime = &startTime
			}
		}
		
		// Parse endTime
		if endTimeStr := r.URL.Query().Get("endTime"); endTimeStr != "" {
			if endTime, err := time.Parse(time.RFC3339, endTimeStr); err == nil {
				query.EndTime = &endTime
			}
		}
		
		// Parse limit
		if limitStr := r.URL.Query().Get("limit"); limitStr != "" {
			if limit, err := parseInt(limitStr); err == nil {
				query.Limit = limit
			}
		}
		
		// Parse offset
		if offsetStr := r.URL.Query().Get("offset"); offsetStr != "" {
			if offset, err := parseInt(offsetStr); err == nil {
				query.Offset = offset
			}
		}
		
		// Query the audit log
		response, err := reader.QueryAuditLog(r.Context(), query, auth.TeamID)
		if err != nil {
			writeJSONError(w, http.StatusInternalServerError, "failed to query audit log")
			return
		}
		
		writeJSON(w, http.StatusOK, response)
	}
}

// Helper function to parse string to int
func parseInt(s string) (int, error) {
	var i int
	_, err := fmt.Sscanf(s, "%d", &i)
	return i, err
}