// proddispatch.go — the §2.9 FR-B3 delegation-with-feedback coordinator
// dispatch BOUND TO THE PRODUCTION SCHEMA (Story 2.9 / ISI-2526). Where
// prodclaim.go (Story 2.2) binds the §6.2 claim, this file binds the loop that
// rides on top of it:
//
//	read-of-record → coordinator DECIDES+PRIORITIZES → new FENCED dispatch
//
// FR-B3: a coordinator Agent (Role=squad lead) creates dependent work items.
// When a dependency completes, the completing Run's handoff is surfaced to the
// coordinator VIA THE COORDINATION RECORD (comments, artifacts, audit — §6.1,
// optionally enriched by §6.6 scoped memory recall at the caller's edge); the
// coordinator then defines the next work item. There is NO agent-to-agent
// channel — structurally: 0001 ships no message table, and DispatchNext below
// has NO parameter that can carry worker-authored content. The only path B's
// words can take into A's decision is a row in the shared record.
//
// Three properties hold by construction here and are proven with differential
// teeth on the shipped DDL by TestSpineProdDispatch (D1..D4) in the same
// required spine-chaos gate as C1..C7/P1..P2:
//
//	P1  no B→A channel — ReadHandoff is the single, read-only view of what the
//	    record yields about the completed item; the decision write accepts only
//	    record identifiers and the COORDINATOR's own title/body.
//	P2  the coordinator DEFINES and PRIORITIZES the next item. B's
//	    recommended_next is ADVISORY, never executed: AdoptRecommendation only
//	    drafts, the coordinator still authors the dispatch, and the created
//	    item's created_by IS the coordinator principal (audited).
//	P3  NO custody transfer. The dispatch never touches coord.claim: the new
//	    item is created in the todo lane where the F3 trigger provisions a
//	    FRESH unheld claim row at fence 0, and the next worker acquires it via
//	    the §6.2 claim (ProdClaimer) with its own fence bump — it never
//	    inherits the completing run's fence, lease or holder identity.
//
// Idempotency (§6.4 discipline, harness C6): one next item per (completed
// source item, completing run), enforced STRUCTURALLY by the coord.dispatch
// marker (db/migrations/0002_coord_dispatch.sql) — INSERT … ON CONFLICT DO
// NOTHING inside the dispatch transaction, so re-drives and concurrent
// coordinators converge on the first writer's item.
//
// Story boundary notes (consumed, not redefined here):
//   - The structured handoff artifact itself is Story 2.8's (ISI-2525); this
//     file pins only its read-side contract: artifact kind "handoff" whose
//     content is the HandoffDoc JSON, fetched through the injectable
//     ArtifactContent resolver (the object-store binding is 2.8/8.x wiring).
//   - Squad-lead AUTHORIZATION is admission's concern (Role CRD / apiserver
//     RBAC): the spine treats CoordinatorPrincipal as pre-authorized and
//     records it for §6.5 provenance.
//   - §6.6 scoped memory recall is an enrichment the caller may fold into its
//     DispatchDecision; nothing in the loop depends on it.
package coord

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

// HandoffKind is the pinned artifact kind of the structured handoff (Story 2.8
// contract). The artifact row lives in coord.artifact; its uri addresses the
// HandoffDoc JSON payload resolved via ArtifactContent.
const HandoffKind = "handoff"

