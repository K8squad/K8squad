// workitemwrite.go — Story S3 (ISI-3959): the human CREATE + FIELD-EDIT custody
// ops for coord.work_item, the write siblings of humanstate.go's lane transition.
//
// The Kanban board is a PROJECTION of coord.work_item (§13 board-derivation); the
// state write path (humanstate.go) let a human MOVE a card, but there was no path
// to CREATE an item or EDIT its fields (title/body/parent). This file adds those
// two ops on the same custody side of the house so the HTTP surface
// (internal/apiserver) stays a thin auth+mapping shell.
//
// Symmetry with humanstate.go is deliberate and load-bearing:
//   - teamID scopes tenancy (§12.1): an item/parent outside the caller's Team is
//     invisible — ErrWorkItemNotFound (404), never a cross-tenant 403. Pass "" only
//     for a trusted, already-tenancy-checked caller (fleet-admin path, ISI-3937).
//   - a field edit carries an optimistic-concurrency guard (expectedUpdatedAt), the
//     edit-side analogue of the state path's fromState: a racing change is rejected
//     ErrStateConflict (409) instead of silently clobbered (last-write-wins).
//   - every write records §6.5 audit provenance in the SAME transaction, fence NULL
//     (ADR-037: neither create nor edit is a custody/lease operation).
//
// STATE IS NOT A FIELD HERE. A create lands in the default entry lane ('backlog',
// §8.6); an edit never touches state. Lane moves keep their own contract
// (humanstate.go, ISI-2909) so the board-derivation invariant (§13) is preserved.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// ErrInvalidWorkItem — the create/edit input is malformed (empty title, no editable
// field, a self-parent, or a reparent that would form a cycle). Maps to 400.
var ErrInvalidWorkItem = errors.New("coord: invalid work item write")

