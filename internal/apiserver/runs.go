package apiserver

import (
	"context"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
	"github.com/K8squad/K8squad/internal/discussion"
)

// ============================================================================
// Run listing and detail endpoints (ISI-4571) — global/fleet run listing,
// project-scoped run listing, and run detail with steps/comments
//
// Pattern: reuse the DashboardService informer cache pattern from dashboard.go
// for listing, and combine coord audit records with progressmirror comments
// for detail.
// ============================================================================

// RunListQuery aggregates the filter/pagination query params
type RunListQuery struct {
	Phase      string    `json:"phase"` // optional filter
	Agent      string    `json:"agent"`  // optional filter
	Window     string    `json:"window"` // optional time window
	Limit      int       `json:"limit"`  // pagination limit
	Offset     int       `json:"offset"` // pagination offset
	ProjectID  string    `json:"projectId"`  // if set, filter to project
}

// RunListItem is one row in a run listing (reuses RunSummary pattern from org.go)
type RunListItem struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Phase           string     `json:"phase"`
	PausedReason    *string    `json:"pausedReason,omitempty"`
	WorkItemRef     string     `json:"workItemRef"`
	ProjectRef      string     `json:"projectRef"`
	StartedAt       *time.Time `json:"startedAt,omitempty"`
	EndedAt         *time.Time `json:"endedAt,omitempty"`
	DurationSeconds *int64     `json:"durationSeconds,omitempty"`
	TraceID         string     `json:"traceId,omitempty"`
}

// RunDetailResponse is the full run detail with steps and thinking
type RunDetailResponse struct {
	Run           *ksquadv1.Run                 `json:"run"`
	Steps         []StepInfo                   `json:"steps"`
	Thinking      []ThinkingEntry              `json:"thinking"`
	LLMInteractions []LLMInteractionDigest      `json:"llmInteractions"`
}

// StepInfo represents a reconcile_step from coord.claim + audit events
type StepInfo struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	Description  string    `json:"description"`
	StartedAt    *time.Time `json:"startedAt,omitempty"`
	CompletedAt  *time.Time `json:"completedAt,omitempty"`
	Status       string    `json:"status"`
	Error        *string   `json:"error,omitempty"`
}

// ThinkingEntry represents agent thinking/comments from progressmirror
type ThinkingEntry struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"` // "comment" | "tool_use" | "observation"
	Content   string    `json:"content"`
	Agent     string    `json:"agent,omitempty"`
	Timestamp time.Time `json:"timestamp"`
}

// LLMInteractionDigest summarizes LLM interactions from run status
type LLMInteractionDigest struct {
	ID          string    `json:"id"`
	Model       string    `json:"model"`
	Role        string    `json:"role"`
	Content     string    `json:"content"`
	Timestamp   time.Time `json:"timestamp"`
	TokensUsed  int       `json:"tokensUsed,omitempty"`
}

// RunsService provides run listing and detail read models
type RunsService struct {
	reader client.Reader
	db     interface{} // sql.DB or nil for fallback mode
}

// NewRunsService builds the runs read model. reader MUST have api/v1alpha1 registered.
func NewRunsService(reader client.Reader) *RunsService {
	return &RunsService{reader: reader, db: nil}
}

// NewRunsServiceWithDB builds the runs read model with database access for step/thinking population.
func NewRunsServiceWithDB(reader client.Reader, db interface{}) *RunsService {
	return &RunsService{reader: reader, db: db}
}

// listRuns answers GET /api/runs (global listing) and /api/projects/{id}/runs (project-scoped)
func listRuns(svc *RunsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}

		// Parse query params
		query := RunListQuery{
			Limit:  50,  // default
			Offset: 0,
		}
		
		if phase := r.URL.Query().Get("phase"); phase != "" {
			query.Phase = phase
		}
		if agent := r.URL.Query().Get("agent"); agent != "" {
			query.Agent = agent
		}
		if window := r.URL.Query().Get("window"); window != "" {
			query.Window = window
		}
		if limit := r.URL.Query().Get("limit"); limit != "" {
			if l, err := strconv.Atoi(limit); err == nil && l > 0 && l <= 200 {
				query.Limit = l
			}
		}
		if offset := r.URL.Query().Get("offset"); offset != "" {
			if o, err := strconv.Atoi(offset); err == nil && o >= 0 {
				query.Offset = o
			}
		}

		// For project-scoped routes, extract project ID from path
		var projectID string
		if projectID = mux.Vars(r)["projectId"]; projectID != "" {
			query.ProjectID = projectID
		}

		// Determine namespace scope
		var namespace string
		var err error
		if auth.IsAdmin {
			// Admin can see across all namespaces
			namespace = "" // fleet-wide
		} else {
			// Non-admins are limited to their team namespace
			namespace = teamNamespace(auth.TeamID.String())
			if namespace == "" {
				writeJSONError(w, http.StatusNotFound, "no team scope for this user")
				return
			}
		}

		runs, err := svc.listRunsInNamespace(r.Context(), namespace, query, auth)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, runs)
	}
}

