// ISI-5536 (ISI-5531 E2) — the decision_request interaction (ADR-0026 §4).
//
// A decision_request is the net-new "an agent suggests structured options a human answers" surface.
// It copies the proposal.go pattern wholesale: an ordinary append-only discussion message with
// kind='decision_request' and a structured payload (the question, the modes, the options), INERT by
// construction — posting it writes no coord row and moves no custody. The lifecycle
// `open → answered | rejected | expired | superseded` lives in the discussion.decision_request side
// table (0031) because the message row itself is append-only (0004 permits only the invalidated_at
// soft-retract) and must stay custody-free.
//
// ONE flexible kind with a `mode` (approve/choose_one/choose_many/free_form) plus a free-text
// fallback — the smallest surface vs. Paperclip's four kinds (ADR-0026 D3). The room never executes:
// AnswerDecisionRequest/RejectDecisionRequest are CAS transitions on the decision record; the
// agent-resume fan-out (re-dispatch the raising agent so it reads the answer from the thread) happens
// in the apiserver answer shell (internal/apiserver/decisionrequest.go), zero new custody semantics.
package discussion

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// ============================================================================
// Kind, modes, phases
// ============================================================================

const (
	// KindDecisionRequest marks a decision-request message (rendered as the decision card).
	KindDecisionRequest = "decision_request"

	// Decision modes (ADR-0026 §4.1). One kind, four modes + a free-text fallback.
	DecisionModeApprove    = "approve"     // a single accept/reject
	DecisionModeChooseOne  = "choose_one"  // pick exactly one option
	DecisionModeChooseMany = "choose_many" // pick N options (min/max selected)
	DecisionModeFreeForm   = "free_form"   // open-ended text answer

	// Decision phases (the decision state machine). Named `phase` (not `state`) so no custody token
	// from the fence's forbidden list enters the discussion schema (see 0031).
	DecisionPhaseOpen       = "open"
	DecisionPhaseAnswered   = "answered"
	DecisionPhaseRejected   = "rejected"
	DecisionPhaseExpired    = "expired"
	DecisionPhaseSuperseded = "superseded"

	// DefaultContinuation is the only continuation v1 ships: resume the raising agent by re-dispatch
	// once the human answers (ADR-0026 §5). The agent reads the typed answer from the thread.
	DefaultContinuation = "resume_agent_on_answer"
)

// DecisionOption is one structured choice offered to the human (ADR-0026 §4.1).
type DecisionOption struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
	Recommended bool   `json:"recommended,omitempty"`
}

// DecisionRequestPayload is the structured payload of a kind='decision_request' message
// (ADR-0026 §4.1, competitive-analysis §4).
type DecisionRequestPayload struct {
	Version                  int              `json:"version"`
	Mode                     string           `json:"mode"`
	Title                    string           `json:"title"`
	DetailsMarkdown          string           `json:"detailsMarkdown,omitempty"`
	Options                  []DecisionOption `json:"options,omitempty"`
	AllowFreeText            bool             `json:"allowFreeText,omitempty"`
	FreeTextLabel            string           `json:"freeTextLabel,omitempty"`
	MinSelected              int              `json:"minSelected,omitempty"`
	MaxSelected              int              `json:"maxSelected,omitempty"`
	DefaultSelectedOptionIDs []string         `json:"defaultSelectedOptionIds,omitempty"`
	AllowReject              bool             `json:"allowReject,omitempty"`
	RejectRequiresReason     bool             `json:"rejectRequiresReason,omitempty"`
}

// DecisionTarget binds a card to what it decides on (ADR-0026 §4.1). A moving revisionId is what the
// supersede CAS watches (ExpireDecisionRequest).
type DecisionTarget struct {
	Type       string `json:"type,omitempty"` // plan|diff|file|none
	Ref        string `json:"ref,omitempty"`
	RevisionID string `json:"revisionId,omitempty"`
}

// DecisionAnswer is the typed answer returned to the agent via the thread (ADR-0026 §4.3).
type DecisionAnswer struct {
	Mode              string     `json:"mode"`
	SelectedOptionIDs []string   `json:"selectedOptionIds,omitempty"`
	FreeText          *string    `json:"freeText,omitempty"`
	Rejected          bool       `json:"rejected"`
	RejectReason      *string    `json:"rejectReason,omitempty"`
	AnsweredBy        string     `json:"answeredBy,omitempty"`
	AnsweredAt        *time.Time `json:"answeredAt,omitempty"`
}