// WorkItemRecord is the persisted item a create/edit returns — the columns the read
// model (and the board projection) draw from. ParentID / TeamID are pointers so a
// root item (no parent) and an unscoped item (no team) serialise as null, not "".
type WorkItemRecord struct {
	ID        string    `json:"id"`
	ProjectID string    `json:"projectId"`
	TeamID    *string   `json:"teamId,omitempty"`
	ParentID  *string   `json:"parentId,omitempty"`
	Title     string    `json:"title"`
	Body      string    `json:"body,omitempty"`
	State     string    `json:"state"`
	CreatedBy string    `json:"createdBy"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// CreateWorkItemInput is one create. ProjectID + Title are required; ParentID makes
// it a sub-issue (inheriting the parent's team, §6.1). TeamID is the caller's Team
// scope: for a root item it becomes the item's team; for a sub-issue it is the
// visibility predicate the parent must satisfy (the child then inherits the parent's
// team, not TeamID). Principal is the §6.5 author; InitiatedByUserID the §12.4
// on-behalf-of id.
type CreateWorkItemInput struct {
	ProjectID         string
	TeamID            string
	ParentID          string
	Title             string
	Body              string
	Principal         string
	InitiatedByUserID string
}

// UpdateWorkItemInput is one field edit. Each editable field is a pointer so an
// absent field is "leave unchanged" and a present-but-empty field is a real clear
// (e.g. blank the body). State is intentionally NOT here — lane moves have their own
// path. ExpectedUpdatedAt, when set, is the optimistic-concurrency precondition (the
// updated_at the client last read); a mismatch is ErrStateConflict (409).
type UpdateWorkItemInput struct {
	Title             *string
	Body              *string
	ParentID          *string // reparent; "" ⇒ detach to root
	ExpectedUpdatedAt string
	TeamID            string
	Principal         string
	InitiatedByUserID string
}

// WorkItemWriteStore executes the human create/edit ops against the shipped coord
// schema (0001). Like HumanStateStore it holds no mutable state beyond *sql.DB, so
// its methods are safe for concurrent use (each write opens its own transaction).
type WorkItemWriteStore struct {
	db *sql.DB
}

// NewWorkItemWriteStore binds the create/edit ops to db.
func NewWorkItemWriteStore(db *sql.DB) (*WorkItemWriteStore, error) {
	if db == nil {
		return nil, errors.New("coord.NewWorkItemWriteStore: nil db")
	}
	return &WorkItemWriteStore{db: db}, nil
}

// CreateWorkItem inserts one work item in the default entry lane ('backlog'),
// atomically with a §6.5 'work_item_created' audit row (fence NULL, ADR-037).
//
// Semantics:
//   - (record, nil): the item exists; created_by/team/parent are as resolved.
//   - (zero, ErrInvalidWorkItem): empty title, or a self-referential parent (400).
//   - (zero, ErrWorkItemNotFound): a ParentID that does not exist / is outside the
//     caller's Team / belongs to another project — existence-hiding (404).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) CreateWorkItem(ctx context.Context, in CreateWorkItemInput) (WorkItemRecord, error) {
	if in.ProjectID == "" || in.Title == "" || in.Principal == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: projectID, title and principal are required", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.CreateWorkItem: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Team the new row lands in. For a root item it is the caller's Team; a sub-issue
	// overrides this with the parent's team below (§6.1 child-inherits-parent).
	teamParam := sql.NullString{}
	if in.TeamID != "" {
		teamParam = sql.NullString{String: in.TeamID, Valid: true}
	}
	parentParam := sql.NullString{}

	if in.ParentID != "" {
		// Resolve + tenancy-check the parent under a row lock: an invisible parent is
		// 404 (existence-hiding), a cross-project parent is rejected, and the child
		// inherits the parent's team so the tree stays tenancy-consistent (§6.1).
		var parentProject string
		var parentTeam sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT project_id, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
			in.ParentID).Scan(&parentProject, &parentTeam)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return WorkItemRecord{}, ErrWorkItemNotFound
		case err != nil:
			return WorkItemRecord{}, fmt.Errorf("coord.CreateWorkItem: read parent: %w", err)
		}
		if in.TeamID != "" && (!parentTeam.Valid || parentTeam.String != in.TeamID) {
			return WorkItemRecord{}, ErrWorkItemNotFound // parent invisible to this caller
		}
		if parentProject != in.ProjectID {
			return WorkItemRecord{}, ErrWorkItemNotFound // no cross-project sub-issues
		}
		parentParam = sql.NullString{String: in.ParentID, Valid: true}
		teamParam = parentTeam // inherit parent's team (may be NULL)
	}

	var rec WorkItemRecord
	var teamOut, parentOut sql.NullString
	var body sql.NullString
	err = tx.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, body, state, created_by)
		VALUES ($1::uuid, $2::uuid, $3::uuid, $4, NULLIF($5,''), 'backlog', $6)
		RETURNING id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at`,
		in.ProjectID, teamParam, parentParam, in.Title, in.Body, in.Principal,
	).Scan(&rec.ID, &rec.ProjectID, &teamOut, &parentOut, &rec.Title, &body, &rec.State, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.CreateWorkItem: insert: %w", err)
	}
	rec.TeamID = nullToPtr(teamOut)
	rec.ParentID = nullToPtr(parentOut)
	rec.Body = body.String

	if err := s.writeAudit(ctx, tx, rec.ID, "work_item_created", in.Principal, in.InitiatedByUserID, map[string]any{
		"title":     rec.Title,
		"parent_id": parentOut.String,
	}); err != nil {
		return WorkItemRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.CreateWorkItem: commit: %w", err)
	}
	return rec, nil
}