// listRunsInNamespace implements the core listing logic with filtering and pagination
func (s *RunsService) listRunsInNamespace(ctx context.Context, namespace string, query RunListQuery, auth discussion.AuthorContext) ([]RunListItem, error) {
	var runs ksquadv1.RunList
	if err := s.reader.List(ctx, &runs, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	// Filter and project
	var filtered []RunListItem
	for i := range runs.Items {
		run := &runs.Items[i]
		
		// Apply project filter if specified
		if query.ProjectID != "" && run.Spec.ProjectRef.Name != query.ProjectID {
			continue
		}
		
		// Apply phase filter if specified
		if query.Phase != "" && string(run.Status.Phase) != query.Phase {
			continue
		}
		
		// Apply agent filter if specified
		if query.Agent != "" {
			matched := false
			for _, agent := range run.Spec.Agents {
				if agent.Name == query.Agent {
					matched = true
					break
				}
			}
			if !matched {
				continue
			}
		}

		// Apply time window filter if specified
		if query.Window != "" {
			if !runInTimeWindow(run, query.Window) {
				continue
			}
		}

		item := runListItem(run)
		filtered = append(filtered, item)
	}

	// Sort by start time (newest first)
	sort.Slice(filtered, func(a, b int) bool {
		at, bt := filtered[a].StartedAt, filtered[b].StartedAt
		if at == nil && bt == nil {
			return false
		}
		if at == nil {
			return false
		}
		if bt == nil {
			return true
		}
		return !at.Before(*bt)
	})

	// Apply pagination
	if query.Offset >= len(filtered) {
		return []RunListItem{}, nil
	}
	end := query.Offset + query.Limit
	if end > len(filtered) {
		end = len(filtered)
	}

	return filtered[query.Offset:end], nil
}

// getRunDetail answers GET /api/runs/{id} with steps, thinking, and interactions
func getRunDetail(svc *RunsService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		auth, ok := discussion.AuthFromContext(r.Context())
		if !ok || auth.Principal == "" {
			writeJSONError(w, http.StatusUnauthorized, "unauthenticated")
			return
		}

		runID := mux.Vars(r)["runId"]
		if runID == "" {
			writeJSONError(w, http.StatusBadRequest, "run ID required")
			return
		}

		// Determine namespace scope
		var namespace string
		if auth.IsAdmin {
			namespace = "" // fleet-wide
		} else {
			namespace = teamNamespace(auth.TeamID.String())
			if namespace == "" {
				writeJSONError(w, http.StatusNotFound, "no team scope for this user")
				return
			}
		}

		detail, err := svc.getRunDetailInNamespace(r.Context(), namespace, runID)
		if err != nil {
			writeJSONError(w, http.StatusBadGateway, err.Error())
			return
		}

		writeJSON(w, http.StatusOK, detail)
	}
}

// getRunDetailInNamespace fetches a single run's detail with enriched data
func (s *RunsService) getRunDetailInNamespace(ctx context.Context, namespace, runID string) (*RunDetailResponse, error) {
	var run ksquadv1.Run
	if err := s.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: runID}, &run); err != nil {
		return nil, err
	}

	response := &RunDetailResponse{
		Run: &run,
	}

	// Populate steps from coord.claim.reconcile_step and audit_log
	if run.Spec.WorkItemRef != "" {
		if err := s.populateSteps(ctx, response); err != nil {
			// Log but don't fail - steps are optional
			fmt.Printf("populateSteps for run %s: %v", runID, err)
		}
	}

	// Populate thinking/comments from progressmirror comments and LLM interactions
	if run.Spec.WorkItemRef != "" {
		if err := s.populateThinking(ctx, response); err != nil {
			// Log but don't fail - thinking is optional
			fmt.Printf("populateThinking for run %s: %v", runID, err)
		}
	}

	return response, nil
}