// DecisionRequest is a decision-request message joined with its lifecycle row. TeamID is the owning
// thread's team (the room's tenancy) so the apiserver answer shell can re-dispatch without a second
// lookup.
type DecisionRequest struct {
	Message        Message
	TeamID         uuid.UUID
	Payload        DecisionRequestPayload
	Target         *DecisionTarget `json:"target,omitempty"`
	WorkItemID     string          `json:"workItemId,omitempty"` // coord ticket the card decides on (BE-6/BE-7); "" ⇒ project-wide
	IdempotencyKey string          `json:"idempotencyKey"`
	Continuation   string          `json:"continuation"`
	Phase          string          `json:"phase"`
	Answer         *DecisionAnswer `json:"answer,omitempty"`
	RejectReason   string          `json:"rejectReason,omitempty"`
	AnsweredBy     string          `json:"answeredBy,omitempty"`
	AnsweredAt     *time.Time      `json:"answeredAt,omitempty"`
}

// Validate enforces the per-mode minimum contract before anything is persisted.
func (p DecisionRequestPayload) Validate() error {
	if p.Title == "" {
		return fmt.Errorf("%w: a decision_request requires a title", ErrInvalidDecisionPayload)
	}
	switch p.Mode {
	case DecisionModeApprove, DecisionModeFreeForm:
		// approve carries no options; free_form answers via text (allowFreeText is implied).
	case DecisionModeChooseOne, DecisionModeChooseMany:
		if len(p.Options) == 0 {
			return fmt.Errorf("%w: mode %q requires at least one option", ErrInvalidDecisionPayload, p.Mode)
		}
		seen := make(map[string]struct{}, len(p.Options))
		for _, o := range p.Options {
			if o.ID == "" || o.Label == "" {
				return fmt.Errorf("%w: every option requires an id and a label", ErrInvalidDecisionPayload)
			}
			if _, dup := seen[o.ID]; dup {
				return fmt.Errorf("%w: duplicate option id %q", ErrInvalidDecisionPayload, o.ID)
			}
			seen[o.ID] = struct{}{}
		}
	default:
		return fmt.Errorf("%w: unknown decision mode %q", ErrInvalidDecisionPayload, p.Mode)
	}
	return nil
}

// ============================================================================
// Errors
// ============================================================================

var (
	// ErrDecisionRequestNotFound — the message is not a decision_request, is retracted, or is outside
	// the caller's Team scope (404-not-403, auth parity with proposals).
	ErrDecisionRequestNotFound = errors.New("discussion: decision_request not found")
	// ErrDecisionRequestNotOpen — the lifecycle already left `open` (answered/rejected/expired/
	// superseded); answer and reject are one-shot decisions.
	ErrDecisionRequestNotOpen = errors.New("discussion: decision_request is no longer open")
	// ErrInvalidDecisionPayload — payload failed Validate (400).
	ErrInvalidDecisionPayload = errors.New("discussion: invalid decision_request payload")
	// ErrRejectReasonRequired — reject called without a reason on a card that requires one (400).
	ErrRejectReasonRequired = errors.New("discussion: this decision_request requires a reason to reject")
)

// ============================================================================
// Store — decision_request lifecycle
// ============================================================================

