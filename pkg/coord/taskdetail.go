// taskdetail.go — the SHARED richer work-item read model (ISI-3601 S2 task 1,
// designed once with ISI-3600 S1). Both consumers use this ONE read:
//
//   - S1's context assembler `Sources.WorkItem` (the push side — snapshots the
//     task into SystemContext at dispatch).
//   - S2's agent-facing `get-task` endpoint (the pull side — pkg/taskio, which
//     projects a TaskDetail onto its JSON wire shape).
//
// The existing dispatch read (sqlDispatchSource.WorkItem, rundrive) returns
// only title+body — deliberately thin for the v1 envelope. This read is the
// richer one both stories called for: title, description, board state,
// blocked-reason, the comment thread, and the current claim/fence state.
//
// AcceptanceCriteria and Goals are part of the agreed shape but have NO
// first-class column in the 0001 coord schema yet, so ReadTaskDetail leaves
// them nil. Wiring a first-class AC/goals surface (a column or a structured
// body convention) is tracked follow-up work and must land in exactly one
// place so both consumers pick it up — do NOT parse them ad hoc per caller.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/lib/pq"
)

// CommentRef is one structured ticket LINK a human comment carried from the
// ticket-detail `#`-picker (ISI-5214, parent ISI-5212 S2). It is the coord-native
// mirror of discussion.TicketRef (internal/discussion/dispatch.go): WorkItemID is
// the linked coord work-item UUID and Title its display title for chip rendering.
// The JSON tags match the discussion wire shape so the console renders a ticket
// comment's links with the SAME parseReferences path it uses for room messages.
// A ref is a LINK, never a dispatch and never a write fence — it is durable
// metadata persisted on coord.comment.payload under the `references` key.
type CommentRef struct {
	WorkItemID string `json:"workItemId"`
	Title      string `json:"title,omitempty"`
}

// TaskComment is one append-only note on a work item (coord.comment), in
// chronological order.
type TaskComment struct {
	Author    string    `json:"author"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"createdAt"`
	// Kind is the two-phase class (ISI-7094): 'human' (default top-level comment),
	// 'summary' (the one per-run recap), or 'progress' (a run sub-step). The
	// default top-level read (readComments) returns only human+summary; progress
	// is read separately via ReadRunActivity. Empty on the wire only for a reader
	// that predates 0042 (never for a row this server wrote — kind is NOT NULL).
	Kind string `json:"kind,omitempty"`
	// RunID is the Run a progress/summary row belongs to (ISI-7094); "" for a
	// human comment and for legacy progress rows written before 0042.
	RunID string `json:"runId,omitempty"`
	// References are the structured ticket links the comment carried (ISI-5214);
	// read straight off coord.comment.payload, never a render-time refetch. nil /
	// omitted for a plain comment, so an older reader and a link-free comment are
	// indistinguishable on the wire.
	References []CommentRef `json:"references,omitempty"`
	// Mentions are the structured agent references the comment carried
	// (ISI-7220 B3, epic ISI-7075): agent names persisted under
	// coord.comment.payload `mentions` by the agent-comment path, read
	// straight off the row — never a render-time roster refetch. nil /
	// omitted for a plain comment. A mention is durable metadata, never
	// a dispatch (FR-B3).
	Mentions []string `json:"mentions,omitempty"`
}

// TaskDetail is the canonical richer read of one work item and its coordination
// state. It is the single shared shape (S1 push / S2 pull) — see file header.
type TaskDetail struct {
	WorkItemID    string
	Title         string
	Description   string // coord.work_item.body
	State         string
	BlockedReason string
	// Priority / WorkMode / Labels are the create-time attributes (ISI-4409).
	// Priority/WorkMode are "" when unset (honest "none", FR-I3); Labels is
	// never nil (an item with no labels reads as an empty slice).
	Priority string
	WorkMode string
	Labels   []string
	// RequestedAgent is the human's pre-run agent choice (requested_agent,
	// ADR-0022 §3 D2 / mig 0021 — ISI-4574): "" when the item was never
	// dispatched or re-assigned. It is the only "who" the detail can answer
	// with BEFORE a claim exists; once a run claims, Holder/RunID/Assignee
	// take over.
	RequestedAgent string
	// AcceptanceCriteria / Goals: agreed shape, not yet backed by a column
	// (nil today). See file header — one wiring site when the surface lands.
	AcceptanceCriteria []string
	Goals              []string
	Comments           []TaskComment
	// ChangeRefs is the agent-reported change summary (commit SHAs / PR links,
	// M1.5/ISI-4131) in chronological order — the third reporting surface next
	// to comments and the state column.
	ChangeRefs []ChangeRef
	// Claim/fence state (coord.claim). FenceToken is the §6.2 monotonic token
	// every artifact write is checked against; Holder is the current lease
	// holder principal (empty ⇒ unclaimed); RunID is the holding run;
	// Assignee is the agent attribution of the current/last attempt
	// (assignee_agent, ISI-4237 — survives the terminal release).
	FenceToken int64
	Holder     string
	RunID      string
	Assignee   string
}

