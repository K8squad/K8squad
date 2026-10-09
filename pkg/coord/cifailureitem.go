// cifailureitem.go — the ISI-5595 WS-C atomic create-if-absent used by the system
// CI-failure triage dispatch adapter. It is the sibling of reviewitem.go
// (EnsureReviewWorkItem) and closes the SAME dedup race: the repo-sync reconcile
// is level-triggered, so two parallel passes (a redelivered webhook + a poll
// tick) can both decide the same failing check run needs triage at the same
// instant. A naive find-then-create would double-create the triage work item.
// EnsureCIFailureWorkItem serialises those passes on the (project, dedup-label)
// pair with a transaction-scoped Postgres advisory lock, so exactly one pass
// inserts and the other observes the existing row.
//
// It reuses the SAME insert + §6.5 audit shape as CreateWorkItem / the review
// variant (a 'backlog' row, fence NULL), so a triage item is byte-identical to
// any other work item on the board — the only distinguishing marks are its dedup
// label and the audit payload's source:"ci-failure". The item is authored under
// the SYSTEM principal the caller passes (never an agent identity): this store op
// has no notion of human-vs-agent custody — the human-only board wall lives at
// the HTTP layer and the agent-authoring lane is a separate method. A
// system-executed standing policy is a third, distinct writer.
package coord

import (
	"context"
	"database/sql"
	"fmt"
)

// EnsureCIFailureWorkItemInput is one idempotent CI-failure triage-item create.
// ProjectID, Title, Principal and DedupLabel are all required. TeamID is the
// coord Team the item belongs to (Team CR uid; may be "" only on an unscoped dev
// host). Principal is the system provenance the row is authored under. DedupLabel
// is the idempotency key stored as a label; ExtraLabels are additional
// descriptive labels.
type EnsureCIFailureWorkItemInput struct {
	ProjectID         string
	TeamID            string
	Title             string
	Body              string
	DedupLabel        string
	ExtraLabels       []string
	Principal         string
	InitiatedByUserID string
}

// EnsureCIFailureWorkItemResult reports the item plus whether THIS call inserted
// it (Created=true) or found an existing row carrying the dedup label
// (Created=false). The caller distinguishes a fresh create (needs dispatch) from
// an idempotent no-op, and — because Item.State is returned — can self-heal a row
// that was created but never advanced past 'backlog' by a prior failed pass.
type EnsureCIFailureWorkItemResult struct {
	Item    WorkItemRecord
	Created bool
}

// EnsureCIFailureWorkItem inserts the triage work item if no item in ProjectID
// already carries DedupLabel, atomically with a §6.5 'work_item_created' audit
// row (fence NULL, source:"ci-failure"). The whole check-and-insert runs under a
// transaction-scoped advisory lock keyed on (ProjectID, DedupLabel) so concurrent
// reconciles cannot double-create.
//
// Semantics match EnsureReviewWorkItem:
//   - (result{Created:true}, nil):  the item was inserted in 'backlog'.
//   - (result{Created:false}, nil): an item with DedupLabel already existed; its
//     current record (including State) is returned unchanged.
//   - (zero, ErrInvalidWorkItem):   a required field was empty, a bad enum/label
//     set, or the dedup label did not survive normalization (400).
//   - (zero, err):                  infrastructure failure; nothing was written.
func (s *WorkItemWriteStore) EnsureCIFailureWorkItem(ctx context.Context, in EnsureCIFailureWorkItemInput) (EnsureCIFailureWorkItemResult, error) {
	if in.ProjectID == "" || in.Title == "" || in.Principal == "" || in.DedupLabel == "" {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("%w: projectID, title, principal and dedupLabel are required", ErrInvalidWorkItem)
	}
	// The dedup label goes first so it is never squeezed out by the maxLabels
	// bound; normalize validates enum-free labels the same way a human create does.
	rawLabels := append([]string{in.DedupLabel}, in.ExtraLabels...)
	_, _, labels, err := normalizeCreateFields("", "", rawLabels)
	if err != nil {
		return EnsureCIFailureWorkItemResult{}, err
	}
	// Guard against a silent dedup break: if trimming/bounding dropped the dedup
	// label the whole idempotency contract is void, so fail closed rather than
	// insert an item that a later lookup can never find.
	if !containsString(labels, in.DedupLabel) {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("%w: dedup label %q dropped by normalization", ErrInvalidWorkItem, in.DedupLabel)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	// Serialise concurrent ensures for the same (project, label). The two-int form
	// takes the project hash and the label hash so unrelated check runs never
	// contend; the lock is held only for this txn and released on commit/rollback.
	if _, err := tx.ExecContext(ctx,
		`SELECT pg_advisory_xact_lock(hashtext($1), hashtext($2))`, in.ProjectID, in.DedupLabel); err != nil {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: lock: %w", err)
	}

	// Already present? (Under the lock, so this is the authoritative answer.)
	existing, found, err := scanReviewItem(tx.QueryRowContext(ctx, `
		SELECT id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at, priority, work_mode, labels
		FROM coord.work_item
		WHERE project_id = $1::uuid AND labels @> ARRAY[$2]::text[]
		ORDER BY created_at ASC
		LIMIT 1`, in.ProjectID, in.DedupLabel))
	if err != nil {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: lookup: %w", err)
	}
	if found {
		if err := tx.Commit(); err != nil {
			return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: commit: %w", err)
		}
		return EnsureCIFailureWorkItemResult{Item: existing, Created: false}, nil
	}

	teamParam := sql.NullString{}
	if in.TeamID != "" {
		teamParam = sql.NullString{String: in.TeamID, Valid: true}
	}

	rec, found, err := scanReviewItem(tx.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, body, state, created_by, priority, work_mode, labels)
		VALUES ($1::uuid, $2::uuid, NULL, $3, NULLIF($4,''), 'backlog', $5, NULL, NULL, $6)
		RETURNING id::text, project_id::text, team_id::text, parent_id::text, title, body, state, created_by, created_at, updated_at, priority, work_mode, labels`,
		in.ProjectID, teamParam, in.Title, in.Body, in.Principal, labels,
	))
	if err != nil {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: insert: %w", err)
	}
	if !found {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: insert returned no row")
	}

	if err := s.writeAudit(ctx, tx, rec.ID, "work_item_created", in.Principal, in.InitiatedByUserID, map[string]any{
		"title":  rec.Title,
		"labels": labels,
		"source": "ci-failure",
	}); err != nil {
		return EnsureCIFailureWorkItemResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return EnsureCIFailureWorkItemResult{}, fmt.Errorf("coord.EnsureCIFailureWorkItem: commit: %w", err)
	}
	return EnsureCIFailureWorkItemResult{Item: rec, Created: true}, nil
}