// PostDecisionRequest appends an inert kind='decision_request' message and its phase='open' lifecycle
// row in one transaction. Agents and humans alike may ask — asking is conversation, not execution
// (same invariant as proposals). The idempotencyKey makes create idempotent: a retry re-posting the
// same key returns the EXISTING card (no second row, no second question), per ADR-0026 §4.2.
func (s *Store) PostDecisionRequest(ctx context.Context, projectID string, teamID, threadID uuid.UUID, auth AuthorContext, body string, payload DecisionRequestPayload, target *DecisionTarget, workItemID, idempotencyKey string, parentID *uuid.UUID) (*Message, error) {
	if body == "" {
		return nil, ErrEmptyBody
	}
	if idempotencyKey == "" {
		return nil, fmt.Errorf("%w: idempotencyKey is required", ErrInvalidDecisionPayload)
	}
	if payload.Version == 0 {
		payload.Version = 1
	}
	if err := payload.Validate(); err != nil {
		return nil, err
	}
	if err := s.assertThreadInScope(ctx, projectID, teamID, threadID); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	payloadJSON := json.RawMessage(raw)

	var targetJSON []byte
	var boundRev sql.NullString
	if target != nil {
		if targetJSON, err = json.Marshal(target); err != nil {
			return nil, err
		}
		if target.RevisionID != "" {
			boundRev = sql.NullString{String: target.RevisionID, Valid: true}
		}
	}

	// work_item_id is a plain uuid column (no coord FK — the fence); validate the caller's id so a
	// malformed ref is a 400 at post time, not a cast error mid-insert. "" ⇒ a project-wide ask with
	// no continuation target (SQL NULL).
	var workItem uuid.NullUUID
	if workItemID != "" {
		wid, perr := uuid.Parse(workItemID)
		if perr != nil {
			return nil, fmt.Errorf("%w: workItemId %q is not a uuid", ErrInvalidDecisionPayload, workItemID)
		}
		workItem = uuid.NullUUID{UUID: wid, Valid: true}
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	m := Message{
		ThreadID:        threadID,
		ParentID:        parentID,
		AuthorPrincipal: auth.Principal,
		AuthorAgentID:   auth.AgentID,
		AuthorRunID:     auth.RunID,
		Body:            body,
		Audience:        "party", // decision cards are room-visible: the party decides
		Kind:            KindDecisionRequest,
		Payload:         &payloadJSON,
	}
	err = tx.QueryRowContext(ctx, `
		INSERT INTO discussion.message
		    (thread_id, parent_id, author_principal, author_agent_id, author_run_id, body, audience, kind, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		RETURNING id, created_at`,
		threadID, parentID, auth.Principal, auth.agentID(), auth.runID(), body, m.Audience, m.Kind, raw,
	).Scan(&m.ID, &m.CreatedAt)
	if err != nil {
		return nil, fmt.Errorf("post decision_request message: %w", err)
	}

	// Idempotent create: a duplicate idempotency_key means this card already exists — roll the whole
	// tx back (discarding the just-inserted message) and return the existing card.
	tag, err := tx.ExecContext(ctx, `
		INSERT INTO discussion.decision_request
		    (message_id, phase, idempotency_key, work_item_id, target, bound_revision_id, continuation)
		VALUES ($1, 'open', $2, $3, $4, $5, $6)
		ON CONFLICT (idempotency_key) DO NOTHING`,
		m.ID, idempotencyKey, workItem, nullJSON(targetJSON), boundRev, DefaultContinuation)
	if err != nil {
		return nil, fmt.Errorf("post decision_request lifecycle: %w", err)
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		_ = tx.Rollback()
		existing, gerr := s.getDecisionRequestByIdempotencyKey(ctx, projectID, teamID, idempotencyKey)
		if gerr != nil {
			return nil, gerr
		}
		return &existing.Message, nil
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &m, nil
}

// decisionSelect is the shared projection for GetDecisionRequest / the list reads / the idempotency
// lookup — the message joined with its lifecycle row through thread, tenancy-scoped by the caller.
const decisionSelect = `
	SELECT m.id, m.thread_id, m.parent_id, m.author_principal, m.author_agent_id, m.author_run_id,
	       m.body, m.audience, m.kind, m.payload, m.created_at, m.invalidated_at,
	       t.team_id, d.phase, d.idempotency_key, d.work_item_id, d.target, d.continuation,
	       d.answer, d.reject_reason, d.answered_by, d.answered_at
	FROM discussion.decision_request d
	JOIN discussion.message m ON m.id = d.message_id
	JOIN discussion.thread t  ON t.id = m.thread_id`

// scanDecisionRequest hydrates one DecisionRequest from a decisionSelect row.
func scanDecisionRequest(sc interface{ Scan(...any) error }) (*DecisionRequest, error) {
	var d DecisionRequest
	var payload, targetJSON, answerJSON []byte
	var parentID, workItem uuid.NullUUID
	var agentID, runID, answeredBy, rejectReason sql.NullString
	var answeredAt sql.NullTime
	if err := sc.Scan(
		&d.Message.ID, &d.Message.ThreadID, &parentID, &d.Message.AuthorPrincipal,
		&agentID, &runID, &d.Message.Body, &d.Message.Audience, &d.Message.Kind,
		&payload, &d.Message.CreatedAt, &d.Message.InvalidatedAt,
		&d.TeamID, &d.Phase, &d.IdempotencyKey, &workItem, &targetJSON, &d.Continuation,
		&answerJSON, &rejectReason, &answeredBy, &answeredAt,
	); err != nil {
		return nil, err
	}
	if workItem.Valid {
		d.WorkItemID = workItem.UUID.String()
	}
	if parentID.Valid {
		pid := parentID.UUID
		d.Message.ParentID = &pid
	}
	if agentID.Valid {
		d.Message.AuthorAgentID = &agentID.String
	}
	if runID.Valid {
		d.Message.AuthorRunID = &runID.String
	}
	if err := json.Unmarshal(payload, &d.Payload); err != nil {
		return nil, fmt.Errorf("discussion: decision_request %s has a malformed payload: %w", d.Message.ID, err)
	}
	if len(targetJSON) > 0 {
		var tgt DecisionTarget
		if err := json.Unmarshal(targetJSON, &tgt); err == nil {
			d.Target = &tgt
		}
	}
	if len(answerJSON) > 0 {
		var ans DecisionAnswer
		if err := json.Unmarshal(answerJSON, &ans); err == nil {
			d.Answer = &ans
		}
	}
	if rejectReason.Valid {
		d.RejectReason = rejectReason.String
	}
	if answeredBy.Valid {
		d.AnsweredBy = answeredBy.String
	}
	if answeredAt.Valid {
		t := answeredAt.Time
		d.AnsweredAt = &t
	}
	return &d, nil
}

// GetDecisionRequest returns the decision_request message + lifecycle row, tenancy-scoped through
// thread (404-not-403 on a cross-tenant probe).
func (s *Store) GetDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID) (*DecisionRequest, error) {
	row := s.db.QueryRowContext(ctx, decisionSelect+`
		WHERE d.message_id = $1 AND t.project_id = $2 AND t.team_id = $3 AND m.invalidated_at IS NULL`,
		messageID, projectID, teamID)
	d, err := scanDecisionRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDecisionRequestNotFound
	}
	return d, err
}