// UpdateWorkItem edits one item's editable fields (title/body/parent), atomically
// with a §6.5 'work_item_edited' audit row. State is never touched here.
//
// Semantics:
//   - (record, nil): the item now carries the requested fields.
//   - (zero, ErrInvalidWorkItem): no editable field supplied, or a reparent that is
//     self-referential or would form a cycle (400).
//   - (zero, ErrWorkItemNotFound): no such item (or a new parent) in the caller's
//     Team scope — existence-hiding (404).
//   - (zero, ErrStateConflict): ExpectedUpdatedAt did not match current — a racing
//     edit; the caller re-reads and retries (409).
//   - (zero, err): infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) UpdateWorkItem(ctx context.Context, workItemID string, in UpdateWorkItemInput) (WorkItemRecord, error) {
	if workItemID == "" || in.Principal == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: workItemID and principal are required", ErrInvalidWorkItem)
	}
	if in.Title == nil && in.Body == nil && in.ParentID == nil {
		return WorkItemRecord{}, fmt.Errorf("%w: no editable field supplied", ErrInvalidWorkItem)
	}
	if in.Title != nil && *in.Title == "" {
		return WorkItemRecord{}, fmt.Errorf("%w: title may not be blank", ErrInvalidWorkItem)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// (1) Lock + read the target; FOR UPDATE serialises concurrent edits/moves.
	var rec WorkItemRecord
	var teamOut, parentOut, bodyOut sql.NullString
	err = tx.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at
		  FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
		workItemID).Scan(&rec.ID, &rec.ProjectID, &teamOut, &parentOut, &rec.Title, &bodyOut, &rec.State, &rec.CreatedBy, &rec.CreatedAt, &rec.UpdatedAt)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return WorkItemRecord{}, ErrWorkItemNotFound
	case err != nil:
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: read current: %w", err)
	}

	// (2) Tenancy (§12.1): an item outside the caller's Team is 404, not 403.
	if in.TeamID != "" && (!teamOut.Valid || teamOut.String != in.TeamID) {
		return WorkItemRecord{}, ErrWorkItemNotFound
	}

	// (3) Optimistic-concurrency guard (edit-side analogue of fromState).
	if in.ExpectedUpdatedAt != "" {
		want, perr := time.Parse(time.RFC3339Nano, in.ExpectedUpdatedAt)
		if perr != nil {
			return WorkItemRecord{}, fmt.Errorf("%w: expectedUpdatedAt not RFC3339", ErrInvalidWorkItem)
		}
		if !rec.UpdatedAt.Equal(want) {
			return WorkItemRecord{}, fmt.Errorf("%w: item changed since %s", ErrStateConflict, in.ExpectedUpdatedAt)
		}
	}

	// (4) Reparent validation (only when a new parent is supplied and non-empty).
	if in.ParentID != nil && *in.ParentID != "" {
		if *in.ParentID == workItemID {
			return WorkItemRecord{}, fmt.Errorf("%w: an item cannot be its own parent", ErrInvalidWorkItem)
		}
		var newParentProject string
		var newParentTeam sql.NullString
		err = tx.QueryRowContext(ctx, `
			SELECT project_id, team_id FROM coord.work_item WHERE id = $1::uuid FOR UPDATE`,
			*in.ParentID).Scan(&newParentProject, &newParentTeam)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			return WorkItemRecord{}, ErrWorkItemNotFound
		case err != nil:
			return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: read new parent: %w", err)
		}
		if in.TeamID != "" && (!newParentTeam.Valid || newParentTeam.String != in.TeamID) {
			return WorkItemRecord{}, ErrWorkItemNotFound
		}
		if newParentProject != rec.ProjectID {
			return WorkItemRecord{}, ErrWorkItemNotFound // no cross-project reparent
		}
		// Cycle guard: the new parent must not be a descendant of the item (§6.1 —
		// the DB has only a cheap self-parent CHECK; the deep walk lives here).
		cyclic, cerr := ancestorIncludes(ctx, tx, *in.ParentID, workItemID)
		if cerr != nil {
			return WorkItemRecord{}, cerr
		}
		if cyclic {
			return WorkItemRecord{}, fmt.Errorf("%w: reparent would form a cycle", ErrInvalidWorkItem)
		}
	}

	// (5) Conditional UPDATE with a re-asserted updated_at CAS so a racing write that
	// slipped between the locked read and here is a conflict, never a silent clobber.
	newTitle := rec.Title
	if in.Title != nil {
		newTitle = *in.Title
	}
	newBody := bodyOut
	if in.Body != nil {
		newBody = sql.NullString{String: *in.Body, Valid: *in.Body != ""}
	}
	newParent := parentOut
	if in.ParentID != nil {
		newParent = sql.NullString{String: *in.ParentID, Valid: *in.ParentID != ""}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE coord.work_item
		   SET title = $2, body = $3, parent_id = $4::uuid, updated_at = now()
		 WHERE id = $1::uuid AND updated_at = $5`,
		workItemID, newTitle, newBody, newParent, rec.UpdatedAt)
	if err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: update: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: rows: %w", err)
	} else if n == 0 {
		return WorkItemRecord{}, fmt.Errorf("%w: concurrent edit", ErrStateConflict)
	}

	// (6) Read back the committed row so the caller re-syncs to server truth.
	var updatedAt time.Time
	if err := tx.QueryRowContext(ctx, `
		SELECT title, body, parent_id::text, updated_at FROM coord.work_item WHERE id = $1::uuid`,
		workItemID).Scan(&newTitle, &newBody, &newParent, &updatedAt); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: read back: %w", err)
	}

	if err := s.writeAudit(ctx, tx, workItemID, "work_item_edited", in.Principal, in.InitiatedByUserID, editedFields(in)); err != nil {
		return WorkItemRecord{}, err
	}
	if err := tx.Commit(); err != nil {
		return WorkItemRecord{}, fmt.Errorf("coord.UpdateWorkItem: commit: %w", err)
	}

	rec.Title = newTitle
	rec.Body = newBody.String
	rec.ParentID = nullToPtr(newParent)
	rec.TeamID = nullToPtr(teamOut)
	rec.UpdatedAt = updatedAt
	return rec, nil
}

// writeAudit inserts the §6.5 provenance row for a create/edit in the same txn,
// fence NULL (ADR-037: neither is a custody op).
func (s *WorkItemWriteStore) writeAudit(ctx context.Context, tx *sql.Tx, workItemID, eventType, principal, initiatedByUserID string, detail map[string]any) error {
	payload, err := json.Marshal(detail)
	if err != nil {
		return fmt.Errorf("coord.workItemWrite: audit payload: %w", err)
	}
	var initiatedBy sql.NullString
	if initiatedByUserID != "" {
		initiatedBy = sql.NullString{String: initiatedByUserID, Valid: true}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log (work_item_id, event_type, principal, initiated_by_user_id, payload)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb)`,
		workItemID, eventType, principal, initiatedBy, string(payload)); err != nil {
		return fmt.Errorf("coord.workItemWrite: audit: %w", err)
	}
	return nil
}