// ReadTaskDetail reads the richer detail for one work item. It is read-only and
// safe for concurrent use. ErrWorkItemNotFound if the item does not exist.
func ReadTaskDetail(ctx context.Context, db *sql.DB, workItemID string) (TaskDetail, error) {
	if db == nil {
		return TaskDetail{}, errors.New("coord.ReadTaskDetail: nil db")
	}
	if workItemID == "" {
		return TaskDetail{}, fmt.Errorf("coord.ReadTaskDetail: workItemID required")
	}

	var (
		td             TaskDetail
		body           sql.NullString
		blockedReason  sql.NullString
		priority       sql.NullString
		workMode       sql.NullString
		requestedAgent sql.NullString
		holder         sql.NullString
		runID          sql.NullString
		assignee       sql.NullString
		fence          sql.NullInt64
	)
	// One row: the item joined to its (always-present, 0001 trigger-provisioned)
	// claim row. LEFT JOIN keeps the read robust even if a claim row were ever
	// missing (reads as unclaimed/fence 0 rather than erroring).
	//
	// labels is text[] and MUST be scanned via pq.Array: on Go < 1.27 the pgx
	// stdlib driver hands database/sql the raw array literal as a *string*, which
	// convertAssign cannot put into a []string (a direct &td.Labels scan errors at
	// runtime → 500). pq.Array's sql.Scanner parses that literal. Do not "simplify"
	// it back to &td.Labels. (ISI-4409.)
	//
	// requested_agent (mig 0021) sits between the create-time attributes and the
	// claim columns — the pre-run intent the console detail renders (ISI-4574).
	err := db.QueryRowContext(ctx, `
		SELECT wi.id::text, wi.title, wi.body, wi.state, wi.blocked_reason,
		       wi.priority, wi.work_mode, wi.labels, wi.requested_agent,
		       c.holder_principal, c.run_id::text, c.fence_token, c.assignee_agent
		  FROM coord.work_item wi
		  LEFT JOIN coord.claim c ON c.work_item_id = wi.id
		 WHERE wi.id = $1::uuid`, workItemID).
		Scan(&td.WorkItemID, &td.Title, &body, &td.State, &blockedReason,
			&priority, &workMode, pq.Array(&td.Labels), &requestedAgent,
			&holder, &runID, &fence, &assignee)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return TaskDetail{}, ErrWorkItemNotFound
	case err != nil:
		return TaskDetail{}, fmt.Errorf("coord.ReadTaskDetail: read work item %s: %w", workItemID, err)
	}
	td.Description = body.String
	td.BlockedReason = blockedReason.String
	td.Priority = priority.String
	td.WorkMode = workMode.String
	if td.Labels == nil {
		td.Labels = []string{}
	}
	td.RequestedAgent = requestedAgent.String
	td.Holder = holder.String
	td.RunID = runID.String
	td.FenceToken = fence.Int64
	td.Assignee = assignee.String

	comments, err := readComments(ctx, db, workItemID)
	if err != nil {
		return TaskDetail{}, err
	}
	td.Comments = comments

	changeRefs, err := readChangeRefs(ctx, db, workItemID)
	if err != nil {
		return TaskDetail{}, err
	}
	td.ChangeRefs = changeRefs
	return td, nil
}

// readComments reads the DEFAULT top-level thread (ISI-7094): human + summary
// comments only. Progress rows (the run sub-step firehose the ProgressMirror
// mirrors — [run …][tool]…[status]) are STORED but excluded here so the thread
// reads like a conversation, not a log; the console surfaces them separately
// under its per-run "Activity (N events)" expander via ReadRunActivity.
func readComments(ctx context.Context, db *sql.DB, workItemID string) ([]TaskComment, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT author_principal, body, created_at, payload, kind, run_id::text
		  FROM coord.comment
		 WHERE work_item_id = $1::uuid
		   AND kind IN ('human', 'summary')
		 ORDER BY created_at ASC, id ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: read comments for %s: %w", workItemID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskComment
	for rows.Next() {
		var c TaskComment
		var payload []byte
		var runID sql.NullString
		if err := rows.Scan(&c.Author, &c.Body, &c.CreatedAt, &payload, &c.Kind, &runID); err != nil {
			return nil, fmt.Errorf("coord.ReadTaskDetail: scan comment: %w", err)
		}
		c.RunID = runID.String
		c.References = referencesFromPayload(payload)
		c.Mentions = mentionsFromPayload(payload)
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ReadTaskDetail: iterate comments: %w", err)
	}
	return out, nil
}