// getDecisionRequestByIdempotencyKey resolves the existing card on the idempotent-create path.
func (s *Store) getDecisionRequestByIdempotencyKey(ctx context.Context, projectID string, teamID uuid.UUID, idempotencyKey string) (*DecisionRequest, error) {
	row := s.db.QueryRowContext(ctx, decisionSelect+`
		WHERE d.idempotency_key = $1 AND t.project_id = $2 AND t.team_id = $3 AND m.invalidated_at IS NULL`,
		idempotencyKey, projectID, teamID)
	d, err := scanDecisionRequest(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrDecisionRequestNotFound
	}
	return d, err
}

// inboxArmLimit caps each Inbox read arm (ADR-0026 §9 fleet fan-out bound), mirroring ListWorkItems'
// LIMIT 500. A truncated read is logged by the handler (no silent cap).
const inboxArmLimit = 500

// OpenDecisionSummary is one open (phase='open') decision_request for the Inbox aggregate
// (ISI-5536 BE-7, ADR-0026 §3.2) — the sibling of OpenProposalSummary. ProjectUID is the Project CR
// id from discussion.thread.project_id (the inbox handler resolves it to "namespace/name"); TicketID
// is the coord.work_item the card decides on (work_item_id), used for the run-join ordering. Mode is
// the decision mode, surfaced as the row's decisionType chip (approve/choose_one/choose_many/free_form).
type OpenDecisionSummary struct {
	MessageID   string    `json:"messageId"`
	ProjectUID  string    `json:"-"` // internal: resolved to ns/name by the inbox handler
	AuthorAgent string    `json:"-"` // agent name that raised the card; "" ⇒ unknown
	Title       string    `json:"title"`
	Mode        string    `json:"mode"`
	TicketID    string    `json:"ticketId,omitempty"` // work_item_id the card binds to; "" ⇒ project-wide
	CreatedAt   time.Time `json:"createdAt"`
}