// ancestorIncludes walks parent_id from start upward and reports whether target is
// on the chain — the bounded cycle guard §6.1 defers to Go rather than a recursive
// SQL trigger. The depthCap is a belt-and-braces stop against a pre-existing cycle
// in the data (which the guards here prevent creating, but old rows might carry).
func ancestorIncludes(ctx context.Context, tx *sql.Tx, start, target string) (bool, error) {
	const depthCap = 256
	cur := start
	for i := 0; i < depthCap; i++ {
		if cur == target {
			return true, nil
		}
		var parent sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT parent_id::text FROM coord.work_item WHERE id = $1::uuid`, cur).Scan(&parent)
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("coord.workItemWrite: ancestor walk: %w", err)
		}
		if !parent.Valid || parent.String == "" {
			return false, nil // reached a root
		}
		cur = parent.String
	}
	return false, fmt.Errorf("coord.workItemWrite: ancestor walk exceeded depth %d (possible pre-existing cycle)", depthCap)
}

// editedFields is the audit detail for an edit: the set of fields the caller changed
// (names only — values live in the row, history need not duplicate them).
func editedFields(in UpdateWorkItemInput) map[string]any {
	fields := []string{}
	if in.Title != nil {
		fields = append(fields, "title")
	}
	if in.Body != nil {
		fields = append(fields, "body")
	}
	if in.ParentID != nil {
		fields = append(fields, "parent_id")
	}
	return map[string]any{"fields": fields}
}

func nullToPtr(n sql.NullString) *string {
	if !n.Valid || n.String == "" {
		return nil
	}
	v := n.String
	return &v
}