// ReadRunActivity reads the PROGRESS rows for a work item — the run sub-steps
// (tool results, status transitions, LLM digests, narration) the default thread
// (readComments) excludes. It is the console's "Activity (N events)" backing
// query (ISI-7095): chronological, carrying each row's run id so the client can
// group the feed per run. A legacy progress row written before 0042 carries a
// NULL run_id ("" here); the console lumps those into one legacy group. Read-only
// and safe for concurrent use.
func ReadRunActivity(ctx context.Context, db *sql.DB, workItemID string) ([]TaskComment, error) {
	if db == nil {
		return nil, errors.New("coord.ReadRunActivity: nil db")
	}
	if workItemID == "" {
		return nil, fmt.Errorf("coord.ReadRunActivity: workItemID required")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT author_principal, body, created_at, kind, run_id::text
		  FROM coord.comment
		 WHERE work_item_id = $1::uuid
		   AND kind = 'progress'
		 ORDER BY created_at ASC, id ASC`, workItemID)
	if err != nil {
		return nil, fmt.Errorf("coord.ReadRunActivity: read progress for %s: %w", workItemID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []TaskComment
	for rows.Next() {
		var c TaskComment
		var runID sql.NullString
		if err := rows.Scan(&c.Author, &c.Body, &c.CreatedAt, &c.Kind, &runID); err != nil {
			return nil, fmt.Errorf("coord.ReadRunActivity: scan progress: %w", err)
		}
		c.RunID = runID.String
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ReadRunActivity: iterate progress: %w", err)
	}
	return out, nil
}

// referencesFromPayload reads the structured ticket links off a comment's jsonb
// payload under the `references` key (ISI-5214), the coord-native mirror of
// discussion.ReferencesOf. A NULL/empty/malformed payload — every pre-0028 row and
// every plain comment — yields nil, so a link-free comment stays link-free.
func referencesFromPayload(payload []byte) []CommentRef {
	if len(payload) == 0 {
		return nil
	}
	var wrapper struct {
		References []CommentRef `json:"references"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return nil
	}
	return wrapper.References
}

// mentionsFromPayload reads the structured agent references off a
// comment's jsonb payload under the `mentions` key (ISI-7220 B3). A
// NULL/empty/malformed payload — every pre-B3 row and every plain
// comment — yields nil, so a mention-free comment stays mention-free.
func mentionsFromPayload(payload []byte) []string {
	if len(payload) == 0 {
		return nil
	}
	var wrapper struct {
		Mentions []string `json:"mentions"`
	}
	if err := json.Unmarshal(payload, &wrapper); err != nil {
		return nil
	}
	return wrapper.Mentions
}

// AppendComment appends one provenanced comment to a work item and returns it.
// This is the SANCTIONED comment-write path (§6.1) both S2's post-comment and
// any other agent write must use — a plain INSERT into the append-only
// coord.comment table (the reject_mutation trigger forbids edit/delete). The
// author is server-supplied (from the run token's principal), never client
// text. ErrWorkItemNotFound if the item does not exist (surfaced from the FK).
func AppendComment(ctx context.Context, db *sql.DB, workItemID, author, body string) (TaskComment, error) {
	if db == nil {
		return TaskComment{}, errors.New("coord.AppendComment: nil db")
	}
	if workItemID == "" || author == "" || body == "" {
		return TaskComment{}, fmt.Errorf("coord.AppendComment: workItemID, author and body are required")
	}
	var created time.Time
	err := db.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body)
		VALUES ($1::uuid, $2, $3)
		RETURNING created_at`, workItemID, author, body).Scan(&created)
	if err != nil {
		// A dangling work item trips the FK (ON DELETE RESTRICT / missing parent).
		return TaskComment{}, fmt.Errorf("coord.AppendComment: insert comment for %s: %w", workItemID, err)
	}
	return TaskComment{Author: author, Body: body, CreatedAt: created}, nil
}

// AppendProgress appends one PROGRESS comment (ISI-7094) — a run sub-step the
// operator-side ProgressMirror mirrors ([run …][tool]…[status], LLM digests).
// It is stored on the SAME append-only coord.comment table as every other
// comment but tagged kind='progress' and stamped with its run id, so the default
// thread (readComments) excludes it and the console groups it per run under the
// Activity expander (ReadRunActivity). runID may be "" for a progress event that
// could not be keyed to a run (it then joins the legacy ungrouped Activity bucket).
func AppendProgress(ctx context.Context, db *sql.DB, workItemID, author, runID, body string) (TaskComment, error) {
	if db == nil {
		return TaskComment{}, errors.New("coord.AppendProgress: nil db")
	}
	if workItemID == "" || author == "" || body == "" {
		return TaskComment{}, fmt.Errorf("coord.AppendProgress: workItemID, author and body are required")
	}
	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}
	var created time.Time
	err := db.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
		VALUES ($1::uuid, $2, $3, 'progress', $4::uuid)
		RETURNING created_at`, workItemID, author, body, runParam).Scan(&created)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendProgress: insert comment for %s: %w", workItemID, err)
	}
	return TaskComment{Author: author, Body: body, CreatedAt: created, Kind: "progress", RunID: runID}, nil
}