// ListOpenDecisionRequestsForTeam returns all open (phase='open', not invalidated) decision_requests
// for the given team (fleet when teamID="" — the admin→fleet widening), newest-first, capped. The
// third union arm of GET /api/squad/inbox (ISI-5536 BE-7, ADR-0026 §3.2); it mirrors
// ListOpenProposalsForTeam's tenancy scoping and flat-summary shape so the inbox handler treats all
// three arms identically. Limited to inboxArmLimit rows.
func (s *Store) ListOpenDecisionRequestsForTeam(ctx context.Context, teamID string) ([]OpenDecisionSummary, error) {
	var teamParam any
	if teamID != "" {
		teamParam = teamID
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT m.id::text, t.project_id::text, m.author_agent_id, m.payload,
		       d.work_item_id, m.created_at
		  FROM discussion.decision_request d
		  JOIN discussion.message m ON m.id = d.message_id
		  JOIN discussion.thread t  ON t.id = m.thread_id
		 WHERE d.phase = 'open'
		   AND m.invalidated_at IS NULL
		   AND ($1::uuid IS NULL OR t.team_id = $1::uuid)
		 ORDER BY m.created_at DESC
		 LIMIT `+fmt.Sprint(inboxArmLimit), teamParam)
	if err != nil {
		return nil, fmt.Errorf("discussion.ListOpenDecisionRequestsForTeam: query: %w", err)
	}
	defer rows.Close()
	var out []OpenDecisionSummary
	for rows.Next() {
		var ds OpenDecisionSummary
		var agentID, workItemID sql.NullString
		var payload []byte
		if err := rows.Scan(&ds.MessageID, &ds.ProjectUID, &agentID, &payload, &workItemID, &ds.CreatedAt); err != nil {
			return nil, fmt.Errorf("discussion.ListOpenDecisionRequestsForTeam: scan: %w", err)
		}
		if agentID.Valid {
			ds.AuthorAgent = agentID.String
		}
		if workItemID.Valid {
			ds.TicketID = workItemID.String
		}
		var pp DecisionRequestPayload
		if len(payload) > 0 {
			if jsonErr := json.Unmarshal(payload, &pp); jsonErr == nil {
				ds.Title = pp.Title
				ds.Mode = pp.Mode
			}
		}
		out = append(out, ds)
	}
	return out, rows.Err()
}

// ListDecisionRequests returns EVERY decision_request card in a thread with its lifecycle phase and
// answer, tenancy-scoped through thread, oldest first (transcript order) — the FE-4 read side, the
// mirror of ListProposals. The message read stays custody-free and phase-less; the ticket-detail
// card renderer joins this list onto the messages by id to show durable card state (open/answered/
// rejected/expired/superseded) after a reload.
func (s *Store) ListDecisionRequests(ctx context.Context, projectID string, teamID, threadID uuid.UUID) ([]DecisionRequest, error) {
	rows, err := s.db.QueryContext(ctx, decisionSelect+`
		WHERE m.thread_id = $1 AND t.project_id = $2 AND t.team_id = $3 AND m.invalidated_at IS NULL
		ORDER BY m.created_at ASC`, threadID, projectID, teamID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DecisionRequest{}
	for rows.Next() {
		d, err := scanDecisionRequest(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *d)
	}
	return out, rows.Err()
}

// AnswerDecisionRequest CAS-advances open→answered, stamps the deciding human, persists the typed
// answer, and in the SAME transaction appends the kind='structured' post-back parented to the card
// (the audit trail: Q + options + answer + who/when, all in the thread — the agent reads it on its
// next run). Single-winner, like ConfirmProposal: the loser of a concurrent answer gets
// ErrDecisionRequestNotOpen. Returns the post-back message so the shell can fan out without a re-read.
//
// Human-only is enforced at the handler (an agent may ask, never answer its own ask), exactly like
// proposalconfirm.go.
func (s *Store) AnswerDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID, auth AuthorContext, answer DecisionAnswer) (*Message, error) {
	answer.AnsweredBy = auth.Principal
	answer.Rejected = false
	answerJSON, err := json.Marshal(answer)
	if err != nil {
		return nil, err
	}
	body := "Decision answered: " + decisionAnswerLine(answer)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tag, err := tx.ExecContext(ctx, `
		UPDATE discussion.decision_request d
		   SET phase = 'answered', answered_by = $4, answered_at = now(), answer = $5, updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE d.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND d.message_id = $3 AND m.invalidated_at IS NULL AND d.phase = 'open'`,
		projectID, teamID, messageID, auth.Principal, answerJSON)
	if err != nil {
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, s.decisionCASMiss(ctx, projectID, teamID, messageID)
	}

	postBack, err := s.postResultMessage(ctx, tx, projectID, teamID, messageID, auth, answerJSON, body)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return postBack, nil
}

// RejectDecisionRequest CAS-advances open→rejected with the human's reason, appending the same
// kind='structured' post-back so the agent sees the rejection + reason on its next run. The handler
// enforces rejectRequiresReason before calling in (ErrRejectReasonRequired).
func (s *Store) RejectDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID, auth AuthorContext, reason string) (*Message, error) {
	ans := DecisionAnswer{Rejected: true, AnsweredBy: auth.Principal}
	if reason != "" {
		r := reason
		ans.RejectReason = &r
	}
	answerJSON, err := json.Marshal(ans)
	if err != nil {
		return nil, err
	}
	body := "Decision rejected"
	if reason != "" {
		body += ": " + reason
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	tag, err := tx.ExecContext(ctx, `
		UPDATE discussion.decision_request d
		   SET phase = 'rejected', answered_by = $4, answered_at = now(), answer = $5,
		       reject_reason = $6, updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE d.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND d.message_id = $3 AND m.invalidated_at IS NULL AND d.phase = 'open'`,
		projectID, teamID, messageID, auth.Principal, answerJSON, sql.NullString{String: reason, Valid: reason != ""})
	if err != nil {
		return nil, err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return nil, s.decisionCASMiss(ctx, projectID, teamID, messageID)
	}

	postBack, err := s.postResultMessage(ctx, tx, projectID, teamID, messageID, auth, answerJSON, body)
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return postBack, nil
}

// ExpireDecisionRequest CAS-advances open→(expired|superseded) when the card is stale — the bound
// target revision moved or a later human comment superseded it (the Paperclip anti-nag lesson: a
// bypassed decision must auto-resolve, never re-spin). No post-back and no fan-out — an unanswered
// card that aged out carries no human intent to relay. `to` must be DecisionPhaseExpired or
// DecisionPhaseSuperseded. Returns ErrDecisionRequestNotOpen if the card was already decided.
func (s *Store) ExpireDecisionRequest(ctx context.Context, projectID string, teamID, messageID uuid.UUID, to string) error {
	if to != DecisionPhaseExpired && to != DecisionPhaseSuperseded {
		return fmt.Errorf("%w: expire target must be expired or superseded, got %q", ErrInvalidDecisionPayload, to)
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.decision_request d
		   SET phase = $4, updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE d.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND d.message_id = $3 AND m.invalidated_at IS NULL AND d.phase = 'open'`,
		projectID, teamID, messageID, to)
	if err != nil {
		return err
	}
	if n, _ := tag.RowsAffected(); n == 0 {
		return s.decisionCASMiss(ctx, projectID, teamID, messageID)
	}
	return nil
}