// populateSteps reads the Run's execution steps from coord.claim.reconcile_step and audit_log.
func (s *RunsService) populateSteps(ctx context.Context, response *RunDetailResponse) error {
	db, ok := s.db.(*sql.DB)
	if !ok || db == nil || response.Run.Status.WorkItemID == "" {
		// Fallback for testing/demo: create placeholder steps based on Run status
		now := time.Now()
		if response.Run.Status.ClaimedAt != nil {
			steps := []Step{
				{
					ID:        "1",
					Action:    "claimed",
					Status:    "completed",
					StartedAt: response.Run.Status.ClaimedAt,
					CompletedAt: response.Run.Status.ClaimedAt,
					Agent:     response.Run.Spec.Agents[0].Name,
				},
			}
			if response.Run.Status.Phase == ksquadv1.RunPhaseRunning {
				steps = append(steps, Step{
					ID:        "2",
					Action:    "dispatching",
					Status:    "running",
					StartedAt: response.Run.Status.ClaimedAt,
					Agent:     response.Run.Spec.Agents[0].Name,
				})
			} else if response.Run.Status.Phase == ksquadv1.RunPhaseComplete {
				steps = append(steps, Step{
					ID:        "2",
					Action:    "executing",
					Status:    "completed",
					StartedAt: response.Run.Status.ClaimedAt,
					CompletedAt: &now,
					Agent:     response.Run.Spec.Agents[0].Name,
				})
			}
			response.Steps = steps
		}
		return nil
	}

	// Query coord.audit_log for step events
	rows, err := db.QueryContext(ctx, `
		SELECT from_state, to_state, principal, created_at, event_type
		FROM coord.audit_log 
		WHERE work_item_id = $1::uuid 
		  AND event_type IN ('state_transition', 'claim_acquired', 
	                          'reconcile_advanced', 'run_terminal')
		ORDER BY id DESC 
		LIMIT 10`, response.Run.Spec.WorkItemRef)
	if err != nil {
		return fmt.Errorf("read steps: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var steps []Step
	for rows.Next() {
		var sc Step
		var from sql.NullString
		if err := rows.Scan(&from, &sc.Action, &sc.Agent, &sc.StartedAt, &sc.Status); err != nil {
			return fmt.Errorf("scan step: %w", err)
		}
		sc.ID = sc.Action
		if from.Valid {
			sc.PreviousState = from.String
		}
		steps = append(steps, sc)
	}

	// Reverse to get chronological order
	for i, j := 0, len(steps)-1; i < j; i, j = i+1, j-1 {
		steps[i], steps[j] = steps[j], steps[i]
	}

	response.Steps = steps
	return nil
}

// populateThinking reads the Run's thinking/comments from progressmirror comments and LLM interactions.
func (s *RunsService) populateThinking(ctx context.Context, response *RunDetailResponse) error {
	db, ok := s.db.(*sql.DB)
	if !ok || db == nil || response.Run.Status.WorkItemID == "" {
		// Fallback for testing/demo: create placeholder thinking
		if response.Run.Status.LLMInteractions != nil {
			thinking := []Thinking{}
			for _, interaction := range response.Run.Status.LLMInteractions {
				thinking = append(thinking, Thinking{
					Type:    "llm_interaction",
					Content: interaction.Prompt,
					Agent:   interaction.Agent,
					Time:    interaction.Timestamp,
				})
			}
			response.Thinking = thinking
		}
		return nil
	}

	// Query coord.comment for progressmirror comments
	rows, err := db.QueryContext(ctx, `
		SELECT author_principal, body, created_at
		FROM coord.comment 
		WHERE work_item_id = $1::uuid 
		ORDER BY created_at ASC
		LIMIT 20`, response.Run.Spec.WorkItemRef)
	if err != nil {
		return fmt.Errorf("read comments: %w", err)
	}
	defer func() { _ = rows.Close() }()

	for rows.Next() {
		var comment Thinking
		if err := rows.Scan(&comment.Agent, &comment.Content, &comment.Time); err != nil {
			return fmt.Errorf("scan comment: %w", err)
		}
		comment.Type = "comment"
		response.Thinking = append(response.Thinking, comment)
	}

	// Add LLM interactions from status
	if response.Run.Status.LLMInteractions != nil {
		for _, interaction := range response.Run.Status.LLMInteractions {
			response.Thinking = append(response.Thinking, Thinking{
				Type:    "llm_interaction",
				Content: interaction.Prompt,
				Agent:   interaction.Agent,
				Time:    interaction.Timestamp,
			})
		}
	}

	return nil
}

// runListItem projects a Run into a listing item (reuses org.go pattern)
func runListItem(run *ksquadv1.Run) RunListItem {
	startedAt, endedAt := runStart(run), runEnd(run)
	duration := int64(0)
	if startedAt != nil && endedAt != nil {
		duration = endedAt.Sub(startedAt).Milliseconds() / 1000
	}

	pausedReason := (*string)(nil)
	for _, condition := range run.Status.Conditions {
		if condition.Type == "Paused" {
			pausedReason = &condition.Reason
			break
		}
	}

	return RunListItem{
		ID:              run.Name,
		Name:            run.Name,
		Phase:           string(run.Status.Phase),
		PausedReason:    pausedReason,
		WorkItemRef:     run.Spec.WorkItemRef,
		ProjectRef:      run.Spec.ProjectRef.Name,
		StartedAt:       startedAt,
		EndedAt:         endedAt,
		DurationSeconds: &duration,
		TraceID:         run.Status.TraceID,
	}
}



// runInTimeWindow checks if a run falls within the specified time window
func runInTimeWindow(run *ksquadv1.Run, window string) bool {
	startTime := runStart(run)
	
	switch window {
	case "1h":
		return startTime.After(time.Now().Add(-1 * time.Hour))
	case "24h":
		return startTime.After(time.Now().Add(-24 * time.Hour))
	case "7d":
		return startTime.After(time.Now().Add(-7 * 24 * time.Hour))
	default:
		return true // unknown window, include all
	}
}

// teamNamespace maps a Team UUID to its namespace (simplified for now)
func teamNamespace(teamID string) string {
	// TODO: Proper team-to-namespace mapping from Team CR
	// For now, return empty string for admin/empty team
	if teamID == "" || teamID == "00000000-0000-0000-0000-000000000000" {
		return ""
	}
	return "team-" + strings.ToLower(teamID[:8]) // simple hash for demo
}