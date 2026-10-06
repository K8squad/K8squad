// reviewread.go — ISI-5535 (E1 of ISI-5531, ADR-0026 §3.2): the CROSS-PROJECT, Team-scoped read of
// work items in the `in_review` lane — one of the three arms the "Needs Human Decision" Inbox unions.
//
// ListWorkItems (workitemread.go) is per-Project; the Inbox is cross-project, so this is a sibling
// read fenced only by team_id (empty ⇒ the trusted fleet-admin path, ISI-3937), with the SAME
// ADR-0024b `source = 'board'` allowlist that hides dispatch-on-mention thread-runs from human
// surfaces. It is a pure §13 board projection (no claim/lease/fence_token/custody move) — the handler
// joins each row to its latest run's claimedAt (in the informer-cache plane) for ordering, a join
// that cannot live in SQL because the ordering key is in a different store (ADR-0026 §1).
//
// NAMING (FR-B3): this file and its exported surface deliberately avoid "inbox"/chat-shaped names —
// frb3_no_chat_contract_test rejects them on pkg/coord. The Inbox aggregation + the per-user
// read-marker are HUMAN-facing concerns that live in the apiserver; coord exposes only this
// custody-free board read.

package coord

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ReviewItem is one `in_review` work item projected cross-project for the Inbox review arm. It
// carries the owning Project CR UID (project_id) so the apiserver can resolve the console-addressable
// "namespace/name" id, and updated_at as the ordering fallback when the item has no observed run yet
// (ADR-0026 §3.3). Assignee is the agent name the item was worked as ("" ⇒ honestly unassigned).
type ReviewItem struct {
	ID        string
	ProjectID string // Project CR UID (coord.work_item.project_id); apiserver resolves → "ns/name"
	Title     string
	UpdatedAt time.Time
	Assignee  string
}

// reviewItemLimit bounds the per-arm fan-out exactly as ListWorkItems bounds a project's card list
// (ADR-0026 §9: a fleet admin's read spans every team's in_review set, so cap it).
// ponytail: truncation at the cap is NOT signalled to the caller — 500 simultaneously-open
// in_review items (fleet-wide) is far past any real "needs human decision" inbox, so the newest
// 500 is the whole set in practice. Upgrade path if that ceiling is ever hit: fetch LIMIT+1 and
// surface a `truncated` flag in the response so the handler can log/paginate.
const reviewItemLimit = 500

// ListReviewItems returns every `in_review` board work item across all Projects the caller may see,
// newest-updated first. teamID scopes tenancy exactly like ListWorkItems — a scoped Team sees only
// its own items; an empty teamID is the trusted fleet-admin path (ISI-3937) and returns every team's.
// Read-only; the custody lanes are untouched.
func (s *WorkItemReadStore) ListReviewItems(ctx context.Context, teamID string) ([]ReviewItem, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT wi.id::text, wi.project_id::text, wi.title, wi.updated_at, c.assignee_agent
		  FROM coord.work_item wi
		  LEFT JOIN coord.claim c ON c.work_item_id = wi.id
		 WHERE wi.state = 'in_review'
		   AND ($1::uuid IS NULL OR wi.team_id = $1::uuid)
		   -- ADR-0024b §4.2 board-hide: dispatch-on-mention thread-runs (source='discussion') never
		   -- surface on a human decision surface. Allowlist semantics, like ListWorkItems.
		   AND wi.source = 'board'
		 ORDER BY wi.updated_at DESC, wi.id
		 LIMIT $2`, nullUUID(teamID), reviewItemLimit)
	if err != nil {
		return nil, fmt.Errorf("coord.ListReviewItems: list: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []ReviewItem
	for rows.Next() {
		var it ReviewItem
		var assignee sql.NullString
		if err := rows.Scan(&it.ID, &it.ProjectID, &it.Title, &it.UpdatedAt, &assignee); err != nil {
			return nil, fmt.Errorf("coord.ListReviewItems: scan: %w", err)
		}
		it.Assignee = assignee.String
		out = append(out, it)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("coord.ListReviewItems: iterate: %w", err)
	}
	return out, nil
}