// AppendSummary appends one SUMMARY comment (ISI-7094) — the per-run structured
// recap a run owes the thread (the agent authors it as its final act; the
// platform synthesizes a minimal one at settle when the agent omits it, so the
// "exactly one summary" invariant holds structurally — see settle.go). It is a
// first-class top-level comment (readComments includes kind='summary'), stamped
// with its run id so settle can answer "did THIS run already summarise?" before
// synthesizing. Callers must not embed the [run/[tool/[status progress tokens in
// body — a summary is the human-readable recap, never a log line.
func AppendSummary(ctx context.Context, db *sql.DB, workItemID, author, runID, body string) (TaskComment, error) {
	if db == nil {
		return TaskComment{}, errors.New("coord.AppendSummary: nil db")
	}
	if workItemID == "" || author == "" || body == "" {
		return TaskComment{}, fmt.Errorf("coord.AppendSummary: workItemID, author and body are required")
	}
	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}
	var created time.Time
	err := db.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body, kind, run_id)
		VALUES ($1::uuid, $2, $3, 'summary', $4::uuid)
		RETURNING created_at`, workItemID, author, body, runParam).Scan(&created)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendSummary: insert comment for %s: %w", workItemID, err)
	}
	return TaskComment{Author: author, Body: body, CreatedAt: created, Kind: "summary", RunID: runID}, nil
}

// AppendInitialFindings appends the run's ONE early initial-findings note
// (ADR-0029 Option B) AND records an 'initial_findings_authored' §6.5 audit row
// in the SAME transaction. The comment lands in the thread exactly like
// AppendComment; the audit row is the durable signal ISI-5602's pending-writeback
// query selects on (event_type='initial_findings_authored'). The note body travels
// in the audit payload so the mirror-to-GitHub step reads it straight off the row
// — the trio work_item_id / run_id / principal are first-class audit columns.
// Author is server-supplied (the run token's principal), never client text; runID
// is the reporting Run (provenance, may be empty). Fence NULL (ADR-037: authoring
// is not a custody op), initiator "agent" — mirrors AppendChangeRef. A dangling
// work item trips the FK (ErrWorkItemNotFound surfaced by the caller's mapper).
func AppendInitialFindings(ctx context.Context, db *sql.DB, workItemID, author, runID, body string) (TaskComment, error) {
	if db == nil {
		return TaskComment{}, errors.New("coord.AppendInitialFindings: nil db")
	}
	if workItemID == "" || author == "" || body == "" {
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: workItemID, author and body are required")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	var created time.Time
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.comment (work_item_id, author_principal, body)
		VALUES ($1::uuid, $2, $3)
		RETURNING created_at`, workItemID, author, body).Scan(&created); err != nil {
		// A dangling work item trips the FK (ON DELETE RESTRICT / missing parent).
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: insert comment for %s: %w", workItemID, err)
	}

	runParam := sql.NullString{}
	if runID != "" {
		runParam = sql.NullString{String: runID, Valid: true}
	}

	// §6.5 provenance, same txn: the note body is mirrored into payload so 5602's
	// pending-writeback query reads it without re-walking the comment thread. Fence
	// NULL (ADR-037), initiator "agent" — this is the run authoring its own note.
	payload, err := json.Marshal(map[string]any{
		"initiator": "agent",
		"body":      body,
	})
	if err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, run_id, event_type, principal, payload)
		VALUES ($1::uuid, $2::uuid, 'initial_findings_authored', $3, $4::jsonb)`,
		workItemID, runParam, author, string(payload)); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: audit: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return TaskComment{}, fmt.Errorf("coord.AppendInitialFindings: commit: %w", err)
	}
	return TaskComment{Author: author, Body: body, CreatedAt: created}, nil
}