// DraftWorkItem is a proposed work item — content only, never a dispatch. B's
// recommended_next items and the coordinator's candidate list both use it; the
// distinction is WHO dispatches (P2), not the shape.
type DraftWorkItem struct {
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// ArtifactRef mirrors the coord.artifact columns the record yields (§6.1/§6.4).
type ArtifactRef struct {
	Kind   string `json:"kind"`
	URI    string `json:"uri"`
	SHA256 string `json:"sha256"`
}

// RecordComment is one append-only §6.1/§6.5 comment row.
type RecordComment struct {
	Author    string `json:"author"`
	Body      string `json:"body"`
	CreatedAt string `json:"created_at"`
}

// HandoffDoc is the structured, ADVISORY-ONLY handoff payload Story 2.8 pins:
// what the completing Run learned (findings), what it would do next
// (recommended_next — a draft list the coordinator may adopt, reorder or
// discard), and which artifacts downstream work should consume.
type HandoffDoc struct {
	Findings               string          `json:"findings"`
	RecommendedNext        []DraftWorkItem `json:"recommended_next"`
	ArtifactsForDownstream []ArtifactRef   `json:"artifacts_for_downstream"`
}

// HandoffView is everything the coordination record yields about a completed
// item — the ONLY input, besides the coordinator's own judgement, that feeds a
// §2.9 dispatch decision (P1).
type HandoffView struct {
	SourceItemID string
	State        string
	// CompletingRunID is the run holding the item's claim row — the §6.3
	// completion evidence the dispatch validates against.
	CompletingRunID string
	Comments        []RecordComment
	Artifacts       []ArtifactRef
	// Handoff is the parsed structured artifact (kind "handoff"), or nil when
	// the completing run left none — the loop degrades to record-only input,
	// it never blocks on the artifact (2.8 lands the writer separately).
	Handoff *HandoffDoc
}

// DispatchDecision is the coordinator's OWN definition of the next work item
// (P2). No field here can carry worker-authored content: Title and Body are the
// coordinator's words, the identifiers reference record rows.
type DispatchDecision struct {
	// CoordinatorPrincipal is the squad-lead principal deciding this dispatch.
	// Pre-authorized at admission (Role CRD); recorded on the created item and
	// in the §6.5 audit row.
	CoordinatorPrincipal string

	// SourceWorkItemID is the completed item whose handoff feeds this dispatch.
	SourceWorkItemID string

	// SourceRunID is the run expected to have completed SourceWorkItemID —
	// validated against the record (claim row) before anything is written.
	SourceRunID string

	// Title and Body are the next item's content, authored by the coordinator.
	// They MAY derive from B's advisory recommendation (the coordinator chose
	// to adopt it) — AdvisoryFollowed records that provenance for audit.
	Title            string
	Body             string
	AdvisoryFollowed bool

	// ParentToSource creates the next item as a CHILD of the completed source
	// (dependency lineage, §6.1 adjacency list). The tenancy trigger then
	// inherits the source's project/team.
	ParentToSource bool
}

// DispatchResult reports the outcome of a dispatch.
type DispatchResult struct {
	// CreatedWorkItemID is the todo-lane item this dispatch (or a prior,
	// deduped twin) created — the id every concurrent caller converges on.
	CreatedWorkItemID string
	// AlreadyDispatched is true when a prior dispatch for the same
	// (source item, completing run) had already landed; this call then made NO
	// change and returns the existing item (§6.4 idempotent re-entry).
	AlreadyDispatched bool
}

// Sentinel errors for the dispatch guards. Callers match with errors.Is.
var (
	// ErrSourceNotComplete: the source item is not in the done lane — the
	// §2.9 loop dispatches on COMPLETION evidence, never on a promise.
	ErrSourceNotComplete = errors.New("coord: dispatch source is not complete (state != done)")
	// ErrCompletingRunMismatch: the source item's claim row names a different
	// completing run than the decision asserts — stale or forged evidence.
	ErrCompletingRunMismatch = errors.New("coord: dispatch source completing-run mismatch")
)

// ArtifactContent resolves an artifact uri to its bytes. The production
// object-store binding is Story 2.8/8.x wiring; tests inject an in-memory
// resolver. Returning an error propagates as a read failure — the loop never
// silently treats an unreadable handoff as an absent one.
type ArtifactContent func(ctx context.Context, uri string) ([]byte, error)

// ProdDispatcher executes the §2.9 coordinator dispatch against the shipped
// coord schema (0001 + 0002). It holds no mutable Go state beyond the *sql.DB,
// its pinned statements and the optional content resolver, so its methods are
// safe for concurrent use by many goroutines (each dispatch opens its own
// transaction).
type ProdDispatcher struct {
	db      *sql.DB
	content ArtifactContent
}

// NewProdDispatcher binds the coordinator dispatch to db. content may be nil —
// a nil resolver yields record-only HandoffViews (Handoff == nil) until the
// 2.8 writer and its object-store binding land.
func NewProdDispatcher(db *sql.DB, content ArtifactContent) (*ProdDispatcher, error) {
	if db == nil {
		return nil, errors.New("coord.NewProdDispatcher: nil db")
	}
	return &ProdDispatcher{db: db, content: content}, nil
}

// ReadHandoff assembles the read-only record view of a completed item: its
// lane, its completing run (claim row), every comment and artifact row, and —
// when a kind "handoff" artifact exists and a content resolver is bound — the
// parsed HandoffDoc. This read is P1's single legitimate surface: everything
// the coordinator may know about B's outcome comes from these rows.
func (d *ProdDispatcher) ReadHandoff(ctx context.Context, sourceWorkItemID string) (HandoffView, error) {
	v := HandoffView{SourceItemID: sourceWorkItemID}

	err := d.db.QueryRowContext(ctx, `
		SELECT w.state, c.run_id::text
		  FROM coord.work_item w
		  JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.id = $1::uuid`, sourceWorkItemID).Scan(&v.State, &v.CompletingRunID)
	if errors.Is(err, sql.ErrNoRows) {
		return v, fmt.Errorf("coord.ReadHandoff: no work item %s", sourceWorkItemID)
	} else if err != nil {
		return v, fmt.Errorf("coord.ReadHandoff: read item: %w", err)
	}

	rows, err := d.db.QueryContext(ctx, `
		SELECT author_principal, body, created_at::text
		  FROM coord.comment
		 WHERE work_item_id = $1::uuid
		 ORDER BY created_at, id`, sourceWorkItemID)
	if err != nil {
		return v, fmt.Errorf("coord.ReadHandoff: comments: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var c RecordComment
		if err := rows.Scan(&c.Author, &c.Body, &c.CreatedAt); err != nil {
			return v, fmt.Errorf("coord.ReadHandoff: scan comment: %w", err)
		}
		v.Comments = append(v.Comments, c)
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("coord.ReadHandoff: comments iteration: %w", err)
	}

	rows, err = d.db.QueryContext(ctx, `
		SELECT kind, uri, sha256
		  FROM coord.artifact
		 WHERE work_item_id = $1::uuid
		 ORDER BY created_at, id`, sourceWorkItemID)
	if err != nil {
		return v, fmt.Errorf("coord.ReadHandoff: artifacts: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a ArtifactRef
		if err := rows.Scan(&a.Kind, &a.URI, &a.SHA256); err != nil {
			return v, fmt.Errorf("coord.ReadHandoff: scan artifact: %w", err)
		}
		v.Artifacts = append(v.Artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return v, fmt.Errorf("coord.ReadHandoff: artifacts iteration: %w", err)
	}

	// Resolve the structured handoff, if the record holds one and a resolver
	// is bound. Absence is not an error — the loop is record-driven, not
	// artifact-driven.
	if d.content != nil {
		for _, a := range v.Artifacts {
			if a.Kind != HandoffKind {
				continue
			}
			raw, err := d.content(ctx, a.URI)
			if err != nil {
				return v, fmt.Errorf("coord.ReadHandoff: resolve %s artifact %s: %w", HandoffKind, a.URI, err)
			}
			var doc HandoffDoc
			if err := json.Unmarshal(raw, &doc); err != nil {
				return v, fmt.Errorf("coord.ReadHandoff: parse %s artifact %s: %w", HandoffKind, a.URI, err)
			}
			v.Handoff = &doc
			break
		}
	}
	return v, nil
}

// AdoptRecommendation returns B's recommended_next drafts from the view —
// ADVISORY ONLY (P2). It exists to make the boundary explicit: adopting the
// recommendation is the coordinator's explicit choice, expressed by passing the
// drafts' content into ITS OWN DispatchDecision; nothing in this package ever
// converts a recommendation into a dispatch.
func AdoptRecommendation(v HandoffView) []DraftWorkItem {
	if v.Handoff == nil {
		return nil
	}
	return v.Handoff.RecommendedNext
}

// DispatchNextOfRecord executes the §2.9 loop's write half in ONE transaction:
//
//	(1) read-of-record guard: the source item is done AND its claim row names
//	    the asserted completing run (completion evidence, not a promise);
//	(2) INSERT the next work item — created_by = the coordinator, todo lane —
//	    so the F3 trigger provisions a FRESH unheld claim row at fence 0 (P3:
//	    no custody transfer; the completing run's claim row is never touched);
//	(3) INSERT the coord.dispatch marker (dedupe key
//	    "<source>:<run>:next") ON CONFLICT DO NOTHING — on conflict the whole
//	    speculative transaction rolls back and the existing winner's item is
//	    returned (§6.4 idempotency, D4);
//	(4) append the §6.5 audit row (event coordinator_dispatched) with the
//	    decision provenance, committing atomically with the item + marker.
//
// The dispatch NEVER writes coord.claim: custody for the new item begins, and
// can only begin, with a §6.2 acquire by the next worker.
func (d *ProdDispatcher) DispatchNextOfRecord(ctx context.Context, dec DispatchDecision) (DispatchResult, error) {
	if dec.CoordinatorPrincipal == "" || dec.SourceWorkItemID == "" || dec.SourceRunID == "" || dec.Title == "" {
		return DispatchResult{}, fmt.Errorf(
			"coord.DispatchNextOfRecord: CoordinatorPrincipal, SourceWorkItemID, SourceRunID and Title are all required (got %+v)", dec)
	}
	dedupe := fmt.Sprintf("%s:%s:next", dec.SourceWorkItemID, dec.SourceRunID)

	tx, err := d.db.BeginTx(ctx, nil)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	// (1) read-of-record guard.
	var state string
	var completingRun string
	var parent any // NULL when not parenting — bound parameter stays typed
	if dec.ParentToSource {
		parent = dec.SourceWorkItemID
	}
	err = tx.QueryRowContext(ctx, `
		SELECT w.state, c.run_id::text
		  FROM coord.work_item w
		  JOIN coord.claim c ON c.work_item_id = w.id
		 WHERE w.id = $1::uuid
		 FOR UPDATE OF w`, dec.SourceWorkItemID).Scan(&state, &completingRun)
	if errors.Is(err, sql.ErrNoRows) {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: no source work item %s", dec.SourceWorkItemID)
	} else if err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: read source: %w", err)
	}
	if state != "done" {
		return DispatchResult{}, ErrSourceNotComplete
	}
	if completingRun != dec.SourceRunID {
		return DispatchResult{}, fmt.Errorf("%w: claim names run %s, decision asserts %s",
			ErrCompletingRunMismatch, completingRun, dec.SourceRunID)
	}

	// (2) the coordinator-authored next item. project/team are inherited from
	// the source row itself (and, when parenting, re-checked by the tenancy
	// trigger), so the dependency stays inside one §12.1 tenancy predicate.
	var created string
	if err := tx.QueryRowContext(ctx, `
		INSERT INTO coord.work_item (project_id, team_id, parent_id, title, body, state, created_by)
		SELECT s.project_id, s.team_id, $2::uuid, $3, $4, 'todo', $5
		  FROM coord.work_item s
		 WHERE s.id = $1::uuid
		 RETURNING id::text`,
		dec.SourceWorkItemID, parent, dec.Title, dec.Body, dec.CoordinatorPrincipal).Scan(&created); err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: create next item: %w", err)
	}

	// (3) §6.4 marker — first writer wins; the loser's speculative item
	// evaporates with this transaction's rollback.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO coord.dispatch
		       (dedupe_key, source_work_item_id, source_run_id, created_work_item_id, coordinator_principal)
		VALUES ($1, $2::uuid, $3::uuid, $4::uuid, $5)
		ON CONFLICT (dedupe_key) DO NOTHING`,
		dedupe, dec.SourceWorkItemID, dec.SourceRunID, created, dec.CoordinatorPrincipal)
	if err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: marker: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// A prior dispatch for this (source, run) already exists: converge on
		// it. Roll back the speculative item and read the winner back.
		var existing string
		if err := d.db.QueryRowContext(ctx,
			`SELECT created_work_item_id::text FROM coord.dispatch WHERE dedupe_key = $1`, dedupe,
		).Scan(&existing); err != nil {
			return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: read existing dispatch: %w", err)
		}
		return DispatchResult{CreatedWorkItemID: existing, AlreadyDispatched: true}, nil
	}

	// (4) §6.5 provenance — what was decided, from which completion, and
	// whether B's advisory recommendation was adopted (audited override).
	payload, err := json.Marshal(map[string]any{
		"source_work_item_id":  dec.SourceWorkItemID,
		"source_run_id":        dec.SourceRunID,
		"created_work_item_id": created,
		"dedupe_key":           dedupe,
		"advisory_followed":    dec.AdvisoryFollowed,
	})
	if err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: audit payload: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO coord.audit_log
		       (work_item_id, run_id, event_type, principal, from_state, to_state, payload)
		VALUES ($1::uuid, $2::uuid, 'coordinator_dispatched', $3, 'done', 'todo', $4::jsonb)`,
		dec.SourceWorkItemID, dec.SourceRunID, dec.CoordinatorPrincipal, string(payload)); err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: audit: %w", err)
	}
	// fence_token is deliberately NULL: a dispatch is decided by the
	// coordinator READING the record, never by holding a claim — there is no
	// custody under which it occurs (P3).

	if err := tx.Commit(); err != nil {
		return DispatchResult{}, fmt.Errorf("coord.DispatchNextOfRecord: commit: %w", err)
	}
	return DispatchResult{CreatedWorkItemID: created}, nil
}