// SupersedeStaleDecisionRequests auto-expires every OPEN card in a thread whose bound revision no
// longer matches `currentRevisionID` — the revision-moved supersede sweep (ADR-0026 §4.2/§5). Cards
// with no bound revision are left alone (they have nothing to go stale against). Returns the count
// superseded. Best-effort: called from the answer/revision seam, never blocks the human path.
func (s *Store) SupersedeStaleDecisionRequests(ctx context.Context, projectID string, teamID uuid.UUID, currentRevisionID string) (int64, error) {
	if currentRevisionID == "" {
		return 0, nil
	}
	tag, err := s.db.ExecContext(ctx, `
		UPDATE discussion.decision_request d
		   SET phase = 'superseded', updated_at = now()
		FROM discussion.message m JOIN discussion.thread t ON t.id = m.thread_id
		WHERE d.message_id = m.id AND t.project_id = $1 AND t.team_id = $2
		  AND d.phase = 'open' AND m.invalidated_at IS NULL
		  AND d.bound_revision_id IS NOT NULL AND d.bound_revision_id <> $3`,
		projectID, teamID, currentRevisionID)
	if err != nil {
		return 0, err
	}
	n, _ := tag.RowsAffected()
	return n, nil
}

// decisionCASMiss distinguishes "invisible" (404) from "visible but not open" (409) after a CAS that
// affected no rows — the same probe ConfirmProposal uses.
func (s *Store) decisionCASMiss(ctx context.Context, projectID string, teamID, messageID uuid.UUID) error {
	if _, gerr := s.GetDecisionRequest(ctx, projectID, teamID, messageID); gerr != nil {
		return gerr
	}
	return ErrDecisionRequestNotOpen
}

// decisionAnswerLine is the human-readable post-back body under the decision card.
func decisionAnswerLine(a DecisionAnswer) string {
	switch {
	case len(a.SelectedOptionIDs) == 1:
		return a.SelectedOptionIDs[0]
	case len(a.SelectedOptionIDs) > 1:
		s := a.SelectedOptionIDs[0]
		for _, id := range a.SelectedOptionIDs[1:] {
			s += ", " + id
		}
		return s
	case a.FreeText != nil && *a.FreeText != "":
		return *a.FreeText
	default:
		return "(no selection)"
	}
}

// nullJSON returns a nil interface for empty jsonb so the column stores SQL NULL rather than an empty
// string (which jsonb would reject).
func nullJSON(b []byte) any {
	if len(b) == 0 {
		return nil
	}
	return b
}
